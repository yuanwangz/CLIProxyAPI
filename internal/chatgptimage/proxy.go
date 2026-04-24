package chatgptimage

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const defaultConnectTimeout = 30 * time.Second

var (
	noDeadline   = time.Time{}
	aLongTimeAgo = time.Unix(1, 0)
)

func newHTTPTransport(raw string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return transport, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "http", "https":
		transport.Proxy = http.ProxyURL(parsed)
		return transport, nil
	case "socks5", "socks5h":
		transport.Proxy = nil
		dialer, errDialer := newSOCKSContextDialer(parsed)
		if errDialer != nil {
			return nil, errDialer
		}
		transport.DialContext = dialer.DialContext
		return transport, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
}

func newTunnelDialContext(raw string) (func(context.Context, string, string) (net.Conn, error), error) {
	direct := (&net.Dialer{Timeout: defaultConnectTimeout}).DialContext
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return direct, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "http", "https":
		return func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialHTTPProxyTunnel(ctx, direct, parsed, network, address)
		}, nil
	case "socks5", "socks5h":
		dialer, errDialer := newSOCKSContextDialer(parsed)
		if errDialer != nil {
			return nil, errDialer
		}
		return dialer.DialContext, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
}

func newSOCKSContextDialer(parsed *url.URL) (xproxy.ContextDialer, error) {
	dialer, err := xproxy.FromURL(parsed, &net.Dialer{Timeout: defaultConnectTimeout})
	if err != nil {
		return nil, fmt.Errorf("build socks proxy dialer: %w", err)
	}
	contextDialer, ok := dialer.(xproxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("proxy dialer does not support context")
	}
	return contextDialer, nil
}

func dialHTTPProxyTunnel(
	ctx context.Context,
	direct func(context.Context, string, string) (net.Conn, error),
	proxyURL *url.URL,
	network string,
	address string,
) (net.Conn, error) {
	proxyAddr := proxyURL.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		port := "80"
		if strings.EqualFold(proxyURL.Scheme, "https") {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), port)
	}

	conn, err := direct(ctx, network, proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}

	if strings.EqualFold(proxyURL.Scheme, "https") {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: proxyURL.Hostname(),
			NextProtos: []string{"http/1.1"},
		})
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("https proxy handshake: %w", err)
		}
		conn = tlsConn
	}

	resetDeadline := bindConnToContext(ctx, conn)
	defer resetDeadline()

	connectReq := buildConnectRequest(proxyURL, address)
	if _, err = io.WriteString(conn, connectReq); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write proxy connect request: %w", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read proxy connect response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = conn.Close()
		return nil, fmt.Errorf("proxy connect failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func bindConnToContext(ctx context.Context, conn net.Conn) func() {
	if deadline, ok := ctx.Deadline(); ok && !deadline.IsZero() {
		_ = conn.SetDeadline(deadline)
	}

	done := ctx.Done()
	if done == nil {
		return func() {
			_ = conn.SetDeadline(noDeadline)
		}
	}

	stop := make(chan struct{})
	go func() {
		select {
		case <-done:
			_ = conn.SetDeadline(aLongTimeAgo)
		case <-stop:
		}
	}()

	return func() {
		close(stop)
		_ = conn.SetDeadline(noDeadline)
	}
}

func buildConnectRequest(proxyURL *url.URL, address string) string {
	headers := []string{
		fmt.Sprintf("CONNECT %s HTTP/1.1", address),
		fmt.Sprintf("Host: %s", address),
		"Proxy-Connection: Keep-Alive",
	}

	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		headers = append(headers, "Proxy-Authorization: Basic "+token)
	}

	return strings.Join(headers, "\r\n") + "\r\n\r\n"
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		return c.Conn.Read(p)
	}
	if c.reader.Buffered() == 0 {
		c.reader = nil
		return c.Conn.Read(p)
	}
	return c.reader.Read(p)
}
