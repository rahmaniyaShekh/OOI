package rtc

import (
	"errors"

	"github.com/pion/rtp/codecs"

	"ooi/internal/jitter"
	"ooi/internal/vdec"
)

// payloadFormat adapts pion's RTP depacketizers to the jitter assembler.
//
// The depacketizers for H.264 (FU-A) and AV1 (fragmented OBUs) carry state
// between packets, so Depacketize builds a fresh one per frame: a frame is
// always depacketized as a complete, in-order unit, and nothing can leak from
// an abandoned frame into the next.
type payloadFormat struct {
	codec vdec.Codec
}

func newPayloadFormat(c vdec.Codec) jitter.Codec { return payloadFormat{codec: c} }

var errUnknownCodec = errors.New("rtc: unknown codec")

func (f payloadFormat) IsHead(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	switch f.codec {
	case vdec.VP8:
		return (&codecs.VP8Packet{}).IsPartitionHead(p)
	case vdec.VP9:
		return (&codecs.VP9Packet{}).IsPartitionHead(p)
	case vdec.H264:
		return (&codecs.H264Packet{}).IsPartitionHead(p)
	case vdec.AV1:
		return (&codecs.AV1Depacketizer{}).IsPartitionHead(p)
	}
	return false
}

func (f payloadFormat) Depacketize(ps [][]byte) ([]byte, error) {
	var out []byte
	switch f.codec {
	case vdec.VP8:
		for _, p := range ps {
			var pk codecs.VP8Packet
			b, err := pk.Unmarshal(p)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	case vdec.VP9:
		for _, p := range ps {
			var pk codecs.VP9Packet
			b, err := pk.Unmarshal(p)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	case vdec.H264:
		var pk codecs.H264Packet // Annex-B output, stateful across FU-A
		for _, p := range ps {
			b, err := pk.Unmarshal(p)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	case vdec.AV1:
		var d codecs.AV1Depacketizer
		for _, p := range ps {
			b, err := d.Unmarshal(p)
			if err != nil {
				return nil, err
			}
			out = append(out, b...)
		}
	default:
		return nil, errUnknownCodec
	}
	return out, nil
}

func (f payloadFormat) IsKey(frame []byte) bool { return vdec.IsKeyframe(f.codec, frame) }
