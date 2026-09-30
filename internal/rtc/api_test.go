package rtc

import (
	"testing"

	"ooi/internal/vdec"
)

func TestParseCodecOrderDefault(t *testing.T) {
	got, err := ParseCodecOrder("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(DefaultCodecOrder) || got[0] != vdec.VP9 {
		t.Fatalf("default order wrong: %v", got)
	}
}

func TestParseCodecOrderExplicit(t *testing.T) {
	got, err := ParseCodecOrder("av1, vp8")
	if err != nil {
		t.Fatal(err)
	}
	// Named codecs come first, in order; the rest follow in default order.
	if got[0] != vdec.AV1 || got[1] != vdec.VP8 {
		t.Fatalf("explicit order not honoured: %v", got)
	}
	seen := map[vdec.Codec]bool{}
	for _, c := range got {
		if seen[c] {
			t.Fatalf("duplicate codec in %v", got)
		}
		seen[c] = true
	}
	if len(got) != 4 {
		t.Fatalf("want all 4 codecs present, got %v", got)
	}
}

func TestParseCodecOrderRejectsUnknown(t *testing.T) {
	if _, err := ParseCodecOrder("vp9,hevc"); err == nil {
		t.Fatal("accepted an unknown codec")
	}
}

func TestNewAPIBuildsForEveryCodec(t *testing.T) {
	// The media engine must build with each codec leading, since RegisterFeedback
	// only decorates codecs already registered — a mis-ordered setup would fail
	// here rather than mysteriously at connect time.
	for _, c := range DefaultCodecOrder {
		order := append([]vdec.Codec{c}, DefaultCodecOrder...)
		if _, err := newAPI(order); err != nil {
			t.Fatalf("newAPI with %s leading: %v", c, err)
		}
	}
}
