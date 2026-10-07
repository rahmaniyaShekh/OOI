//go:build windows

package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"syscall"
	"time"

	"ooi/internal/control"
	"ooi/internal/procctl"
	"ooi/internal/touchpad"
	"ooi/internal/vdec"
)

var (
	user32Ctl           = syscall.NewLazyDLL("user32.dll")
	procGetSystemMetric = user32Ctl.NewProc("GetSystemMetrics")
)

func getSystemMetrics(index int32) int32 {
	r, _, _ := procGetSystemMetric.Call(uintptr(index))
	return int32(r)
}

// decoderVersion reports the linked decoder libraries, for `ooi version`.
func decoderVersion() string { return "  decoders: " + vdec.Version() }

// gestureStatusLine reports whether touchpad control will actually engage.
func gestureStatusLine(enabled bool) string {
	if !enabled {
		return "disabled (--no-gestures)"
	}
	if ok, err := touchpad.Available(); !ok {
		return fmt.Sprintf("unavailable (%v) - hotkeys still work", err)
	}
	return "ON (touchpad detected)"
}

// controlClientFor builds a control client from the stored loopback URL.
func controlClientFor(local string) (*control.Client, error) {
	u, err := url.Parse(local)
	if err != nil {
		return nil, fmt.Errorf("bad stored url %q: %w", local, err)
	}
	u.Path = ""
	return control.NewClient(u.String()), nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := procctl.Dir()
	if err != nil {
		return err
	}
	st, err := procctl.Load(dir)
	if err != nil {
		fmt.Println("ooi is not running")
		return nil
	}

	cl, err := controlClientFor(st.Local)
	if err != nil {
		return err
	}
	s, err := cl.Status()
	if err != nil {
		fmt.Printf("ooi pid %d is running but not answering on %s\n", st.PID, st.Local)
		return nil
	}

	protection := "ON (verified with the OS just now)"
	if !s.Protected {
		protection = "*** OFF - THE OVERLAY IS VISIBLE TO RECORDERS ***"
	}
	state := "no friend connected"
	if s.Connected {
		state = "connected"
	}
	shown := "on screen"
	if !s.Enabled {
		shown = "hidden (still streaming)"
	}

	fmt.Printf("ooi is running\n\n")
	fmt.Printf("  pid          %d\n", st.PID)
	fmt.Printf("  uptime       %s\n", time.Since(st.Started).Round(time.Second))
	fmt.Printf("  code         %s\n", s.Code)
	fmt.Printf("  link         %s\n", s.Link)
	fmt.Printf("  protection   %s\n", protection)
	if s.AffinityRestored > 0 {
		fmt.Printf("  watchdog     re-applied protection %d time(s)\n", s.AffinityRestored)
	}
	if s.FailClosed > 0 {
		fmt.Printf("  fail-closed  refused to show %d time(s) (protection unconfirmed)\n", s.FailClosed)
	}
	fmt.Printf("  overlay      %s\n", shown)
	fmt.Printf("  state        %s\n", state)
	if s.Connected {
		path := "direct (peer to peer)"
		if s.Path == "relay" {
			path = "relayed through the rendezvous (end-to-end encrypted; no direct path between the networks)"
		}
		fmt.Println("  path         " + path)
		fmt.Printf("  video        %s %dx%d", s.Codec, s.Width, s.Height)
		if s.RTTMillis > 0 {
			fmt.Printf(", rtt %dms", s.RTTMillis)
		}
		fmt.Println()
		fmt.Printf("  frames       %d shown, %d keyframes, %d decode errors\n", s.Decoded, s.KeyFrames, s.DecodeErr)
		fmt.Printf("  recovery     %d resyncs, %d keyframe requests\n", s.Resyncs, s.KeyRequests)
		fmt.Printf("  received     %s\n", humanBytes(s.BytesIn))
		if s.LastFrameAgoMS >= 0 {
			fmt.Printf("  last frame   %dms ago\n", s.LastFrameAgoMS)
		}
	}
	if s.FailedJoins > 0 {
		fmt.Printf("  failed joins %d (last %ds ago: %s)\n", s.FailedJoins, s.LastFailureAgoS, s.LastFailure)
	}
	if !s.Protected {
		return fmt.Errorf("capture protection is not active")
	}
	return nil
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	force := fs.Bool("force", false, "terminate the process instead of asking it to exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := procctl.Dir()
	if err != nil {
		return err
	}
	st, err := procctl.Load(dir)
	if err != nil {
		fmt.Println("ooi is not running")
		return nil
	}

	if !*force {
		if cl, err := controlClientFor(st.Local); err == nil {
			if err := cl.Stop(); err == nil && waitGone(st.PID, 5*time.Second) {
				procctl.Clear(dir)
				fmt.Printf("stopped ooi (pid %d)\n", st.PID)
				return nil
			}
		}
		fmt.Println("graceful stop did not take effect, terminating...")
	}

	if err := procctl.Kill(st.PID); err != nil {
		return err
	}
	waitGone(st.PID, 3*time.Second)
	procctl.Clear(dir)
	fmt.Printf("terminated ooi (pid %d)\n", st.PID)
	return nil
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !procctl.Alive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !procctl.Alive(pid)
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
