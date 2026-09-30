//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"ooi/internal/overlay"
	"ooi/internal/verify"
)

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		// Deliberately not "-w"/"-h": "-h" would shadow the conventional
		// help flag and turn `verify -h` into a parse error.
		w      = fs.Int("width", 640, "test window width")
		h      = fs.Int("height", 360, "test window height")
		x      = fs.Int("x", 120, "test window x position")
		y      = fs.Int("y", 120, "test window y position")
		settle = fs.Duration("settle", 600*time.Millisecond, "time to let the compositor settle between phases")
		shots  = fs.String("save-shots", "", "directory to write before/after PNGs of every capture")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi verify - prove the overlay is excluded from screen capture

It briefly shows a bright test window, then captures the screen twice with
each capture back end: once with protection off (the window must be visible)
and once with protection on (it must be gone). A back end only passes if it
sees the window in the first pass and loses it in the second.

FLAGS
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("  Running capture-protection verification.")
	fmt.Println()
	fmt.Println("  A bright red test window will appear for a few seconds. It is shown")
	fmt.Println("  UNPROTECTED first on purpose: each capture method has to prove it can")
	fmt.Println("  see the window before its inability to see the protected one means")
	fmt.Println("  anything. If you screenshot during those first seconds you will catch")
	fmt.Println("  it -- that is the control, not a failure.")
	fmt.Println()

	ov := overlay.New(overlay.Config{
		X: *x, Y: *y, W: *w, H: *h,
		Opacity:      255,
		ClickThrough: true,
		Protect:      false, // the control phase needs it visible first
		Hotkeys:      false,
		Placeholder:  "overlayc capture test",
	})

	// The overlay owns an OS thread, so it runs on its own goroutine while
	// the verification driver works from here.
	runErr := make(chan error, 1)
	go func() { runErr <- ov.Run() }()

	select {
	case <-ov.Ready():
	case err := <-runErr:
		if err != nil {
			return fmt.Errorf("could not create the test overlay: %w", err)
		}
		return fmt.Errorf("test overlay exited before it was ready")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("test overlay did not become ready within 10s")
	}

	rect := verify.Rect{X: *x, Y: *y, W: *w, H: *h}
	report := verify.Run(ov, rect, verify.Options{Settle: *settle, SaveDir: *shots})

	ov.Stop()
	select {
	case <-ov.Closed():
	case <-time.After(3 * time.Second):
	}

	fmt.Print(report.String())

	if *shots != "" {
		fmt.Printf("  Before/after PNGs written to %s\n", *shots)
	}

	passed, leaked, unclear, _ := report.Counts()
	if leaked == 0 && unclear == 0 && passed > 0 {
		fmt.Printf("\nRESULT: PROTECTED on all %d capture methods tested.\n\n", passed)
		fmt.Println("Want to see it for yourself rather than trust this table?")
		fmt.Println("  ooi demo      leaves a protected window up so you can try")
		fmt.Println("                     recording it with OBS, Game Bar, Snipping Tool...")
		fmt.Println()
		fmt.Println("Not covered by this test, and not defeated by this technique:")
		fmt.Println("  - a phone or camera pointed at the screen")
		fmt.Println("  - an HDMI capture card between the GPU and the monitor")
		fmt.Println("  - kernel-mode capture drivers or mirror display drivers")
		return nil
	}

	fmt.Println("\nRESULT: NOT fully verified. See the table above.")
	return fmt.Errorf("%d method(s) leaked, %d inconclusive", leaked, unclear)
}
