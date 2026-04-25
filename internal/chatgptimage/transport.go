package chatgptimage

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func newChromeTransport(proxyURL string) (http.RoundTripper, error) {
	fallbackTransport, err := newHTTPTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	tunnelDialContext, err := newTunnelDialContext(proxyURL)
	if err != nil {
		return nil, err
	}

	return &chromeTransport{
		fallback:   fallbackTransport,
		tunnelDial: tunnelDialContext,
	}, nil
}

type chromeTransport struct {
	mu         sync.Mutex
	h2Conns    map[string]*http2.ClientConn
	fallback   http.RoundTripper
	tunnelDial func(context.Context, string, string) (net.Conn, error)
}

func (t *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}
	if !strings.EqualFold(req.URL.Scheme, "https") {
		return t.fallback.RoundTrip(req)
	}

	addr := req.URL.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr += ":443"
	}
	host := req.URL.Hostname()

	t.mu.Lock()
	if t.h2Conns == nil {
		t.h2Conns = make(map[string]*http2.ClientConn)
	}
	if cc := t.h2Conns[addr]; cc != nil {
		if cc.CanTakeNewRequest() {
			t.mu.Unlock()
			resp, err := cc.RoundTrip(req)
			if err != nil {
				t.dropConn(addr, cc)
				return nil, err
			}
			return resp, nil
		}
		delete(t.h2Conns, addr)
	}
	t.mu.Unlock()

	conn, err := t.dialTLS(req.Context(), addr, host)
	if err != nil {
		return nil, err
	}

	tr := &http2.Transport{}
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("http2 client conn: %w", err)
	}

	t.mu.Lock()
	t.h2Conns[addr] = cc
	t.mu.Unlock()

	resp, err := cc.RoundTrip(req)
	if err != nil {
		t.dropConn(addr, cc)
		return nil, err
	}
	return resp, nil
}

func (t *chromeTransport) dialTLS(ctx context.Context, addr, host string) (net.Conn, error) {
	conn, err := t.tunnelDial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConn := utls.UClient(conn, &utls.Config{
		ServerName: host,
		NextProtos: []string{"h2", "http/1.1"},
	}, utls.HelloChrome_Auto)

	if err = tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return tlsConn, nil
}

func (t *chromeTransport) dropConn(addr string, cc *http2.ClientConn) {
	if cc == nil {
		return
	}

	t.mu.Lock()
	if t.h2Conns != nil && t.h2Conns[addr] == cc {
		delete(t.h2Conns, addr)
	}
	t.mu.Unlock()

	if err := cc.Close(); err != nil {
		return
	}
}
