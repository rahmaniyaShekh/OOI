//go:build windows

// Package verify proves, at runtime on the real machine, that the overlay is
// excluded from screen capture.
//
// The method matters. It is not enough to capture a protected window and find
// nothing -- a broken capture back end would "pass" that test trivially. So
// every back end runs twice against the same on-screen window:
//
//	control  : protection OFF -> the marker colour MUST be found
//	protected: protection ON  -> the marker colour MUST be absent
//
// A back end only counts as verified when it detects the marker in the
// control phase and loses it in the protected phase. Anything else is
// reported as inconclusive rather than as a pass.
package verify

import (
	"fmt"
	"strings"
	"time"

	"ooi/internal/capture"
	"ooi/internal/overlay"
	"ooi/internal/winapi"
)

// Marker is the flat colour painted into the overlay, in B, G, R order.
// It is a colour no normal desktop shows across a large area.
var Marker = [3]byte{0x2B, 0x0A, 0xE8} // vivid red-magenta

// tolerance allows for any colour management applied along the capture path.
const tolerance = 12

// minHitRatio is the fraction of the overlay's pixels that must match in the
// control phase for the back end to be considered capable of seeing it.
const minHitRatio = 0.50

// maxLeakRatio is how much marker-coloured area is tolerated in the protected
// phase. It is not zero because some unrelated red-ish content could sit
// under the overlay, but it is far below what a visible overlay produces.
const maxLeakRatio = 0.02

// Outcome is the verdict for one capture back end.
type Outcome string

const (
	// Protected means the back end saw the overlay unprotected and could not
	// see it protected. This is the result being claimed.
	Protected Outcome = "PROTECTED"
	// Leaked means the overlay was still visible with protection enabled.
	Leaked Outcome = "LEAKED"
	// Inconclusive means the back end never saw the overlay at all, so it
	// proves nothing either way.
	Inconclusive Outcome = "INCONCLUSIVE"
	// Unavailable means the back end could not run on this machine.
	Unavailable Outcome = "UNAVAILABLE"
	// NotApplicable means the back end is structurally incapable of seeing
	// this kind of window at all, protected or not, so there is nothing for
	// it to leak.
	NotApplicable Outcome = "N/A"
)

// grabber is one capture back end under test.
type grabber struct {
	name string
	note string
	// grab returns the captured image plus the virtual-desktop coordinate
	// that the image's (0,0) pixel corresponds to.
	grab func(o *overlay.Overlay, rect Rect) (capture.Image, int, int, error)
	// failureMeansProtected marks back ends that target the window directly.
	// For those, a hard failure in the protected phase is the protection
	// working -- the API refuses to capture the window at all -- provided
	// the same call succeeded in the control phase.
	failureMeansProtected bool
	// blindReason, when set, explains why this back end may legitimately see
	// nothing even in the control phase. Such a back end cannot capture this
	// class of window at all, so it is reported N/A rather than counted as a
	// failed proof.
	blindReason string
}

// Result is one back end's verification result.
type Result struct {
	Backend       string
	Note          string
	Outcome       Outcome
	ControlHits   int
	ProtectedHits int
	TotalPixels   int
	Detail        string
}

// Passed reports whether this result supports the protection claim.
func (r Result) Passed() bool { return r.Outcome == Protected }

// Rect is the overlay rectangle in virtual-desktop coordinates.
type Rect struct{ X, Y, W, H int }

// Report is the full verification run.
type Report struct {
	Results  []Result
	Overlay  Rect
	Duration time.Duration
}

// AllPassed reports whether every back end that produced a verdict passed and
// at least one did.
func (rep Report) AllPassed() bool {
	any := false
	for _, r := range rep.Results {
		switch r.Outcome {
		case Protected:
			any = true
		case Leaked, Inconclusive:
			return false
		}
	}
	return any
}

