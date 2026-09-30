// Package frame turns encoded frames coming off the wire into the exact
// top-down BGRA byte layout that UpdateLayeredWindow expects.
//
// The buffer produced here uses *premultiplied* alpha because the overlay is
// drawn with ULW_ALPHA/AC_SRC_ALPHA. Letterbox bars are written as all-zero
// pixels, which is fully transparent under premultiplication, so the overlay
// only ever paints the actual video area.
package frame

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // decoders registered for Decode
	_ "image/png"
)

// MaxFrameBytes bounds a single encoded frame so a hostile or buggy sender
// cannot make the receiver allocate without limit.
const MaxFrameBytes = 16 << 20 // 16 MiB

var (
	// ErrTooLarge is returned when an encoded frame exceeds MaxFrameBytes.
	ErrTooLarge = errors.New("frame: encoded frame too large")
	// ErrEmpty is returned for a zero-length payload.
	ErrEmpty = errors.New("frame: empty payload")
)

// Rect is a plain integer rectangle (x, y, w, h).
type Rect struct{ X, Y, W, H int }

// Fit computes the largest rectangle with the aspect ratio of srcW x srcH that
// fits inside dstW x dstH, centred. It is the letterbox calculation and is
// deliberately free of any Windows dependency so it can be tested anywhere.
//
// Degenerate inputs (any dimension <= 0) yield a zero rectangle.
func Fit(srcW, srcH, dstW, dstH int) Rect {
	if srcW <= 0 || srcH <= 0 || dstW <= 0 || dstH <= 0 {
		return Rect{}
	}
	// Compare srcW/srcH against dstW/dstH without floating point.
	var w, h int
	if srcW*dstH > dstW*srcH {
		// Source is relatively wider: width-limited.
		w = dstW
		h = srcH * dstW / srcW
	} else {
		// Source is relatively taller: height-limited.
		h = dstH
		w = srcW * dstH / srcH
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	if w > dstW {
		w = dstW
	}
	if h > dstH {
		h = dstH
	}
	return Rect{X: (dstW - w) / 2, Y: (dstH - h) / 2, W: w, H: h}
}

// Decode decodes an encoded frame (JPEG/PNG) with a size guard.
func Decode(b []byte) (image.Image, error) {
	if len(b) == 0 {
		return nil, ErrEmpty
	}
	if len(b) > MaxFrameBytes {
		return nil, ErrTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("frame: decode: %w", err)
	}
	return img, nil
}

// Canvas is a fixed-size top-down BGRA surface.
//
// Pix aliases memory owned by a Windows DIB section in production, so Canvas
// never reallocates it; callers construct the Canvas around existing storage.
type Canvas struct {
	Pix  []byte // len == W*H*4, order B,G,R,A
	W, H int
}

// NewCanvas allocates a standalone canvas. Used by tests and by the headless
// code paths; the overlay wraps DIB memory with Canvas{} directly.
func NewCanvas(w, h int) *Canvas {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	return &Canvas{Pix: make([]byte, w*h*4), W: w, H: h}
}

// Clear zeroes the canvas, which is fully transparent under premultiplied alpha.
func (c *Canvas) Clear() {
	clear(c.Pix)
}

// Valid reports whether the backing buffer matches the declared dimensions.
func (c *Canvas) Valid() bool {
	return c.W > 0 && c.H > 0 && len(c.Pix) >= c.W*c.H*4
}

// Prepared is a canvas-sized, premultiplied, top-down BGRA image that has
// already been resampled and letterboxed (by the C video pipeline). When its
// size matches the canvas, drawing it is a single copy.
type Prepared struct {
	Pix  []byte // len == W*H*4, order B,G,R,A, premultiplied
	W, H int
}

// ColorModel implements image.Image.
func (p *Prepared) ColorModel() color.Model { return color.RGBAModel }

// Bounds implements image.Image.
func (p *Prepared) Bounds() image.Rectangle { return image.Rect(0, 0, p.W, p.H) }

// At implements image.Image. It is only used on the slow path, when the overlay
// was resized between rendering this frame and drawing it.
func (p *Prepared) At(x, y int) color.Color {
	if x < 0 || y < 0 || x >= p.W || y >= p.H {
		return color.RGBA{}
	}
	i := (y*p.W + x) * 4
	return color.RGBA{R: p.Pix[i+2], G: p.Pix[i+1], B: p.Pix[i+0], A: p.Pix[i+3]}
}

// Draw letterboxes src into the canvas, clearing the surrounding area.
// Scaling is nearest-neighbour: senders are told the exact target size, so the
// common path is 1:1 and this only engages when the sender lags a resize.
//
// It returns the destination rectangle that was painted.
func (c *Canvas) Draw(src image.Image) Rect {
	// Fast path: a frame the video pipeline already fitted to this canvas.
	if p, ok := src.(*Prepared); ok && c.Valid() && p.W == c.W && p.H == c.H && len(p.Pix) >= c.W*c.H*4 {
		copy(c.Pix, p.Pix[:c.W*c.H*4])
		return Rect{W: c.W, H: c.H}
	}
	c.Clear()
	if !c.Valid() || src == nil {
		return Rect{}
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return Rect{}
	}
	dst := Fit(sw, sh, c.W, c.H)
	if dst.W == 0 || dst.H == 0 {
		return Rect{}
	}

	switch s := src.(type) {
	case *image.RGBA:
		c.drawRGBA(s, dst, sw, sh)
	case *image.NRGBA:
		c.drawNRGBA(s, dst, sw, sh)
	case *image.YCbCr:
		c.drawYCbCr(s, dst, sw, sh)
	default:
		c.drawGeneric(src, dst, sw, sh)
	}
	return dst
}

// rowMap precomputes the source coordinate for each destination coordinate.
func rowMap(dstLen, srcLen int) []int {
	m := make([]int, dstLen)
	for i := range m {
		v := i * srcLen / dstLen
		if v >= srcLen {
			v = srcLen - 1
		}
		m[i] = v
	}
	return m
}

func (c *Canvas) drawRGBA(s *image.RGBA, dst Rect, sw, sh int) {
	xm := rowMap(dst.W, sw)
	ym := rowMap(dst.H, sh)
	for dy := 0; dy < dst.H; dy++ {
		srow := s.PixOffset(s.Rect.Min.X, s.Rect.Min.Y+ym[dy])
		drow := ((dst.Y + dy) * c.W) * 4
		for dx := 0; dx < dst.W; dx++ {
			si := srow + xm[dx]*4
			di := drow + (dst.X+dx)*4
			// image.RGBA is already premultiplied; just swap R and B.
			c.Pix[di+0] = s.Pix[si+2]
			c.Pix[di+1] = s.Pix[si+1]
			c.Pix[di+2] = s.Pix[si+0]
			c.Pix[di+3] = s.Pix[si+3]
		}
	}
}

func (c *Canvas) drawNRGBA(s *image.NRGBA, dst Rect, sw, sh int) {
	xm := rowMap(dst.W, sw)
	ym := rowMap(dst.H, sh)
	for dy := 0; dy < dst.H; dy++ {
		srow := s.PixOffset(s.Rect.Min.X, s.Rect.Min.Y+ym[dy])
		drow := ((dst.Y + dy) * c.W) * 4
		for dx := 0; dx < dst.W; dx++ {
			si := srow + xm[dx]*4
			di := drow + (dst.X+dx)*4
			a := uint32(s.Pix[si+3])
			// Premultiply.
			c.Pix[di+0] = byte(uint32(s.Pix[si+2]) * a / 255)
			c.Pix[di+1] = byte(uint32(s.Pix[si+1]) * a / 255)
			c.Pix[di+2] = byte(uint32(s.Pix[si+0]) * a / 255)
			c.Pix[di+3] = byte(a)
		}
	}
}

func (c *Canvas) drawYCbCr(s *image.YCbCr, dst Rect, sw, sh int) {
	xm := rowMap(dst.W, sw)
	ym := rowMap(dst.H, sh)
	for dy := 0; dy < dst.H; dy++ {
		sy := s.Rect.Min.Y + ym[dy]
		yrow := (sy - s.Rect.Min.Y) * s.YStride
		drow := ((dst.Y + dy) * c.W) * 4
		for dx := 0; dx < dst.W; dx++ {
			sx := s.Rect.Min.X + xm[dx]
			yi := yrow + (sx - s.Rect.Min.X)
			ci := s.COffset(sx, sy)
			r, g, b := ycbcrToRGB(s.Y[yi], s.Cb[ci], s.Cr[ci])
			di := drow + (dst.X+dx)*4
			c.Pix[di+0] = b
			c.Pix[di+1] = g
			c.Pix[di+2] = r
			c.Pix[di+3] = 0xFF
		}
	}
}

func (c *Canvas) drawGeneric(src image.Image, dst Rect, sw, sh int) {
	sb := src.Bounds()
	xm := rowMap(dst.W, sw)
	ym := rowMap(dst.H, sh)
	for dy := 0; dy < dst.H; dy++ {
		drow := ((dst.Y + dy) * c.W) * 4
		for dx := 0; dx < dst.W; dx++ {
			// RGBA() returns alpha-premultiplied 16-bit values.
			r, g, b, a := src.At(sb.Min.X+xm[dx], sb.Min.Y+ym[dy]).RGBA()
			di := drow + (dst.X+dx)*4
			c.Pix[di+0] = byte(b >> 8)
			c.Pix[di+1] = byte(g >> 8)
			c.Pix[di+2] = byte(r >> 8)
			c.Pix[di+3] = byte(a >> 8)
		}
	}
}

// FillBGRA paints the whole canvas one opaque colour. Used by the capture
// verifier to place an unmistakable marker on screen.
func (c *Canvas) FillBGRA(b, g, r byte) {
	if !c.Valid() {
		return
	}
	n := c.W * c.H * 4
	for i := 0; i < n; i += 4 {
		c.Pix[i+0] = b
		c.Pix[i+1] = g
		c.Pix[i+2] = r
		c.Pix[i+3] = 0xFF
	}
}

// ycbcrToRGB defers to the standard library so the JFIF conversion (including
// its clamping behaviour) matches image/jpeg's own output exactly.
func ycbcrToRGB(y, cb, cr byte) (byte, byte, byte) {
	return color.YCbCrToRGB(y, cb, cr)
}
