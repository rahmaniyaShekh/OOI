//go:build windows

package verify

import (
	"strings"
	"testing"
	"time"

	"ooi/internal/capture"
	"ooi/internal/overlay"
)

func TestSlug(t *testing.T) {
	tests := []struct{ in, want string }{
		{"WGC monitor capture", "wgc-monitor-capture"},
		{"GDI BitBlt + CAPTUREBLT", "gdi-bitblt-captureblt"},
		{"PrintWindow (full content)", "printwindow-full-content"},
		{"   ", ""},
		{"a", "a"},
	}
	for _, tc := range tests {
		if got := slug(tc.in); got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNear(t *testing.T) {
	if !near(100, 100) {
		t.Error("near(100,100) = false")
	}
	if !near(100, 100+tolerance) {
		t.Error("a difference of exactly the tolerance should be near")
	}
	if near(100, 100+tolerance+1) {
		t.Error("a difference beyond the tolerance should not be near")
	}
	if !near(0, 0) || near(0, 255) {
		t.Error("edge values misclassified")
	}
}

func TestCountInRect(t *testing.T) {
	// A 4x4 image whose top-left 2x2 block is the marker colour.
	img := capture.Image{W: 4, H: 4, Pix: make([]byte, 4*4*4)}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			i := (y*4 + x) * 4
			if x < 2 && y < 2 {
				img.Pix[i+0], img.Pix[i+1], img.Pix[i+2] = Marker[0], Marker[1], Marker[2]
			}
			img.Pix[i+3] = 0xFF
		}
	}

	if got := countInRect(img, 0, 0, Rect{0, 0, 4, 4}); got != 4 {
		t.Errorf("full rect = %d markers, want 4", got)
	}
	if got := countInRect(img, 0, 0, Rect{0, 0, 2, 2}); got != 4 {
		t.Errorf("marker block = %d, want 4", got)
	}
	if got := countInRect(img, 0, 0, Rect{2, 2, 2, 2}); got != 0 {
		t.Errorf("non-marker block = %d, want 0", got)
	}

	// With an offset, the same rect maps elsewhere in the image.
	if got := countInRect(img, 10, 10, Rect{10, 10, 2, 2}); got != 4 {
		t.Errorf("offset mapping = %d, want 4", got)
	}
	// Coordinates outside the image contribute nothing rather than panicking.
	if got := countInRect(img, 0, 0, Rect{100, 100, 50, 50}); got != 0 {
		t.Errorf("out-of-range rect = %d, want 0", got)
	}
	if got := countInRect(capture.Image{}, 0, 0, Rect{0, 0, 4, 4}); got != 0 {
		t.Errorf("empty image = %d, want 0", got)
	}
}

func TestReportCountsAndString(t *testing.T) {
	rep := Report{
		Overlay: Rect{X: 10, Y: 20, W: 640, H: 360},
		Results: []Result{
			{Backend: "A", Outcome: Protected, TotalPixels: 100, ControlHits: 100},
			{Backend: "B", Outcome: Protected, TotalPixels: 100, ControlHits: 100},
			{Backend: "C", Outcome: Leaked, TotalPixels: 100, ControlHits: 100, ProtectedHits: 90},
			{Backend: "D", Outcome: Inconclusive},
			{Backend: "E", Outcome: Unavailable},
			{Backend: "F", Outcome: NotApplicable},
		},
		Duration: 1500 * time.Millisecond,
	}
	passed, leaked, unclear, skipped := rep.Counts()
	if passed != 2 || leaked != 1 || unclear != 1 || skipped != 2 {
		t.Errorf("Counts() = %d,%d,%d,%d, want 2,1,1,2", passed, leaked, unclear, skipped)
	}
	if rep.AllPassed() {
		t.Error("AllPassed() = true despite a leak")
	}

	s := rep.String()
	for _, want := range []string{"640x360", "(10,20)", "PASS", "FAIL", "UNCLEAR", "N/A", "SKIPPED"} {
		if !strings.Contains(s, want) {
			t.Errorf("report is missing %q:\n%s", want, s)
		}
	}
}

func TestAllPassedRequiresAtLeastOnePass(t *testing.T) {
	empty := Report{}
	if empty.AllPassed() {
		t.Error("an empty report should not count as passed")
	}
	allSkipped := Report{Results: []Result{{Outcome: Unavailable}, {Outcome: NotApplicable}}}
	if allSkipped.AllPassed() {
		t.Error("a report with no verdicts should not count as passed")
	}
	onePass := Report{Results: []Result{{Outcome: Protected}, {Outcome: Unavailable}}}
	if !onePass.AllPassed() {
		t.Error("one pass alongside a skip should count as passed")
	}
}

func TestBackendsAreWellFormed(t *testing.T) {
	bs := Backends()
	if len(bs) < 8 {
		t.Fatalf("only %d back ends registered, expected the full set", len(bs))
	}
	seen := map[string]bool{}
	for _, b := range bs {
		if b.name == "" {
			t.Error("a back end has no name")
		}
		if seen[b.name] {
			t.Errorf("duplicate back end name %q", b.name)
		}
		seen[b.name] = true
		if b.grab == nil {
			t.Errorf("back end %q has no grab function", b.name)
		}
	}
	// The two capture APIs that matter most today must be present.
	for _, must := range []string{"WGC monitor capture", "WGC window capture", "DXGI Desktop Duplication"} {
		if !seen[must] {
			t.Errorf("back end %q is missing", must)
		}
	}
}

// TestFullVerificationRun is the end-to-end proof: a real window, real
// capture APIs, real A/B comparison. It is the same code path `overlayc
// verify` runs.
func TestFullVerificationRun(t *testing.T) {
	if testing.Short() {
		t.Skip("puts a window on screen and drives every capture API; skipped in -short")
	}

	rect := Rect{X: 150, Y: 150, W: 480, H: 300}
	ov := overlay.New(overlay.Config{
		X: rect.X, Y: rect.Y, W: rect.W, H: rect.H,
		Opacity:      255,
		ClickThrough: true,
		Protect:      false,
		Hotkeys:      false,
		Placeholder:  "verification test",
	})

	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()

	select {
	case <-ov.Ready():
	case err := <-runErr:
		t.Fatalf("overlay exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("overlay did not become ready")
	}
	defer func() {
		ov.Stop()
		select {
		case <-ov.Closed():
		case <-time.After(5 * time.Second):
		}
	}()

	rep := Run(ov, rect, Options{Settle: 700 * time.Millisecond, SaveDir: t.TempDir()})
	t.Logf("verification report:\n%s", rep.String())

	passed, leaked, unclear, _ := rep.Counts()

	if leaked > 0 {
		for _, r := range rep.Results {
			if r.Outcome == Leaked {
				t.Errorf("%s LEAKED: %s", r.Backend, r.Detail)
			}
		}
	}
	if unclear > 0 {
		for _, r := range rep.Results {
			if r.Outcome == Inconclusive {
				t.Errorf("%s was inconclusive: %s", r.Backend, r.Detail)
			}
		}
	}
	if passed == 0 {
		t.Fatal("no capture back end produced a verdict; the run proves nothing")
	}

	// The modern capture APIs are the ones the claim really rests on, so
	// require that at least one of them was actually exercised.
	modern := false
	for _, r := range rep.Results {
		if r.Outcome == Protected &&
			(strings.HasPrefix(r.Backend, "WGC") || strings.HasPrefix(r.Backend, "DXGI")) {
			modern = true
		}
	}
	if !modern {
		t.Error("neither WGC nor DXGI produced a PASS; the important paths went untested")
	}
}
