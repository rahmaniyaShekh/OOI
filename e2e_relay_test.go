//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"ooi/internal/code"
	"ooi/internal/overlay"
	"ooi/internal/relay"
	"ooi/internal/rendezvous"
	"ooi/internal/rtc"
	"ooi/internal/seal"
	"ooi/internal/vdec/vdectest"
)

func e2eService() string {
	if s := os.Getenv("OOI_E2E_SERVICE"); s != "" {
		return s
	}
	return rendezvous.DefaultService
}

// TestEndToEndRelay forces the relay, as a page does when the two networks
// cannot form a direct path: a headless page posts a relay request inside its
// code-sealed answer, joins the relay socket, and streams real VP9 frames
// sealed with its own key. The host must decode them onto the overlay, ask for
// a keyframe after a broken frame and recover, and end the session when the
// page leaves.
func TestEndToEndRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("network + window; skipped in -short")
	}
	joinCode, err := code.Generate()
	if err != nil {
		t.Fatal(err)
	}
	roomID := code.RoomID(joinCode)
	service := e2eService()

	frames, err := vdectest.Encode(vdectest.VP9, 640, 360, 45)
	if err != nil {
		t.Fatal(err)
	}

	ov := overlay.New(overlay.Config{X: 220, Y: 180, W: 640, H: 360, Opacity: 255,
		ClickThrough: true, Protect: true, Placeholder: "relay e2e"})
	go ov.Run()
	select {
	case <-ov.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("overlay not ready")
	}
	t.Cleanup(func() { ov.Stop(); <-ov.Closed() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rz := rendezvous.New(service)
	connected := make(chan rtc.Stats, 4)
	driver, err := rtc.NewDriver(rtc.DriverConfig{
		Code: joinCode, Sink: ov, Rz: rz, Log: testLogger{t},
		OnConnect: func(s rtc.Stats) { connected <- s },
	})
	if err != nil {
		t.Fatal(err)
	}
	go driver.Run(ctx)

	// Wait for the offer and check it advertises the relay.
	var session string
	for deadline := time.Now().Add(30 * time.Second); ; {
		resp, err := http.Get(service + "/api/room/" + roomID)
		if err == nil && resp.StatusCode == 200 {
			var room struct {
				Session string   `json:"session"`
				Caps    []string `json:"caps"`
			}
			json.NewDecoder(resp.Body).Decode(&room)
			resp.Body.Close()
			if len(room.Caps) != 1 || room.Caps[0] != "relay" {
				t.Fatalf("host did not advertise the relay: caps=%v", room.Caps)
			}
			session = room.Session
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("host never published")
		}
		time.Sleep(300 * time.Millisecond)
	}

	// The relay request: a fresh key inside the code-sealed answer.
	key := make([]byte, 32)
	rand.Read(key)
	req, _ := json.Marshal(map[string]any{"relay": 1, "key": base64.RawURLEncoding.EncodeToString(key)})
	sealed, err := seal.Seal(string(req), joinCode)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rz.PostAnswer(ctx, roomID, session, sealed); err != nil {
		t.Fatal(err)
	}
	aead, _ := relay.NewAEAD(key)
	wsURL := strings.Replace(service, "http", "ws", 1) + "/api/room/" + roomID + "/relay?session=" + session + "&role=viewer"
	ws, err := websocket.Dial(wsURL, "", strings.Replace(service, "/ooi", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.PayloadType = websocket.BinaryFrame
	send := func(typ byte, p []byte) {
		msg, _ := relay.Seal(aead, false, typ, p)
		if err := websocket.Message.Send(ws, msg); err != nil {
			t.Logf("send: %v", err)
		}
	}
	ctl := func(m map[string]any) { b, _ := json.Marshal(m); send(relay.TypeControl, b) }

	type hostMsg struct {
		T   string `json:"t"`
		Dec int    `json:"dec"`
		Q   int    `json:"q"`
	}
	inbox := make(chan hostMsg, 64)
	go func() {
		for {
			var raw []byte
			if websocket.Message.Receive(ws, &raw) != nil {
				close(inbox)
				return
			}
			typ, p, err := relay.Open(aead, true, raw)
			if err != nil || typ != relay.TypeControl {
				continue
			}
			var m hostMsg
			json.Unmarshal(p, &m)
			inbox <- m
		}
	}()

	// Say hi until the host's hello arrives (it joins after reading the answer).
	ctl(map[string]any{"t": "hi", "v": 1})
	hello := false
	for !hello {
		select {
		case m := <-inbox:
			hello = m.T == "hello"
		case <-time.After(time.Second):
			ctl(map[string]any{"t": "hi", "v": 1})
		case <-ctx.Done():
			t.Fatal("no hello from the host")
		}
	}
	t.Logf("relay hello after %s", time.Since(start).Round(time.Millisecond))
	ctl(map[string]any{"t": "config", "codec": "vp9"})

	// Stream at 15 fps; a keyframe request is answered with the next keyframe.
	wantKey := make(chan struct{}, 1)
	keyReqs := 0
	go func() {
		for m := range inbox {
			if m.T == "keyframe" {
				keyReqs++
				select {
				case wantKey <- struct{}{}:
				default:
				}
			}
		}
	}()
	stream := func(n int, corruptAt int) {
		tick := time.NewTicker(time.Second / 15)
		defer tick.Stop()
		i := 0
		for sent := 0; sent < n; sent++ {
			<-tick.C
			select {
			case <-wantKey:
				i = 0 // frame 0 is a keyframe
			default:
			}
			f := frames[i%len(frames)]
			data := f.Data
			if sent == corruptAt {
				data = bytes.Repeat([]byte{0xff}, 300) // a broken delta frame
			}
			var flags byte
			if f.Key && sent != corruptAt {
				flags = relay.FlagKey
			}
			send(relay.TypeViewerMedia, relay.EncodeMedia(flags, uint32(time.Now().UnixMilli()), data))
			i++
			if i == len(frames) {
				i = 1 // loop over the deltas without a new keyframe
			}
		}
	}

	go stream(60, -1)
	select {
	case s := <-connected:
		t.Logf("live through the relay after %s: path=%s codec=%s %dx%d",
			time.Since(start).Round(time.Millisecond), s.Path, s.Codec, s.Width, s.Height)
		if s.Path != "relay" {
			t.Fatalf("path = %q, want relay", s.Path)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no picture through the relay")
	}
	time.Sleep(4 * time.Second)
	before := driver.Stats()
	t.Logf("before recovery: decoded=%d decodeErrors=%d keyRequests=%d", before.Decoded, before.DecodeErrors, before.KeyRequests)

	// Recovery: a broken frame must trigger a keyframe request and the picture
	// must resume once the keyframe arrives.
	stream(45, 5)
	after := driver.Stats()
	t.Logf("after recovery: decoded=%d decodeErrors=%d keyRequests=%d resyncs=%d", after.Decoded, after.DecodeErrors, after.KeyRequests, after.Resyncs)
	if after.KeyRequests <= before.KeyRequests {
		t.Fatal("host did not ask for a keyframe after a broken frame")
	}
	if after.Decoded < before.Decoded+20 {
		t.Fatalf("picture did not resume after the keyframe: %d -> %d", before.Decoded, after.Decoded)
	}
	if !ov.Protected() {
		t.Fatal("overlay lost protection")
	}

	// The page leaving ends the session; the host republishes a new offer.
	ws.Close()
	for deadline := time.Now().Add(15 * time.Second); ; {
		resp, err := http.Get(service + "/api/room/" + roomID)
		if err == nil && resp.StatusCode == 200 {
			var room struct {
				Session string `json:"session"`
			}
			json.NewDecoder(resp.Body).Decode(&room)
			resp.Body.Close()
			if room.Session != session {
				t.Logf("host republished %s after the page left", time.Since(start).Round(time.Millisecond))
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("host did not republish after the page left")
		}
		time.Sleep(300 * time.Millisecond)
	}
}
