// Package jitter reassembles RTP packets into complete, decodable video frames.
//
// It replaces pion's samplebuilder for one reason that matters a great deal at
// 5-10 fps: samplebuilder holds a finished frame until the *next* packet shows
// up. Screen sharing goes quiet when the screen stops changing, so the last
// frame of a scroll could sit undisplayed for seconds. This assembler emits a
// frame the moment its final packet arrives.
//
// It also owns the loss policy, which is where picture quality on a bad link
// is won or lost:
//
//   - A missing packet blocks the frames behind it for an RTT-adaptive window,
//     long enough for a NACK round trip to bring the retransmission. Recovering
//     a packet is far cheaper than a keyframe, which on a slow link can take
//     several seconds to arrive.
//   - If that window expires, the assembler switches to seeking: it discards
//     delta frames (decoding them against a missing reference paints
//     artefacts) and resumes at the next complete keyframe. The caller is told
//     so it can request one.
//   - If a complete keyframe is already buffered behind a stalled frame, it
//     skips ahead at once instead of waiting out the window.
package jitter

import (
	"sort"
	"time"
)

// Packet is one RTP packet as the assembler needs it.
type Packet struct {
	Seq     uint16
	TS      uint32
	Marker  bool
	Payload []byte
	Arrival time.Time
}

// Frame is a complete compressed frame, ready for the decoder.
type Frame struct {
	Data     []byte
	TS       uint32
	Key      bool
	Packets  int
	FirstSeq uint16
	LastSeq  uint16
	// FirstArrival and Completed bracket how long the frame took to assemble,
	// which includes any time spent waiting for retransmissions.
	FirstArrival time.Time
	Completed    time.Time
}

// Codec supplies the payload-format knowledge the assembler needs.
type Codec interface {
	// IsHead reports whether a payload starts a new frame.
	IsHead(payload []byte) bool
	// Depacketize joins a frame's payloads, in order, into one bitstream unit.
	Depacketize(payloads [][]byte) ([]byte, error)
	// IsKey reports whether a depacketized frame decodes on its own.
	IsKey(frame []byte) bool
}

// Stats are cumulative counters.
type Stats struct {
	Frames      uint64 // emitted
	KeyFrames   uint64
	Resyncs     uint64 // times the assembler gave up and sought a keyframe
	SkipAheads  uint64 // stalls resolved early by a buffered keyframe
	Discarded   uint64 // delta frames dropped while seeking
	Duplicates  uint64
	LatePackets uint64 // arrived after their frame was emitted or discarded
	Evicted     uint64 // dropped to bound memory
}

// Options tune the assembler.
type Options struct {
	// MaxWait is how long a gap may block before the assembler resyncs.
	MaxWait time.Duration
	// MaxPackets bounds the buffer.
	MaxPackets int
}

// Assembler is not safe for concurrent use.
type Assembler struct {
	codec Codec
	opts  Options

	buf     map[int64]*Packet
	highest int64 // highest extended sequence number seen
	lowest  int64 // lowest extended sequence number still buffered
	started bool

	// next is the extended sequence number the next frame must start at.
	// It is only meaningful while synced.
	next   int64
	synced bool

	// blockedSince is when the current gap was first observed.
	blockedSince time.Time

	// onResync is called whenever the assembler enters seeking mode.
	onResync func(reason string)

	stats Stats
}

// New returns an assembler that starts out seeking a keyframe.
func New(c Codec, opts Options) *Assembler {
	if opts.MaxWait <= 0 {
		opts.MaxWait = 600 * time.Millisecond
	}
	if opts.MaxPackets <= 0 {
		opts.MaxPackets = 8192
	}
	return &Assembler{codec: c, opts: opts, buf: make(map[int64]*Packet)}
}

// OnResync registers a callback for loss of sync. It runs synchronously inside
// Push, Tick or Resync.
func (a *Assembler) OnResync(fn func(reason string)) { a.onResync = fn }

// SetMaxWait retunes the gap window, typically from a fresh RTT estimate.
func (a *Assembler) SetMaxWait(d time.Duration) {
	if d > 0 {
		a.opts.MaxWait = d
	}
}

// MaxWait reports the current gap window.
func (a *Assembler) MaxWait() time.Duration { return a.opts.MaxWait }