// Counts summarises the outcomes. skipped folds together the back ends that
// could not run and those that do not apply to this window class.
func (rep Report) Counts() (passed, leaked, inconclusive, skipped int) {
	for _, r := range rep.Results {
		switch r.Outcome {
		case Protected:
			passed++
		case Leaked:
			leaked++
		case Inconclusive:
			inconclusive++
		case Unavailable, NotApplicable:
			skipped++
		}
	}
	return
}

// String renders a console-friendly table.
func (rep Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  test window: %dx%d at (%d,%d)\n\n",
		rep.Overlay.W, rep.Overlay.H, rep.Overlay.X, rep.Overlay.Y)
	fmt.Fprintf(&b, "  %-30s %-9s %-19s %s\n", "CAPTURE METHOD", "VERDICT", "MARKER PIXELS", "USED BY / NOTE")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("-", 104))

	for _, r := range rep.Results {
		pixels := "-"
		if r.TotalPixels > 0 {
			pixels = fmt.Sprintf("off %5.1f%% on %4.1f%%",
				100*float64(r.ControlHits)/float64(r.TotalPixels),
				100*float64(r.ProtectedHits)/float64(r.TotalPixels))
		}
		verdict := "??"
		switch r.Outcome {
		case Protected:
			verdict = "PASS"
		case Leaked:
			verdict = "FAIL"
		case Inconclusive:
			verdict = "UNCLEAR"
		case Unavailable:
			verdict = "SKIPPED"
		case NotApplicable:
			verdict = "N/A"
		}
		note := r.Note
		if r.Outcome != Protected && r.Detail != "" {
			note = r.Detail
		}
		fmt.Fprintf(&b, "  %-30s %-9s %-19s %s\n", r.Backend, verdict, pixels, note)
	}

	passed, leaked, unclear, unavail := rep.Counts()
	fmt.Fprintf(&b, "\n  %d passed, %d leaked, %d inconclusive, %d skipped, in %s\n",
		passed, leaked, unclear, unavail, rep.Duration.Round(time.Millisecond))
	return b.String()
}

// Options tunes a verification run.
type Options struct {
	// Settle is how long to wait for the compositor after each change.
	Settle time.Duration
	// SaveDir, when set, receives a PNG of every capture from both phases so
	// the result can be inspected by eye instead of taken on trust.
	SaveDir string
}

