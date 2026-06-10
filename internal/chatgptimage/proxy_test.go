package chatgptimage

import "testing"

func TestProxyTransportsAcceptExplicitDirectSettings(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"direct", "none"} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			transport, errHTTP := newHTTPTransport(raw)
			if errHTTP != nil {
				t.Fatalf("newHTTPTransport(%q) returned error: %v", raw, errHTTP)
			}
			if transport == nil {
				t.Fatalf("newHTTPTransport(%q) returned nil transport", raw)
			}
			if transport.Proxy != nil {
				t.Fatalf("newHTTPTransport(%q) should disable proxy function", raw)
			}

			if _, errDialer := newTunnelDialContext(raw); errDialer != nil {
				t.Fatalf("newTunnelDialContext(%q) returned error: %v", raw, errDialer)
			}
		})
	}
}
