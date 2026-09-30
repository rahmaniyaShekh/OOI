//go:build windows

package verify

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"ooi/internal/capture"
)

// savePNG writes the region of a capture that corresponds to the test window,
// so both phases can be compared by eye.
//
// This exists because a table of percentages asks the reader to take the
// result on trust. Two PNGs -- one showing the window, one showing whatever
// was behind it -- do not.
func savePNG(dir, backend, phase string, img capture.Image, offX, offY int, rect Rect) {
	if dir == "" || img.W == 0 || img.H == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	out := image.NewRGBA(image.Rect(0, 0, rect.W, rect.H))
	for y := 0; y < rect.H; y++ {
		iy := rect.Y + y - offY
		for x := 0; x < rect.W; x++ {
			ix := rect.X + x - offX
			b, g, r, ok := img.At(ix, iy)
			di := out.PixOffset(x, y)
			if !ok {
				// Outside the captured area: mark it mid-grey rather than
				// leaving it black, which could be mistaken for content.
				out.Pix[di+0], out.Pix[di+1], out.Pix[di+2], out.Pix[di+3] = 0x40, 0x40, 0x40, 0xFF
				continue
			}
			out.Pix[di+0], out.Pix[di+1], out.Pix[di+2], out.Pix[di+3] = r, g, b, 0xFF
		}
	}

	name := filepath.Join(dir, slug(backend)+"__"+phase+".png")
	f, err := os.Create(name)
	if err != nil {
		return
	}
	defer f.Close()
	png.Encode(f, out)
}

// slug turns a back-end name into a safe file name component.
func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
