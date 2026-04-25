package chatgptimage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type errorAfterReader struct {
	data []byte
	err  error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func TestParseSSEWithoutAsyncRecoversFromConversation(t *testing.T) {
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
						Body: io.NopCloser(strings.NewReader(`{
							"mapping": {
								"user-1": {
									"message": {
										"id": "user-1",
										"author": {"role": "user"},
										"status": "finished_successfully",
										"content": {"content_type": "text", "parts": ["prompt"]}
									},
									"children": ["tool-1"]
								},
								"tool-1": {
									"message": {
										"id": "tool-1",
										"author": {"role": "tool"},
										"status": "finished_successfully",
										"content": {
											"content_type": "multimodal_text",
											"parts": [{
												"content_type": "image_asset_pointer",
												"asset_pointer": "sediment://file-1",
												"metadata": {"dalle": {"gen_id": "gen-1", "prompt": "prompt"}}
											}]
										}
									}
								}
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-1/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/1.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		pollInterval: time.Millisecond,
		pollMaxWait:  time.Second,
	}

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-1","message":{"id":"tool-1","author":{"role":"tool"},"status":"finished_successfully","content":{"content_type":"text","parts":["still working"]}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	images, err := client.parseSSE(context.Background(), strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-1",
		SubmittedMessageID: "user-1",
	})
	if err != nil {
		t.Fatalf("parseSSE returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one recovered image, got %d", len(images))
	}
	if images[0].FileID != "file-1" {
		t.Fatalf("expected recovered file-1, got %s", images[0].FileID)
	}
}

func TestParseSSEReadErrorRecoversByPollingConversation(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-err"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{
							"mapping": {
								"user-err": {
									"message": {
										"id": "user-err",
										"author": {"role": "user"},
										"status": "finished_successfully",
										"content": {"content_type": "text", "parts": ["prompt"]}
									},
									"children": ["tool-err"]
								},
								"tool-err": {
									"message": {
										"id": "tool-err",
										"author": {"role": "tool"},
										"status": "finished_successfully",
										"content": {
											"content_type": "multimodal_text",
											"parts": [{
												"content_type": "image_asset_pointer",
												"asset_pointer": "sediment://file-err",
												"metadata": {"dalle": {"gen_id": "gen-err", "prompt": "prompt"}}
											}]
										}
									}
								}
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-err/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/err.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		pollInterval: time.Millisecond,
		pollMaxWait:  time.Second,
	}

	reader := &errorAfterReader{
		data: []byte(`data: {"conversation_id":"conv-err","message":{"id":"tool-err","author":{"role":"tool"},"status":"in_progress","content":{"content_type":"text","parts":["working"]}}}` + "\n"),
		err:  errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer"),
	}

	images, err := client.parseSSE(context.Background(), reader, conversationRequestContext{
		ConversationID:     "conv-err",
		SubmittedMessageID: "user-err",
	})
	if err != nil {
		t.Fatalf("parseSSE returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one recovered image, got %d", len(images))
	}
	if images[0].FileID != "file-err" {
		t.Fatalf("expected recovered file-err, got %s", images[0].FileID)
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

func TestGenerateImagePrefersFConversation(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/sentinel/chat-requirements"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"token":"sentinel","proofofwork":{"required":false}}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-f/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/f.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected api request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		streamClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/f/conversation") {
					t.Fatalf("unexpected stream request: %s %s", req.Method, req.URL.String())
				}
				stream := strings.Join([]string{
					`data: {"conversation_id":"conv-f","message":{"id":"tool-f","author":{"role":"tool"},"status":"finished_successfully","content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"sediment://file-f","metadata":{"dalle":{"gen_id":"gen-f","prompt":"prompt"}}}]}}}`,
					"",
					`data: [DONE]`,
					"",
				}, "\n")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(stream)),
				}, nil
			}),
		},
	}

	images, err := client.GenerateImage(context.Background(), "draw a cat", "gpt-5.4-mini", "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("GenerateImage len = %d, want 1", len(images))
	}
	if images[0].FileID != "file-f" {
		t.Fatalf("expected file-f, got %s", images[0].FileID)
	}
}

func TestGenerateImageFallsBackToConversationWhenFConversationFails(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/sentinel/chat-requirements"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"token":"sentinel","proofofwork":{"required":false}}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-c/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/c.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected api request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		streamClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/f/conversation"):
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
					}, nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/conversation"):
					stream := strings.Join([]string{
						`data: {"conversation_id":"conv-c","message":{"id":"tool-c","author":{"role":"tool"},"status":"finished_successfully","content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"sediment://file-c","metadata":{"dalle":{"gen_id":"gen-c","prompt":"prompt"}}}]}}}`,
						"",
						`data: [DONE]`,
						"",
					}, "\n")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(stream)),
					}, nil
				default:
					t.Fatalf("unexpected stream request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
	}

	images, err := client.GenerateImage(context.Background(), "draw a cat", "gpt-5.4-mini", "1024x1024", "", "")
	if err != nil {
		t.Fatalf("GenerateImage returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("GenerateImage len = %d, want 1", len(images))
	}
	if images[0].FileID != "file-c" {
		t.Fatalf("expected file-c, got %s", images[0].FileID)
	}
}

func TestShouldFallbackFromFConversation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "request error falls back", err: errors.New("f conversation request: dial tcp timeout"), want: true},
		{name: "5xx response falls back", err: &statusError{statusCode: http.StatusInternalServerError, message: "f conversation request returned 500: boom"}, want: true},
		{name: "sse read error does not fall back", err: errors.New("sse read error: unexpected EOF"), want: false},
		{name: "bad request does not fall back", err: &statusError{statusCode: http.StatusBadRequest, message: "f conversation request returned 400: nope"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFallbackFromFConversation(tt.err); got != tt.want {
				t.Fatalf("shouldFallbackFromFConversation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
