package frame

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestFit(t *testing.T) {
	tests := []struct {
		name           string
		sw, sh, dw, dh int
		want           Rect
	}{
		{"exact match", 100, 100, 100, 100, Rect{0, 0, 100, 100}},
		{"wide source into square", 200, 100, 100, 100, Rect{0, 25, 100, 50}},
		{"tall source into square", 100, 200, 100, 100, Rect{25, 0, 50, 100}},
		{"16:9 into 4:3", 1920, 1080, 800, 600, Rect{0, 75, 800, 450}},
		{"4:3 into 16:9", 640, 480, 1920, 1080, Rect{240, 0, 1440, 1080}},
		{"upscale preserves ratio", 16, 9, 160, 90, Rect{0, 0, 160, 90}},
		{"zero source", 0, 10, 100, 100, Rect{}},
		{"zero dest", 10, 10, 0, 100, Rect{}},
		{"negative", -5, 10, 100, 100, Rect{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Fit(tc.sw, tc.sh, tc.dw, tc.dh)
			if got != tc.want {
				t.Errorf("Fit(%d,%d,%d,%d) = %+v, want %+v", tc.sw, tc.sh, tc.dw, tc.dh, got, tc.want)
			}
		})
	}
}

// TestFitNeverExceedsDest is the invariant that actually protects the DIB:
// a result larger than the canvas would write out of bounds.
func TestFitNeverExceedsDest(t *testing.T) {
	for sw := 1; sw <= 40; sw += 3 {
		for sh := 1; sh <= 40; sh += 3 {
			for dw := 1; dw <= 40; dw += 7 {
				for dh := 1; dh <= 40; dh += 7 {
					r := Fit(sw, sh, dw, dh)
					if r.W > dw || r.H > dh {
						t.Fatalf("Fit(%d,%d,%d,%d) = %+v exceeds destination", sw, sh, dw, dh, r)
					}
					if r.X < 0 || r.Y < 0 || r.X+r.W > dw || r.Y+r.H > dh {
						t.Fatalf("Fit(%d,%d,%d,%d) = %+v falls outside destination", sw, sh, dw, dh, r)
					}
				}
			}
		}
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	if _, err := Decode(nil); err != ErrEmpty {
		t.Errorf("Decode(nil) error = %v, want ErrEmpty", err)
	}
	if _, err := Decode(make([]byte, MaxFrameBytes+1)); err != ErrTooLarge {
		t.Errorf("Decode(oversized) error = %v, want ErrTooLarge", err)
	}
	if _, err := Decode([]byte("this is definitely not an image")); err == nil {
		t.Error("Decode(garbage) succeeded, want error")
	}
}

func TestDecodeJPEGAndPNG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			src.Set(x, y, color.RGBA{R: 200, G: 40, B: 90, A: 255})
		}
	}

	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, src, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	img, err := Decode(jbuf.Bytes())
	if err != nil {
		t.Fatalf("Decode(jpeg): %v", err)
	}
	if got := img.Bounds().Dx(); got != 8 {
		t.Errorf("jpeg width = %d, want 8", got)
	}

	var pbuf bytes.Buffer
	if err := png.Encode(&pbuf, src); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if _, err := Decode(pbuf.Bytes()); err != nil {
		t.Fatalf("Decode(png): %v", err)
	}
}

func TestCanvasValid(t *testing.T) {
	if (&Canvas{}).Valid() {
		t.Error("zero Canvas reported valid")
	}
	if (&Canvas{Pix: make([]byte, 4), W: 2, H: 2}).Valid() {
		t.Error("undersized Canvas reported valid")
	}
	if !NewCanvas(3, 3).Valid() {
		t.Error("NewCanvas(3,3) reported invalid")
	}
}

// TestDrawLetterboxIsTransparent checks the property the overlay depends on:
// the bars around a letterboxed frame must be fully-zero pixels, which is
// transparent under premultiplied alpha.
func TestDrawLetterboxIsTransparent(t *testing.T) {
	// A 4x2 source into an 8x8 canvas letterboxes to rows 2..5.
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for i := range src.Pix {
		src.Pix[i] = 0xFF // opaque white
	}

	c := NewCanvas(8, 8)
	got := c.Draw(src)
	want := Rect{X: 0, Y: 2, W: 8, H: 4}
	if got != want {
		t.Fatalf("Draw returned %+v, want %+v", got, want)
	}

	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			i := (y*8 + x) * 4
			inside := y >= want.Y && y < want.Y+want.H
			alpha := c.Pix[i+3]
			if inside && alpha != 0xFF {
				t.Fatalf("pixel (%d,%d) inside image has alpha %d, want 255", x, y, alpha)
			}
			if !inside {
				if c.Pix[i] != 0 || c.Pix[i+1] != 0 || c.Pix[i+2] != 0 || alpha != 0 {
					t.Fatalf("letterbox pixel (%d,%d) = %v, want all zero", x, y,
						c.Pix[i:i+4])
				}
			}
		}
	}
}

func TestDrawSwapsToBGRA(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	// Distinct channel values so a swap error is unmissable.
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			src.Set(x, y, color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xFF})
		}
	}
	c := NewCanvas(2, 2)
	c.Draw(src)
	if c.Pix[0] != 0x30 || c.Pix[1] != 0x20 || c.Pix[2] != 0x10 || c.Pix[3] != 0xFF {
		t.Errorf("pixel = B:%#x G:%#x R:%#x A:%#x, want B:0x30 G:0x20 R:0x10 A:0xff",
			c.Pix[0], c.Pix[1], c.Pix[2], c.Pix[3])
	}
}

