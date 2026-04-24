package chatgptimage

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestParseSSENoAsyncDoesNotPoll(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatalf("unexpected polling request: %s", req.URL.String())
				return nil, nil
			}),
		},
	}

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-1","message":{"id":"tool-1","author":{"role":"tool"},"status":"finished_successfully","content":{"content_type":"text","parts":["still working"]}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	_, err := client.parseSSE(context.Background(), strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-1",
		SubmittedMessageID: "user-1",
	})
	if err == nil {
		t.Fatal("expected parseSSE to fail without images")
	}
	if !strings.Contains(err.Error(), "no images generated") {
		t.Fatalf("expected no-images error, got %v", err)
	}
}

func TestFetchConversationImagesRestrictsToSubmittedBranch(t *testing.T) {
	conversationJSON := `{
		"mapping": {
			"old-user": {
				"message": {
					"id": "old-user",
					"author": {"role": "user"},
					"status": "finished_successfully",
					"content": {"content_type": "text", "parts": ["old prompt"]}
				},
				"children": ["old-tool"]
			},
			"old-tool": {
				"message": {
					"id": "old-tool",
					"author": {"role": "tool"},
					"status": "finished_successfully",
					"content": {
						"content_type": "multimodal_text",
						"parts": [
							{
								"content_type": "image_asset_pointer",
								"asset_pointer": "sediment://file-old",
								"metadata": {"dalle": {"gen_id": "gen-old", "prompt": "old prompt"}}
							}
						]
					}
				}
			},
			"new-user": {
				"message": {
					"id": "new-user",
					"author": {"role": "user"},
					"status": "finished_successfully",
					"content": {"content_type": "text", "parts": ["new prompt"]}
				},
				"children": ["new-tool"]
			},
			"new-tool": {
				"message": {
					"id": "new-tool",
					"author": {"role": "tool"},
					"status": "finished_successfully",
					"content": {
						"content_type": "multimodal_text",
						"parts": [
							{
								"content_type": "image_asset_pointer",
								"asset_pointer": "sediment://file-new",
								"metadata": {"dalle": {"gen_id": "gen-new", "prompt": "new prompt"}}
							}
						]
					}
				}
			}
		}
	}`

	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-1"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(conversationJSON)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-new/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/new.png"}`)),
					}, nil
				case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/attachment/file-old/download"):
					t.Fatalf("old branch attachment should not be requested: %s", req.URL.String())
					return nil, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
	}

	images, err := client.fetchConversationImages(context.Background(), "conv-1", "new-user")
	if err != nil {
		t.Fatalf("fetchConversationImages returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected exactly one image, got %d", len(images))
	}
	if images[0].FileID != "file-new" {
		t.Fatalf("expected file-new, got %s", images[0].FileID)
	}
	if images[0].GenID != "gen-new" {
		t.Fatalf("expected gen-new, got %s", images[0].GenID)
	}
	if images[0].ParentMsgID != "new-tool" {
		t.Fatalf("expected parent message new-tool, got %s", images[0].ParentMsgID)
	}
}
