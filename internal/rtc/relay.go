package rtc

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"ooi/internal/relay"
	"ooi/internal/vdec"
)

// Relay mode: the sharing page could not reach this host directly, so it
// encodes the screen itself (WebCodecs) and sends each frame through the
// rendezvous relay, sealed with a key from its code-sealed answer. The path is
// TCP, so nothing is lost and no NACK/FEC is needed; the page does the rate
// control from its own send backlog and the decode backlog reported here.

// relayQueue is how many frames may wait for the decoder. Past it the host
// drops to the next keyframe and asks for one, so latency never builds up.
const relayQueue = 4

type relayItem struct {
	codec *vdec.Codec // non-nil: (re)open the decoder for this codec
	frame relay.Frame
}

// relayCtl is the control JSON in both directions.
type relayCtl struct {
	T      string   `json:"t"`
	V      int      `json:"v,omitempty"`
	Codecs []string `json:"codecs,omitempty"`
	MaxFPS int      `json:"maxFps,omitempty"`
	Codec  string   `json:"codec,omitempty"`
	ID     int64    `json:"id,omitempty"`
	// Host -> page, once a second.
	Q    int   `json:"q,omitempty"`    // frames waiting for the decoder
	Dec  int   `json:"dec,omitempty"`  // frames decoded in total
	FPS  int   `json:"fps,omitempty"`  // frames decoded last second
	Drop int   `json:"drop,omitempty"` // frames dropped last second
	Kbps int   `json:"kbps,omitempty"` // received last second
	Ping int64 `json:"ping,omitempty"` // echo as {t:"pong", id}
}

// runRelay joins the relay and streams until the socket ends. It reports
// through the session's decoded/failed channels like the WebRTC path.
func (s *session) runRelay(url string, key []byte) {
	defer s.markFailed()
	conn, err := relay.Dial(url, key)
	if err != nil {
		s.log.Printf("relay: %v", err)
		return
	}
	s.mu.Lock()
	s.relay = conn
	s.stats.Path = "relay"
	s.mu.Unlock()
	defer conn.Close()
	s.log.Printf("relay: joined; waiting for the page")

	codecs := make([]string, len(s.order))
	for i, c := range s.order {
		codecs[i] = strings.ToLower(c.String())
	}
	send := func(m relayCtl) {
		b, _ := json.Marshal(m)
		if err := conn.Send(relay.TypeControl, b); err != nil {
			conn.Close()
		}
	}
	hello := relayCtl{T: "hello", V: 1, Codecs: codecs, MaxFPS: 15}

	items := make(chan relayItem, relayQueue)
	var (
		seekKey   atomic.Bool // dropping until the next keyframe
		decoded   atomic.Int64
		dropped   atomic.Int64
		bytesIn   atomic.Int64
		heard     atomic.Bool
		lastAsk   atomic.Int64
		decodeEnd = make(chan struct{})
	)
	seekKey.Store(true)
	askKey := func() {
		now := time.Now().UnixMilli()
		if now-lastAsk.Load() < 500 {
			return
		}
		lastAsk.Store(now)
		s.mu.Lock()
		s.stats.KeyRequests++
		s.mu.Unlock()
		send(relayCtl{T: "keyframe"})
	}

	// Decoder goroutine: the decoder is not safe for concurrent use, so it
	// is opened, used and closed here only.
	go func() {
		defer close(decodeEnd)
		var dec *vdec.Decoder
		defer func() {
			if dec != nil {
				dec.Close()
			}
		}()
		for it := range items {
			if it.codec != nil {
				if dec != nil {
					dec.Close()
				}
				d, err := vdec.New(*it.codec, s.threads)
				if err != nil {
					s.log.Printf("relay: decoder: %v", err)
					dec = nil
					conn.Close()
					continue // keep draining so the reader never blocks
				}
				dec = d
				s.mu.Lock()
				s.stats.Codec = it.codec.String()
				s.mu.Unlock()
				seekKey.Store(true)
				askKey()
				continue
			}
			if dec == nil {
				continue
			}
			ok, err := dec.Decode(it.frame.Data)
			if err != nil {
				s.mu.Lock()
				s.stats.DecodeErrors++
				s.stats.Resyncs++
				s.mu.Unlock()
				seekKey.Store(true)
				askKey()
				continue
			}
			if ok {
				decoded.Add(1)
				s.show(dec)
			}
		}
	}()

	// Reader goroutine.
	readEnd := make(chan struct{})
	go func() {
		defer close(readEnd)
		defer close(items)
		var asm relay.Assembler
		for {
			typ, p, err := conn.Recv()
			if err != nil {
				return
			}
			bytesIn.Add(int64(len(p)))
			if !heard.Swap(true) {
				s.log.Printf("relay: page connected")
			}
			switch typ {
			case relay.TypeControl:
				var m relayCtl
				if json.Unmarshal(p, &m) != nil {
					continue
				}
				switch m.T {
				case "hi":
					send(hello)
				case "config":
					c, ok := vdec.CodecFromMime("video/" + m.Codec)
					if !ok {
						s.log.Printf("relay: page chose unsupported codec %q", m.Codec)
						continue
					}
					s.log.Printf("relay: receiving %s", c)
					items <- relayItem{codec: &c}
				case "keyframe":
				case "ping":
					send(relayCtl{T: "pong", ID: m.ID})
				case "pong":
					if m.ID > 0 {
						rtt := time.Now().UnixMilli() - m.ID
						s.mu.Lock()
						s.stats.RTTMillis = rtt
						s.mu.Unlock()
					}
				}
			case relay.TypeViewerMedia:
				f, ok, err := asm.Push(p)
				if err != nil || !ok {
					continue
				}
				s.mu.Lock()
				s.stats.Frames++
				s.stats.BytesIn += uint64(len(f.Data))
				if f.Key {
					s.stats.KeyFrames++
				}
				s.mu.Unlock()
				if seekKey.Load() {
					if !f.Key {
						dropped.Add(1)
						askKey()
						continue
					}
					seekKey.Store(false)
				}
				select {
				case items <- relayItem{frame: f}:
				default:
					// The decoder is behind: drop to the next keyframe
					// rather than let latency build up.
					dropped.Add(1)
					seekKey.Store(true)
					askKey()
				}
			}
		}
	}()

	// Hello until the page speaks (it may join after us), then report once a
	// second: the page sizes its bitrate from the backlog reported here.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	send(hello)
	var lastDec, lastBytes int64
	failed := s.failed
	for {
		select {
		case <-readEnd:
			conn.Close()
			<-decodeEnd
			s.log.Printf("relay: ended")
			return
		case <-failed:
			failed = nil
			conn.Close()
		case <-tick.C:
			if !heard.Load() {
				send(hello)
				if err := conn.Ping(); err != nil {
					conn.Close()
				}
				continue
			}
			d, b := decoded.Load(), bytesIn.Load()
			send(relayCtl{T: "rx", Q: len(items), Dec: int(d), FPS: int(d - lastDec),
				Drop: int(dropped.Swap(0)), Kbps: int((b - lastBytes) * 8 / 1000),
				Ping: time.Now().UnixMilli()})
			lastDec, lastBytes = d, b
		}
	}
}
