// Package gesture turns a stream of touchpad frames into overlay commands.
//
// It is deliberately free of any Windows dependency: it consumes plain
// timestamped frames and emits plain events. That is what makes every rule
// below — including the parts that are genuinely subtle, like telling a
// deliberate two-finger hold apart from the start of an ordinary scroll —
// testable without a touchpad.
//
// # The conflict this design exists to solve
//
// A touchpad is already fully booked. One finger moves the cursor, two
// fingers scroll, taps click. Any gesture that claims one of those inputs
// breaks normal use of the machine.
//
// The resolution used here is a *stationary hold prefix*. Every overlay
// gesture requires the fingers to go down and stay still for HoldDelay before
// anything happens. Nobody holds still before scrolling — a scroll starts
// moving immediately — so ordinary input never crosses the threshold and is
// never intercepted. Only a deliberate press-and-wait enters overlay control.
//
// Recognition is purely passive. Input is observed through Raw Input in
// parallel with the rest of the system; nothing is hooked, delayed or
// swallowed, so the touchpad keeps behaving exactly as it always does.
//
// Once a hold is recognised, dragging continues to be owned by the overlay
// until the fingers lift, which is what lets the same two-finger gesture both
// dim the overlay (while still) and move it (once it slides).
package gesture

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Config tunes recognition. The defaults are chosen to sit well clear of
// ordinary touchpad use rather than to be maximally sensitive.
type Config struct {
	// HoldDelay is how long fingers must stay still before a hold begins.
	// This is the single most important value: it is what separates a
	// deliberate gesture from a scroll or a cursor move.
	HoldDelay time.Duration

	// MoveSlop is how far (in 0..1 pad units) contacts may drift during the
	// hold delay and still count as stationary.
	MoveSlop float64

	// DragThreshold is how far a recognised hold must travel before it is
	// treated as a drag rather than a continuing hold.
	DragThreshold float64

	// TapMaxDuration is the longest a touch can last and still be a tap.
	TapMaxDuration time.Duration
	// TapMaxMove is how far a touch may travel and still be a tap.
	TapMaxMove float64
	// TapMaxGap is the longest pause allowed between taps in a burst.
	TapMaxGap time.Duration
	// TapDisableCount is how many taps in a burst hide the overlay while it is
	// showing. TapEnableCount is how many bring it back while it is hidden.
	//
	// The asymmetry is deliberate. Hiding should be quick; revealing should
	// take a clearly intentional burst, so that a stray triple-tap (for
	// instance triple-clicking to select a paragraph) can never put the
	// overlay back on screen.
	TapDisableCount int
	TapEnableCount  int

	// RampPerSecond is how much opacity changes per second of holding,
	// expressed in 0..1 units of the full range.
	RampPerSecond float64

	// MoveScale converts pad travel (0..1) into screen pixels.
	MoveScale float64
}

// DefaultConfig returns tuned defaults.
func DefaultConfig() Config {
	return Config{
		// 350ms is long enough that no scroll or cursor move reaches it, and
		// short enough to feel immediate when you mean it.
		HoldDelay:      350 * time.Millisecond,
		MoveSlop:       0.035,
		DragThreshold:  0.030,
		TapMaxDuration: 200 * time.Millisecond,
		TapMaxMove:     0.030,
		// Generous, because taps "with a small human delay" are the stated
		// requirement. An unhurried rhythm pauses roughly 300-450ms between
		// taps, so the budget sits well above that.
		TapMaxGap:       600 * time.Millisecond,
		TapDisableCount: 3,
		TapEnableCount:  6,
		RampPerSecond:   0.85,
		MoveScale:       1400,
	}
}

// Kind identifies an emitted event.
type Kind int

const (
	// KindDisable is the three-tap burst while the overlay is showing: take it
	// off the screen entirely. Streaming continues in the background.
	KindDisable Kind = iota
	// KindEnable is the six-tap burst while the overlay is hidden: put it back.
	KindEnable
	// KindOpacityDelta asks for a relative opacity change in 0..1 units.
	KindOpacityDelta
	// KindMove asks for a relative window move in pixels.
	KindMove
	// KindHoldStart and KindHoldEnd bracket an active gesture. The overlay
	// uses them to suppress scrolling only while a gesture owns the pad.
	KindHoldStart
	KindHoldEnd
)

