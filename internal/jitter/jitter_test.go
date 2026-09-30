package jitter

import (
	"bytes"
	"testing"
	"time"
)

// fakeCodec: payload[0] bit0 = frame head, bit1 = keyframe (on the head).
// The depacketized frame is [key flag] + every payload's body.
type fakeCodec struct{}

func (fakeCodec) IsHead(p []byte) bool { return len(p) > 0 && p[0]&1 != 0 }
func (fakeCodec) Depacketize(ps [][]byte) ([]byte, error) {
	out := []byte{ps[0][0] >> 1 & 1}
	for _, p := range ps {
		out = append(out, p[1:]...)
	}
	return out, nil
}
func (fakeCodec) IsKey(f []byte) bool { return len(f) > 0 && f[0] == 1 }

var t0 = time.Unix(1000, 0)

// stream builds frames of n packets each; frame i has timestamp i*9000.
type stream struct {
	seq uint16
	pk  []Packet
}

func (s *stream) frame(ts uint32, n int, key bool, body byte) []Packet {
	var out []Packet
	for i := range n {
		flag := byte(0)
		if i == 0 {
			flag |= 1
			if key {
				flag |= 2
			}
		}
		out = append(out, Packet{Seq: s.seq, TS: ts, Marker: i == n-1, Payload: []byte{flag, body}})
		s.seq++
	}
	return out
}

func push(a *Assembler, ps []Packet, at time.Time) []Frame {
	var out []Frame
	for _, p := range ps {
		p.Arrival = at
		out = append(out, a.Push(p)...)
	}
	return out
}

func TestEmitsImmediatelyOnMarker(t *testing.T) {
	// The whole reason this package exists: the frame must come out when its
	// last packet lands, not when the next frame starts.
	a := New(fakeCodec{}, Options{})
	var s stream
	got := push(a, s.frame(0, 3, true, 'k'), t0)
	if len(got) != 1 || !got[0].Key || got[0].Packets != 3 {
		t.Fatalf("keyframe not emitted on its marker: %+v", got)
	}
	got = push(a, s.frame(9000, 2, false, 'd'), t0)
	if len(got) != 1 || got[0].Key {
		t.Fatalf("delta not emitted on its marker: %+v", got)
	}
}

func TestReorderWithinFrame(t *testing.T) {
	a := New(fakeCodec{}, Options{})
	var s stream
	ps := s.frame(0, 4, true, 'k')
	order := []int{2, 0, 3, 1}
	var got []Frame
	for _, i := range order {
		p := ps[i]
		p.Arrival = t0
		got = append(got, a.Push(p)...)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Data, []byte{1, 'k', 'k', 'k', 'k'}) {
		t.Fatalf("reordered frame assembled wrong: %+v", got)
	}
}

func TestStartsOnlyAtKeyframe(t *testing.T) {
	a := New(fakeCodec{}, Options{})
	var s stream
	// Joined mid-stream: two delta frames, then a keyframe.
	got := push(a, s.frame(0, 2, false, 'a'), t0)
	got = append(got, push(a, s.frame(9000, 2, false, 'b'), t0)...)
	if len(got) != 0 {
		t.Fatalf("emitted a delta frame before any keyframe: %+v", got)
	}
	got = push(a, s.frame(18000, 2, true, 'k'), t0)
	if len(got) != 1 || !got[0].Key {
		t.Fatalf("keyframe not emitted: %+v", got)
	}
}

func TestLossRecoveredByRetransmission(t *testing.T) {
	a := New(fakeCodec{}, Options{MaxWait: 500 * time.Millisecond})
	resyncs := 0
	a.OnResync(func(string) { resyncs++ })
	var s stream
	push(a, s.frame(0, 2, true, 'k'), t0)

	f1 := s.frame(9000, 3, false, 'x')
	f2 := s.frame(18000, 2, false, 'y')
	// Packet 1 of f1 is lost; everything after it arrives.
	got := push(a, []Packet{f1[0], f1[2]}, t0)
	got = append(got, push(a, f2, t0)...)
	if len(got) != 0 {
		t.Fatalf("emitted past a gap: %+v", got)
	}
	// 200ms later the retransmission arrives: both frames flow, in order.
	got = push(a, []Packet{f1[1]}, t0.Add(200*time.Millisecond))
	if len(got) != 2 || got[0].TS != 9000 || got[1].TS != 18000 {
		t.Fatalf("recovered frames wrong: %+v", got)
	}
	if resyncs != 0 || a.Seeking() {
		t.Fatal("a recovered loss must not force a keyframe")
	}
}

