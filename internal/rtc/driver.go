package rtc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"ooi/internal/code"
	"ooi/internal/rendezvous"
	"ooi/internal/seal"
	"ooi/internal/vdec"
)

// Rendezvous is the subset of the rendezvous client the driver uses.
type Rendezvous interface {
	Publish(ctx context.Context, p rendezvous.PublishParams) error
	PollAnswer(ctx context.Context, id, session string) (answer string, ok bool, err error)
	Delete(ctx context.Context, id string) error
	RelayURL(id, session, owner string) string
}

// DriverConfig configures the reconnect loop.
type DriverConfig struct {
	Code       string
	Order      []vdec.Codec
	ICEServers []webrtc.ICEServer
	Threads    int

	Sink Sink
	Rz   Rendezvous
	Log  Logger

	// OnConnect / OnDisconnect fire when a session becomes live and when it
	// ends. Used to drive the overlay's placeholder and any UI.
	OnConnect    func(Stats)
	OnDisconnect func()

	// NoRelay stops advertising the relay (pages then only try direct).
	NoRelay bool

	// Timings; zero values use the defaults below.
	GatherTimeout    time.Duration // offer ICE gathering cap: 8s
	HandshakeTimeout time.Duration // 30s after an answer, until the first frame
	PollInterval     time.Duration // 2s
	KnockMinAge      time.Duration // 15s before honouring a knock
}

func (c *DriverConfig) defaults() {
	if c.GatherTimeout <= 0 {
		c.GatherTimeout = 8 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 30 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.KnockMinAge <= 0 {
		c.KnockMinAge = 15 * time.Second
	}
	if len(c.Order) == 0 {
		c.Order = DefaultCodecOrder
	}
}

// Driver owns the whole receiver lifecycle: publish, wait, stream, re-offer.
type Driver struct {
	cfg   DriverConfig
	api   *webrtc.API
	owner string // proves to the relay that this host published the offer

	mu          sync.Mutex
	current     *session
	failedJoins uint64
	lastFailure string
	lastFailAt  time.Time
}

// NewDriver builds a driver. It fails only if the media engine cannot be built.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	cfg.defaults()
	api, err := newAPI(cfg.Order)
	if err != nil {
		return nil, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg, api: api, owner: hex.EncodeToString(b[:])}, nil
}

// Stats returns the live session's stats, or the zero value when idle, plus
// the driver-wide failed-join record.
func (d *Driver) Stats() Stats {
	d.mu.Lock()
	s := d.current
	st := Stats{FailedJoins: d.failedJoins, LastFailure: d.lastFailure, LastFailureAt: d.lastFailAt}
	d.mu.Unlock()
	if s != nil {
		cur := s.Stats()
		cur.FailedJoins, cur.LastFailure, cur.LastFailureAt = st.FailedJoins, st.LastFailure, st.LastFailureAt
		st = cur
	}
	return st
}

// failedJoin records a join that signalled but never got a picture on screen.
func (d *Driver) failedJoin(reason string) {
	d.mu.Lock()
	d.failedJoins++
	d.lastFailure = reason
	d.lastFailAt = time.Now()
	d.mu.Unlock()
	d.cfg.Log.Printf("join failed: %s", reason)
}

// outcome is how one publish/handshake/stream cycle ended.
type outcome int

const (
	outError  outcome = iota // could not publish or poll: back off
	outFailed                // a joiner answered but no picture arrived: re-offer now
	outGood                  // was live
	outKnock                 // superseded before going live: re-offer now
)