// Event is one recognised command.
type Event struct {
	Kind Kind
	// Fingers is how many contacts the gesture started with.
	Fingers int
	// Delta is the opacity change for KindOpacityDelta.
	Delta float64
	// DX, DY are the pixel offsets for KindMove.
	DX, DY float64
}

// phase is the recogniser's state.
type phase int

const (
	phaseIdle phase = iota
	// phasePending: fingers are down but have not yet held still long enough.
	phasePending
	// phaseHold: a stationary hold was recognised; ramping opacity.
	phaseHold
	// phaseDrag: the recognised hold started moving; driving the window.
	phaseDrag
	// phaseDead: this touch was disqualified (it moved too early, or the
	// finger count changed mid-gesture). Nothing happens until all fingers
	// lift. This is what lets an ordinary scroll pass through untouched.
	phaseDead
)

// Recognizer is the gesture state machine. It is not safe for concurrent use;
// the overlay drives it from its window thread.
type Recognizer struct {
	cfg Config

	phase   phase
	fingers int

	// Touch geometry.
	startX, startY float64
	startAt        time.Time
	lastX, lastY   float64
	lastAt         time.Time
	// maxDrift is the furthest the centroid has been from its start, used to
	// disqualify taps and pending holds.
	maxDrift float64

	// Tap burst tracking.
	tapCount int
	lastTap  time.Time

	// hidden mirrors whether the overlay is currently off screen. It selects
	// which burst length counts. The overlay keeps it in sync with SetHidden,
	// so a hotkey toggle and a tap toggle can never disagree.
	hidden bool

	// Diagnostic, when set, receives a note for every completed touch,
	// including the ones rejected as taps and why. `probe-touchpad
	// --gestures` uses it; guessing at thresholds without this is guesswork.
	Diagnostic func(string)
}

// New creates a Recognizer.
func New(cfg Config) *Recognizer {
	if cfg.HoldDelay <= 0 {
		cfg = DefaultConfig()
	}
	def := DefaultConfig()
	if cfg.TapDisableCount < 2 {
		cfg.TapDisableCount = def.TapDisableCount
	}
	if cfg.TapEnableCount < 2 {
		cfg.TapEnableCount = def.TapEnableCount
	}
	return &Recognizer{cfg: cfg}
}

// Hidden reports whether the recogniser believes the overlay is off screen.
func (r *Recognizer) Hidden() bool { return r.hidden }

// SetHidden tells the recogniser the overlay's real state, and abandons any
// burst in progress so taps counted against one threshold never complete the
// other.
func (r *Recognizer) SetHidden(h bool) {
	if r.hidden != h {
		r.tapCount = 0
	}
	r.hidden = h
}

// tapTarget is the burst length that acts in the current state.
func (r *Recognizer) tapTarget() int {
	if r.hidden {
		return r.cfg.TapEnableCount
	}
	return r.cfg.TapDisableCount
}

// Active reports whether a recognised hold or drag is in progress. It is
// informational only: the overlay watches input passively and never blocks or
// swallows anything, so ordinary scrolling and clicking always reach the
// application underneath.
func (r *Recognizer) Active() bool {
	return r.phase == phaseHold || r.phase == phaseDrag
}

// Phase exposes the internal state for tests and diagnostics.
func (r *Recognizer) Phase() phase { return r.phase }

// Reset returns to idle, abandoning any gesture in progress.
func (r *Recognizer) Reset() {
	r.phase = phaseIdle
	r.fingers = 0
	r.tapCount = 0
	r.maxDrift = 0
}

// Frame is one instant of touch data, in normalised 0..1 pad coordinates.
type Frame struct {
	Count int
	X, Y  float64 // centroid
	At    time.Time
}

