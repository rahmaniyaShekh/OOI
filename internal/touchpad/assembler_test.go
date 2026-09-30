//go:build windows

package touchpad

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func c(id int, x, y float64) Contact { return Contact{ID: id, X: x, Y: y} }

// TestSingleReportFrame is the common case: every finger fits in one report.
func TestSingleReportFrame(t *testing.T) {
	var a Assembler
	f, ok := a.Push([]Contact{c(1, 0.2, 0.3), c(2, 0.4, 0.5)}, 2, at(0))
	if !ok {
		t.Fatal("a complete single-report frame was not emitted")
	}
	if f.Count() != 2 {
		t.Errorf("Count = %d, want 2", f.Count())
	}
	if a.Pending() != 0 {
		t.Errorf("Pending = %d after emitting, want 0", a.Pending())
	}
}

// TestSplitFrameIsStitched is the reason this type exists: a device that
// splits three fingers across two reports must not look like a one-finger
// touch followed by a two-finger touch.
func TestSplitFrameIsStitched(t *testing.T) {
	var a Assembler

	// First report: total is 3, but only 2 fingers fit.
	if _, ok := a.Push([]Contact{c(1, 0.1, 0.1), c(2, 0.2, 0.2)}, 3, at(0)); ok {
		t.Fatal("an incomplete frame was emitted early")
	}
	if a.Pending() != 2 {
		t.Errorf("Pending = %d, want 2", a.Pending())
	}

	// Continuation: count 0, the third finger.
	f, ok := a.Push([]Contact{c(3, 0.3, 0.3)}, 0, at(2))
	if !ok {
		t.Fatal("the frame was not emitted once complete")
	}
	if f.Count() != 3 {
		t.Fatalf("Count = %d, want 3", f.Count())
	}
	for i, want := range []int{1, 2, 3} {
		if f.Contacts[i].ID != want {
			t.Errorf("contact %d has ID %d, want %d", i, f.Contacts[i].ID, want)
		}
	}
}

func TestThreeWaySplit(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.1, 0.1)}, 3, at(0))
	a.Push([]Contact{c(2, 0.2, 0.2)}, 0, at(1))
	f, ok := a.Push([]Contact{c(3, 0.3, 0.3)}, 0, at(2))
	if !ok || f.Count() != 3 {
		t.Fatalf("three-way split produced ok=%v count=%d, want true/3", ok, f.Count())
	}
}

// TestAllFingersLiftedEmitsEmptyFrame: the lift is what ends a gesture, so it
// must always surface.
func TestAllFingersLiftedEmitsEmptyFrame(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.5, 0.5)}, 1, at(0))

	f, ok := a.Push(nil, 0, at(10))
	if !ok {
		t.Fatal("the all-lifted report did not emit a frame")
	}
	if f.Count() != 0 {
		t.Errorf("Count = %d, want 0", f.Count())
	}
}

// TestLiftDuringIncompleteFrameStillEmits covers fingers leaving the pad
// midway through a split frame; leaving the assembler stuck would freeze
// gesture recognition permanently.
func TestLiftDuringIncompleteFrameStillEmits(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.1, 0.1)}, 3, at(0)) // expects 3, has 1

	f, ok := a.Push(nil, 0, at(5))
	if !ok {
		t.Fatal("a lift during an incomplete frame did not emit")
	}
	if f.Count() != 0 {
		t.Errorf("Count = %d, want 0", f.Count())
	}
	if a.Pending() != 0 {
		t.Errorf("Pending = %d after the lift, want 0", a.Pending())
	}
}

// TestNewFrameDiscardsStalePartial covers a dropped continuation report: the
// next frame's count restarts assembly rather than concatenating.
func TestNewFrameDiscardsStalePartial(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.1, 0.1)}, 3, at(0)) // continuation never arrives

	f, ok := a.Push([]Contact{c(7, 0.7, 0.7)}, 1, at(20))
	if !ok {
		t.Fatal("the new frame was not emitted")
	}
	if f.Count() != 1 {
		t.Fatalf("Count = %d, want 1; the stale partial leaked in", f.Count())
	}
	if f.Contacts[0].ID != 7 {
		t.Errorf("contact ID = %d, want 7", f.Contacts[0].ID)
	}
}