// Run drives connections until ctx is cancelled. The same code is republished
// after every drop, so the joiner rejoins with no new code. Backoff grows only
// on repeated service errors; a failed join re-offers at once.
func (d *Driver) Run(ctx context.Context) error {
	roomID := code.RoomID(d.cfg.Code)

	// Withdraw the code on the way out so joiners stop answering a dead offer.
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		d.cfg.Rz.Delete(dctx, roomID)
	}()

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		out, err := d.oneSession(ctx, roomID)
		d.mu.Lock()
		d.current = nil
		d.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			d.cfg.Log.Printf("session ended: %v", err)
		}
		wait := 500 * time.Millisecond
		switch out {
		case outGood:
			backoff = 2 * time.Second // a good session resets the penalty
			d.cfg.Sink.Status("waiting for your friend to reconnect")
		case outError:
			wait = backoff
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
		if d.cfg.OnDisconnect != nil {
			d.cfg.OnDisconnect()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return ctx.Err()
}

// oneSession runs a single publish/handshake/stream cycle.
func (d *Driver) oneSession(ctx context.Context, roomID string) (outcome, error) {
	sessionID, err := code.NewSession()
	if err != nil {
		return outError, err
	}

	sess := newSession(d.api, d.cfg.Order, d.cfg.Sink, d.cfg.Threads, d.cfg.Log)
	defer sess.Close()
	d.mu.Lock()
	d.current = sess
	d.mu.Unlock()

	d.cfg.Sink.Status("publishing your code")
	offerSDP, err := sess.createOffer(ctx, d.cfg.GatherTimeout, d.cfg.ICEServers)
	if err != nil {
		return outError, err
	}
	sealed, err := seal.Seal(offerSDP, d.cfg.Code)
	if err != nil {
		return outError, err
	}
	var caps []string
	if !d.cfg.NoRelay {
		caps = []string{"relay"}
	}
	if err := d.cfg.Rz.Publish(ctx, rendezvous.PublishParams{
		ID: roomID, Session: sessionID, Offer: sealed, Owner: d.owner, Caps: caps,
	}); err != nil {
		return outError, err
	}

	d.cfg.Sink.Status("ready - waiting for your friend")
	answer, err := d.waitAnswer(ctx, roomID, sessionID)
	if err != nil {
		return outError, err
	}

	// The handshake clock starts at the answer, never at the offer: an offer
	// can wait minutes for a joiner. During it the page may give up on a
	// direct path and ask for the relay on this same session.
	answeredAt := time.Now()
	relayed := false
	if out, ok := d.apply(sess, roomID, sessionID, answer, &relayed); !ok {
		return out, nil
	}
	d.cfg.Sink.Status("connecting")
	poll := time.NewTicker(d.cfg.PollInterval)
	defer poll.Stop()
	timeout := time.NewTimer(d.cfg.HandshakeTimeout)
	defer timeout.Stop()
	for waiting := true; waiting; {
		select {
		case <-sess.decoded:
			waiting = false
		case <-sess.failed:
			if relayed {
				d.failedJoin("relay closed before the first frame")
			} else {
				d.failedJoin("could not connect directly")
			}
			return outFailed, nil
		case <-timeout.C:
			if relayed {
				d.failedJoin("no picture through the relay within 30s")
			} else {
				d.failedJoin("could not connect directly; a current page falls back to the relay")
			}
			return outFailed, nil
		case <-ctx.Done():
			return outError, ctx.Err()
		case <-poll.C:
			ans, ok, err := d.cfg.Rz.PollAnswer(ctx, roomID, sessionID)
			if err == rendezvous.ErrRoomGone {
				return outError, errRoomGone
			}
			if err != nil || !ok {
				continue
			}
			if relayed {
				d.cfg.Log.Printf("knock during relay handshake; re-offering")
				return outKnock, nil
			}
			wasRelay := isRelayRequest(ans, d.cfg.Code)
			if !wasRelay {
				d.cfg.Log.Printf("knock during handshake; re-offering")
				return outKnock, nil
			}
			d.cfg.Log.Printf("the page could not reach us directly (%s after its answer); switching to the relay",
				time.Since(answeredAt).Round(100*time.Millisecond))
			sess.closePeer()
			if out, ok := d.apply(sess, roomID, sessionID, ans, &relayed); !ok {
				return out, nil
			}
			timeout.Reset(d.cfg.HandshakeTimeout)
		}
	}
	d.cfg.Log.Printf("live (%s): first frame %s after the answer", sess.Stats().Path,
		time.Since(answeredAt).Round(100*time.Millisecond))
	if d.cfg.OnConnect != nil {
		d.cfg.OnConnect(sess.Stats())
	}

	// Live. Watch for the session ending or for a knock (a new, different
	// answer under the same session = someone else wants in).
	return outGood, d.streamUntilDone(ctx, sess, roomID, sessionID, time.Now())
}

// apply opens a sealed answer and starts either the WebRTC handshake or the
// relay. ok is false when the answer was unusable.
func (d *Driver) apply(sess *session, roomID, sessionID, answer string, relayed *bool) (outcome, bool) {
	plain, err := seal.Open(answer, d.cfg.Code)
	if err != nil {
		d.cfg.Log.Printf("answer rejected: %v", err) // not ours
		return outKnock, false
	}
	if strings.HasPrefix(plain, "{") {
		// The page's network cannot reach ours directly, so it asked for the
		// relay; its sealed answer carries the relay key instead of an SDP.
		key, ok := relayKey(plain)
		if !ok {
			d.cfg.Log.Printf("answer rejected: malformed relay request")
			return outKnock, false
		}
		*relayed = true
		d.cfg.Sink.Status("connecting through the relay")
		go sess.runRelay(d.cfg.Rz.RelayURL(roomID, sessionID, d.owner), key)
		return 0, true
	}
	if err := sess.acceptAnswer(plain); err != nil {
		d.cfg.Log.Printf("answer not applied: %v", err)
		return outKnock, false
	}
	return 0, true
}

func relayKey(plain string) ([]byte, bool) {
	var req struct {
		Relay int    `json:"relay"`
		Key   string `json:"key"`
	}
	if json.Unmarshal([]byte(plain), &req) != nil || req.Relay != 1 {
		return nil, false
	}
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(req.Key, "="))
	if err != nil || len(key) != 32 {
		return nil, false
	}
	return key, true
}

func isRelayRequest(answer, joinCode string) bool {
	plain, err := seal.Open(answer, joinCode)
	return err == nil && strings.HasPrefix(plain, "{")
}

// waitAnswer polls the session's answer slot until one arrives. A 404 means the
// room evaporated (expired/deleted) and the session must republish.
func (d *Driver) waitAnswer(ctx context.Context, roomID, sessionID string) (string, error) {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()
	for {
		ans, ok, err := d.cfg.Rz.PollAnswer(ctx, roomID, sessionID)
		if err == rendezvous.ErrRoomGone {
			return "", errRoomGone
		}
		if err == nil && ok {
			return ans, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// streamUntilDone blocks while the session is live. It keeps polling the answer
// slot: a NEW answer is a knock from another joiner (or the same page asking
// for the relay after a drop) and ends the session so it can re-offer.
func (d *Driver) streamUntilDone(ctx context.Context, sess *session, roomID, sessionID string, since time.Time) error {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.failed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			// Only honour a knock once the session has had time to settle, so
			// two joiners holding the code alternate slowly instead of evicting
			// each other on connect.
			if time.Since(since) < d.cfg.KnockMinAge {
				continue
			}
			if _, ok, err := d.cfg.Rz.PollAnswer(ctx, roomID, sessionID); err == nil && ok {
				d.cfg.Log.Printf("knock: another joiner wants in; re-offering")
				return nil
			} else if err == rendezvous.ErrRoomGone {
				return errRoomGone
			}
		}
	}
}

// errRoomGone signals the room expired or was deleted; the driver republishes.
var errRoomGone = errorString("rtc: room gone")

type errorString string

func (e errorString) Error() string { return string(e) }