// Update feeds one frame in and returns any events it produced.
//
// Events are appended to out, which may be nil; returning the slice lets a
// caller reuse a buffer on the hot path.
func (r *Recognizer) Update(f Frame, out []Event) []Event {
	if f.Count == 0 {
		return r.release(f.At, out)
	}

	// First contact of a new touch.
	if r.phase == phaseIdle {
		r.begin(f)
		return out
	}

	if f.Count != r.fingers {
		// Fingers never land on the same millisecond. A two-finger gesture
		// almost always reports one contact first and the second a few tens
		// of milliseconds later, so a rising count before the gesture has
		// been recognised is the touch settling, not a different gesture.
		// Treating it as a change here made two-finger gestures impossible.
		if r.phase == phasePending && f.Count > r.fingers {
			r.fingers = f.Count
			// Restart the hold from the moment the last finger arrived, and
			// re-anchor: the centroid jumps when a finger is added, and
			// measuring drift from the old anchor would instantly
			// disqualify the touch.
			r.startX, r.startY = f.X, f.Y
			r.lastX, r.lastY = f.X, f.Y
			r.startAt, r.lastAt = f.At, f.At
			r.maxDrift = 0
			return out
		}

		// Anything else — a finger lifting part way through, or landing
		// after the gesture was recognised — is ambiguous, so the touch is
		// abandoned rather than guessed at.
		if r.phase == phaseHold || r.phase == phaseDrag {
			out = append(out, Event{Kind: KindHoldEnd, Fingers: r.fingers})
		}
		r.phase = phaseDead
		r.tapCount = 0
		r.lastAt = f.At
		return out
	}

	drift := dist(f.X-r.startX, f.Y-r.startY)
	if drift > r.maxDrift {
		r.maxDrift = drift
	}

	switch r.phase {
	case phasePending:
		// Moving before the hold delay means this is ordinary input — a
		// cursor move or a scroll. Disqualify it and let Windows have it.
		if drift > r.cfg.MoveSlop {
			r.phase = phaseDead
			r.lastAt = f.At
			return out
		}
		if f.At.Sub(r.startAt) >= r.cfg.HoldDelay {
			r.phase = phaseHold
			// Re-anchor so drag distance is measured from where the hold was
			// recognised, not from touchdown.
			r.startX, r.startY = f.X, f.Y
			r.lastX, r.lastY = f.X, f.Y
			r.lastAt = f.At
			out = append(out, Event{Kind: KindHoldStart, Fingers: r.fingers})
			return out
		}

	case phaseHold:
		// Travelling far enough converts the hold into a drag. Only the
		// two-finger gesture drives the window; one finger keeps ramping so
		// a little wobble during a long hold does not stop it.
		if r.fingers >= 2 && dist(f.X-r.startX, f.Y-r.startY) > r.cfg.DragThreshold {
			r.phase = phaseDrag
			// Emit the travel that crossed the threshold rather than
			// swallowing it. Anchoring at the crossing point instead would
			// discard real finger movement, and a quick flick — which may
			// only produce two or three frames on a slower touchpad — would
			// move the overlay not at all.
			dx := (f.X - r.startX) * r.cfg.MoveScale
			dy := (f.Y - r.startY) * r.cfg.MoveScale
			r.lastX, r.lastY = f.X, f.Y
			r.lastAt = f.At
			return append(out, Event{Kind: KindMove, Fingers: r.fingers, DX: dx, DY: dy})
		}
		out = r.ramp(f, out)

	case phaseDrag:
		dx := (f.X - r.lastX) * r.cfg.MoveScale
		dy := (f.Y - r.lastY) * r.cfg.MoveScale
		r.lastX, r.lastY = f.X, f.Y
		r.lastAt = f.At
		if dx != 0 || dy != 0 {
			out = append(out, Event{Kind: KindMove, Fingers: r.fingers, DX: dx, DY: dy})
		}

	case phaseDead:
		r.lastAt = f.At
	}
	return out
}

