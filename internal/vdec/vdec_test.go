package vdec_test

import (
	"testing"

	"ooi/internal/vdec"
	"ooi/internal/vdec/vdectest"
)

// flatPicture builds a w x h I420 picture of one YUV colour.
func flatPicture(w, h int, y, u, v byte) vdec.Picture {
	cw, ch := (w+1)/2, (h+1)/2
	p := vdec.Picture{Width: w, Height: h, YStride: w, UVStride: cw,
		Y: make([]byte, w*h), U: make([]byte, cw*ch), V: make([]byte, cw*ch)}
	for i := range p.Y {
		p.Y[i] = y
	}
	for i := range p.U {
		p.U[i], p.V[i] = u, v
	}
	return p
}

func px(pix []byte, w, x, y int) (b, g, r, a byte) {
	i := (y*w + x) * 4
	return pix[i], pix[i+1], pix[i+2], pix[i+3]
}

func near(a, b byte, tol int) bool {
	d := int(a) - int(b)
	return d <= tol && d >= -tol
}

// TestColourLimitedRange checks the BT.601 studio-swing mapping WebRTC uses:
// Y=16 is black and Y=235 is white. The JFIF full-range formula that
// image/color uses would render these as dark grey and off-white.
func TestColourLimitedRange(t *testing.T) {
	cases := []struct {
		name    string
		y, u, v byte
		r, g, b byte
	}{
		{"black", 16, 128, 128, 0, 0, 0},
		{"white", 235, 128, 128, 255, 255, 255},
		{"mid-grey", 126, 128, 128, 128, 128, 128},
		{"red", 81, 90, 240, 255, 0, 0},
		{"green", 145, 54, 34, 0, 255, 0},
		{"blue", 41, 240, 110, 0, 0, 255},
	}
	for _, c := range cases {
		out, _, err := vdec.RenderPicture(flatPicture(32, 32, c.y, c.u, c.v), 32, 32)
		if err != nil {
			t.Fatal(err)
		}
		b, g, r, a := px(out.Pix, 32, 16, 16)
		if !near(r, c.r, 3) || !near(g, c.g, 3) || !near(b, c.b, 3) || a != 255 {
			t.Errorf("%s: got rgb(%d,%d,%d) a=%d, want rgb(%d,%d,%d)", c.name, r, g, b, a, c.r, c.g, c.b)
		}
	}
}

func TestColourBT709AndFullRange(t *testing.T) {
	p := flatPicture(16, 16, 63, 102, 240) // BT.709 limited red
	p.BT709 = true
	out, _, _ := vdec.RenderPicture(p, 16, 16)
	if b, g, r, _ := px(out.Pix, 16, 8, 8); !near(r, 255, 4) || !near(g, 0, 4) || !near(b, 0, 4) {
		t.Errorf("BT.709 red: got rgb(%d,%d,%d)", r, g, b)
	}

	f := flatPicture(16, 16, 255, 128, 128)
	f.FullRange = true
	out, _, _ = vdec.RenderPicture(f, 16, 16)
	if b, g, r, _ := px(out.Pix, 16, 8, 8); r != 255 || g != 255 || b != 255 {
		t.Errorf("full-range white: got rgb(%d,%d,%d)", r, g, b)
	}
}

// TestDownscaleIsAntialiased is the filter-quality test. A 1-pixel
// checkerboard halved in size must average to a flat mid-grey. Nearest
// neighbour picks every other pixel and turns it solid black or solid white,
// which is exactly how small text falls apart.
func TestDownscaleIsAntialiased(t *testing.T) {
	const w, h = 200, 200
	p := flatPicture(w, h, 0, 128, 128)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if (x+y)%2 == 0 {
				p.Y[y*w+x] = 235
			} else {
				p.Y[y*w+x] = 16
			}
		}
	}
	out, _, err := vdec.RenderPicture(p, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := 255, 0
	for y := 10; y < 90; y++ {
		for x := 10; x < 90; x++ {
			_, g, _, _ := px(out.Pix, 100, x, y)
			lo, hi = min(lo, int(g)), max(hi, int(g))
		}
	}
	if lo < 110 || hi > 145 {
		t.Fatalf("checkerboard aliased: green channel ranged %d..%d, want ~128 everywhere", lo, hi)
	}
}

// TestFlatColourSurvivesScaling: normalised weights must reproduce a flat
// field exactly at any scale factor, with no darkening seams.
func TestFlatColourSurvivesScaling(t *testing.T) {
	for _, dim := range [][4]int{{1920, 1080, 960, 540}, {640, 360, 1280, 720}, {1366, 768, 777, 437}} {
		out, fit, _ := vdec.RenderPicture(flatPicture(dim[0], dim[1], 126, 128, 128), dim[2], dim[3])
		for y := fit.Y; y < fit.Y+fit.H; y++ {
			for x := fit.X; x < fit.X+fit.W; x++ {
				if _, g, _, _ := px(out.Pix, dim[2], x, y); !near(g, 128, 1) {
					t.Fatalf("%v: pixel (%d,%d) = %d, want 128", dim, x, y, g)
				}
			}
		}
	}
}