// TestContinuationWithoutStartIsKept: some devices omit the count entirely.
// Dropping those reports would make the touchpad appear dead.
func TestContinuationWithoutStartIsKept(t *testing.T) {
	var a Assembler
	f, ok := a.Push([]Contact{c(1, 0.4, 0.4), c(2, 0.6, 0.6)}, 0, at(0))
	if !ok {
		t.Fatal("a countless report was dropped")
	}
	if f.Count() != 2 {
		t.Errorf("Count = %d, want 2", f.Count())
	}
}

// TestMoreContactsThanCount guards against a device reporting a count lower
// than the contacts it actually sent.
func TestMoreContactsThanCount(t *testing.T) {
	var a Assembler
	f, ok := a.Push([]Contact{c(1, 0.1, 0.1), c(2, 0.2, 0.2), c(3, 0.3, 0.3)}, 2, at(0))
	if !ok {
		t.Fatal("frame not emitted")
	}
	if f.Count() != 3 {
		t.Errorf("Count = %d, want 3 (all reported contacts kept)", f.Count())
	}
}

// TestEmittedSliceIsIndependent: the assembler reuses its buffer, so a
// caller holding a previous frame must not see it mutate.
func TestEmittedSliceIsIndependent(t *testing.T) {
	var a Assembler
	f1, _ := a.Push([]Contact{c(1, 0.1, 0.1)}, 1, at(0))
	f2, _ := a.Push([]Contact{c(2, 0.9, 0.9)}, 1, at(10))

	if f1.Contacts[0].ID != 1 || f1.Contacts[0].X != 0.1 {
		t.Errorf("the first frame was mutated by the second: %+v", f1.Contacts[0])
	}
	if f2.Contacts[0].ID != 2 {
		t.Errorf("second frame = %+v, want ID 2", f2.Contacts[0])
	}
}

func TestResetClearsPartial(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.1, 0.1)}, 3, at(0))
	if a.Pending() == 0 {
		t.Fatal("nothing pending to reset")
	}
	a.Reset()
	if a.Pending() != 0 {
		t.Errorf("Pending = %d after Reset, want 0", a.Pending())
	}
}

func TestCentroid(t *testing.T) {
	f := Frame{Contacts: []Contact{c(1, 0.0, 0.0), c(2, 1.0, 0.5)}}
	x, y := f.Centroid()
	if x != 0.5 || y != 0.25 {
		t.Errorf("Centroid = (%.3f, %.3f), want (0.500, 0.250)", x, y)
	}

	empty := Frame{}
	if x, y := empty.Centroid(); x != 0 || y != 0 {
		t.Errorf("empty Centroid = (%.3f, %.3f), want (0, 0)", x, y)
	}
}

// TestRepeatedLiftsAreIdempotent: devices often send several all-up reports.
func TestRepeatedLiftsAreIdempotent(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.5, 0.5)}, 1, at(0))
	for i := 0; i < 4; i++ {
		f, ok := a.Push(nil, 0, at(10+i))
		if !ok || f.Count() != 0 {
			t.Fatalf("lift %d produced ok=%v count=%d", i, ok, f.Count())
		}
	}
}

// TestLiftFrameMustSurvive is a regression guard for the bug that made taps
// and gesture release stop working on real hardware.
//
// The all-lifted report carries no contacts and a contact count of zero. An
// earlier version reported that as "not a touch report", so the reader threw
// it away, release() was never called, and the recogniser stayed stuck in the
// previous gesture forever. The assembler must always turn it into a frame.
func TestLiftFrameMustSurvive(t *testing.T) {
	var a Assembler

	// A touch, then the lift that ends it.
	if _, ok := a.Push([]Contact{c(1, 0.5, 0.5)}, 1, at(0)); !ok {
		t.Fatal("the touch frame was not emitted")
	}
	f, ok := a.Push(nil, 0, at(50))
	if !ok {
		t.Fatal("the all-lifted report produced no frame; gestures can never end")
	}
	if f.Count() != 0 {
		t.Errorf("lift frame Count = %d, want 0", f.Count())
	}

	// A completely fresh gesture must work immediately afterwards.
	f2, ok := a.Push([]Contact{c(1, 0.2, 0.2), c(2, 0.3, 0.3)}, 2, at(100))
	if !ok || f2.Count() != 2 {
		t.Errorf("the next gesture produced ok=%v count=%d, want true/2", ok, f2.Count())
	}
}

