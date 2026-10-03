package jev

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// freezeProxy forwards TCP to target. freeze() stalls every connection open
// so far without closing it, the way a TCP connection silently dies when
// the Wi-Fi link drops: nothing arrives and nothing is reset. Connections
// made afterwards work normally.
type freezeProxy struct {
	ln     net.Listener
	mu     sync.Mutex
	frozen []chan struct{}
}

func newFreezeProxy(t *testing.T, target string) *freezeProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", target)
			if err != nil {
				c.Close()
				continue
			}
			stop := make(chan struct{})
			p.mu.Lock()
			p.frozen = append(p.frozen, stop)
			p.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					select {
					case <-stop:
						<-make(chan struct{}) // stalled forever, never closed
					default:
					}
					if n > 0 {
						dst.Write(buf[:n])
					}
					if err != nil {
						dst.Close()
						return
					}
				}
			}
			go pipe(s, c)
			go pipe(c, s)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return p
}

func (p *freezeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.frozen {
		close(s)
	}
	p.frozen = nil
}

func staleSetup(t *testing.T) (*httptest.Server, *freezeProxy) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1}}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, newFreezeProxy(t, srv.Listener.Addr().String())
}

func call(c *Client, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := c.Evaluate(ctx, "s", map[string]Question{"q": Noul("q?")})
	return err
}

// After a link outage, a call times out on the dead connection. The next
// call, on a working link, must not reuse that connection.
func TestRecoversFromDeadConnection(t *testing.T) {
	srv, p := staleSetup(t)
	c := New("k")
	c.Endpoint = "https://" + p.ln.Addr().String()
	// Trust the test server without replacing the client's TLS config, so
	// the protocols the client offers are the ones it ships with.
	if c.transport.TLSClientConfig == nil {
		c.transport.TLSClientConfig = &tls.Config{}
	}
	c.transport.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	if err := call(c, 2*time.Second); err != nil {
		t.Fatalf("first call: %v", err)
	}
	p.freeze()
	if err := call(c, 300*time.Millisecond); err == nil {
		t.Fatal("call over a frozen connection should time out")
	}
	for i := 0; i < 3; i++ {
		if err := call(c, 2*time.Second); err != nil {
			t.Fatalf("call %d after the outage: %v (stuck on the dead connection)", i+1, err)
		}
	}
}
