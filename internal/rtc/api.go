package rtc

import (
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"

	"ooi/internal/vdec"
)

// Payload types. Each video codec has an RTX twin carrying retransmissions, so a
// resent packet travels on its own SSRC and never distorts the loss statistics
// the sender's congestion controller relies on.
const (
	ptVP9, ptVP9RTX   = 98, 99
	ptAV1, ptAV1RTX   = 45, 46
	ptVP8, ptVP8RTX   = 96, 97
	ptH264, ptH264RTX = 102, 103
)

// codecEntry is one offered codec.
type codecEntry struct {
	codec vdec.Codec
	mime  string
	pt    uint8
	rtx   uint8
	fmtp  string
}

// allCodecs lists every codec the receiver can decode.
var allCodecs = map[vdec.Codec]codecEntry{
	vdec.VP9: {vdec.VP9, webrtc.MimeTypeVP9, ptVP9, ptVP9RTX, "profile-id=0"},
	vdec.AV1: {vdec.AV1, webrtc.MimeTypeAV1, ptAV1, ptAV1RTX, ""},
	vdec.VP8: {vdec.VP8, webrtc.MimeTypeVP8, ptVP8, ptVP8RTX, ""},
	vdec.H264: {vdec.H264, webrtc.MimeTypeH264, ptH264, ptH264RTX,
		"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
}

// DefaultCodecOrder is the preference order offered to the browser.
//
// VP9 leads: every Chromium browser encodes it, its screen-content mode keeps
// text sharp at low bitrates, and it is verified end to end here. AV1 is more
// efficient still but costs the sender far more CPU. VP8 and H.264 are the
// universal fallbacks (Firefox, Safari).
var DefaultCodecOrder = []vdec.Codec{vdec.VP9, vdec.AV1, vdec.VP8, vdec.H264}

// ParseCodecOrder turns "av1,vp9" into a preference list; unknown names are an
// error, and anything not named is appended in the default order.
func ParseCodecOrder(s string) ([]vdec.Codec, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultCodecOrder, nil
	}
	seen := map[vdec.Codec]bool{}
	var out []vdec.Codec
	for _, name := range strings.Split(s, ",") {
		c, ok := vdec.CodecFromMime("video/" + strings.TrimSpace(name))
		if !ok {
			return nil, &codecNameError{strings.TrimSpace(name)}
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range DefaultCodecOrder {
		if !seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

type codecNameError struct{ name string }

func (e *codecNameError) Error() string {
	return "rtc: unknown codec " + `"` + e.name + `"` + " (want vp9, av1, vp8 or h264)"
}

// newAPI builds a pion API tuned for a receive-only screen share over a slow,
// lossy path.
func newAPI(order []vdec.Codec) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}

	// Codecs first: the feedback registrations below decorate whatever is
	// already registered.
	for _, c := range order {
		e := allCodecs[c]
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: e.mime, ClockRate: 90000, SDPFmtpLine: e.fmtp,
				RTCPFeedback: []webrtc.RTCPFeedback{{Type: "ccm", Parameter: "fir"}},
			},
			PayloadType: webrtc.PayloadType(e.pt),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeRTX, ClockRate: 90000,
				SDPFmtpLine: "apt=" + itoa(int(e.pt)),
			},
			PayloadType: webrtc.PayloadType(e.rtx),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}

	reg := &interceptor.Registry{}

	// NACK: ask for lost packets back. Capped per packet, because on a long
	// round trip the generator would otherwise re-ask every 100ms before the
	// first retransmission could possibly have arrived, and each duplicate
	// retransmission spends bandwidth the link does not have.
	if err := webrtc.ConfigureNackWithOptions(m, reg, []nack.GeneratorOption{
		nack.GeneratorSize(2048),
		nack.GeneratorInterval(60 * time.Millisecond),
		nack.GeneratorMaxNacksPerPacket(4),
	}); err != nil {
		return nil, err
	}
	// Receiver reports give the sender loss and jitter for its loss-based
	// bandwidth estimate.
	if err := webrtc.ConfigureRTCPReports(reg); err != nil {
		return nil, err
	}
	// Transport-wide congestion control feedback drives the browser's
	// delay-based estimator (GCC), which is what lets it back off before a
	// slow link starts dropping packets, and ramp up again as it clears.
	if err := webrtc.ConfigureTWCCSender(m, reg); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureStatsInterceptor(reg); err != nil {
		return nil, err
	}

	var se webrtc.SettingEngine
	// Disconnected after 6s of silence, failed after 20s. Generous on purpose:
	// on a congested link consent checks are the first thing to be delayed,
	// and tearing down a connection that would have recovered costs a full
	// renegotiation plus a keyframe.
	se.SetICETimeouts(6*time.Second, 20*time.Second, 2*time.Second)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	// Virtual adapters produce candidates that can never work and slow down
	// gathering, which a non-trickle handshake waits on in full.
	se.SetInterfaceFilter(func(name string) bool {
		n := strings.ToLower(name)
		for _, bad := range []string{"vethernet", "vmware", "virtualbox", "loopback",
			"isatap", "teredo", "bluetooth", "hyper-v", "wsl", "docker", "npcap"} {
			if strings.Contains(n, bad) {
				return false
			}
		}
		return true
	})

	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(reg),
		webrtc.WithSettingEngine(se),
	), nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