// TestLiftReportedAsCountWithNoTipDown is the regression guard for the bug
// that survived the first fix and still broke taps on real hardware.
//
// An ELAN Precision Touchpad signals a lift by reporting the departing
// contact one more time with its tip switch off — so the frame still declares
// a contact count of one, but no contact is actually touching. Waiting for a
// contact that never arrives strands the assembler, and since a lift is what
// completes a tap and ends a hold, most gestures silently stop working.
func TestLiftReportedAsCountWithNoTipDown(t *testing.T) {
	var a Assembler

	if _, ok := a.Push([]Contact{c(1, 0.5, 0.5)}, 1, at(0)); !ok {
		t.Fatal("the touch frame was not emitted")
	}

	// The lift: count still says 1, but nothing has its tip down.
	f, ok := a.Push(nil, 1, at(40))
	if !ok {
		t.Fatal("a lift reported as count>0 with no tip-down contacts was swallowed")
	}
	if f.Count() != 0 {
		t.Errorf("lift frame Count = %d, want 0", f.Count())
	}
	if a.Pending() != 0 {
		t.Errorf("Pending = %d after the lift, want 0 (assembler stranded)", a.Pending())
	}

	// The next gesture must work immediately.
	f2, ok := a.Push([]Contact{c(1, 0.2, 0.2), c(2, 0.3, 0.3)}, 2, at(100))
	if !ok || f2.Count() != 2 {
		t.Errorf("the next gesture produced ok=%v count=%d, want true/2", ok, f2.Count())
	}
}

// TestTwoFingerLiftReportedWithCount covers both fingers leaving at once.
func TestTwoFingerLiftReportedWithCount(t *testing.T) {
	var a Assembler
	a.Push([]Contact{c(1, 0.4, 0.4), c(2, 0.6, 0.6)}, 2, at(0))

	f, ok := a.Push(nil, 2, at(30))
	if !ok || f.Count() != 0 {
		t.Errorf("two-finger lift produced ok=%v count=%d, want true/0", ok, f.Count())
	}
}

// TestNormaliseClampsToUnitRange checks the coordinate mapping that makes the
// gesture layer independent of pad size, which is what lets it work on any
// laptop without retuning.
func TestNormaliseClampsToUnitRange(t *testing.T) {
	tests := []struct {
		name   string
		v      uint32
		lo, hi int32
		want   float64
	}{
		{"midpoint", 500, 0, 1000, 0.5},
		{"at minimum", 0, 0, 1000, 0.0},
		{"at maximum", 1000, 0, 1000, 1.0},
		{"beyond maximum clamps", 5000, 0, 1000, 1.0},
		{"offset range", 150, 100, 200, 0.5},
		{"degenerate range", 50, 100, 100, 0.0},
		{"inverted range", 50, 200, 100, 0.0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := norm(tc.v, tc.lo, tc.hi); got != tc.want {
				t.Errorf("norm(%d, %d, %d) = %.3f, want %.3f", tc.v, tc.lo, tc.hi, got, tc.want)
			}
		})
	}
}

// TestDifferentPadSizesProduceSameNormalised is the portability guarantee: a
// touch in the middle of a small pad and a large one must look identical.
func TestDifferentPadSizesProduceSameNormalised(t *testing.T) {
	small := norm(640, 0, 1280)  // a compact pad
	large := norm(2900, 0, 5800) // a large one
	if small != large {
		t.Errorf("mid-pad touch normalised differently: %.4f vs %.4f", small, large)
	}
}