// Seeking reports whether the assembler is waiting for a keyframe.
func (a *Assembler) Seeking() bool { return !a.synced }

// Stats returns the counters.
func (a *Assembler) Stats() Stats { return a.stats }

// Buffered reports how many packets are held.
func (a *Assembler) Buffered() int { return len(a.buf) }

// Resync abandons the current position and seeks the next keyframe. The
// decoder calls this when a frame fails to decode.
func (a *Assembler) Resync(reason string) {
	a.enterSeeking(reason)
}

func (a *Assembler) enterSeeking(reason string) {
	wasSynced := a.synced
	a.synced = false
	a.blockedSince = time.Time{}
	if wasSynced {
		a.stats.Resyncs++
	}
	if a.onResync != nil {
		a.onResync(reason)
	}
}

// extend maps a 16-bit sequence number onto the extended 64-bit line closest to
// the highest seen, which handles wraparound and reordering alike.
func (a *Assembler) extend(seq uint16) int64 {
	if !a.started {
		return int64(seq) + 1<<16 // headroom so early reordering stays positive
	}
	diff := int64(int16(seq - uint16(a.highest)))
	return a.highest + diff
}

// Push adds a packet and returns every frame that became ready.
func (a *Assembler) Push(p Packet) []Frame {
	ext := a.extend(p.Seq)
	if !a.started {
		a.started = true
		a.highest, a.lowest = ext, ext
	}

	if a.synced && ext < a.next {
		a.stats.LatePackets++ // its frame is already out, or was abandoned
		return nil
	}
	if _, dup := a.buf[ext]; dup {
		a.stats.Duplicates++
		return nil
	}
	cp := p
	cp.Payload = append([]byte(nil), p.Payload...)
	a.buf[ext] = &cp
	if ext > a.highest {
		a.highest = ext
	}
	if ext < a.lowest {
		a.lowest = ext
	}
	a.evict()
	return a.drain(p.Arrival)
}

// Tick enforces the gap deadline. Call it periodically (every ~20-50 ms).
func (a *Assembler) Tick(now time.Time) []Frame {
	return a.drain(now)
}

func (a *Assembler) evict() {
	for len(a.buf) > a.opts.MaxPackets {
		delete(a.buf, a.lowest)
		a.stats.Evicted++
		a.lowest++
		for a.lowest < a.highest && a.buf[a.lowest] == nil {
			a.lowest++
		}
		if a.synced && a.next < a.lowest {
			a.enterSeeking("buffer overflow")
		}
	}
}

// drain emits every frame that is ready at time now.
func (a *Assembler) drain(now time.Time) []Frame {
	var out []Frame
	for {
		if !a.synced {
			f, ok := a.seekKeyframe(now)
			if !ok {
				return out
			}
			out = append(out, f)
			continue
		}

		f, status := a.tryAt(a.next, now)
		switch status {
		case complete:
			a.blockedSince = time.Time{}
			out = append(out, f)
			continue
		case waiting:
			// Nothing after the gap has arrived, so it is not yet a gap.
			return out
		case blocked:
			if a.blockedSince.IsZero() {
				a.blockedSince = now
			}
			// A complete keyframe already buffered makes the wait pointless.
			if kf, ok := a.seekKeyframeAfter(a.next, now); ok {
				a.stats.SkipAheads++
				a.blockedSince = time.Time{}
				out = append(out, kf)
				continue
			}
			if now.Sub(a.blockedSince) < a.opts.MaxWait {
				return out
			}
			a.enterSeeking("packet loss not recovered in time")
			continue
		}
	}
}

type tryStatus int

const (
	complete tryStatus = iota
	waiting            // the next packet simply has not arrived; no evidence of loss
	blocked            // a packet is missing and later ones have arrived
)

// tryAt attempts to assemble the frame starting exactly at start.
func (a *Assembler) tryAt(start int64, now time.Time) (Frame, tryStatus) {
	first := a.buf[start]
	if first == nil {
		if a.highest > start {
			return Frame{}, blocked
		}
		return Frame{}, waiting
	}
	if !a.codec.IsHead(first.Payload) {
		// The previous frame ended without its marker, or packets arrived
		// corrupt. Either way this is not a clean frame boundary.
		return Frame{}, blocked
	}
	end, ok := a.frameEnd(start)
	if !ok {
		if a.highest > start && a.hasHole(start) {
			return Frame{}, blocked
		}
		return Frame{}, waiting
	}
	f, err := a.build(start, end, now)
	if err != nil {
		return Frame{}, blocked
	}
	if !f.Key && a.stats.Frames == 0 {
		// Never start a stream on a delta frame.
		return Frame{}, blocked
	}
	a.consume(start, end)
	a.next = end + 1
	return f, complete
}

