//go:build windows

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"ooi/internal/capture"
	"ooi/internal/code"
	"ooi/internal/overlay"
	"ooi/internal/rendezvous"
	"ooi/internal/rtc"
	"ooi/internal/seal"
	"ooi/internal/vdec/vdectest"
	"ooi/internal/verify"
)

// TestEndToEndOverRealRendezvous runs the whole receiver against a headless
// Pion joiner that sends a real VP9 stream, connected through the DEPLOYED
// Cloudflare rendezvous. It proves the entire path end to end: persistent code
// -> sealed offer -> rendezvous -> ICE/DTLS/SRTP -> RTP -> jitter reassembly ->
// VP9 decode -> protected overlay, and that the overlay stays uncapturable
// while the stream is live.
//
// It needs the network. Run with:  go test -run TestEndToEnd -tags e2e
// (skipped by default so the offline suite stays hermetic).
func TestEndToEndOverRealRendezvous(t *testing.T) {
	if testing.Short() {
		t.Skip("network + window; skipped in -short")
	}

	// A throwaway code so the test never collides with a real device's room.
	joinCode, err := code.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roomID := code.RoomID(joinCode)
	service := e2eService()

	// Pre-encode a couple of seconds of real VP9 to replay on a loop.
	const vw, vh = 640, 360
	frames, err := vdectest.Encode(vdectest.VP9, vw, vh, 60)
	if err != nil {
		t.Fatalf("encode test stream: %v", err)
	}

	rect := verify.Rect{X: 200, Y: 160, W: vw, H: vh}
	ov := overlay.New(overlay.Config{
		X: rect.X, Y: rect.Y, W: rect.W, H: rect.H,
		Opacity: 255, ClickThrough: true, Protect: true, Hotkeys: false, Gestures: false,
		Placeholder: "e2e",
	})
	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()
	select {
	case <-ov.Ready():
	case err := <-runErr:
		t.Fatalf("overlay exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("overlay not ready")
	}
	t.Cleanup(func() { ov.Stop(); <-ov.Closed() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	rz := rendezvous.New(service)
	connected := make(chan rtc.Stats, 1)
	driver, err := rtc.NewDriver(rtc.DriverConfig{
		Code: joinCode, Sink: ov, Rz: rz, Log: testLogger{t},
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		OnConnect: func(s rtc.Stats) {
			select {
			case connected <- s:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go driver.Run(ctx)

	// The headless joiner: fetch the offer, answer with a VP9 sender, and (like
	// the real browser) loop so it rejoins after a drop.
	stopJoiner := make(chan struct{})
	reconnect := make(chan struct{}, 1)
	forceReconnect := func() {
		select {
		case reconnect <- struct{}{}:
		default:
		}
	}
	go func() {
		for ctx.Err() == nil {
			select {
			case <-stopJoiner:
				return
			default:
			}
			runJoinerAttempt(t, ctx, rz, roomID, joinCode, frames, stopJoiner, reconnect)
			time.Sleep(500 * time.Millisecond)
		}
	}()
	defer close(stopJoiner)

	select {
	case s := <-connected:
		t.Logf("connected: codec=%s %dx%d", s.Codec, s.Width, s.Height)
	case <-time.After(60 * time.Second):
		t.Fatal("host never reported a live connection")
	}

	// Give a few frames time to decode and reach the surface.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if driver.Stats().Decoded > 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	st := driver.Stats()
	if st.Decoded < 3 {
		t.Fatalf("only %d frames decoded; pipeline not delivering video", st.Decoded)
	}
	t.Logf("decoded=%d keyframes=%d resyncs=%d bytesIn=%d rtt=%dms",
		st.Decoded, st.KeyFrames, st.Resyncs, st.BytesIn, st.RTTMillis)

	// The whole point: while real video is streaming, no capture backend may see
	// the overlay -- in every operating mode the user can put it through.
	check := func(when string) {
		if !ov.Protected() {
			t.Fatalf("overlay lost protection %s", when)
		}
		assertNoCaptureDuringStream(t, ov, rect)
	}
	check("while streaming")

	// transparency control
	for _, o := range []int{40, 120, 255, 80} {
		ov.SetOpacity(o)
		time.Sleep(150 * time.Millisecond)
	}
	check("after opacity changes")

	// moving
	for _, d := range [][2]int{{60, 0}, {0, 40}, {-60, -40}} {
		ov.MoveBy(d[0], d[1])
		time.Sleep(150 * time.Millisecond)
	}
	check("after moving")

	// disable (off screen, still streaming) then enable
	ov.SetEnabled(false)
	time.Sleep(400 * time.Millisecond)
	if ov.Enabled() || !ov.Cloaked() {
		t.Fatal("disable did not take effect during a live stream")
	}
	if !ov.Protected() {
		t.Fatal("lost protection while disabled")
	}
	assertNoCaptureDuringStream(t, ov, rect) // must be invisible while disabled too
	ov.SetEnabled(true)
	time.Sleep(400 * time.Millisecond)
	if !ov.Enabled() || ov.Cloaked() {
		t.Fatal("enable did not take effect")
	}
	check("after disable/enable")

	// repair: force the peer connection to drop and let the driver rebuild the
	// session (rejoin) under the same code, then confirm video resumes and
	// protection never lapsed. Per-session stats reset, so resume is detected by
	// a fresh OnConnect followed by newly decoded frames.
	for { // drain any stale connect signal
		select {
		case <-connected:
			continue
		default:
		}
		break
	}
	forceReconnect()
	select {
	case s := <-connected:
		t.Logf("reconnected: codec=%s %dx%d", s.Codec, s.Width, s.Height)
	case <-time.After(60 * time.Second):
		t.Fatal("driver did not reconnect after a forced drop (repair)")
	}
	resumed := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if driver.Stats().Decoded > 2 {
			resumed = true
			break
		}
		if !ov.Protected() {
			t.Fatal("lost protection during repair/reconnect")
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !resumed {
		t.Fatal("video did not resume after a forced reconnect (repair)")
	}
	check("after repair/reconnect")

	if n := ov.AffinityRestored(); n != 0 {
		t.Errorf("protection was restored %d time(s) during streaming", n)
	}
	if n := ov.FailClosedCount(); n != 0 {
		t.Errorf("overlay failed closed %d time(s) during streaming", n)
	}
}

// runJoiner is a headless stand-in for the browser: it answers the host's offer
// and streams canned VP9 on a loop.
func runJoinerAttempt(t *testing.T, ctx context.Context, rz *rendezvous.Client, roomID, joinCode string,
	frames []vdectest.Frame, stop <-chan struct{}, reconnect <-chan struct{}) {

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0"},
		PayloadType:        98,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Errorf("joiner codec: %v", err)
		return
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	// Poll for the host's offer.
	var offerBlob, sessionID string
	for ctx.Err() == nil {
		ob, sess, ok, err := rz.GetOffer(ctx, roomID)
		if err == nil && ok {
			offerBlob, sessionID = ob, sess
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-time.After(1 * time.Second):
		}
	}

	offerSDP, err := seal.Open(offerBlob, joinCode)
	if err != nil {
		t.Errorf("joiner cannot open offer: %v", err)
		return
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		t.Errorf("joiner pc: %v", err)
		return
	}
	defer pc.Close()
	pcDead := make(chan struct{})
	var deadOnce sync.Once
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed ||
			st == webrtc.PeerConnectionStateDisconnected {
			deadOnce.Do(func() { close(pcDead) })
		}
	})

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP9, ClockRate: 90000}, "screen", "ooi-e2e")
	if err != nil {
		t.Errorf("joiner track: %v", err)
		return
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Errorf("joiner addtrack: %v", err)
		return
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		t.Errorf("joiner setRemote: %v", err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Errorf("joiner answer: %v", err)
		return
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		t.Errorf("joiner setLocal: %v", err)
		return
	}
	select {
	case <-gather:
	case <-time.After(8 * time.Second):
	}

	sealed, err := seal.Seal(pc.LocalDescription().SDP, joinCode)
	if err != nil {
		t.Errorf("joiner seal: %v", err)
		return
	}
	if err := rz.PostAnswer(ctx, roomID, sessionID, sealed); err != nil {
		if errors.Is(err, rendezvous.ErrStale) {
			// The host re-offered while we answered: take the new offer, as
			// the page does.
			t.Logf("joiner: offer replaced while answering; retrying")
			return
		}
		t.Errorf("joiner post answer: %v", err)
		return
	}

	// Replay the canned stream at ~10 fps until told to stop.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-reconnect:
			return // simulate a repair: drop and let the outer loop rejoin
		case <-pcDead:
			return
		case <-ticker.C:
			f := frames[i%len(frames)]
			i++
			_ = track.WriteSample(media.Sample{Data: f.Data, Duration: 100 * time.Millisecond})
		}
	}
}

func assertNoCaptureDuringStream(t *testing.T, ov *overlay.Overlay, rect verify.Rect) {
	t.Helper()
	x, y, w, h := capture.ScreenBounds()
	shots := map[string]func() (capture.Image, error){
		"GDI BitBlt + CAPTUREBLT": func() (capture.Image, error) { return capture.GDIScreen(x, y, w, h) },
		"DXGI Desktop Duplication": func() (capture.Image, error) {
			img, _, err := capture.DXGIScreen(x+w/2, y+h/2, 4*time.Second)
			return img, err
		},
		"WGC monitor capture": func() (capture.Image, error) {
			img, _, _, err := capture.WGCScreen(x+w/2, y+h/2, 5*time.Second)
			return img, err
		},
		"WGC window capture": func() (capture.Image, error) {
			img, _, _, err := capture.WGCWindowCapture(ov.HWND(), 5*time.Second)
			return img, err
		},
	}
	ran := 0
	for name, grab := range shots {
		img, err := grab()
		if err != nil {
			t.Logf("%s unavailable: %v", name, err)
			continue
		}
		ran++
		// The streamed test pattern includes a saturated red box; the verify
		// marker colour is close enough that a visible overlay would light up
		// many pixels. Use a generous cap: a truly captured 640x360 window is
		// tens of thousands of pixels, a clean capture is ~zero.
		if hits := img.CountNear(0x2B, 0x0A, 0xE8, 40); hits > 3000 {
			t.Errorf("%s captured the streaming overlay: %d marker-ish pixels", name, hits)
		}
	}
	if ran == 0 {
		t.Fatal("no capture backend ran; assertion vacuous")
	}
}

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(f string, a ...any) { l.t.Logf(f, a...) }
