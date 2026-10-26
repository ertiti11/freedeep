// Package cdp is a minimal Chrome DevTools Protocol client over a tiny
// RFC 6455 WebSocket implementation (client side, text frames only), so the
// login helper needs nothing beyond the standard library.
package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Conn is a CDP session on one target.
type Conn struct {
	nc      net.Conn
	br      *bufio.Reader
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan response
	events  []Event
	closed  chan struct{}
	err     error
}

// Event is a CDP event.
type Event struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type response struct {
	Result json.RawMessage
	Err    error
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Dial connects to a ws:// debugger URL.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("cdp: unsupported scheme %q", u.Scheme)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: " + key +
		"\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := nc.Write([]byte(req)); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		nc.Close()
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		nc.Close()
		return nil, fmt.Errorf("cdp: websocket handshake failed (HTTP %d)", resp.StatusCode)
	}
	c := &Conn{nc: nc, br: br, pending: map[int64]chan response{}, closed: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.nc.Write(append(hdr, masked...))
	return err
}

func (c *Conn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin, opcode = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 256<<20 {
		err = errors.New("cdp: frame too large")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func (c *Conn) readLoop() {
	var msg []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			c.shutdown(err)
			return
		}
		switch op {
		case 0x8: // close
			c.shutdown(io.EOF)
			return
		case 0x9: // ping
			_ = c.writeFrame(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2:
			msg = append(msg[:0], payload...)
		case 0x0:
			msg = append(msg, payload...)
		}
		if fin {
			c.dispatch(msg)
		}
	}
}

func (c *Conn) dispatch(data []byte) {
	var m struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(data, &m) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.ID != 0 {
		if ch, ok := c.pending[m.ID]; ok {
			delete(c.pending, m.ID)
			r := response{Result: m.Result}
			if len(m.Error) > 0 {
				r.Err = fmt.Errorf("cdp: %s", m.Error)
			}
			ch <- r
		}
		return
	}
	if m.Method != "" {
		c.events = append(c.events, Event{Method: m.Method, Params: m.Params})
		if len(c.events) > 1000 {
			c.events = c.events[1:]
		}
	}
}

func (c *Conn) shutdown(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return
	default:
	}
	c.err = err
	close(c.closed)
	for id, ch := range c.pending {
		ch <- response{Err: fmt.Errorf("cdp: connection closed: %w", err)}
		delete(c.pending, id)
	}
}

// Call sends a command and decodes its result into out (may be nil).
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return fmt.Errorf("cdp: connection closed: %w", c.err)
	default:
	}
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if params == nil {
		params = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.writeFrame(0x1, payload); err != nil {
		return err
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return r.Err
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Events returns a copy of the buffered events (most recent 1000).
func (c *Conn) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

// Close closes the connection.
func (c *Conn) Close() error {
	_ = c.writeFrame(0x8, nil)
	return c.nc.Close()
}

// Target is an entry of /json.
type Target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func getJSON(ctx context.Context, u string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s -> HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// DevtoolsReady reports whether a DevTools endpoint answers on port.
func DevtoolsReady(ctx context.Context, port int) bool {
	var v map[string]any
	return getJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), &v) == nil
}

// PageTarget finds a page target, preferring one whose URL contains match.
func PageTarget(ctx context.Context, port int, match string) (*Target, error) {
	var targets []Target
	if err := getJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d/json", port), &targets); err != nil {
		return nil, err
	}
	var fallback *Target
	for i := range targets {
		t := &targets[i]
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		if strings.Contains(t.URL, match) {
			return t, nil
		}
		if fallback == nil {
			fallback = t
		}
	}
	if fallback == nil {
		return nil, errors.New("no Chrome page target found")
	}
	return fallback, nil
}
