package imagesfallback

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResponseStatusErrorIncludesActionAndDetail(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader(`{"detail":"Internal Server Error"}`)),
	}

	err := responseStatusError("send image conversation", resp)
	if err == nil {
		t.Fatalf("expected responseStatusError to return an error")
	}

	if got := err.Error(); got != "send image conversation failed with status 500: Internal Server Error" {
		t.Fatalf("responseStatusError() = %q", got)
	}
}

func TestShouldRetryWebRequest(t *testing.T) {
	if !shouldRetryWebRequest(nil, io.ErrUnexpectedEOF) {
		t.Fatalf("expected transport errors to retry")
	}

	resp := &http.Response{StatusCode: http.StatusInternalServerError}
	if !shouldRetryWebRequest(resp, nil) {
		t.Fatalf("expected 500 responses to retry")
	}

	resp = &http.Response{StatusCode: http.StatusBadRequest}
	if shouldRetryWebRequest(resp, nil) {
		t.Fatalf("expected 400 responses to skip retry")
	}
}

func TestNewWebSessionUsesWhitelistedFingerprintInputs(t *testing.T) {
	service := &Service{}
	auth := &coreauth.Auth{
		Attributes: map[string]string{
			"header:User-Agent":      "TestUA/1.0",
			"header:Accept-Language": "zh-CN,zh;q=0.9",
			"header:oai-device-id":   "device-from-header",
		},
		Metadata: map[string]any{
			"user-agent":         "MetaUA/2.0",
			"sec-ch-ua":          `"Custom Brand";v="1"`,
			"sec-ch-ua-mobile":   "?1",
			"sec-ch-ua-platform": `"Linux"`,
			"oai-device-id":      "device-from-meta",
		},
	}

	session, err := service.newWebSession(context.Background(), auth)
	if err != nil {
		t.Fatalf("newWebSession() error = %v", err)
	}

	if session.userAgent != "TestUA/1.0" {
		t.Fatalf("session.userAgent = %q", session.userAgent)
	}
	if session.acceptLanguage != defaultAcceptLanguage {
		t.Fatalf("session.acceptLanguage = %q, want %q", session.acceptLanguage, defaultAcceptLanguage)
	}
	if session.deviceID != "device-from-header" {
		t.Fatalf("session.deviceID = %q", session.deviceID)
	}
	if session.secChUA != `"Custom Brand";v="1"` {
		t.Fatalf("session.secChUA = %q", session.secChUA)
	}
	if session.secChUAMobile != "?1" {
		t.Fatalf("session.secChUAMobile = %q", session.secChUAMobile)
	}
	if session.secChUAPlat != `"Linux"` {
		t.Fatalf("session.secChUAPlat = %q", session.secChUAPlat)
	}
}

func TestDefaultClientContextualInfoMatchesChatGPTWebRanges(t *testing.T) {
	info := defaultClientContextualInfo()

	assertInRange := func(name string, value any, min int, max int) {
		t.Helper()
		got, ok := value.(int)
		if !ok {
			t.Fatalf("%s type = %T, want int", name, value)
		}
		if got < min || got > max {
			t.Fatalf("%s = %d, want [%d,%d]", name, got, min, max)
		}
	}

	assertInRange("time_since_loaded", info["time_since_loaded"], 50, 499)
	assertInRange("page_height", info["page_height"], 500, 999)
	assertInRange("page_width", info["page_width"], 1000, 1999)
	assertInRange("screen_height", info["screen_height"], 800, 1199)
	assertInRange("screen_width", info["screen_width"], 1200, 2199)

	if got, ok := info["pixel_ratio"].(float64); !ok || got != 1.2 {
		t.Fatalf("pixel_ratio = %#v, want 1.2", info["pixel_ratio"])
	}
}
