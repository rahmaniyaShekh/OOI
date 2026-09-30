package gesture

import (
	"math"
	"testing"
	"time"
)

// base is a fixed origin so every test timeline is deterministic.
var base = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

// driver feeds frames into a Recognizer and collects everything it emits.
type driver struct {
	r      *Recognizer
	events []Event
}

func newDriver(cfg Config) *driver { return &driver{r: New(cfg)} }

// touch pushes one frame.
func (d *driver) touch(count int, x, y float64, ms int) {
	d.events = d.r.Update(Frame{Count: count, X: x, Y: y, At: at(ms)}, d.events)
}

// lift pushes an all-fingers-up frame.
func (d *driver) lift(ms int) {
	d.events = d.r.Update(Frame{Count: 0, At: at(ms)}, d.events)
}

// hold presses `count` fingers at (x,y) and keeps them there, emitting a frame
// every `stepMs` from startMs to endMs inclusive.
func (d *driver) hold(count int, x, y float64, startMs, endMs, stepMs int) {
	for t := startMs; t <= endMs; t += stepMs {
		d.touch(count, x, y, t)
	}
}

func (d *driver) countOf(k Kind) int {
	n := 0
	for _, e := range d.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (d *driver) sumDelta() float64 {
	var s float64
	for _, e := range d.events {
		if e.Kind == KindOpacityDelta {
			s += e.Delta
		}
	}
	return s
}

func (d *driver) sumMove() (float64, float64) {
	var dx, dy float64
	for _, e := range d.events {
		if e.Kind == KindMove {
			dx += e.DX
			dy += e.DY
		}
	}
	return dx, dy
}

func (d *driver) kinds() []Kind {
	out := make([]Kind, len(d.events))
	for i, e := range d.events {
		out[i] = e.Kind
	}
	return out
}

// ------------------------------------------------ 3-tap hide / 6-tap show

// taps presses n quick single-finger taps starting at startMs, 200ms apart.
func (d *driver) taps(n, startMs int) int {
	for i := 0; i < n; i++ {
		s := startMs + i*200
		d.touch(1, 0.5, 0.5, s)
		d.lift(s + 60)
	}
	return startMs + n*200
}

func TestThreeTapsDisable(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.taps(3, 0)
	if got := d.countOf(KindDisable); got != 1 {
		t.Fatalf("Disable fired %d times after three taps, want 1 (events: %v)", got, d.kinds())
	}
	if d.countOf(KindEnable) != 0 || !d.r.Hidden() {
		t.Fatal("three taps did not leave the overlay hidden")
	}
}

func TestTwoTapsDoNothing(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.taps(2, 0)
	if len(d.events) != 0 {
		t.Fatalf("two taps emitted %v", d.kinds())
	}
}

func TestSixTapsEnable(t *testing.T) {
	d := newDriver(DefaultConfig())
	now := d.taps(3, 0) // hide
	now += 2000         // separate burst
	d.taps(5, now)
	if d.countOf(KindEnable) != 0 {
		t.Fatal("five taps revealed a hidden overlay; six are required")
	}
	d.lift(now + 5000)
	d.r.Expire(at(now + 5000))
	d.taps(6, now+6000)
	if got := d.countOf(KindEnable); got != 1 {
		t.Fatalf("Enable fired %d times after six taps, want 1", got)
	}
	if d.r.Hidden() {
		t.Fatal("still hidden after six taps")
	}
}

// TestStrayTripleTapNeverReveals is the safety property of the asymmetric
// thresholds: triple-clicking to select text while hidden must not bring the
// overlay back.
func TestStrayTripleTapNeverReveals(t *testing.T) {
	d := newDriver(DefaultConfig())
	now := d.taps(3, 0) // hide
	for i := 0; i < 5; i++ {
		now += 2000 // each triple-tap is its own burst
		now = d.taps(3, now)
	}
	if d.countOf(KindEnable) != 0 || !d.r.Hidden() {
		t.Fatal("separate triple-taps revealed the hidden overlay")
	}
}

func TestLongBurstWhileShowingDisablesOnlyOnce(t *testing.T) {
	// Six taps while showing: the third hides, and the remaining three count
	// toward the six needed to reveal, so the overlay stays hidden.
	d := newDriver(DefaultConfig())
	d.taps(6, 0)
	if d.countOf(KindDisable) != 1 || d.countOf(KindEnable) != 0 {
		t.Fatalf("six taps from showing gave %v, want exactly one Disable", d.kinds())
	}
	if !d.r.Hidden() {
		t.Fatal("overlay not hidden")
	}
}

func TestSetHiddenSyncsWithHotkey(t *testing.T) {
	// A hotkey hid the overlay; the recogniser must now demand six taps.
	d := newDriver(DefaultConfig())
	d.r.SetHidden(true)
	d.taps(3, 0)
	if len(d.events) != 0 {
		t.Fatalf("three taps acted while hidden: %v", d.kinds())
	}
	d.lift(3000)
	d.r.Expire(at(3000))
	d.taps(6, 4000)
	if d.countOf(KindEnable) != 1 {
		t.Fatal("six taps did not reveal after a hotkey hide")
	}
}

func TestSetHiddenAbandonsBurst(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.taps(2, 0)
	d.r.SetHidden(true) // hotkey mid-burst
	if d.r.TapCount() != 0 {
		t.Fatal("a burst survived a state change and could complete the wrong threshold")
	}
}

// TestTapsTolerateHumanDelay: the taps are not machine-fast, so a realistic
// uneven rhythm must still register.
func TestTapsTolerateHumanDelay(t *testing.T) {
	d := newDriver(DefaultConfig())
	gaps := []int{0, 120, 400, 300, 250, 420, 180, 350, 300}
	now := 0
	for _, g := range gaps {
		now += g
		d.touch(1, 0.5, 0.5, now)
		now += 70
		d.lift(now)
	}
	// Tap 3 hides; taps 4..9 are six more, which reveal.
	if d.countOf(KindDisable) != 1 || d.countOf(KindEnable) != 1 {
		t.Errorf("human-paced taps gave %v, want Disable then Enable", d.kinds())
	}
}

func TestTapBurstBreaksOnLongPause(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.taps(2, 0)
	d.taps(2, 2000) // pause far beyond TapMaxGap
	if len(d.events) != 0 {
		t.Errorf("a broken burst emitted %v", d.kinds())
	}
	if got := d.r.TapCount(); got != 2 {
		t.Errorf("TapCount = %d, want 2 (the new burst)", got)
	}
}

func TestSlowTapIsNotATap(t *testing.T) {
	d := newDriver(DefaultConfig())
	for i := 0; i < 6; i++ {
		start := i * 400
		d.touch(1, 0.5, 0.5, start)
		d.lift(start + 300) // exceeds TapMaxDuration (200ms)
	}
	if len(d.events) != 0 {
		t.Errorf("slow presses emitted %v", d.kinds())
	}
}

func TestSmearedTapIsNotATap(t *testing.T) {
	d := newDriver(DefaultConfig())
	for i := 0; i < 6; i++ {
		start := i * 200
		d.touch(1, 0.5, 0.5, start)
		d.touch(1, 0.7, 0.5, start+30) // moved 0.2, far beyond TapMaxMove
		d.lift(start + 60)
	}
	if len(d.events) != 0 {
		t.Errorf("smeared taps emitted %v", d.kinds())
	}
}

// TestTwoFingerTapsNeverToggle matters because a two-finger tap is Windows'
// right-click.
func TestTwoFingerTapsNeverToggle(t *testing.T) {
	d := newDriver(DefaultConfig())
	for i := 0; i < 8; i++ {
		d.touch(2, 0.5, 0.5, i*200)
		d.lift(i*200 + 60)
	}
	if len(d.events) != 0 {
		t.Errorf("two-finger taps emitted %v", d.kinds())
	}
}

func TestExpireClearsStaleBurst(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.touch(1, 0.5, 0.5, 0)
	d.lift(60)
	if d.r.TapCount() != 1 {
		t.Fatalf("TapCount = %d, want 1", d.r.TapCount())
	}
	d.r.Expire(at(100)) // still inside the window
	if d.r.TapCount() != 1 {
		t.Error("Expire cleared a fresh burst")
	}
	d.r.Expire(at(5000))
	if d.r.TapCount() != 0 {
		t.Error("Expire did not clear a stale burst")
	}
}

// ------------------------------------------------------------ opacity ramping

func TestOneFingerHoldIncreasesOpacity(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(1, 0.5, 0.5, 0)
	d.hold(1, 0.5, 0.5, 20, 1000, 20)
	d.lift(1020)

	if d.countOf(KindHoldStart) != 1 {
		t.Fatalf("HoldStart fired %d times, want 1", d.countOf(KindHoldStart))
	}
	if d.countOf(KindHoldEnd) != 1 {
		t.Fatalf("HoldEnd fired %d times, want 1", d.countOf(KindHoldEnd))
	}
	sum := d.sumDelta()
	if sum <= 0 {
		t.Fatalf("one-finger hold produced delta %.3f, want positive", sum)
	}
	// Held ~650ms past the 350ms delay at 0.85/s.
	if want := 0.65 * 0.85; math.Abs(sum-want) > 0.08 {
		t.Errorf("delta sum = %.3f, want about %.3f", sum, want)
	}
}

func TestTwoFingerHoldDecreasesOpacity(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(2, 0.5, 0.5, 0)
	d.hold(2, 0.5, 0.5, 20, 1000, 20)
	d.lift(1020)

	sum := d.sumDelta()
	if sum >= 0 {
		t.Fatalf("two-finger hold produced delta %.3f, want negative", sum)
	}
	if want := -0.65 * 0.85; math.Abs(sum-want) > 0.08 {
		t.Errorf("delta sum = %.3f, want about %.3f", sum, want)
	}
}

// TestRampIsLinearInTime is the stated requirement ("linear as touch
// continues") and also guarantees the rate does not depend on the touchpad's
// report rate, which differs between laptops.
func TestRampIsLinearInTime(t *testing.T) {
	run := func(stepMs int) float64 {
		d := newDriver(DefaultConfig())
		d.touch(1, 0.5, 0.5, 0)
		d.hold(1, 0.5, 0.5, stepMs, 1000, stepMs)
		d.lift(1000 + stepMs)
		return d.sumDelta()
	}
	fast := run(8)  // ~125 Hz touchpad
	slow := run(40) // ~25 Hz touchpad

	if math.Abs(fast-slow) > 0.06 {
		t.Errorf("ramp depends on report rate: %.3f at 125Hz vs %.3f at 25Hz", fast, slow)
	}
}

func TestRampDoublesWithDoubleDuration(t *testing.T) {
	run := func(endMs int) float64 {
		d := newDriver(DefaultConfig())
		d.touch(1, 0.5, 0.5, 0)
		d.hold(1, 0.5, 0.5, 20, endMs, 20)
		d.lift(endMs + 20)
		return d.sumDelta()
	}
	short := run(850) // 500ms of ramp
	long := run(1350) // 1000ms of ramp

	if ratio := long / short; math.Abs(ratio-2) > 0.15 {
		t.Errorf("ramp is not linear: doubling the hold gave a ratio of %.2f, want ~2", ratio)
	}
}

func TestStalledStreamDoesNotJump(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.touch(1, 0.5, 0.5, 0)
	d.touch(1, 0.5, 0.5, 400) // hold recognised
	// A five-second gap, as if the process was suspended.
	d.touch(1, 0.5, 0.5, 5400)
	d.lift(5420)

	if sum := d.sumDelta(); sum > 0.30 {
		t.Errorf("a stalled stream produced a %.3f jump; the per-frame delta is not clamped", sum)
	}
}

// -------------------------------------------------------- scroll pass-through

// TestScrollIsNotIntercepted is the most important test in this file. An
// ordinary two-finger scroll starts moving immediately, so it must never
// reach the hold state and must emit nothing at all.
func TestScrollIsNotIntercepted(t *testing.T) {
	d := newDriver(DefaultConfig())

	// Two fingers down, immediately sliding upward, as a scroll does.
	y := 0.7
	for i, tms := 0, 0; i < 40; i, tms = i+1, tms+12 {
		d.touch(2, 0.5, y, tms)
		y -= 0.012
	}
	d.lift(520)

	if len(d.events) != 0 {
		t.Errorf("a plain two-finger scroll emitted %d events, want 0: %v",
			len(d.events), d.kinds())
	}
	if d.r.Active() {
		t.Error("Recognizer reports Active() during an ordinary scroll")
	}
}

// TestFastCursorMoveIsNotIntercepted is the one-finger equivalent.
func TestFastCursorMoveIsNotIntercepted(t *testing.T) {
	d := newDriver(DefaultConfig())
	x := 0.2
	for i, tms := 0, 0; i < 30; i, tms = i+1, tms+10 {
		d.touch(1, x, 0.5, tms)
		x += 0.02
	}
	d.lift(320)

	if len(d.events) != 0 {
		t.Errorf("a plain cursor move emitted %d events, want 0: %v", len(d.events), d.kinds())
	}
}

// TestSlowScrollStillPassesThrough covers a deliberately gentle scroll: as
// long as it keeps moving past the slop before the hold delay, it is ignored.
func TestSlowScrollStillPassesThrough(t *testing.T) {
	d := newDriver(DefaultConfig())
	cfg := DefaultConfig()

	y := 0.6
	// Drift past MoveSlop within the hold delay.
	for tms := 0; tms < int(cfg.HoldDelay/time.Millisecond); tms += 20 {
		d.touch(2, 0.5, y, tms)
		y -= 0.006
	}
	d.lift(400)

	if len(d.events) != 0 {
		t.Errorf("a slow scroll emitted %d events, want 0: %v", len(d.events), d.kinds())
	}
}

// TestTinyJitterDuringHoldStillCounts: real fingers are never perfectly
// still, so jitter inside MoveSlop must not disqualify the hold.
func TestTinyJitterDuringHoldStillCounts(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(1, 0.500, 0.500, 0)
	jitter := []float64{0.004, -0.003, 0.005, -0.002, 0.001}
	for i, tms := 0, 40; tms <= 800; i, tms = i+1, tms+40 {
		j := jitter[i%len(jitter)]
		d.touch(1, 0.500+j, 0.500-j, tms)
	}
	d.lift(840)

	if d.countOf(KindHoldStart) != 1 {
		t.Errorf("jitter within the slop prevented the hold (events: %v)", d.kinds())
	}
	if d.sumDelta() <= 0 {
		t.Error("no opacity change during a jittery hold")
	}
}

// ------------------------------------------------------------------- movement

// TestTwoFingerHoldThenDragMoves is the resolution of the stated conflict:
// the same two-finger gesture dims while still, then moves once it slides.
func TestTwoFingerHoldThenDragMoves(t *testing.T) {
	cfg := DefaultConfig()
	d := newDriver(cfg)

	// Hold still long enough to be recognised.
	d.touch(2, 0.5, 0.5, 0)
	d.hold(2, 0.5, 0.5, 20, 500, 20)

	dimmed := d.sumDelta()
	if dimmed >= 0 {
		t.Fatalf("expected dimming during the stationary phase, got %.3f", dimmed)
	}

	// Now slide right and down.
	d.touch(2, 0.60, 0.55, 540)
	d.touch(2, 0.70, 0.60, 560)
	d.lift(580)

	dx, dy := d.sumMove()
	if dx <= 0 || dy <= 0 {
		t.Fatalf("drag produced no movement: dx=%.1f dy=%.1f (events %v)", dx, dy, d.kinds())
	}
	// 0.2 pad units right at the default scale.
	if want := 0.20 * cfg.MoveScale; math.Abs(dx-want) > want*0.25 {
		t.Errorf("dx = %.1f, want about %.1f", dx, want)
	}

	// Dimming must stop once dragging starts.
	if after := d.sumDelta(); after != dimmed {
		t.Errorf("opacity kept changing during the drag: %.3f -> %.3f", dimmed, after)
	}
}

func TestOneFingerHoldNeverMoves(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(1, 0.5, 0.5, 0)
	d.hold(1, 0.5, 0.5, 20, 500, 20)
	// Slide a long way with one finger still down.
	d.touch(1, 0.9, 0.9, 540)
	d.lift(560)

	if d.countOf(KindMove) != 0 {
		t.Error("a one-finger hold produced movement; only two fingers should move the overlay")
	}
	if d.sumDelta() <= 0 {
		t.Error("a one-finger hold stopped ramping when it drifted")
	}
}

func TestDragEmitsIncrementalDeltas(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(2, 0.5, 0.5, 0)
	d.hold(2, 0.5, 0.5, 20, 500, 20)

	// Three equal steps must produce three roughly equal deltas, not a
	// growing absolute offset.
	d.touch(2, 0.55, 0.5, 520)
	d.touch(2, 0.60, 0.5, 540)
	d.touch(2, 0.65, 0.5, 560)
	d.lift(580)

	var moves []float64
	for _, e := range d.events {
		if e.Kind == KindMove {
			moves = append(moves, e.DX)
		}
	}
	if len(moves) < 3 {
		t.Fatalf("got %d move events, want at least 3", len(moves))
	}
	last := moves[len(moves)-3:]
	for i := 1; i < len(last); i++ {
		if math.Abs(last[i]-last[0]) > math.Abs(last[0])*0.2 {
			t.Errorf("move deltas are not incremental: %v", last)
			break
		}
	}
}

// ---------------------------------------------- realistic finger landing

// TestTwoFingersLandingApartStillDims reproduces how a real touchpad reports a
// two-finger press: the first contact arrives, then the second a few tens of
// milliseconds later. Treating that rising count as "the gesture changed"
// made every two-finger gesture impossible on real hardware.
func TestTwoFingersLandingApartStillDims(t *testing.T) {
	for _, lagMs := range []int{8, 20, 45, 90} {
		t.Run(msName(lagMs), func(t *testing.T) {
			d := newDriver(DefaultConfig())

			// First finger lands alone.
			d.touch(1, 0.50, 0.50, 0)
			d.touch(1, 0.50, 0.50, lagMs/2)
			// Second finger lands; the centroid shifts as it does.
			d.touch(2, 0.55, 0.52, lagMs)
			d.hold(2, 0.55, 0.52, lagMs+20, lagMs+900, 20)
			d.lift(lagMs + 920)

			if d.countOf(KindHoldStart) != 1 {
				t.Fatalf("no hold recognised with a %dms landing lag (events %v)",
					lagMs, d.kinds())
			}
			if sum := d.sumDelta(); sum >= 0 {
				t.Errorf("delta = %.3f with a %dms lag, want negative (dimming)", sum, lagMs)
			}
		})
	}
}

// TestThreeFingersLandingOneByOne is the same settling behaviour with more
// contacts arriving in sequence.
func TestThreeFingersLandingOneByOne(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.touch(1, 0.50, 0.50, 0)
	d.touch(2, 0.52, 0.50, 25)
	d.touch(3, 0.54, 0.51, 50)
	d.hold(3, 0.54, 0.51, 70, 900, 20)
	d.lift(920)

	if d.countOf(KindHoldStart) != 1 {
		t.Fatalf("no hold recognised while fingers landed one by one: %v", d.kinds())
	}
	if sum := d.sumDelta(); sum >= 0 {
		t.Errorf("delta = %.3f, want negative", sum)
	}
}

// TestHoldDelayMeasuredFromLastFingerLanding: the countdown must restart when
// a finger is added, otherwise a slow two-finger press would be recognised
// the instant the second finger touches down.
func TestHoldDelayMeasuredFromLastFingerLanding(t *testing.T) {
	d := newDriver(DefaultConfig())

	// One finger rests for well over the hold delay...
	d.touch(1, 0.50, 0.50, 0)
	d.hold(1, 0.50, 0.50, 20, 600, 20)
	brightened := d.sumDelta()
	if brightened <= 0 {
		t.Fatal("the one-finger hold did not brighten")
	}

	// ...then a second finger lands. That ends the one-finger hold.
	d.touch(2, 0.52, 0.50, 620)
	if d.countOf(KindHoldEnd) != 1 {
		t.Errorf("adding a finger during a recognised hold did not end it")
	}
}

// TestScrollStartingWithOneContactStillPassesThrough: a scroll also begins
// with a single contact, so the settling rule must not let one through.
func TestScrollStartingWithOneContactStillPassesThrough(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(1, 0.50, 0.70, 0)
	d.touch(2, 0.50, 0.70, 15)
	y := 0.70
	for tms := 30; tms < 500; tms += 12 {
		y -= 0.015
		d.touch(2, 0.50, y, tms)
	}
	d.lift(520)

	if len(d.events) != 0 {
		t.Errorf("a scroll that began with one contact emitted %d events: %v",
			len(d.events), d.kinds())
	}
}

func msName(ms int) string {
	switch ms {
	case 8:
		return "8ms lag"
	case 20:
		return "20ms lag"
	case 45:
		return "45ms lag"
	default:
		return "90ms lag"
	}
}

// ------------------------------------------------------ mid-gesture changes

// TestFingerCountChangeAbandonsGesture covers resting a third finger part way
// through, which is ambiguous and must not be guessed at.
func TestFingerCountChangeAbandonsGesture(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(1, 0.5, 0.5, 0)
	d.hold(1, 0.5, 0.5, 20, 500, 20)
	before := d.sumDelta()
	if before <= 0 {
		t.Fatal("no ramp before the finger count changed")
	}

	// A second finger lands.
	d.touch(2, 0.5, 0.5, 520)
	d.hold(2, 0.5, 0.5, 540, 1200, 20)
	d.lift(1220)

	if d.countOf(KindHoldEnd) != 1 {
		t.Errorf("HoldEnd fired %d times, want exactly 1 when the gesture was abandoned",
			d.countOf(KindHoldEnd))
	}
	if after := d.sumDelta(); after != before {
		t.Errorf("opacity kept changing after the finger count changed: %.3f -> %.3f",
			before, after)
	}
}

func TestPartialLiftDoesNotStartNewGesture(t *testing.T) {
	d := newDriver(DefaultConfig())

	// Two fingers hold, then one lifts without the touch ending.
	d.touch(2, 0.5, 0.5, 0)
	d.hold(2, 0.5, 0.5, 20, 500, 20)
	d.touch(1, 0.5, 0.5, 520)
	d.hold(1, 0.5, 0.5, 540, 1400, 20)
	d.lift(1420)

	// It must not start ramping upward as a fresh one-finger hold.
	for _, e := range d.events {
		if e.Kind == KindOpacityDelta && e.Delta > 0 {
			t.Fatal("a partial lift was treated as a new one-finger hold")
		}
	}
}

func TestHoldStartEndAlwaysPaired(t *testing.T) {
	cases := []struct {
		name string
		play func(*driver)
	}{
		{"one finger hold", func(d *driver) {
			d.touch(1, 0.5, 0.5, 0)
			d.hold(1, 0.5, 0.5, 20, 600, 20)
			d.lift(620)
		}},
		{"two finger hold then drag", func(d *driver) {
			d.touch(2, 0.5, 0.5, 0)
			d.hold(2, 0.5, 0.5, 20, 600, 20)
			d.touch(2, 0.8, 0.8, 620)
			d.lift(640)
		}},
		{"hold interrupted by finger change", func(d *driver) {
			d.touch(1, 0.5, 0.5, 0)
			d.hold(1, 0.5, 0.5, 20, 600, 20)
			d.touch(3, 0.5, 0.5, 620)
			d.lift(640)
		}},
		{"scroll only", func(d *driver) {
			y := 0.7
			for tms := 0; tms < 400; tms += 15 {
				d.touch(2, 0.5, y, tms)
				y -= 0.02
			}
			d.lift(400)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDriver(DefaultConfig())
			tc.play(d)
			starts, ends := d.countOf(KindHoldStart), d.countOf(KindHoldEnd)
			if starts != ends {
				t.Errorf("HoldStart=%d HoldEnd=%d, must be paired (events %v)",
					starts, ends, d.kinds())
			}
			if d.r.Active() {
				t.Error("Recognizer still Active() after all fingers lifted")
			}
		})
	}
}

// --------------------------------------------------------------- robustness

func TestActiveOnlyDuringRecognisedGesture(t *testing.T) {
	d := newDriver(DefaultConfig())

	d.touch(2, 0.5, 0.5, 0)
	if d.r.Active() {
		t.Error("Active() immediately on touchdown; the scroll grace period is gone")
	}
	d.touch(2, 0.5, 0.5, 100)
	if d.r.Active() {
		t.Error("Active() before the hold delay elapsed")
	}
	d.touch(2, 0.5, 0.5, 400)
	if !d.r.Active() {
		t.Error("not Active() after the hold was recognised")
	}
	d.lift(420)
	if d.r.Active() {
		t.Error("still Active() after lifting")
	}
}

func TestResetClearsEverything(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.touch(1, 0.5, 0.5, 0)
	d.hold(1, 0.5, 0.5, 20, 600, 20)

	d.r.Reset()
	if d.r.Active() {
		t.Error("Active() after Reset")
	}
	if d.r.Phase() != phaseIdle {
		t.Error("phase is not idle after Reset")
	}
	// A frame after reset starts cleanly rather than resuming.
	d.events = d.events[:0]
	d.touch(1, 0.5, 0.5, 700)
	if len(d.events) != 0 {
		t.Errorf("events emitted immediately after Reset: %v", d.kinds())
	}
}

func TestZeroConfigFallsBackToDefaults(t *testing.T) {
	r := New(Config{})
	var out []Event
	out = r.Update(Frame{Count: 1, X: 0.5, Y: 0.5, At: at(0)}, out)
	for tms := 20; tms <= 800; tms += 20 {
		out = r.Update(Frame{Count: 1, X: 0.5, Y: 0.5, At: at(tms)}, out)
	}
	found := false
	for _, e := range out {
		if e.Kind == KindOpacityDelta {
			found = true
		}
	}
	if !found {
		t.Error("a zero Config produced no behaviour; defaults were not applied")
	}
}

func TestRepeatedLiftsAreHarmless(t *testing.T) {
	d := newDriver(DefaultConfig())
	for i := 0; i < 5; i++ {
		d.lift(i * 10)
	}
	if len(d.events) != 0 {
		t.Errorf("lifting with nothing down emitted %v", d.kinds())
	}
}

func TestOutOfOrderTimestampsDoNotPanic(t *testing.T) {
	d := newDriver(DefaultConfig())
	d.touch(1, 0.5, 0.5, 1000)
	d.touch(1, 0.5, 0.5, 900) // clock went backwards
	d.touch(1, 0.5, 0.5, 1400)
	d.lift(1420)
	// No assertion beyond "did not panic and did not produce a wild delta".
	if math.Abs(d.sumDelta()) > 1.0 {
		t.Errorf("out-of-order timestamps produced delta %.3f", d.sumDelta())
	}
}

// TestManyFingersBehaveLikeTwo: a three- or four-finger hold should still be
// well defined rather than falling through into an unhandled branch.
func TestManyFingersBehaveLikeTwo(t *testing.T) {
	for _, n := range []int{3, 4, 5} {
		d := newDriver(DefaultConfig())
		d.touch(n, 0.5, 0.5, 0)
		d.hold(n, 0.5, 0.5, 20, 700, 20)
		d.lift(720)

		if d.sumDelta() >= 0 {
			t.Errorf("%d-finger hold did not dim (delta %.3f)", n, d.sumDelta())
		}
		if d.countOf(KindHoldStart) != 1 || d.countOf(KindHoldEnd) != 1 {
			t.Errorf("%d-finger hold did not bracket properly", n)
		}
	}
}

// TestFullSequenceCombination runs several gestures back to back to make sure
// state does not leak between them.
func TestFullSequenceCombination(t *testing.T) {
	d := newDriver(DefaultConfig())
	now := 0
	step := func(ms int) int { now += ms; return now }

	// 1. a scroll (ignored)
	y := 0.7
	for i := 0; i < 20; i++ {
		d.touch(2, 0.5, y, step(12))
		y -= 0.02
	}
	d.lift(step(20))
	if len(d.events) != 0 {
		t.Fatalf("scroll leaked events: %v", d.kinds())
	}

	// 2. three taps -> hidden, then six taps -> shown again
	for i := 0; i < 3; i++ {
		d.touch(1, 0.5, 0.5, step(180))
		d.lift(step(60))
	}
	step(2000)
	for i := 0; i < 6; i++ {
		d.touch(1, 0.5, 0.5, step(180))
		d.lift(step(60))
	}
	if d.countOf(KindDisable) != 1 || d.countOf(KindEnable) != 1 {
		t.Fatalf("toggle did not fire after the scroll; state leaked")
	}

	// 3. one-finger hold -> brighten
	d.touch(1, 0.5, 0.5, step(500))
	for i := 0; i < 40; i++ {
		d.touch(1, 0.5, 0.5, step(20))
	}
	d.lift(step(20))
	if d.sumDelta() <= 0 {
		t.Fatalf("one-finger hold did not brighten after a toggle")
	}

	// 4. two-finger hold then drag
	before := d.sumDelta()
	d.touch(2, 0.5, 0.5, step(500))
	for i := 0; i < 30; i++ {
		d.touch(2, 0.5, 0.5, step(20))
	}
	if d.sumDelta() >= before {
		t.Fatalf("two-finger hold did not dim")
	}
	d.touch(2, 0.7, 0.5, step(20))
	d.lift(step(20))
	if dx, _ := d.sumMove(); dx <= 0 {
		t.Fatalf("drag after the hold did not move")
	}

	if d.countOf(KindHoldStart) != d.countOf(KindHoldEnd) {
		t.Errorf("holds unbalanced across the sequence: %d starts, %d ends",
			d.countOf(KindHoldStart), d.countOf(KindHoldEnd))
	}
}
