package rtc

import (
	"errors"
	"io"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"ooi/internal/jitter"
	"ooi/internal/vdec"
)

// pipeline moves one track's packets through the jitter assembler into the
// decoder and onto the overlay. It runs on the goroutine pion gives OnTrack.
type pipeline struct {
	s    *session
	dec  *vdec.Decoder
	asm  *jitter.Assembler
	ssrc webrtc.SSRC
}

func (p *pipeline) run(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	// A ticker enforces the assembler's gap deadline even when no new packet is
	// arriving (a stalled or idle link), and periodically refreshes the RTT the
	// assembler uses to size its wait window.
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()

	frames := make(chan jitter.Frame, 16)
	go p.decodeLoop(frames)
	defer close(frames)

	// Read RTP on its own goroutine so the ticker is never starved by a
	// blocking Read.
	type readItem struct {
		pkt *rtp.Packet
		n   int
	}
	reads := make(chan jitter.Packet, 256)
	go func() {
		defer close(reads)
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					p.s.log.Printf("track read: %v", err)
				}
				p.s.markFailed()
				return
			}
			n := len(pkt.Payload)
			p.s.mu.Lock()
			p.s.stats.BytesIn += uint64(n)
			p.s.mu.Unlock()
			if n == 0 {
				continue // padding / probe
			}
			select {
			case reads <- jitter.Packet{
				Seq: pkt.SequenceNumber, TS: pkt.Timestamp, Marker: pkt.Marker,
				Payload: pkt.Payload, Arrival: time.Now(),
			}:
			default:
				// Reader outran the assembler: drop oldest by forcing a resync
				// rather than blocking the SRTP reader.
				p.asm.Resync("read buffer overflow")
			}
		}
	}()

	rttRefresh := time.NewTicker(2 * time.Second)
	defer rttRefresh.Stop()

	for {
		select {
		case pk, ok := <-reads:
			if !ok {
				return
			}
			for _, f := range p.asm.Push(pk) {
				p.emit(frames, f)
			}
		case now := <-tick.C:
			for _, f := range p.asm.Tick(now) {
				p.emit(frames, f)
			}
		case <-rttRefresh.C:
			if rtt := p.s.updateRTT(); rtt > 0 {
				// Wait at least one RTT plus slack for a retransmission, bounded
				// so a pathological RTT cannot freeze the picture indefinitely.
				w := rtt*2 + 120*time.Millisecond
				if w < 200*time.Millisecond {
					w = 200 * time.Millisecond
				}
				if w > 1200*time.Millisecond {
					w = 1200 * time.Millisecond
				}
				p.asm.SetMaxWait(w)
			}
		case <-p.s.failed:
			return
		}
	}
}

func (p *pipeline) emit(frames chan<- jitter.Frame, f jitter.Frame) {
	p.s.mu.Lock()
	p.s.stats.Frames++
	if f.Key {
		p.s.stats.KeyFrames++
	}
	p.s.mu.Unlock()
	select {
	case frames <- f:
	case <-p.s.failed:
	}
}

// decodeLoop decodes complete frames and renders the newest onto the overlay.
// Keeping decode off the read/assemble goroutine means a momentary decode stall
// (a large keyframe on a busy CPU) never delays NACK generation.
func (p *pipeline) decodeLoop(frames <-chan jitter.Frame) {
	for f := range frames {
		ok, err := p.dec.Decode(f.Data)
		if err != nil {
			p.s.mu.Lock()
			p.s.stats.DecodeErrors++
			p.s.mu.Unlock()
			// A frame that would not decode cleanly means the reference chain is
			// broken; drop to seeking and ask for a keyframe.
			p.asm.Resync("decode error: " + err.Error())
			continue
		}
		if !ok {
			continue
		}
		p.s.mu.Lock()
		p.s.stats.Decoded++
		w, h := p.dec.Size()
		p.s.stats.Width, p.s.stats.Height = w, h
		p.s.stats.LastFrame = time.Now()
		p.s.mu.Unlock()

		cw, ch := p.s.sink.Size()
		if cw <= 0 || ch <= 0 {
			continue
		}
		img, _, err := p.dec.Render(cw, ch)
		if err != nil {
			continue
		}
		p.s.sink.Frame(img)
		p.s.markDecoded()
	}
}
