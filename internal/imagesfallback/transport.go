package imagesfallback

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

var chatGPTWebHosts = map[string]struct{}{
	"chatgpt.com":     {},
	"chat.openai.com": {},
	"openai.com":      {},
	"auth.openai.com": {},
}

type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
}

func newUTLSRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if strings.TrimSpace(proxyURL) != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("images fallback: failed to configure proxy dialer for %q: %v", proxyURL, errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(strings.TrimSpace(req.URL.Hostname()))
	if _, ok := chatGPTWebHosts[host]; !ok || req.URL.Scheme != "https" {
		return http.DefaultTransport.RoundTrip(req)
	}

	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(host, port)

	conn, err := t.getOrCreateConnection(host, addr)
	if err != nil {
		return nil, err
	}

	resp, err := conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[host]; ok && cached == conn {
			delete(t.connections, host)
		}
		t.mu.Unlock()
		return nil, err
	}
	return resp, nil
}

func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()
	if conn, ok := t.connections[host]; ok && conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return conn, nil
	}
	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if conn, ok := t.connections[host]; ok && conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return conn, nil
		}
	}
	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	delete(t.pending, host)
	cond.Broadcast()
	if err == nil {
		t.connections[host] = conn
	}
	t.mu.Unlock()

	return conn, err
}

func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	rawConn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{ServerName: host}
	tlsConn := tls.UClient(rawConn, tlsConfig, tls.HelloEdge_Auto)
	if err = tlsConn.Handshake(); err != nil {
		_ = rawConn.Close()
		return nil, err
	}

	tr := &http2.Transport{}
	conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return conn, nil
}

type fallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && strings.EqualFold(req.URL.Scheme, "https") {
		if _, ok := chatGPTWebHosts[strings.ToLower(strings.TrimSpace(req.URL.Hostname()))]; ok && f.utls != nil {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

func newChatGPTHTTPTransport(proxyURL string, fallback http.RoundTripper) http.RoundTripper {
	standard := fallback
	if standard == nil {
		standard = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		if strings.TrimSpace(proxyURL) != "" {
			if transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL); errBuild == nil && transport != nil {
				standard = transport
			} else if errBuild != nil {
				log.Errorf("images fallback: failed to build proxy transport for %q: %v", proxyURL, errBuild)
			}
		}
	}

	return &fallbackRoundTripper{
		utls:     newUTLSRoundTripper(proxyURL),
		fallback: standard,
	}
}