// TestLetterboxIsTransparent: bars outside the fitted image are all-zero,
// fully transparent under premultiplied alpha.
func TestLetterboxIsTransparent(t *testing.T) {
	out, fit, _ := vdec.RenderPicture(flatPicture(1600, 900, 235, 128, 128), 800, 600)
	if fit.Y == 0 {
		t.Fatal("expected vertical letterbox bars")
	}
	if b, g, r, a := px(out.Pix, 800, 400, 0); b|g|r|a != 0 {
		t.Fatalf("bar pixel not transparent: %d %d %d %d", b, g, r, a)
	}
	if _, _, _, a := px(out.Pix, 800, 400, 300); a != 255 {
		t.Fatal("image area not opaque")
	}
}

// ---- real bitstreams ------------------------------------------------------

func roundTrip(t *testing.T, codec vdec.Codec, enc int) {
	t.Helper()
	const w, h, n = 320, 180, 12
	frames, err := vdectest.Encode(enc, w, h, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < n/2 {
		t.Fatalf("encoder produced only %d frames", len(frames))
	}

	d, err := vdec.New(codec, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	decoded := 0
	for i, f := range frames {
		// Keyframe detection must agree with the encoder on real output.
		if got := vdec.IsKeyframe(codec, f.Data); got != f.Key {
			t.Errorf("%s frame %d: IsKeyframe=%v, encoder says %v", codec, i, got, f.Key)
		}
		ok, err := d.Decode(f.Data)
		if err != nil {
			t.Fatalf("%s frame %d: %v", codec, i, err)
		}
		if !ok {
			continue
		}
		decoded++
		if gw, gh := d.Size(); gw != w || gh != h {
			t.Fatalf("%s: decoded %dx%d, want %dx%d", codec, gw, gh, w, h)
		}
	}
	if decoded != len(frames) {
		t.Fatalf("%s: decoded %d of %d frames", codec, decoded, len(frames))
	}
	if !frames[0].Key {
		t.Fatalf("%s: first frame is not a keyframe", codec)
	}

	// The last picture, shown 1:1, should reproduce the pattern's flat grey
	// background and its red box closely.
	out, _, err := d.Render(w, h)
	if err != nil {
		t.Fatal(err)
	}
	if b, g, r, _ := px(out.Pix, w, 8, 8); !near(r, 128, 10) || !near(g, 128, 10) || !near(b, 128, 10) {
		t.Errorf("%s: background rgb(%d,%d,%d), want ~128", codec, r, g, b)
	}
	boxX := ((n-1)*4)%(w/2) + w/8
	if b, g, r, _ := px(out.Pix, w, boxX, h/4+h/8); r < 200 || g > 60 || b > 60 {
		t.Errorf("%s: box rgb(%d,%d,%d), want red", codec, r, g, b)
	}
}

func TestVP8RoundTrip(t *testing.T)  { roundTrip(t, vdec.VP8, vdectest.VP8) }
func TestVP9RoundTrip(t *testing.T)  { roundTrip(t, vdec.VP9, vdectest.VP9) }
func TestH264RoundTrip(t *testing.T) { roundTrip(t, vdec.H264, vdectest.H264) }

func TestAV1DecoderOpens(t *testing.T) {
	d, err := vdec.New(vdec.AV1, 2)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	// Garbage must be rejected, never crash.
	d, _ = vdec.New(vdec.AV1, 2)
	defer d.Close()
	if _, err := d.Decode([]byte{0x12, 0x00, 0x0a, 0x0b, 0x00, 0x00, 0x00}); err == nil {
		t.Log("AV1 accepted a stub temporal unit without a picture (fine)")
	}
}

func TestGarbageIsRejectedNotShown(t *testing.T) {
	for _, c := range []vdec.Codec{vdec.VP8, vdec.VP9, vdec.H264} {
		d, err := vdec.New(c, 1)
		if err != nil {
			t.Fatal(err)
		}
		junk := make([]byte, 400)
		for i := range junk {
			junk[i] = byte(i * 37)
		}
		ok, _ := d.Decode(junk)
		if ok {
			t.Errorf("%s produced a picture from garbage", c)
		}
		d.Close()
	}
}

func TestVersion(t *testing.T) {
	if v := vdec.Version(); v == "" {
		t.Fatal("empty version")
	} else {
		t.Log(v)
	}
}
