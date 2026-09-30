// Package config holds the runtime options and the pure parsing/validation
// helpers behind them. Nothing here touches Windows, so it is fully testable.
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Geometry describes the overlay rectangle in physical pixels.
type Geometry struct {
	W, H int
	X, Y int
	// HasPos is false when the caller only specified a size.
	HasPos bool
}

// geometryRe matches "WxH", "WxH+X+Y", "WxH-X-Y" and the sign combinations.
var geometryRe = regexp.MustCompile(`^(\d+)x(\d+)(?:([+-]\d+)([+-]\d+))?$`)

// ParseGeometry parses an X11-style geometry string such as "1280x720+40+40".
func ParseGeometry(s string) (Geometry, error) {
	s = strings.TrimSpace(s)
	m := geometryRe.FindStringSubmatch(s)
	if m == nil {
		return Geometry{}, fmt.Errorf("config: invalid geometry %q (want WxH or WxH+X+Y)", s)
	}
	w, err := strconv.Atoi(m[1])
	if err != nil {
		return Geometry{}, fmt.Errorf("config: geometry width: %w", err)
	}
	h, err := strconv.Atoi(m[2])
	if err != nil {
		return Geometry{}, fmt.Errorf("config: geometry height: %w", err)
	}
	if w < MinDim || h < MinDim {
		return Geometry{}, fmt.Errorf("config: geometry must be at least %dx%d", MinDim, MinDim)
	}
	if w > MaxDim || h > MaxDim {
		return Geometry{}, fmt.Errorf("config: geometry must be at most %dx%d", MaxDim, MaxDim)
	}
	g := Geometry{W: w, H: h}
	if m[3] != "" {
		x, err := strconv.Atoi(m[3])
		if err != nil {
			return Geometry{}, fmt.Errorf("config: geometry x: %w", err)
		}
		y, err := strconv.Atoi(m[4])
		if err != nil {
			return Geometry{}, fmt.Errorf("config: geometry y: %w", err)
		}
		g.X, g.Y, g.HasPos = x, y, true
	}
	return g, nil
}

// Dimension bounds. The upper bound keeps a typo from asking for a
// multi-gigabyte DIB section.
const (
	MinDim = 64
	MaxDim = 16384
)

// Options is the fully-resolved configuration for `serve`.
type Options struct {
	Addr      string   // listen address, e.g. "0.0.0.0:8787"
	Geometry  Geometry // overlay rectangle
	Opacity   int      // 0..255 window-wide alpha
	FPS       int      // frame rate hint sent to the sender
	Quality   int      // JPEG quality hint sent to the sender, 1..100
	ClickThru bool     // pass mouse input through the overlay
	Hotkeys   bool     // register global hotkeys
	Gestures  bool     // enable touchpad gesture control
	ReadLimit int64    // max bytes for one websocket message
	StatePath string   // where the pid/url state file lives
}

// Validate checks the resolved options and normalises what it can.
func (o *Options) Validate() error {
	if err := ValidateAddr(o.Addr); err != nil {
		return err
	}
	if o.Geometry.W < MinDim || o.Geometry.H < MinDim {
		return fmt.Errorf("config: overlay must be at least %dx%d", MinDim, MinDim)
	}
	if o.Geometry.W > MaxDim || o.Geometry.H > MaxDim {
		return fmt.Errorf("config: overlay must be at most %dx%d", MaxDim, MaxDim)
	}
	if o.Opacity < 1 || o.Opacity > 255 {
		return fmt.Errorf("config: opacity must be 1..255, got %d", o.Opacity)
	}
	if o.FPS < 1 || o.FPS > 60 {
		return fmt.Errorf("config: fps must be 1..60, got %d", o.FPS)
	}
	if o.Quality < 1 || o.Quality > 100 {
		return fmt.Errorf("config: quality must be 1..100, got %d", o.Quality)
	}
	if o.ReadLimit <= 0 {
		return errors.New("config: read limit must be positive")
	}
	return nil
}

// ValidateAddr checks a host:port listen address.
func ValidateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("config: invalid address %q: %w", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("config: invalid port %q", port)
	}
	if host != "" && net.ParseIP(host) == nil {
		return fmt.Errorf("config: invalid host %q", host)
	}
	return nil
}

// ClampOpacity keeps a hotkey-driven opacity change inside the legal range.
func ClampOpacity(v int) int {
	if v < 8 {
		return 8
	}
	if v > 255 {
		return 255
	}
	return v
}
