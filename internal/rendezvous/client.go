// Package rendezvous is the host's client for the Cloudflare Worker mailbox.
//
// The Worker swaps one sealed offer and one sealed answer between the peers and
// then leaves the path. The host publishes and polls; the joiner pushes. Neither
// side needs a WebSocket or an inbound port.
package rendezvous

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultService is the deployed rendezvous. It is overridable with --service so
// staging and self-hosting work without a rebuild.
const DefaultService = "https://share.mdarif.online/ooi"

// ErrRoomGone means the room expired or was deleted. The host should republish.
var ErrRoomGone = errors.New("rendezvous: room not found")

// Client talks to one rendezvous service.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for base, e.g. "https://share.mdarif.online/ooi".
func New(base string) *Client {
	if base == "" {
		base = DefaultService
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				// ProxyFromEnvironment honours an explicit HTTPS_PROXY but never
				// runs WPAD auto-discovery, which on Windows can stall the first
				// request for ~8 s on networks that have no WPAD server.
				Proxy: http.ProxyFromEnvironment,
				// Some networks intermittently drop SYNs to one of the service's
				// two addresses, and a plain connect then waits out Windows' 21 s
				// SYN retry. Go splits the dial timeout across the addresses, so
				// 7 s caps each one at 3.5 s before moving to the next.
				DialContext: (&net.Dialer{
					Timeout:   7 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   8 * time.Second,
				ResponseHeaderTimeout: 12 * time.Second,
				MaxIdleConnsPerHost:   2,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// Base returns the service root.
func (c *Client) Base() string { return c.base }

// JoinURL is the link a friend opens. The code travels in the fragment, which a
// browser never sends to any server, so the rendezvous never learns it.
func (c *Client) JoinURL(pretty string) string {
	return c.base + "/#" + pretty
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("cache-control", "no-store")
	return c.http.Do(req)
}

// PublishParams is one offer to publish.
type PublishParams struct {
	ID, Session, Offer string
	// Owner is a secret only this host knows; the relay admits it as the host.
	Owner string
	// Caps advertises what this host can do beyond direct WebRTC ("relay").
	Caps []string
}

// RelayURL is the host's relay WebSocket for a session.
func (c *Client) RelayURL(id, session, owner string) string {
	return c.base + "/api/room/" + id + "/relay?session=" + session + "&role=host&owner=" + owner
}

// Publish creates or overwrites the room with a fresh offer. Publishing is the one
// call the whole flow depends on, so it retries with backoff before failing.
func (c *Client) Publish(ctx context.Context, p PublishParams) error {
	payload := map[string]any{"id": p.ID, "session": p.Session, "offer": p.Offer}
	if p.Owner != "" {
		payload["owner"] = p.Owner
	}
	if len(p.Caps) > 0 {
		payload["caps"] = p.Caps
	}
	var last error
	for attempt, wait := 0, 500*time.Millisecond; attempt < 4; attempt, wait = attempt+1, wait*2 {
		resp, err := c.do(ctx, http.MethodPost, "/api/room", payload)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("rendezvous: publish: HTTP %d", resp.StatusCode)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return last // a 4xx will not fix itself on retry
			}
		} else {
			last = fmt.Errorf("rendezvous: publish: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return last
}

// PollAnswer checks the session's answer slot once.
//
// It returns the answer and true when one is waiting (the slot is read-once, so
// a second call returns false), false when not yet, and ErrRoomGone when the room
// no longer exists. Polling also keeps the room alive past its idle TTL.
func (c *Client) PollAnswer(ctx context.Context, id, session string) (string, bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/room/"+id+"/answer?session="+session, nil)
	if err != nil {
		return "", false, fmt.Errorf("rendezvous: poll: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
			return "", false, fmt.Errorf("rendezvous: poll: bad body: %w", err)
		}
		return out.Answer, out.Answer != "", nil
	case http.StatusNoContent:
		return "", false, nil
	case http.StatusNotFound:
		return "", false, ErrRoomGone
	default:
		return "", false, fmt.Errorf("rendezvous: poll: HTTP %d", resp.StatusCode)
	}
}

// Delete withdraws the code, so joiners stop answering an offer nobody listens to.
func (c *Client) Delete(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/api/room/"+id, nil)
	if err != nil {
		return fmt.Errorf("rendezvous: delete: %w", err)
	}
	resp.Body.Close()
	return nil
}

// GetOffer fetches the current sealed offer and its session. ok is false when
// the room does not exist yet. These two methods are the joiner side of the
// mailbox; the native host itself does not use them, but a headless joiner
// (and the end-to-end test) does.
func (c *Client) GetOffer(ctx context.Context, id string) (offer, session string, ok bool, err error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/room/"+id, nil)
	if err != nil {
		return "", "", false, fmt.Errorf("rendezvous: get offer: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Offer   string `json:"offer"`
			Session string `json:"session"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
			return "", "", false, err
		}
		return out.Offer, out.Session, true, nil
	case http.StatusNotFound:
		return "", "", false, nil
	default:
		return "", "", false, fmt.Errorf("rendezvous: get offer: HTTP %d", resp.StatusCode)
	}
}

// PostAnswer submits a sealed answer for a session. It reports ErrStale (with the
// current session) when the offer was replaced under the joiner.
func (c *Client) PostAnswer(ctx context.Context, id, session, answer string) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/room/"+id+"/answer",
		map[string]string{"answer": answer, "session": session})
	if err != nil {
		return fmt.Errorf("rendezvous: post answer: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return ErrStale
	case http.StatusNotFound:
		return ErrRoomGone
	default:
		return fmt.Errorf("rendezvous: post answer: HTTP %d", resp.StatusCode)
	}
}

// ErrStale means the offer was replaced; the joiner should fetch the new one.
var ErrStale = errors.New("rendezvous: stale session")
