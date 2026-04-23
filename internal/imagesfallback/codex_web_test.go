package imagesfallback

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
