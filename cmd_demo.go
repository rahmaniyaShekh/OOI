//go:build windows

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"time"

	"ooi/internal/overlay"
	"ooi/internal/verify"
)

// cmdDemo puts a protected window on screen and leaves it there.
//
// The verify command proves the point by measurement, which asks you to trust
// the measurement. This command proves it the other way: the window sits on
// your desktop until you dismiss it, so you can point any recorder you like
// at it and look at the result yourself.
func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		// Deliberately not "-w"/"-h": "-h" would shadow the conventional
		// help flag and turn `demo -h` into a parse error.
		w         = fs.Int("width", 640, "window width")
		h         = fs.Int("height", 360, "window height")
		x         = fs.Int("x", 120, "window x position")
		y         = fs.Int("y", 120, "window y position")
		unprotect = fs.Bool("no-protect", false, "start with protection OFF, as an A/B baseline")
		seconds   = fs.Int("seconds", 0, "close automatically after N seconds (0 = wait for Enter)")
		noGesture = fs.Bool("no-gestures", false, "disable touchpad gesture control")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi demo - leave a protected window on screen so you can try to record it

Point anything you like at it: OBS, Xbox Game Bar (Win+G), the Snipping Tool
(Win+Shift+S), Teams, Zoom, Discord, or a plain Print Screen. The window is
visible to your eyes but should be absent from every recording.

Press Ctrl+Alt+P while it is up to toggle protection off and on, so you can
see the same window appear and disappear in your recorder.

FLAGS
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	ov := overlay.New(overlay.Config{
		X: *x, Y: *y, W: *w, H: *h,
		Opacity:      255,
		ClickThrough: true,
		Protect:      !*unprotect,
		Hotkeys:      true,
		Gestures:     !*noGesture,
		Placeholder:  "ooi demo",
	})

	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()

	select {
	case <-ov.Ready():
	case err := <-runErr:
		if err != nil {
			return fmt.Errorf("could not create the demo window: %w", err)
		}
		return fmt.Errorf("demo window exited before it was ready")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("demo window did not become ready within 10s")
	}

	state := "ON"
	label := "PROTECTED\nthis window must NOT appear in any recording\nCtrl+Alt+P toggles protection"
	if *unprotect {
		state = "OFF"
		label = "PROTECTION OFF\nthis window SHOULD appear in recordings\nCtrl+Alt+P toggles protection"
	}
	ov.SetMarker(verify.Marker[0], verify.Marker[1], verify.Marker[2], label)
	fmt.Println()
	fmt.Printf("  A bright test window is now on screen at %dx%d+%d+%d.\n", *w, *h, *x, *y)
	fmt.Printf("  Capture protection is %s.\n", state)
	fmt.Println()
	fmt.Println("  Try to record it:")
	fmt.Println("    Win+G          Xbox Game Bar  (uses Windows.Graphics.Capture)")
	fmt.Println("    Win+Shift+S    Snipping Tool  (uses Windows.Graphics.Capture)")
	fmt.Println("    Print Screen   clipboard screenshot")
	fmt.Println("    OBS            Display Capture or Window Capture")
	fmt.Println("    Teams / Zoom / Discord screen share")
	fmt.Println()
	fmt.Println("  Ctrl+Alt+P toggles protection, so you can watch the same window")
	fmt.Println("  appear and disappear inside your recorder.")
	fmt.Println()
	if !*noGesture {
		fmt.Printf("  Touchpad gestures: %s\n", gestureStatusLine(true))
		fmt.Println("    3 quick taps           hide (keeps streaming); 6 taps show again")
		fmt.Println("    1 finger held still    brighten, continuously")
		fmt.Println("    2 fingers held still   dim, continuously")
		fmt.Println("    2 fingers held, slide  move the overlay")
		fmt.Println("  Hold still for a moment before sliding. Ordinary scrolling and")
		fmt.Println("  cursor movement are never intercepted.")
		fmt.Println()
	}
	var done <-chan struct{}
	if *seconds > 0 {
		fmt.Printf("  Closing automatically in %d seconds.\n\n", *seconds)
		t := time.NewTimer(time.Duration(*seconds) * time.Second)
		defer t.Stop()
		ch := make(chan struct{})
		go func() { <-t.C; close(ch) }()
		done = ch
	} else {
		fmt.Println("  Press Enter here (or Ctrl+Alt+Q) to close it.")
		fmt.Println()
		ch := make(chan struct{})
		go func() {
			bufio.NewReader(os.Stdin).ReadString('\n')
			close(ch)
		}()
		done = ch
	}

	select {
	case <-done:
		ov.Stop()
	case <-ov.Closed():
	}

	select {
	case <-ov.Closed():
	case <-time.After(3 * time.Second):
	}
	return nil
}
