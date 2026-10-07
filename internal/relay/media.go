package relay

import (
	"encoding/binary"
	"errors"
)

// A media message (type 3) carries one encoded video frame, or one part of a
// frame too large for a single message:
//
//	flags(1) | timestamp ms (u32 BE) | length (u32 BE) | bytes
//
// Over the relay the path is TCP, so nothing is lost or reordered and no
// FEC/NACK is needed: a frame's parts arrive back to back.
const (
	FlagKey  = 0x01 // a keyframe: the decoder can start here
	FlagMore = 0x02 // more parts of this frame follow
)

const mediaHeader = 9

// MaxFrame bounds a reassembled frame; a larger one is dropped.
const MaxFrame = 8 << 20

// Frame is one complete encoded video frame.
type Frame struct {
	Key  bool
	TS   uint32 // milliseconds, sender clock
	Data []byte
}

// EncodeMedia builds one media message part (used by tests and tools).
func EncodeMedia(flags byte, ts uint32, data []byte) []byte {
	out := make([]byte, mediaHeader+len(data))
	out[0] = flags
	binary.BigEndian.PutUint32(out[1:], ts)
	binary.BigEndian.PutUint32(out[5:], uint32(len(data)))
	copy(out[mediaHeader:], data)
	return out
}

var errBadMedia = errors.New("relay: malformed media message")

// Assembler joins frame parts into frames.
type Assembler struct {
	buf  []byte
	key  bool
	ts   uint32
	open bool
}

// Push adds one media message. It returns a frame when the message completed
// one; ok is false while parts are still outstanding.
func (a *Assembler) Push(msg []byte) (f Frame, ok bool, err error) {
	if len(msg) < mediaHeader {
		a.Reset()
		return Frame{}, false, errBadMedia
	}
	flags := msg[0]
	ts := binary.BigEndian.Uint32(msg[1:])
	n := binary.BigEndian.Uint32(msg[5:])
	if uint64(n) != uint64(len(msg)-mediaHeader) {
		a.Reset()
		return Frame{}, false, errBadMedia
	}
	data := msg[mediaHeader:]
	if a.open && ts != a.ts {
		a.Reset() // a part went missing: abandon the partial frame
	}
	if !a.open {
		a.open, a.ts, a.key, a.buf = true, ts, flags&FlagKey != 0, a.buf[:0]
	}
	if len(a.buf)+len(data) > MaxFrame {
		a.Reset()
		return Frame{}, false, errBadMedia
	}
	a.buf = append(a.buf, data...)
	if flags&FlagMore != 0 {
		return Frame{}, false, nil
	}
	f = Frame{Key: a.key, TS: a.ts, Data: append([]byte(nil), a.buf...)}
	a.Reset()
	return f, true, nil
}

// Reset drops any partial frame.
func (a *Assembler) Reset() { a.open = false; a.buf = a.buf[:0] }