func TestUnrecoveredLossResyncsAndDropsDeltas(t *testing.T) {
	a := New(fakeCodec{}, Options{MaxWait: 300 * time.Millisecond})
	var reason string
	a.OnResync(func(r string) { reason = r })
	var s stream
	push(a, s.frame(0, 2, true, 'k'), t0)

	f1 := s.frame(9000, 3, false, 'x')
	push(a, []Packet{f1[0], f1[2]}, t0) // f1[1] never comes
	push(a, s.frame(18000, 2, false, 'y'), t0)

	if got := a.Tick(t0.Add(299 * time.Millisecond)); len(got) != 0 || a.Seeking() {
		t.Fatal("gave up before the window expired")
	}
	a.Tick(t0.Add(301 * time.Millisecond))
	if !a.Seeking() || reason == "" {
		t.Fatal("did not resync after the window expired")
	}
	// Further delta frames are discarded until a keyframe.
	if got := push(a, s.frame(27000, 2, false, 'z'), t0.Add(400*time.Millisecond)); len(got) != 0 {
		t.Fatalf("decoded a delta frame after an unrecovered loss: %+v", got)
	}
	got := push(a, s.frame(36000, 2, true, 'K'), t0.Add(500*time.Millisecond))
	if len(got) != 1 || !got[0].Key {
		t.Fatalf("keyframe after resync not emitted: %+v", got)
	}
	if a.Seeking() {
		t.Fatal("still seeking after a keyframe")
	}
}

func TestKeyframeSkipAhead(t *testing.T) {
	// A complete keyframe behind a stalled delta ends the wait immediately.
	a := New(fakeCodec{}, Options{MaxWait: 10 * time.Second})
	var s stream
	push(a, s.frame(0, 2, true, 'k'), t0)
	f1 := s.frame(9000, 3, false, 'x')
	push(a, []Packet{f1[0], f1[2]}, t0)
	got := push(a, s.frame(18000, 2, true, 'K'), t0)
	if len(got) != 1 || !got[0].Key || got[0].TS != 18000 {
		t.Fatalf("did not skip ahead to the buffered keyframe: %+v", got)
	}
	if a.Stats().SkipAheads != 1 {
		t.Fatalf("skip-ahead not counted: %+v", a.Stats())
	}
	// The late retransmission for the abandoned frame is ignored.
	if got := push(a, []Packet{f1[1]}, t0); len(got) != 0 {
		t.Fatalf("late packet produced a frame: %+v", got)
	}
}

func TestSequenceWraparound(t *testing.T) {
	a := New(fakeCodec{}, Options{})
	s := stream{seq: 65533}
	var got []Frame
	got = append(got, push(a, s.frame(0, 3, true, 'k'), t0)...)      // 65533..65535
	got = append(got, push(a, s.frame(9000, 3, false, 'd'), t0)...)  // 0..2
	got = append(got, push(a, s.frame(18000, 2, false, 'e'), t0)...) // 3..4
	if len(got) != 3 {
		t.Fatalf("wraparound broke assembly: %d frames", len(got))
	}
	if got[1].FirstSeq != 0 || got[1].LastSeq != 2 {
		t.Fatalf("wrong seq range across wrap: %+v", got[1])
	}
}

func TestDuplicatesIgnored(t *testing.T) {
	a := New(fakeCodec{}, Options{})
	var s stream
	ps := s.frame(0, 2, true, 'k')
	got := push(a, []Packet{ps[0], ps[0], ps[1], ps[1]}, t0)
	if len(got) != 1 {
		t.Fatalf("duplicates changed output: %+v", got)
	}
	if a.Stats().Duplicates+a.Stats().LatePackets != 2 {
		t.Fatalf("duplicates not counted: %+v", a.Stats())
	}
}

func TestResyncOnDecoderRequest(t *testing.T) {
	a := New(fakeCodec{}, Options{})
	var s stream
	push(a, s.frame(0, 2, true, 'k'), t0)
	a.Resync("decode error")
	if got := push(a, s.frame(9000, 2, false, 'd'), t0); len(got) != 0 {
		t.Fatal("delta frame emitted after the decoder asked for a keyframe")
	}
	if got := push(a, s.frame(18000, 1, true, 'K'), t0); len(got) != 1 {
		t.Fatal("keyframe not emitted after resync")
	}
}

func TestBoundedMemory(t *testing.T) {
	a := New(fakeCodec{}, Options{MaxPackets: 64, MaxWait: time.Hour})
	var s stream
	push(a, s.frame(0, 1, true, 'k'), t0)
	s.seq++ // lose one packet so nothing after it can ever drain
	for i := range 500 {
		push(a, s.frame(uint32(9000*(i+2)), 1, false, 'd'), t0)
	}
	if a.Buffered() > 64 {
		t.Fatalf("buffer grew to %d packets", a.Buffered())
	}
}
