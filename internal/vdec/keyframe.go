package vdec

// IsKeyframe reports whether a complete compressed frame can be decoded on its
// own, without any earlier frame.
//
// After a loss the receiver must discard delta frames until one of these
// arrives: decoding a delta frame against a missing reference produces
// artefacts, and the whole point of the pipeline is never to show those.
func IsKeyframe(c Codec, b []byte) bool {
	if len(b) == 0 {
		return false
	}
	switch c {
	case VP8:
		// RFC 6386 9.1: bit 0 of the frame tag is 0 for a key frame.
		return b[0]&0x01 == 0
	case VP9:
		return vp9Keyframe(b)
	case H264:
		return h264Keyframe(b)
	case AV1:
		return av1Keyframe(b)
	}
	return false
}

// vp9Keyframe reads the start of the VP9 uncompressed header.
//
//	frame_marker(2) profile_low(1) profile_high(1) [reserved(1) if profile==3]
//	show_existing_frame(1) frame_type(1)   // frame_type 0 = KEY_FRAME
func vp9Keyframe(b []byte) bool {
	br := bitReader{b: b}
	if br.bits(2) != 2 {
		return false
	}
	lo := br.bits(1)
	hi := br.bits(1)
	profile := hi<<1 | lo
	if profile == 3 {
		br.bits(1)
	}
	if br.bits(1) == 1 { // show_existing_frame: re-shows a decoded frame
		return false
	}
	if br.err {
		return false
	}
	return br.bits(1) == 0 && !br.err
}

// h264Keyframe scans Annex-B NAL units for an IDR slice.
func h264Keyframe(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		// Start code 00 00 01 (a 4-byte 00 00 00 01 contains it).
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if typ := b[i+3] & 0x1F; typ == 5 {
				return true
			}
			i += 2
		}
	}
	return false
}

// av1Keyframe reports whether a temporal unit carries a sequence header OBU.
// WebRTC AV1 encoders emit one with every keyframe (a random access point), and
// never with a delta frame, so its presence is a reliable marker.
func av1Keyframe(b []byte) bool {
	const obuSequenceHeader = 1
	for i := 0; i < len(b); {
		h := b[i]
		typ := (h >> 3) & 0x0F
		ext := h&0x04 != 0
		hasSize := h&0x02 != 0
		i++
		if ext {
			i++
		}
		if typ == obuSequenceHeader {
			return true
		}
		if !hasSize {
			return false // the last OBU fills the rest; nothing more to scan
		}
		size, n := leb128(b[min(i, len(b)):])
		if n == 0 {
			return false
		}
		i += n + int(size)
	}
	return false
}

func leb128(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 8; i++ {
		v |= uint64(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

type bitReader struct {
	b   []byte
	pos int
	err bool
}

func (r *bitReader) bits(n int) int {
	v := 0
	for range n {
		byteIdx := r.pos >> 3
		if byteIdx >= len(r.b) {
			r.err = true
			return 0
		}
		bit := (r.b[byteIdx] >> (7 - uint(r.pos&7))) & 1
		v = v<<1 | int(bit)
		r.pos++
	}
	return v
}