// frameEnd walks contiguous packets from start to the one carrying the marker
// bit. It stops early at a timestamp change, which also ends a frame.
func (a *Assembler) frameEnd(start int64) (int64, bool) {
	ts := a.buf[start].TS
	for i := start; i <= a.highest; i++ {
		p := a.buf[i]
		if p == nil {
			return 0, false
		}
		if p.TS != ts {
			return i - 1, true
		}
		if p.Marker {
			return i, true
		}
	}
	return 0, false
}

// hasHole reports whether any packet is missing between start and the highest
// sequence number seen.
func (a *Assembler) hasHole(start int64) bool {
	for i := start; i <= a.highest; i++ {
		if a.buf[i] == nil {
			return true
		}
	}
	return false
}

func (a *Assembler) build(start, end int64, now time.Time) (Frame, error) {
	payloads := make([][]byte, 0, end-start+1)
	firstArrival := a.buf[start].Arrival
	for i := start; i <= end; i++ {
		p := a.buf[i]
		payloads = append(payloads, p.Payload)
		if p.Arrival.Before(firstArrival) {
			firstArrival = p.Arrival
		}
	}
	data, err := a.codec.Depacketize(payloads)
	if err != nil || len(data) == 0 {
		return Frame{}, errBadFrame
	}
	f := Frame{
		Data:         data,
		TS:           a.buf[start].TS,
		Key:          a.codec.IsKey(data),
		Packets:      int(end - start + 1),
		FirstSeq:     a.buf[start].Seq,
		LastSeq:      a.buf[end].Seq,
		FirstArrival: firstArrival,
		Completed:    now,
	}
	a.stats.Frames++
	if f.Key {
		a.stats.KeyFrames++
	}
	return f, nil
}

func (a *Assembler) consume(start, end int64) {
	for i := a.lowest; i <= end; i++ {
		delete(a.buf, i)
	}
	a.lowest = end + 1
	for a.lowest < a.highest && a.buf[a.lowest] == nil {
		a.lowest++
	}
}

// seekKeyframe scans the whole buffer for the first complete keyframe.
func (a *Assembler) seekKeyframe(now time.Time) (Frame, bool) {
	return a.seekKeyframeAfter(a.lowest-1, now)
}

// seekKeyframeAfter finds the first complete keyframe starting after `after`,
// discarding the complete delta frames it walks past.
func (a *Assembler) seekKeyframeAfter(after int64, now time.Time) (Frame, bool) {
	keys := make([]int64, 0, len(a.buf))
	for k := range a.buf {
		if k > after {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	for _, start := range keys {
		p := a.buf[start]
		if p == nil || !a.codec.IsHead(p.Payload) {
			continue
		}
		end, ok := a.frameEnd(start)
		if !ok {
			continue
		}
		payloads := make([][]byte, 0, end-start+1)
		for i := start; i <= end; i++ {
			payloads = append(payloads, a.buf[i].Payload)
		}
		data, err := a.codec.Depacketize(payloads)
		if err != nil || !a.codec.IsKey(data) {
			continue
		}
		// Everything before this keyframe is now useless.
		dropped := a.countFramesBefore(start)
		a.stats.Discarded += uint64(dropped)
		f, err := a.build(start, end, now)
		if err != nil {
			continue
		}
		a.consume(start, end)
		a.next = end + 1
		a.synced = true
		a.blockedSince = time.Time{}
		return f, true
	}
	return Frame{}, false
}

// countFramesBefore counts buffered frame heads below start, for statistics.
func (a *Assembler) countFramesBefore(start int64) int {
	n := 0
	for k, p := range a.buf {
		if k < start && a.codec.IsHead(p.Payload) {
			n++
		}
	}
	return n
}

type badFrame struct{}

func (badFrame) Error() string { return "jitter: frame did not depacketize" }

var errBadFrame error = badFrame{}
