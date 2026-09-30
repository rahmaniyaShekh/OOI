package rtc

import (
	"context"
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
	Publish(ctx context.Context, id, session, offer string) error
	PollAnswer(ctx context.Context, id, session string) (answer string, ok bool, err error)
	Delete(ctx context.Context, id string) error
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

	// Timings; zero values use the contract defaults from the spec.
	GatherTimeout    time.Duration // host: <= 15s
	HandshakeTimeout time.Duration // 15s after reading an answer
	PollInterval     time.Duration // 2s
	KnockMinAge      time.Duration // 15s before honouring a knock
}

func (c *DriverConfig) defaults() {
	if c.GatherTimeout <= 0 {
		c.GatherTimeout = 15 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 15 * time.Second
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
	cfg DriverConfig
	api *webrtc.API

	mu      sync.Mutex
	current *session
}

// NewDriver builds a driver. It fails only if the media engine cannot be built.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	cfg.defaults()
	api, err := newAPI(cfg.Order)
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg, api: api}, nil
}

// Stats returns the live session's stats, or the zero value when idle.
func (d *Driver) Stats() Stats {
	d.mu.Lock()
	s := d.current
	d.mu.Unlock()
	if s == nil {
		return Stats{}
	}
	return s.Stats()
}

// Run drives connections until ctx is cancelled. The same code is republished
// after every drop, so the joiner rejoins with no new code. Backoff grows on
// repeated failure and resets after a good session.
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
		good, err := d.oneSession(ctx, roomID)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			d.cfg.Log.Printf("session ended: %v", err)
		}
		if good {
			backoff = 2 * time.Second // a good session resets the penalty
		}
		d.cfg.Sink.Status("waiting for your friend to reconnect")
		if d.cfg.OnDisconnect != nil {
			d.cfg.OnDisconnect()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if !good {
			backoff *= 2
			if backoff > 15*time.Second {
				backoff = 15 * time.Second
			}
		}
	}
	return ctx.Err()
}

// oneSession runs a single publish/handshake/stream cycle. It returns whether
// the session ever went live.
func (d *Driver) oneSession(ctx context.Context, roomID string) (bool, error) {
	sessionID, err := code.NewSession()
	if err != nil {
		return false, err
	}

	sess := newSession(d.api, d.cfg.Order, d.cfg.Sink, d.cfg.Threads, d.cfg.Log)
	defer sess.Close()
	d.mu.Lock()
	d.current = sess
	d.mu.Unlock()

	d.cfg.Sink.Status("publishing your code")
	offerSDP, err := sess.createOffer(ctx, d.cfg.GatherTimeout, d.cfg.ICEServers)
	if err != nil {
		return false, err
	}
	sealed, err := seal.Seal(offerSDP, d.cfg.Code)
	if err != nil {
		return false, err
	}
	if err := d.cfg.Rz.Publish(ctx, roomID, sessionID, sealed); err != nil {
		return false, err
	}

	d.cfg.Sink.Status("ready - waiting for your friend")
	answer, err := d.waitAnswer(ctx, roomID, sessionID)
	if err != nil {
		return false, err
	}
	answerSDP, err := seal.Open(answer, d.cfg.Code)
	if err != nil {
		return false, err // an answer we cannot open is not ours
	}
	if err := sess.acceptAnswer(answerSDP); err != nil {
		return false, err
	}

	// Wait for real media, not merely ICE, up to the handshake timeout.
	d.cfg.Sink.Status("connecting")
	select {
	case <-sess.decoded:
		d.cfg.Log.Printf("live: first frame decoded")
		if d.cfg.OnConnect != nil {
			d.cfg.OnConnect(sess.Stats())
		}
	case <-sess.failed:
		return false, nil
	case <-time.After(d.cfg.HandshakeTimeout):
		return false, nil // republish a fresh session
	case <-ctx.Done():
		return false, ctx.Err()
	}

	// Live. Watch for the session ending or for a knock (a new, different
	// answer under the same session = someone else wants in).
	live := time.Now()
	return true, d.streamUntilDone(ctx, sess, roomID, sessionID, live)
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
// slot: a NEW answer is a knock from another joiner and ends the session so it
// can re-offer. Duplicate deliveries of the same answer are ignored.
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
