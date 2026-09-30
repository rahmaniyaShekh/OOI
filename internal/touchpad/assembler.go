//go:build windows

package touchpad

import "time"

// Frame is a complete set of contacts touching the pad at one instant.
type Frame struct {
	Contacts []Contact
	At       time.Time
}

// Count returns how many fingers are down.
func (f Frame) Count() int { return len(f.Contacts) }

// Centroid returns the average position of the contacts, in 0..1 pad space.
// A frame with no contacts returns (0, 0).
func (f Frame) Centroid() (float64, float64) {
	if len(f.Contacts) == 0 {
		return 0, 0
	}
	var sx, sy float64
	for _, c := range f.Contacts {
		sx += c.X
		sy += c.Y
	}
	n := float64(len(f.Contacts))
	return sx / n, sy / n
}

// Assembler reconstructs whole frames from the HID report stream.
//
// A Precision Touchpad may split one moment in time across several reports:
// the first carries the total Contact Count, and any that follow carry the
// remaining fingers with a count of zero. Treating each report as a frame
// would make a three-finger touch look like a one-finger touch followed by a
// two-finger touch, so the reports have to be stitched back together.
//
// The type holds no Windows state, which is what lets the whole protocol be
// tested without a touchpad.
type Assembler struct {
	pending  []Contact
	expected int
	// inFrame is true between the first report of a frame and its completion.
	inFrame bool
}

// Reset drops any partially assembled frame.
func (a *Assembler) Reset() {
	a.pending = a.pending[:0]
	a.expected = 0
	a.inFrame = false
}

// Pending reports how many contacts are buffered for an incomplete frame.
// Exposed for tests and diagnostics.
func (a *Assembler) Pending() int { return len(a.pending) }

// Push feeds one decoded report in.
//
// It returns a completed frame and true when this report finishes one. The
// returned slice is owned by the caller: it is copied out of the assembler's
// buffer, so the assembler can safely keep reusing that buffer.
func (a *Assembler) Push(contacts []Contact, count int, at time.Time) (Frame, bool) {
	switch {
	case count > 0 && len(contacts) == 0:
		// The report declares contacts but none of them have their tip
		// switch down: every finger has lifted.
		//
		// Real devices commonly signal a lift this way — they keep
		// reporting the departing contact with tip=0 for one more frame
		// rather than dropping the count straight to zero. Waiting for a
		// contact that will never arrive strands the assembler, and because
		// a lift is what completes a tap and ends a hold, that silently
		// disables most gestures.
		a.Reset()
		return Frame{Contacts: nil, At: at}, true

	case count > 0:
		// First report of a new frame. Anything half-assembled is stale.
		a.pending = append(a.pending[:0], contacts...)
		a.expected = count
		a.inFrame = true

	case len(contacts) > 0:
		// Continuation. If no frame was started, treat this as the whole
		// frame rather than dropping real input on the floor.
		if !a.inFrame {
			a.pending = append(a.pending[:0], contacts...)
			a.expected = len(contacts)
			a.inFrame = true
		} else {
			a.pending = append(a.pending, contacts...)
		}

	default:
		// No count and no contacts: every finger has lifted. This is the
		// signal that ends a gesture, so it must always be emitted.
		a.Reset()
		return Frame{Contacts: nil, At: at}, true
	}

	if len(a.pending) >= a.expected {
		out := make([]Contact, len(a.pending))
		copy(out, a.pending)
		a.Reset()
		return Frame{Contacts: out, At: at}, true
	}
	return Frame{}, false
}
