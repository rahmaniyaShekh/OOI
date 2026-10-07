//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"ooi/internal/code"
	"ooi/internal/config"
	"ooi/internal/control"
	"ooi/internal/overlay"
	"ooi/internal/procctl"
	"ooi/internal/rendezvous"
	"ooi/internal/rtc"
	"ooi/internal/vdec"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		geom       = fs.String("geometry", "1280x720", "overlay size and position: WxH or WxH+X+Y (physical pixels)")
		opacity    = fs.Int("opacity", 235, "overlay opacity, 1-255")
		service    = fs.String("service", rendezvous.DefaultService, "rendezvous base URL")
		codecs     = fs.String("codecs", "", "codec preference, comma separated (vp9,av1,vp8,h264); default vp9,av1,vp8,h264")
		threads    = fs.Int("threads", 0, "decoder threads (0 = auto)")
		turn       = fs.String("turn", "", "TURN server URL (turn:host:port), used only when a direct path fails")
		turnUser   = fs.String("turn-user", "", "TURN username")
		turnPass   = fs.String("turn-pass", "", "TURN credential")
		noStun     = fs.Bool("no-stun", false, "do not use public STUN (LAN-only, no external contact)")
		clickThru  = fs.Bool("click-through", true, "let mouse clicks pass through the overlay")
		noHotkeys  = fs.Bool("no-hotkeys", false, "do not register global hotkeys")
		noGesture  = fs.Bool("no-gestures", false, "disable touchpad gesture control")
		newCode    = fs.Bool("new-code", false, "rotate the join code before starting")
		foreground = fs.Bool("foreground", false, "stay in this terminal with a live log (default: run in the background)")
		_          = fs.Bool("detach", true, "run in the background (the default; kept for compatibility)")
		logFile    = fs.String("log-file", "", "write the log to this file instead of the terminal")
		quiet      = fs.Bool("quiet", false, "suppress the log stream")
		noRelay    = fs.Bool("no-relay", false, "do not offer the encrypted relay through the rendezvous when a direct path fails")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `ooi serve - start the receiver

Prints a code and a link. Your friend opens the link (or the site and types the
code), chooses a window or screen to share, and it appears in your protected
overlay. The code belongs to this device and works again next time.

FLAGS
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
HOTKEYS (while running)
  Ctrl+Alt+H            hide / show the overlay (off screen, still streaming)
  Ctrl+Alt+Q            quit
  Ctrl+Alt+M            toggle mouse click-through
  Ctrl+Alt+Arrows       move the overlay
  Ctrl+Alt+Shift+Arrows resize the overlay
  Ctrl+Alt+Plus/Minus   opacity up / down

TOUCHPAD
  3 quick taps   hide the overlay (streaming continues in the background)
  6 quick taps   bring it back
  1 finger held  brighten     2 fingers held  dim     then slide  move

Capture protection is always on and cannot be disabled here.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	g, err := config.ParseGeometry(*geom)
	if err != nil {
		return err
	}
	if !g.HasPos {
		g.X, g.Y = defaultPosition(g.W, g.H)
	}
	if *opacity < 1 || *opacity > 255 {
		return fmt.Errorf("opacity must be 1..255, got %d", *opacity)
	}
	order, err := rtc.ParseCodecOrder(*codecs)
	if err != nil {
		return err
	}

	// Background is the default: start, print the link, return the prompt.
	// The relaunched child gets --foreground so it does not detach again.
	if !*foreground {
		return runDetached(args)
	}

	dir, err := procctl.Dir()
	if err != nil {
		return err
	}
	if *newCode {
		if _, err := code.Rotate(dir); err != nil {
			return err
		}
	}
	joinCode, _, err := code.LoadOrCreate(dir)
	if err != nil {
		return err
	}

	iceServers := buildICE(*noStun, *turn, *turnUser, *turnPass)

	return runServe(serveParams{
		geom: g, opacity: *opacity, service: *service, order: order, threads: *threads,
		ice: iceServers, clickThru: *clickThru, hotkeys: !*noHotkeys, gestures: !*noGesture,
		code: joinCode, quiet: *quiet, dir: dir, logFile: *logFile, noRelay: *noRelay,
		args: restartArgs(args),
	})
}

type serveParams struct {
	geom      config.Geometry
	opacity   int
	service   string
	order     []vdec.Codec
	threads   int
	ice       []webrtc.ICEServer
	clickThru bool
	hotkeys   bool
	gestures  bool
	code      string
	quiet     bool
	logFile   string
	dir       string
	noRelay   bool
	args      []string
}

// restartArgs drops the flags that only describe how this copy was launched,
// leaving the user's own serve flags for a restart after an update.
func restartArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := strings.TrimLeft(args[i], "-")
		switch {
		case a == "foreground" || a == "foreground=true" || a == "foreground=false" ||
			strings.HasPrefix(a, "detach") || a == "new-code" || a == "new-code=true":
			continue
		case a == "log-file":
			i++ // and its value
			continue
		case strings.HasPrefix(a, "log-file="):
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func buildICE(noStun bool, turn, user, pass string) []webrtc.ICEServer {
	var ice []webrtc.ICEServer
	if !noStun {
		ice = append(ice, webrtc.ICEServer{URLs: []string{
			"stun:stun.cloudflare.com:3478",
			"stun:stun.l.google.com:19302",
		}})
	}
	if turn != "" {
		s := webrtc.ICEServer{URLs: []string{turn}}
		if user != "" {
			s.Username = user
			s.Credential = pass
			s.CredentialType = webrtc.ICECredentialTypePassword
		}
		ice = append(ice, s)
	}
	return ice
}

func runServe(p serveParams) error {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	if p.logFile != "" {
		// Truncated on each start so it cannot grow without bound.
		if f, err := os.OpenFile(p.logFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err == nil {
			defer f.Close()
			logger = log.New(f, "", log.LstdFlags)
		}
	}
	if p.quiet {
		logger = log.New(io.Discard, "", 0)
	}
	logger.Printf("ooi %s starting: code %s, service %s, overlay %dx%d", version,
		code.Pretty(p.code), p.service, p.geom.W, p.geom.H)

	ov := overlay.New(overlay.Config{
		X: p.geom.X, Y: p.geom.Y, W: p.geom.W, H: p.geom.H,
		Opacity:      p.opacity,
		ClickThrough: p.clickThru,
		Protect:      true, // never optional
		Hotkeys:      p.hotkeys,
		Gestures:     p.gestures,
		Placeholder:  "ooi - waiting for your friend",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rz := rendezvous.New(p.service)
	driver, err := rtc.NewDriver(rtc.DriverConfig{
		Code:       p.code,
		Order:      p.order,
		ICEServers: p.ice,
		Threads:    p.threads,
		Sink:       ov,
		Rz:         rz,
		Log:        logger,
		NoRelay:    p.noRelay,
		OnConnect: func(st rtc.Stats) {
			logger.Printf("connected (%s): %s %dx%d", st.Path, st.Codec, st.Width, st.Height)
		},
		OnDisconnect: func() { ov.ClearFrame() },
	})
	if err != nil {
		return err
	}

	// Loopback control plane for status/stop.
	// Once a stop is requested, the process is gone within 4 s whatever
	// happens: a clean shutdown is preferred, a hung one is not allowed.
	var watchdog sync.Once
	stop := func() {
		watchdog.Do(func() {
			go func() {
				time.Sleep(4 * time.Second)
				logger.Printf("shutdown took over 4s; exiting")
				os.Exit(0)
			}()
		})
		cancel()
	}
	ctrl, port, err := control.Start(func() control.Status {
		return snapshot(ov, driver, p.code, rz.JoinURL(code.Pretty(p.code)))
	}, stop)
	if err != nil {
		return err
	}
	defer ctrl.Close()

	// Publish state so `status`/`stop` can find us.
	st := procctl.State{
		PID:     os.Getpid(),
		URL:     rz.JoinURL(code.Pretty(p.code)),
		Local:   fmt.Sprintf("http://127.0.0.1:%d/", port),
		Started: time.Now(),
		Args:    p.args,
	}
	if err := procctl.Save(p.dir, st); err != nil {
		return err
	}
	defer procctl.Clear(p.dir)

	if !p.quiet && p.logFile == "" {
		printBanner(p, rz.JoinURL(code.Pretty(p.code)))
	}

	// Drive connections until the overlay or a signal stops us.
	driverDone := make(chan struct{})
	go func() {
		defer close(driverDone)
		if err := driver.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("driver stopped: %v", err)
		}
		ov.Stop()
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		select {
		case <-sigc:
			stop()
			ov.Stop()
		case <-ctx.Done():
			ov.Stop()
		}
	}()

	runErr := ov.Run()
	stop()
	// Let the driver withdraw the code (bounded; the watchdog backs it up).
	select {
	case <-driverDone:
	case <-time.After(2500 * time.Millisecond):
	}
	logger.Printf("stopped")
	return runErr
}

func snapshot(ov *overlay.Overlay, d *rtc.Driver, joinCode, link string) control.Status {
	s := d.Stats()
	lastAgo := int64(-1)
	if !s.LastFrame.IsZero() {
		lastAgo = time.Since(s.LastFrame).Milliseconds()
	}
	return control.Status{
		Code:             code.Pretty(joinCode),
		Link:             link,
		Connected:        s.Connected,
		Protected:        ov.Protected(),
		Enabled:          ov.Enabled(),
		Cloaked:          ov.Cloaked(),
		Codec:            s.Codec,
		Width:            s.Width,
		Height:           s.Height,
		RTTMillis:        s.RTTMillis,
		Frames:           s.Frames,
		KeyFrames:        s.KeyFrames,
		Decoded:          s.Decoded,
		DecodeErr:        s.DecodeErrors,
		Resyncs:          s.Resyncs,
		KeyRequests:      s.KeyRequests,
		BytesIn:          s.BytesIn,
		AffinityRestored: ov.AffinityRestored(),
		FailClosed:       ov.FailClosedCount(),
		LastFrameAgoMS:   lastAgo,
		Path:             s.Path,
		FailedJoins:      s.FailedJoins,
		LastFailure:      s.LastFailure,
		LastFailureAgoS:  failAgo(s.LastFailureAt),
	}
}

func failAgo(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return int64(time.Since(t).Seconds()) + 1
}

func printBanner(p serveParams, link string) {
	fmt.Println()
	fmt.Println("  ooi receiver is running")
	fmt.Println()
	fmt.Println("  Send your friend this link:")
	fmt.Println()
	fmt.Println("      " + link)
	fmt.Println()
	fmt.Printf("  Or tell them the code:   %s\n", code.Pretty(p.code))
	fmt.Println("  (they open " + strings.TrimSuffix(p.service, "/") + " and type it)")
	fmt.Println()
	fmt.Printf("  overlay      %dx%d at (%d,%d), opacity %d\n", p.geom.W, p.geom.H, p.geom.X, p.geom.Y, p.opacity)
	fmt.Printf("  protection   ALWAYS ON (WDA_EXCLUDEFROMCAPTURE, re-checked every 2s)\n")
	fmt.Printf("  codecs       %s\n", codecList(p.order))
	fmt.Printf("  gestures     %s\n", gestureStatusLine(p.gestures))
	fmt.Println()
	fmt.Println("  Ctrl+Alt+H hide/show   Ctrl+Alt+Q quit   Ctrl+Alt+Arrows move")
	fmt.Println("  Touchpad:  3 taps hide, 6 taps show; hold to dim/brighten, slide to move")
	fmt.Println()
}

func codecList(order []vdec.Codec) string {
	names := make([]string, len(order))
	for i, c := range order {
		names[i] = c.String()
	}
	return strings.Join(names, " > ")
}

// runDetached relaunches this exe in the background with --foreground and a
// log file, waits for it to publish its state, and prints the link.
func runDetached(args []string) error {
	dir, err := procctl.Dir()
	if err != nil {
		return err
	}
	if s, err := procctl.Load(dir); err == nil {
		fmt.Printf("ooi is already running (pid %d)\n\n", s.PID)
		printLink(s.URL)
		fmt.Println("  ooi status    code, connection and protection state")
		fmt.Println("  ooi stop      stop it")
		return nil
	}
	logPath := filepath.Join(dir, "ooi.log")
	child := []string{"serve"}
	for _, a := range args {
		switch strings.TrimLeft(a, "-") {
		case "detach", "detach=true", "detach=false", "foreground", "foreground=false":
			continue
		}
		child = append(child, a)
	}
	child = append(child, "--foreground", "--log-file", logPath)

	pid, err := procctl.Detach(child)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := procctl.Load(dir); err == nil && s.PID == pid {
			fmt.Printf("ooi is running in the background (pid %d)\n\n", pid)
			printLink(s.URL)
			fmt.Println("  ooi status    code, connection and protection state")
			fmt.Println("  ooi stop      stop it")
			fmt.Println("  log           " + logPath)
			return nil
		}
		if !procctl.Alive(pid) {
			return fmt.Errorf("ooi exited during startup; see %s", logPath)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("started pid %d but it did not come up within 15s; see %s", pid, logPath)
}

func printLink(url string) {
	fmt.Println("  Send your friend this link:")
	fmt.Println()
	fmt.Println("      " + url)
	fmt.Println()
}

// defaultPosition centres the overlay near the top of the primary monitor.
func defaultPosition(w, h int) (int, int) {
	const smCXScreen, smCYScreen = 0, 1
	sw := int(getSystemMetrics(smCXScreen))
	sh := int(getSystemMetrics(smCYScreen))
	if sw <= 0 || sh <= 0 {
		return 40, 40
	}
	x := (sw - w) / 2
	if x < 0 {
		x = 0
	}
	y := 40
	if h+80 > sh {
		y = 0
	}
	return x, y
}