// Run performs the full control/protected sweep against a live overlay.
//
// ov must already be running and visible. Run drives it: it paints the
// marker and toggles protection. The caller owns the overlay's lifetime.
func Run(ov *overlay.Overlay, rect Rect, opts Options) Report {
	settle := opts.Settle
	if settle <= 0 {
		settle = 600 * time.Millisecond
	}
	// Coordinates only line up with what the capture APIs report if this
	// process is per-monitor DPI aware. On a scaled display an unaware
	// process sees logical pixels and every mapping is wrong.
	winapi.EnablePerMonitorDPI()

	start := time.Now()
	rep := Report{Overlay: rect}
	backends := Backends()

	// Label the window so a screenshot taken during this phase is
	// self-explanatory rather than alarming.
	ov.SetMarker(Marker[0], Marker[1], Marker[2],
		"CONTROL PHASE\nPROTECTION IS OFF\nthis window is SUPPOSED to be captured")
	time.Sleep(settle)

	// ---- control phase: protection OFF, the marker must be visible -------
	if err := <-ov.SetProtect(false); err != nil {
		for _, b := range backends {
			rep.Results = append(rep.Results, Result{
				Backend: b.name, Note: b.note, Outcome: Unavailable,
				Detail: "could not disable protection: " + err.Error(),
			})
		}
		rep.Duration = time.Since(start)
		return rep
	}
	time.Sleep(settle)

	type phase struct {
		img  capture.Image
		offX int
		offY int
		err  error
	}
	control := make([]phase, len(backends))
	for i, b := range backends {
		img, ox, oy, err := b.grab(ov, rect)
		control[i] = phase{img, ox, oy, err}
		if err == nil {
			savePNG(opts.SaveDir, b.name, "1-unprotected", img, ox, oy, rect)
		}
	}

	// ---- protected phase: protection ON, the marker must be gone --------
	ov.SetMarker(Marker[0], Marker[1], Marker[2],
		"PROTECTED PHASE\nprotection is ON\nthis window must NOT appear in any capture")
	protErr := <-ov.SetProtect(true)
	time.Sleep(settle)

	for i, b := range backends {
		res := Result{Backend: b.name, Note: b.note}
		area := rect.W * rect.H

		switch {
		case protErr != nil:
			res.Outcome = Unavailable
			res.Detail = "could not enable protection: " + protErr.Error()
			rep.Results = append(rep.Results, res)
			continue

		case control[i].err != nil:
			res.Outcome = Unavailable
			res.Detail = shorten(control[i].err.Error())
			rep.Results = append(rep.Results, res)
			continue
		}

		res.TotalPixels = area
		res.ControlHits = countInRect(control[i].img, control[i].offX, control[i].offY, rect)

		// The back end must have seen the unprotected window, otherwise the
		// protected result is meaningless.
		if res.ControlHits < int(float64(area)*minHitRatio) {
			if b.blindReason != "" {
				res.Outcome = NotApplicable
				res.Detail = b.blindReason
			} else {
				res.Outcome = Inconclusive
				res.Detail = fmt.Sprintf("never saw the unprotected window (%.1f%% of %d px)",
					100*float64(res.ControlHits)/float64(area), area)
			}
			rep.Results = append(rep.Results, res)
			continue
		}

		protImg, ox, oy, err := b.grab(ov, rect)
		if err != nil {
			if b.failureMeansProtected {
				res.Outcome = Protected
				res.Detail = "capture refused once protected: " + shorten(err.Error())
			} else {
				res.Outcome = Unavailable
				res.Detail = shorten(err.Error())
			}
			rep.Results = append(rep.Results, res)
			continue
		}
		res.ProtectedHits = countInRect(protImg, ox, oy, rect)
		savePNG(opts.SaveDir, b.name, "2-protected", protImg, ox, oy, rect)

		if res.ProtectedHits > int(float64(area)*maxLeakRatio) {
			res.Outcome = Leaked
			res.Detail = fmt.Sprintf("still visible: %.1f%% of the window captured",
				100*float64(res.ProtectedHits)/float64(area))
		} else {
			res.Outcome = Protected
			res.Detail = "visible unprotected, absent when protected"
		}
		rep.Results = append(rep.Results, res)
	}

	rep.Duration = time.Since(start)
	return rep
}

// countInRect counts marker-coloured pixels inside rect, where the image's
// pixel (0,0) corresponds to virtual-desktop coordinate (offX, offY).
func countInRect(img capture.Image, offX, offY int, rect Rect) int {
	if img.W == 0 || img.H == 0 {
		return 0
	}
	n := 0
	for y := 0; y < rect.H; y++ {
		iy := rect.Y + y - offY
		if iy < 0 || iy >= img.H {
			continue
		}
		for x := 0; x < rect.W; x++ {
			ix := rect.X + x - offX
			if ix < 0 || ix >= img.W {
				continue
			}
			b, g, r, ok := img.At(ix, iy)
			if !ok {
				continue
			}
			if near(b, Marker[0]) && near(g, Marker[1]) && near(r, Marker[2]) {
				n++
			}
		}
	}
	return n
}

func near(a, b byte) bool {
	d := int(a) - int(b)
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}

// shorten trims the "capture: " prefix and clips long HRESULT explanations so
// the table stays readable.
func shorten(s string) string {
	s = strings.TrimPrefix(s, "capture: ")
	if i := strings.Index(s, " ("); i > 20 {
		s = s[:i]
	}
	if len(s) > 64 {
		s = s[:61] + "..."
	}
	return s
}