// begin records the start of a new touch.
func (r *Recognizer) begin(f Frame) {
	r.phase = phasePending
	r.fingers = f.Count
	r.startX, r.startY = f.X, f.Y
	r.lastX, r.lastY = f.X, f.Y
	r.startAt, r.lastAt = f.At, f.At
	r.maxDrift = 0
}

// ramp emits the opacity change for one frame of holding.
//
// The change is proportional to elapsed time rather than a fixed step per
// frame, so the rate is the same regardless of the touchpad's report rate —
// which varies a lot between laptops.
func (r *Recognizer) ramp(f Frame, out []Event) []Event {
	dt := f.At.Sub(r.lastAt).Seconds()
	r.lastAt = f.At
	if dt <= 0 {
		return out
	}
	// Guard against a stalled stream producing one enormous jump.
	if dt > 0.25 {
		dt = 0.25
	}
	delta := r.cfg.RampPerSecond * dt
	if r.fingers >= 2 {
		delta = -delta // two fingers dim, one finger brightens
	}
	return append(out, Event{Kind: KindOpacityDelta, Fingers: r.fingers, Delta: delta})
}

// release handles every finger lifting.
func (r *Recognizer) release(at time.Time, out []Event) []Event {
	switch r.phase {
	case phaseHold, phaseDrag:
		out = append(out, Event{Kind: KindHoldEnd, Fingers: r.fingers})

	case phasePending:
		// A short, still, single-finger touch is a tap. Only single-finger
		// taps count: a two-finger tap is Windows' right-click.
		dur := at.Sub(r.startAt)
		isTap := r.fingers == 1 &&
			dur <= r.cfg.TapMaxDuration &&
			r.maxDrift <= r.cfg.TapMaxMove

		if r.Diagnostic != nil {
			why := "accepted"
			switch {
			case r.fingers != 1:
				why = "rejected: not a single finger"
			case dur > r.cfg.TapMaxDuration:
				why = "rejected: held too long (limit " + r.cfg.TapMaxDuration.String() + ")"
			case r.maxDrift > r.cfg.TapMaxMove:
				why = "rejected: moved too far (limit " +
					strconv.FormatFloat(r.cfg.TapMaxMove, 'f', 3, 64) + ")"
			}
			r.Diagnostic(fmt.Sprintf(
				"tap candidate: %d finger(s), %v, drift %.4f -> %s (burst %d/%d)",
				r.fingers, dur.Round(time.Millisecond), r.maxDrift, why,
				r.tapCount, r.tapTarget()))
		}

		if isTap {
			// A long pause breaks the burst and starts a new one.
			//
			// The gap is measured from the previous tap's release to this
			// tap's touchdown — the interval the user actually perceives as
			// the pause. Measuring release-to-release would fold each tap's
			// own duration into the budget and reject an unhurried rhythm.
			if !r.lastTap.IsZero() && r.startAt.Sub(r.lastTap) > r.cfg.TapMaxGap {
				r.tapCount = 0
			}
			r.tapCount++
			r.lastTap = at
			if r.tapCount >= r.tapTarget() {
				r.tapCount = 0
				kind := KindDisable
				if r.hidden {
					kind = KindEnable
				}
				r.hidden = !r.hidden
				out = append(out, Event{Kind: kind, Fingers: 1})
			}
		} else {
			r.tapCount = 0
		}

	case phaseDead:
		// A disqualified touch also breaks a tap burst: the user was doing
		// something else in the middle of it.
		r.tapCount = 0
	}

	r.phase = phaseIdle
	r.fingers = 0
	r.maxDrift = 0
	return out
}

// Expire clears a stale tap burst. The overlay calls this on a timer so a
// burst that was abandoned half way does not combine with the next taps
// minutes later.
func (r *Recognizer) Expire(now time.Time) {
	if r.tapCount > 0 && !r.lastTap.IsZero() && now.Sub(r.lastTap) > r.cfg.TapMaxGap {
		r.tapCount = 0
	}
}

// TapCount exposes burst progress for tests and diagnostics.
func (r *Recognizer) TapCount() int { return r.tapCount }

func dist(dx, dy float64) float64 { return math.Hypot(dx, dy) }
