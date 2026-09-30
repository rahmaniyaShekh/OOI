// Package control is the loopback-only management channel between `ooi status`
// / `ooi stop` and a running (possibly detached) instance.
//
// It binds 127.0.0.1 only, so the LAN can never stop the overlay or read its
// state, while local commands work regardless of where media is flowing.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// Status is the snapshot a running instance reports.
type Status struct {
	Code        string `json:"code"`
	Link        string `json:"link"`
	Connected   bool   `json:"connected"`
	Protected   bool   `json:"protected"`
	Enabled     bool   `json:"enabled"`
	Cloaked     bool   `json:"cloaked"`
	Codec       string `json:"codec"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	FPSHint     int    `json:"fps_hint"`
	RTTMillis   int64  `json:"rtt_ms"`
	Frames      uint64 `json:"frames"`
	KeyFrames   uint64 `json:"key_frames"`
	Decoded     uint64 `json:"decoded"`
	DecodeErr   uint64 `json:"decode_errors"`
	Resyncs     uint64 `json:"resyncs"`
	KeyRequests uint64 `json:"key_requests"`
	BytesIn     uint64 `json:"bytes_in"`

	AffinityRestored uint64 `json:"affinity_restored"`
	FailClosed       uint64 `json:"fail_closed"`
	LastFrameAgoMS   int64  `json:"last_frame_ago_ms"`
}

// Server is the loopback control listener.
type Server struct {
	ln       net.Listener
	srv      *http.Server
	onStop   func()
	snapshot func() Status
}

// Start binds a loopback control server on an ephemeral port and returns it
// along with the port chosen.
func Start(snapshot func() Status, onStop func()) (*Server, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	s := &Server{ln: ln, onStop: onStop, snapshot: snapshot}
	mux := http.NewServeMux()
	mux.HandleFunc("/control/status", s.handleStatus)
	mux.HandleFunc("/control/stop", s.handleStop)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	go s.srv.Serve(ln)
	return s, ln.Addr().(*net.TCPAddr).Port, nil
}

// Close shuts the control server down.
func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	json.NewEncoder(w).Encode(s.snapshot())
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "loopback only", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if s.onStop != nil {
		go s.onStop()
	}
}

// isLoopback double-checks the peer address, belt-and-braces on top of binding
// to 127.0.0.1.
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Client talks to a running instance over loopback.
type Client struct{ base string }

// NewClient targets http://127.0.0.1:<port>.
func NewClient(base string) *Client { return &Client{base: base} }

var errUnreachable = errors.New("control: instance not answering")

func (c *Client) get(path string) (*http.Response, error) {
	cl := &http.Client{Timeout: 4 * time.Second}
	return cl.Get(c.base + path)
}

// Status fetches a running instance's status.
func (c *Client) Status() (Status, error) {
	resp, err := c.get("/control/status")
	if err != nil {
		return Status{}, errUnreachable
	}
	defer resp.Body.Close()
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Status{}, err
	}
	return s, nil
}

// Stop asks a running instance to exit.
func (c *Client) Stop() error {
	cl := &http.Client{Timeout: 4 * time.Second}
	resp, err := cl.Post(c.base+"/control/stop", "application/json", nil)
	if err != nil {
		return errUnreachable
	}
	resp.Body.Close()
	return nil
}
