package rtc

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"

	"ooi/internal/jitter"
	"ooi/internal/vdec"
)

// Sink is what a session renders into. The overlay satisfies it.
type Sink interface {
	Frame(image.Image) // display a decoded frame (newest wins)
	Status(string)     // placeholder text while no frame is shown
	Size() (int, int)  // current target dimensions in physical pixels
}

// Stats is a snapshot of one session's health, surfaced by `status`.
type Stats struct {
	Codec         string
	Frames        uint64
	KeyFrames     uint64
	Decoded       uint64
	DecodeErrors  uint64
	Resyncs       uint64
	KeyRequests   uint64
	BytesIn       uint64
	RTTMillis     int64
	Width, Height int
	LastFrame     time.Time
	Connected     bool
}

// session runs exactly one peer connection, from offer to teardown.
type session struct {
	api     *webrtc.API
	order   []vdec.Codec
	sink    Sink
	log     Logger
	threads int

	pc *webrtc.PeerConnection

	mu    sync.Mutex
	stats Stats

	// decoded is closed once the first frame is on screen: the signal that the
	// connection is genuinely live, not merely ICE-connected.
	decoded   chan struct{}
	decodedOK sync.Once

	// state transitions that end a session.
	failed   chan struct{}
	failOnce sync.Once
}

// Logger is the minimal logging surface a session needs.
type Logger interface{ Printf(string, ...any) }

func newSession(api *webrtc.API, order []vdec.Codec, sink Sink, threads int, log Logger) *session {
	return &session{
		api: api, order: order, sink: sink, threads: threads, log: log,
		decoded: make(chan struct{}), failed: make(chan struct{}),
	}
}

func (s *session) markFailed() { s.failOnce.Do(func() { close(s.failed) }) }
func (s *session) markDecoded() {
	s.decodedOK.Do(func() {
		s.mu.Lock()
		s.stats.Connected = true
		s.mu.Unlock()
		close(s.decoded)
	})
}

// createOffer builds the peer connection, adds one recvonly video transceiver,
// and returns the complete SDP after non-trickle ICE gathering.
func (s *session) createOffer(ctx context.Context, gatherTimeout time.Duration, ice []webrtc.ICEServer) (string, error) {
	pc, err := s.api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return "", err
	}
	s.pc = pc

	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		pc.Close()
		return "", err
	}

	pc.OnTrack(s.onTrack)
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.log.Printf("peer connection: %s", st)
		switch st {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.markFailed()
		case webrtc.PeerConnectionStateDisconnected:
			// Give ICE a chance to recover before tearing down; the timeouts in
			// the setting engine decide when it becomes 'failed'.
			s.sink.Status("connection interrupted, recovering")
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return "", err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return "", err
	}
	select {
	case <-gather:
	case <-time.After(gatherTimeout):
		// Non-trickle: send whatever candidates we have rather than stalling.
		s.log.Printf("ICE gathering timed out after %s; sending partial candidates", gatherTimeout)
	case <-ctx.Done():
		pc.Close()
		return "", ctx.Err()
	}
	return pc.LocalDescription().SDP, nil
}

// acceptAnswer applies the joiner's answer.
func (s *session) acceptAnswer(sdp string) error {
	return s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
}

// onTrack owns the receive/decode loop for one media track.
func (s *session) onTrack(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
	mime := track.Codec().MimeType
	codec, ok := vdec.CodecFromMime(mime)
	if !ok {
		s.log.Printf("track with unsupported codec %q ignored", mime)
		return
	}
	s.mu.Lock()
	s.stats.Codec = codec.String()
	s.mu.Unlock()
	s.log.Printf("receiving %s (payload %d)", codec, track.PayloadType())

	dec, err := vdec.New(codec, s.threads)
	if err != nil {
		s.log.Printf("decoder: %v", err)
		s.markFailed()
		return
	}
	defer dec.Close()

	asm := jitter.New(newPayloadFormat(codec), jitter.Options{MaxWait: 500 * time.Millisecond})
	asm.OnResync(func(reason string) {
		s.mu.Lock()
		s.stats.Resyncs++
		s.mu.Unlock()
		s.requestKeyframe(track.SSRC())
		s.log.Printf("resync: %s -> requested keyframe", reason)
	})

	pip := &pipeline{s: s, dec: dec, asm: asm, ssrc: track.SSRC()}
	pip.run(track, recv)
}

// requestKeyframe sends a PLI, asking the sender for a fresh keyframe. This is
// the recovery of last resort: it is expensive on a slow link, so the jitter
// assembler exhausts retransmission first.
func (s *session) requestKeyframe(ssrc webrtc.SSRC) {
	if s.pc == nil {
		return
	}
	_ = s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)}})
	s.mu.Lock()
	s.stats.KeyRequests++
	s.mu.Unlock()
}

// Stats returns a snapshot.
func (s *session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// updateRTT refreshes the round-trip estimate from ICE stats and returns it.
func (s *session) updateRTT() time.Duration {
	if s.pc == nil {
		return 0
	}
	for _, st := range s.pc.GetStats() {
		if pair, ok := st.(webrtc.ICECandidatePairStats); ok && pair.Nominated && pair.CurrentRoundTripTime > 0 {
			rtt := time.Duration(pair.CurrentRoundTripTime * float64(time.Second))
			s.mu.Lock()
			s.stats.RTTMillis = rtt.Milliseconds()
			s.mu.Unlock()
			return rtt
		}
	}
	return 0
}

// Close tears the peer connection down.
func (s *session) Close() {
	if s.pc != nil {
		s.pc.Close()
	}
}

var errClosed = errors.New("rtc: session closed")

func (s *session) statusf(format string, a ...any) { s.sink.Status(fmt.Sprintf(format, a...)) }
