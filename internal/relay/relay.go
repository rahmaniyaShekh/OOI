// Package relay is the host's end of the rendezvous relay: a WebSocket to the
// room's Durable Object that forwards sealed messages to and from the sharing
// page, for the network pairs that can never form a direct WebRTC path.
//
// Every message is iv(12) | AES-256-GCM(type(1) | payload) | tag(16) under a
// key the page generated and sent inside its code-sealed answer, so the server
// forwards ciphertext it cannot read. The additional data names the direction,
// so a message can never be reflected back to the side that sent it.
package relay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Message types.
const (
	TypeHostMedia   = 1 // media from the host (unused: OOI's host only receives)
	TypeControl     = 2 // control JSON, both directions
	TypeViewerMedia = 3 // encoded video from the sharing page
)

// Direction labels, bound into every message as GCM additional data.
var (
	adHostToViewer = []byte("OOI-relay/v1 h2v")
	adViewerToHost = []byte("OOI-relay/v1 v2h")
)

const (
	ivLen  = 12
	tagLen = 16
	// MaxMessage bounds what the host accepts; the Worker caps the page at 1 MiB.
	MaxMessage = 1 << 20

	connectTimeout   = 3500 * time.Millisecond
	handshakeTimeout = 8 * time.Second
	writeTimeout     = 10 * time.Second
)

// Seal encrypts one message in the given direction.
func Seal(aead cipher.AEAD, fromHost bool, typ byte, payload []byte) ([]byte, error) {
	iv := make([]byte, ivLen, ivLen+1+len(payload)+tagLen)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	plain := make([]byte, 0, 1+len(payload))
	plain = append(plain, typ)
	plain = append(plain, payload...)
	return aead.Seal(iv, iv, plain, ad(fromHost)), nil
}

// Open decrypts one message sent in the given direction.
func Open(aead cipher.AEAD, fromHost bool, msg []byte) (byte, []byte, error) {
	if len(msg) < ivLen+1+tagLen {
		return 0, nil, errors.New("relay: short message")
	}
	plain, err := aead.Open(nil, msg[:ivLen], msg[ivLen:], ad(fromHost))
	if err != nil {
		return 0, nil, errors.New("relay: message failed authentication")
	}
	return plain[0], plain[1:], nil
}

func ad(fromHost bool) []byte {
	if fromHost {
		return adHostToViewer
	}
	return adViewerToHost
}

// NewAEAD builds the cipher from the page's 32-byte key.
func NewAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("relay: key is %d bytes, want 32", len(key))
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Conn is the host's relay socket. Send is safe from any goroutine; Recv must
// be called from one goroutine only.
type Conn struct {
	ws   *websocket.Conn
	aead cipher.AEAD

	wmu    sync.Mutex
	closed chan struct{}
	once   sync.Once
}

// Dial joins the relay at rawURL (https://... or wss://...). The TCP connect is
// capped at 3.5 s and retried once on a fresh connection: some networks drop
// SYNs to one of the service's addresses, and Windows would otherwise wait out
// a 21 s retry.
func Dial(rawURL string, key []byte) (*Conn, error) {
	aead, err := NewAEAD(key)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	var ws *websocket.Conn
	for attempt := 0; attempt < 2; attempt++ {
		if ws, err = dialOnce(u); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	return &Conn{ws: ws, aead: aead, closed: make(chan struct{})}, nil
}

func dialOnce(u *url.URL) (*websocket.Conn, error) {
	origin := &url.URL{Scheme: "https", Host: u.Host}
	if u.Scheme == "ws" {
		origin.Scheme = "http"
	}
	cfg, err := websocket.NewConfig(u.String(), origin.String())
	if err != nil {
		return nil, err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	d := &net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}
	raw, err := d.Dial("tcp", host)
	if err != nil {
		return nil, fmt.Errorf("relay: connect: %w", err)
	}
	// The TLS and upgrade handshakes get a deadline too, so a stalled server can
	// never hang the host.
	raw.SetDeadline(time.Now().Add(handshakeTimeout))
	conn := raw
	if u.Scheme == "wss" {
		tc := tls.Client(raw, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			raw.Close()
			return nil, fmt.Errorf("relay: tls: %w", err)
		}
		conn = tc
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay: upgrade: %w", err)
	}
	raw.SetDeadline(time.Time{})
	ws.MaxPayloadBytes = MaxMessage + 1024
	ws.PayloadType = websocket.BinaryFrame
	return ws, nil
}

// Send seals and sends one host-to-page message.
func (c *Conn) Send(typ byte, payload []byte) error {
	msg, err := Seal(c.aead, true, typ, payload)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return websocket.Message.Send(c.ws, msg)
}

// Ping sends the text keepalive the Durable Object answers by itself.
func (c *Conn) Ping() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return websocket.Message.Send(c.ws, "ping")
}

// Recv returns the next authentic page-to-host message. Keepalive replies and
// anything that fails authentication are skipped.
func (c *Conn) Recv() (byte, []byte, error) {
	for {
		var msg []byte
		if err := websocket.Message.Receive(c.ws, &msg); err != nil {
			return 0, nil, err
		}
		if len(msg) < ivLen+1+tagLen {
			continue // "pong", or junk
		}
		typ, payload, err := Open(c.aead, false, msg)
		if err != nil {
			continue
		}
		return typ, payload, nil
	}
}

// Close ends the relay. It never holds the write lock while closing, so a
// sender blocked on a full socket cannot deadlock shutdown.
func (c *Conn) Close() {
	c.once.Do(func() {
		close(c.closed)
		c.ws.Close()
	})
}

// Closed is closed once Close has been called.
func (c *Conn) Closed() <-chan struct{} { return c.closed }
