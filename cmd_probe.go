//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"ooi/internal/gesture"
	"ooi/internal/touchpad"
	"ooi/internal/winapi"
)

// describeGesture prints a recognised event in the terms the user thinks in.
func describeGesture(e gesture.Event, _ *gesture.Recognizer) {
	switch e.Kind {
	case gesture.KindDisable:
		fmt.Printf("  [3 TAPS]  disable -> overlay OFF (still streaming)\n")
	case gesture.KindEnable:
		fmt.Printf("  [6 TAPS]  enable  -> overlay ON\n")
	case gesture.KindHoldStart:
		what := "brighten"
		if e.Fingers >= 2 {
			what = "dim"
		}
		fmt.Printf("  [HOLD]    %d finger(s) recognised -> %s\n", e.Fingers, what)
	case gesture.KindHoldEnd:
		fmt.Printf("  [RELEASE] %d finger(s)\n", e.Fingers)
	case gesture.KindMove:
		fmt.Printf("  [MOVE]    dx=%+.0f dy=%+.0f px\n", e.DX, e.DY)
	}
}

// cmdProbeTouchpad dumps what this machine's touchpad actually reports.
//
// It exists because the gesture layer must work on any laptop, and the only
// honest way to confirm that on a given machine is to look at its real HID
// stream rather than assume a layout.
func cmdProbeTouchpad(args []string) error {
	fs := flag.NewFlagSet("probe-touchpad", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	seconds := fs.Int("seconds", 10, "how long to listen")
	raw := fs.Bool("raw", false, "print every frame instead of a summary")
	gestures := fs.Bool("gestures", false, "run the real recogniser and print recognised gestures")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi probe-touchpad - show what your touchpad reports

Listens to raw HID input from the Precision Touchpad and prints the contacts
it sees. Use it to confirm gesture support works on a given laptop, or to
diagnose why a gesture is not being recognised.

Reading is passive: the cursor, taps and two-finger scrolling keep working
normally while this runs.

FLAGS
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println()
	if ok, err := touchpad.Available(); !ok {
		fmt.Println("  No Precision Touchpad detected:", err)
		fmt.Println()
		fmt.Println("  Gesture control needs a Windows Precision Touchpad. Most laptops")
		fmt.Println("  since about 2015 have one. Vendor-specific (Synaptics/ELAN legacy)")
		fmt.Println("  drivers do not expose raw contacts; overlayc falls back to hotkeys.")
		return nil
	}
	fmt.Println("  Precision Touchpad detected. Listening...")
	fmt.Println("  Move fingers on the touchpad now. Try 1, 2 and 3 fingers.")
	fmt.Println()

	// Raw input needs a window to deliver to, and the window owns a thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	winapi.EnablePerMonitorDPI()

	reader := touchpad.NewReader()

	var (
		maxFingers  int
		frames      int
		wmInputMsgs int
		liftFrames  int
		byCount     = map[int]int{}
		lastPrint   time.Time

		recog = gesture.New(gesture.DefaultConfig())
		evs   []gesture.Event
	)
	if *gestures {
		// Report every completed touch, including taps that were rejected
		// and why. Without this, a tap that never registers is invisible.
		recog.Diagnostic = func(s string) { fmt.Println("  " + s) }
		fmt.Println("  Gesture recogniser is running. Try:")
		fmt.Println("    1 finger held still    2 fingers held still")
		fmt.Println("    2 fingers held, slide  4 quick taps")
		fmt.Println()
	}

	hwnd, cleanup, err := newMessageOnlyWindow(func(msg, wp, lp uintptr) bool {
		if msg != winapi.WM_INPUT {
			return false
		}
		// Counted separately from decoded frames so a silent touchpad can be
		// diagnosed: no messages at all is a registration problem, messages
		// without frames is a report-parsing problem.
		wmInputMsgs++
		reader.Handle(lp, time.Now(), func(f touchpad.Frame) {
			frames++
			byCount[f.Count()]++
			if f.Count() > maxFingers {
				maxFingers = f.Count()
			}
			if f.Count() == 0 {
				liftFrames++
			}

			// Running the real recogniser here is the point: it proves the
			// gesture layer works against this machine's actual report
			// stream rather than against a synthetic one.
			if *gestures {
				cx, cy := f.Centroid()
				evs = recog.Update(gesture.Frame{
					Count: f.Count(), X: cx, Y: cy, At: f.At,
				}, evs[:0])
				for _, e := range evs {
					describeGesture(e, recog)
				}
			}

			if *raw {
				cx, cy := f.Centroid()
				fmt.Printf("  %2d finger(s)  centroid=(%.3f, %.3f)  %v\n",
					f.Count(), cx, cy, f.Contacts)
			} else if !*gestures && time.Since(lastPrint) > 250*time.Millisecond {
				lastPrint = time.Now()
				cx, cy := f.Centroid()
				fmt.Printf("\r  fingers: %d   centroid: (%.3f, %.3f)   frames: %-6d",
					f.Count(), cx, cy, frames)
			}
		})
		return true
	})
	if err != nil {
		return err
	}
	defer cleanup()

	if err := reader.Register(hwnd); err != nil {
		return fmt.Errorf("could not subscribe to touchpad input: %w", err)
	}
	defer reader.Unregister()

	pumpMessages(time.Duration(*seconds) * time.Second)

	gotFrames, errs, devices := reader.Stats()
	fmt.Println()
	fmt.Println()
	fmt.Println("  ---- summary ----")
	fmt.Printf("  WM_INPUT messages %d\n", wmInputMsgs)
	// A lift frame is what ends a gesture and completes a tap. Zero of them
	// means the release path is broken, whatever else looks healthy.
	fmt.Printf("  lift frames       %d\n", liftFrames)
	fmt.Printf("  devices seen      %d\n", devices)
	for _, n := range reader.DeviceNames() {
		fmt.Printf("    %s\n", n)
	}
	fmt.Printf("  frames decoded    %d\n", gotFrames)
	fmt.Printf("  decode errors     %d\n", errs)
	fmt.Printf("  max simultaneous  %d finger(s)\n", maxFingers)
	for n := 1; n <= 5; n++ {
		if c := byCount[n]; c > 0 {
			fmt.Printf("    %d-finger frames  %d\n", n, c)
		}
	}
	fmt.Println()

	if liftFrames == 0 && frames > 0 {
		fmt.Println("  WARNING: no lift frames were seen. Gestures cannot complete")
		fmt.Println("  without them: taps are counted on release, and holds never end.")
		fmt.Println()
	}

	switch {
	case wmInputMsgs == 0:
		fmt.Println("  No raw input arrived at all.")
		fmt.Println("  Either the touchpad was not touched during the listening window,")
		fmt.Println("  or this process cannot receive raw input in its current session.")
	case gotFrames == 0:
		fmt.Println("  Raw input arrived but no contacts could be decoded. The device's")
		fmt.Println("  report descriptor does not expose standard digitizer usages.")
	case maxFingers >= 3:
		fmt.Println("  Multi-finger contacts confirmed. Every gesture is supported here.")
	case maxFingers == 2:
		fmt.Println("  Two contacts seen. Three-finger data was not exercised; that only")
		fmt.Println("  matters if you want three-finger gestures.")
	default:
		fmt.Println("  Only single contacts were seen. Try again pressing two fingers down.")
	}
	return nil
}
