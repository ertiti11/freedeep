package cdp

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeChrome is a one-connection WebSocket server that answers CDP calls,
// emits an event first, and sends one large (64-bit length) frame.
func fakeChrome(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsGUID))
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+
			base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
		send := func(payload []byte) {
			hdr := []byte{0x81}
			switch n := len(payload); {
			case n < 126:
				hdr = append(hdr, byte(n))
			case n <= 0xFFFF:
				hdr = append(hdr, 126, byte(n>>8), byte(n))
			default:
				hdr = append(hdr, 127)
				hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
			}
			c.Write(append(hdr, payload...))
		}
		send([]byte(`{"method":"Network.requestWillBeSent","params":{"request":{"headers":{"x-hif-leim":"L"}}}}`))
		for {
			var h [2]byte
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return
			}
			n := int(h[1] & 0x7F)
			if n == 126 {
				var b [2]byte
				io.ReadFull(br, b[:])
				n = int(binary.BigEndian.Uint16(b[:]))
			}
			var mask [4]byte
			io.ReadFull(br, mask[:])
			p := make([]byte, n)
			io.ReadFull(br, p)
			for i := range p {
				p[i] ^= mask[i%4]
			}
			if h[0]&0x0F == 0x8 {
				return
			}
			var m struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			json.Unmarshal(p, &m)
			big := strings.Repeat("v", 70000)
			resp, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{"method": m.Method, "big": big}})
			send(resp)
		}
	}()
	return "ws://" + ln.Addr().String() + "/devtools/page/1"
}

func TestCallRoundTripAndEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, fakeChrome(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var out struct {
		Method string `json:"method"`
		Big    string `json:"big"`
	}
	if err := conn.Call(ctx, "Runtime.evaluate", map[string]any{"expression": strings.Repeat("x", 300)}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != "Runtime.evaluate" || len(out.Big) != 70000 {
		t.Errorf("result %q len=%d", out.Method, len(out.Big))
	}
	if ev := conn.Events(); len(ev) != 1 || ev[0].Method != "Network.requestWillBeSent" {
		t.Errorf("events %+v", ev)
	}
}