func TestDrawNRGBAPremultiplies(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	// Straight alpha: half-transparent pure red.
	src.SetNRGBA(0, 0, color.NRGBA{R: 0xFF, G: 0x00, B: 0x00, A: 0x80})

	c := NewCanvas(1, 1)
	c.Draw(src)

	// Premultiplied red should be about 0xFF * 0x80/0xFF = 0x80.
	if c.Pix[3] != 0x80 {
		t.Fatalf("alpha = %#x, want 0x80", c.Pix[3])
	}
	if c.Pix[2] > 0x81 || c.Pix[2] < 0x7F {
		t.Errorf("red = %#x, want ~0x80 (premultiplied)", c.Pix[2])
	}
	if c.Pix[0] != 0 || c.Pix[1] != 0 {
		t.Errorf("blue/green = %#x/%#x, want 0/0", c.Pix[0], c.Pix[1])
	}
}

// TestDrawYCbCr covers the hot path: image/jpeg hands back *image.YCbCr.
func TestDrawYCbCr(t *testing.T) {
	orig := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			orig.Set(x, y, color.RGBA{R: 0xE0, G: 0x30, B: 0x40, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, orig, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := img.(*image.YCbCr); !ok {
		t.Fatalf("expected *image.YCbCr from jpeg, got %T", img)
	}

	c := NewCanvas(16, 16)
	c.Draw(img)

	// Sample the centre; JPEG is lossy so allow a generous tolerance.
	i := (8*16 + 8) * 4
	b, g, r, a := c.Pix[i], c.Pix[i+1], c.Pix[i+2], c.Pix[i+3]
	if a != 0xFF {
		t.Errorf("alpha = %#x, want 0xff", a)
	}
	if absInt(int(r)-0xE0) > 12 || absInt(int(g)-0x30) > 12 || absInt(int(b)-0x40) > 12 {
		t.Errorf("colour = R:%#x G:%#x B:%#x, want ~R:0xe0 G:0x30 B:0x40", r, g, b)
	}
}

func TestDrawGenericPath(t *testing.T) {
	// image.Gray takes the default (At-based) branch.
	src := image.NewGray(image.Rect(0, 0, 4, 4))
	for i := range src.Pix {
		src.Pix[i] = 0x80
	}
	c := NewCanvas(4, 4)
	if got := c.Draw(src); got.W != 4 || got.H != 4 {
		t.Fatalf("Draw returned %+v, want 4x4", got)
	}
	if c.Pix[0] != 0x80 || c.Pix[3] != 0xFF {
		t.Errorf("grey pixel = %v, want B=0x80 A=0xff", c.Pix[0:4])
	}
}

func TestDrawHandlesNilAndInvalid(t *testing.T) {
	c := NewCanvas(4, 4)
	if got := c.Draw(nil); got != (Rect{}) {
		t.Errorf("Draw(nil) = %+v, want zero", got)
	}
	empty := &Canvas{}
	if got := empty.Draw(image.NewRGBA(image.Rect(0, 0, 2, 2))); got != (Rect{}) {
		t.Errorf("invalid canvas Draw = %+v, want zero", got)
	}
}

// TestDrawSubImageOffset guards the offset arithmetic: a sub-image has a
// non-zero Bounds().Min, and reading from the wrong origin is a classic bug.
func TestDrawSubImageOffset(t *testing.T) {
	full := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			// Left half black, right half a known colour.
			if x >= 4 {
				full.Set(x, y, color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xFF})
			} else {
				full.Set(x, y, color.RGBA{A: 0xFF})
			}
		}
	}
	sub := full.SubImage(image.Rect(4, 0, 8, 8)).(*image.RGBA)

	c := NewCanvas(4, 8)
	c.Draw(sub)
	if c.Pix[0] != 0x33 || c.Pix[1] != 0x22 || c.Pix[2] != 0x11 {
		t.Errorf("sub-image pixel = %v, want B:0x33 G:0x22 R:0x11 (read from wrong origin?)",
			c.Pix[0:4])
	}
}

func TestFillBGRA(t *testing.T) {
	c := NewCanvas(3, 3)
	c.FillBGRA(0x0A, 0x0B, 0x0C)
	for i := 0; i < len(c.Pix); i += 4 {
		if c.Pix[i] != 0x0A || c.Pix[i+1] != 0x0B || c.Pix[i+2] != 0x0C || c.Pix[i+3] != 0xFF {
			t.Fatalf("pixel %d = %v, want 0A 0B 0C FF", i/4, c.Pix[i:i+4])
		}
	}
}

func TestClear(t *testing.T) {
	c := NewCanvas(2, 2)
	c.FillBGRA(1, 2, 3)
	c.Clear()
	for i, v := range c.Pix {
		if v != 0 {
			t.Fatalf("byte %d = %d after Clear, want 0", i, v)
		}
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func BenchmarkDrawYCbCr1080p(b *testing.B) {
	orig := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	var buf bytes.Buffer
	jpeg.Encode(&buf, orig, &jpeg.Options{Quality: 70})
	img, err := Decode(buf.Bytes())
	if err != nil {
		b.Fatal(err)
	}
	c := NewCanvas(1920, 1080)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Draw(img)
	}
}
