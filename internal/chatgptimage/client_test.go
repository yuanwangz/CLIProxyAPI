package chatgptimage

import (
	"context"
	"encoding/json"
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

func TestParseSSEWithoutAsyncPollsUntilDelayedConversationImageAppears(t *testing.T) {
	var conversationFetches int
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-delay"):
					conversationFetches++
					if conversationFetches < 4 {
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body: io.NopCloser(strings.NewReader(`{
								"mapping": {
									"user-delay": {
										"message": {
											"id": "user-delay",
											"author": {"role": "user"},
											"status": "finished_successfully",
											"content": {"content_type": "text", "parts": ["prompt"]}
										},
										"children": ["tool-delay"]
									},
									"tool-delay": {
										"message": {
											"id": "tool-delay",
											"author": {"role": "tool"},
											"status": "in_progress",
											"content": {"content_type": "text", "parts": ["still working"]}
										}
									}
								}
							}`)),
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{
							"mapping": {
								"user-delay": {
									"message": {
										"id": "user-delay",
										"author": {"role": "user"},
										"status": "finished_successfully",
										"content": {"content_type": "text", "parts": ["prompt"]}
									},
									"children": ["tool-delay"]
								},
								"tool-delay": {
									"message": {
										"id": "tool-delay",
										"author": {"role": "tool"},
										"status": "finished_successfully",
										"content": {
											"content_type": "multimodal_text",
											"parts": [{
												"content_type": "image_asset_pointer",
												"asset_pointer": "sediment://file-delay",
												"metadata": {"dalle": {"gen_id": "gen-delay", "prompt": "prompt"}}
											}]
										}
									}
								}
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-delay/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/delay.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		pollInterval: 5 * time.Millisecond,
		pollMaxWait:  100 * time.Millisecond,
	}

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-delay","message":{"id":"tool-delay","author":{"role":"tool"},"status":"in_progress","content":{"content_type":"text","parts":["still working"]}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	images, err := client.parseSSE(context.Background(), strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-delay",
		SubmittedMessageID: "user-delay",
	})
	if err != nil {
		t.Fatalf("parseSSE returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one recovered image, got %d", len(images))
	}
	if images[0].FileID != "file-delay" {
		t.Fatalf("expected recovered file-delay, got %s", images[0].FileID)
	}
	if conversationFetches < 4 {
		t.Fatalf("expected multiple delayed conversation fetches, got %d", conversationFetches)
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

func TestParseSSEAsyncPlaceholderPollsConversationWithoutWaitingForDone(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-async"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{
							"mapping": {
								"user-async": {
									"message": {
										"id": "user-async",
										"author": {"role": "user"},
										"status": "finished_successfully",
										"content": {"content_type": "text", "parts": ["prompt"]}
									},
									"children": ["tool-pending"]
								},
								"tool-pending": {
									"message": {
										"id": "tool-pending",
										"author": {"role": "tool"},
										"status": "finished_successfully",
										"content": {"content_type": "text", "parts": ["working"]},
										"metadata": {
											"image_gen_async": true,
											"trigger_async_ux": true,
											"image_gen_task_id": "task-1"
										}
									},
									"children": ["tool-final"]
								},
								"tool-final": {
									"message": {
										"id": "tool-final",
										"author": {"role": "tool"},
										"status": "finished_successfully",
										"content": {
											"content_type": "multimodal_text",
											"parts": [{
												"content_type": "image_asset_pointer",
												"asset_pointer": "sediment://file-async",
												"metadata": {"dalle": {"gen_id": "gen-async", "prompt": "prompt"}}
											}]
										}
									}
								}
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-async/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/async.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
		pollInterval:        time.Millisecond,
		pollMaxWait:         time.Second,
		pollRateLimitBudget: time.Second,
	}

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-async","message":{"id":"tool-pending","author":{"role":"tool"},"status":"finished_successfully","content":{"content_type":"text","parts":["working"]},"metadata":{"image_gen_async":true,"trigger_async_ux":true,"image_gen_task_id":"task-1"}}}`,
		"",
		`data: {"type":"message_stream_complete","conversation_id":"conv-async"}`,
		"",
	}, "\n")

	images, err := client.parseSSE(context.Background(), strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-async",
		SubmittedMessageID: "user-async",
	})
	if err != nil {
		t.Fatalf("parseSSE returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one recovered image, got %d", len(images))
	}
	if images[0].FileID != "file-async" {
		t.Fatalf("expected recovered file-async, got %s", images[0].FileID)
	}
}

func TestParseSSEReturnsTerminalAssistantTextWithoutPolling(t *testing.T) {
	client := &ChatGPTClient{
		accessToken: "token",
		oaiDeviceID: "device",
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				t.Fatalf("unexpected conversation poll: %s %s", req.Method, req.URL.String())
				return nil, nil
			}),
		},
		pollInterval: time.Millisecond,
		pollMaxWait:  time.Second,
	}

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-refusal","message":{"id":"assistant-refusal","author":{"role":"assistant"},"status":"finished_successfully","content":{"content_type":"text","parts":["I can't help create that image."]}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	_, err := client.parseSSE(context.Background(), strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-refusal",
		SubmittedMessageID: "user-refusal",
	})
	if err == nil {
		t.Fatal("expected terminal assistant text error, got nil")
	}
	if statusCode(err) != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 status, got %d (%v)", statusCode(err), err)
	}
	if !strings.Contains(err.Error(), "I can't help create that image.") {
		t.Fatalf("unexpected error message: %v", err)
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

func TestFetchConversationImagesFallsBackToFullConversationScan(t *testing.T) {
	conversationJSON := `{
		"mapping": {
			"user-1": {
				"message": {
					"id": "user-1",
					"author": {"role": "user"},
					"status": "finished_successfully",
					"content": {"content_type": "text", "parts": ["prompt"]}
				},
				"children": ["tool-progress"]
			},
			"tool-progress": {
				"message": {
					"id": "tool-progress",
					"author": {"role": "tool"},
					"status": "in_progress",
					"content": {"content_type": "text", "parts": ["working"]}
				}
			},
			"detached-tool": {
				"message": {
					"id": "detached-tool",
					"author": {"role": "tool"},
					"status": "finished_successfully",
					"content": {
						"content_type": "multimodal_text",
						"parts": [
							{
								"content_type": "image_asset_pointer",
								"asset_pointer": "sediment://file-detached",
								"metadata": {"dalle": {"gen_id": "gen-detached", "prompt": "prompt"}}
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
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-fallback"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(conversationJSON)),
					}, nil
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/attachment/file-detached/download"):
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"download_url":"https://files.example/detached.png"}`)),
					}, nil
				default:
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
					return nil, nil
				}
			}),
		},
	}

	images, err := client.fetchConversationImages(context.Background(), "conv-fallback", "user-1")
	if err != nil {
		t.Fatalf("fetchConversationImages returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected exactly one image, got %d", len(images))
	}
	if images[0].FileID != "file-detached" {
		t.Fatalf("expected file-detached, got %s", images[0].FileID)
	}
}

func TestParseSSEReturnsPollErrorWhenRecoveryContextIsCanceled(t *testing.T) {
	client := &ChatGPTClient{
		accessToken:         "token",
		oaiDeviceID:         "device",
		apiClient:           &http.Client{},
		pollInterval:        time.Millisecond,
		pollMaxWait:         time.Second,
		pollRateLimitBudget: time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stream := strings.Join([]string{
		`data: {"conversation_id":"conv-canceled","message":{"id":"tool-canceled","author":{"role":"tool"},"status":"in_progress","content":{"content_type":"text","parts":["still working"]}}}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	_, err := client.parseSSE(ctx, strings.NewReader(stream), conversationRequestContext{
		ConversationID:     "conv-canceled",
		SubmittedMessageID: "user-canceled",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestPollForImagesBacksOffAfterRateLimitAndEventuallySucceeds(t *testing.T) {
	var conversationFetches int
	client := &ChatGPTClient{
		accessToken:         "token",
		oaiDeviceID:         "device",
		pollInterval:        time.Millisecond,
		pollMaxWait:         200 * time.Millisecond,
		pollRateLimitBudget: time.Second,
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/conversation/conv-rate-limit"):
					conversationFetches++
					if conversationFetches < 3 {
						header := make(http.Header)
						header.Set("Retry-After", "0")
						return &http.Response{
							StatusCode: http.StatusTooManyRequests,
							Header:     header,
							Body:       io.NopCloser(strings.NewReader(`{"detail":"Too many requests"}`)),
						}, nil
					}
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
	}

	images, err := client.pollForImages(context.Background(), "conv-rate-limit", "user-1")
	if err != nil {
		t.Fatalf("pollForImages returned error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected one recovered image, got %d", len(images))
	}
	if conversationFetches != 3 {
		t.Fatalf("expected 3 conversation fetches, got %d", conversationFetches)
	}
}

func TestPollForImagesStopsAfterRateLimitBudget(t *testing.T) {
	client := &ChatGPTClient{
		accessToken:         "token",
		oaiDeviceID:         "device",
		pollInterval:        time.Millisecond,
		pollMaxWait:         time.Second,
		pollRateLimitBudget: 20 * time.Millisecond,
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/conversation/conv-budget") {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				}
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"detail":"Too many requests"}`)),
				}, nil
			}),
		},
	}

	_, err := client.pollForImages(context.Background(), "conv-budget", "user-1")
	if err == nil {
		t.Fatal("expected rate limit error, got nil")
	}
	if statusCode(err) != http.StatusTooManyRequests {
		t.Fatalf("expected 429 status, got %d (%v)", statusCode(err), err)
	}
}

func TestPollForImagesReturnsTerminalAssistantTextResponse(t *testing.T) {
	client := &ChatGPTClient{
		accessToken:         "token",
		oaiDeviceID:         "device",
		pollInterval:        time.Millisecond,
		pollMaxWait:         time.Second,
		pollRateLimitBudget: time.Second,
		apiClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/conversation/conv-refusal") {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{
						"current_node": "assistant-1",
						"mapping": {
							"user-1": {
								"message": {
									"id": "user-1",
									"author": {"role": "user"},
									"status": "finished_successfully",
									"content": {"content_type": "text", "parts": ["prompt"]}
								},
								"children": ["assistant-1"]
							},
							"assistant-1": {
								"message": {
									"id": "assistant-1",
									"author": {"role": "assistant"},
									"status": "finished_successfully",
									"recipient": "all",
									"content": {"content_type": "text", "parts": ["I can’t help create sexualized livestream imagery."]}
								}
							}
						}
					}`)),
				}, nil
			}),
		},
	}

	_, err := client.pollForImages(context.Background(), "conv-refusal", "user-1")
	if err == nil {
		t.Fatal("expected terminal assistant text error, got nil")
	}
	if statusCode(err) != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 status, got %d (%v)", statusCode(err), err)
	}
	if !strings.Contains(err.Error(), "sexualized livestream imagery") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGenerateImagePrefersFConversation(t *testing.T) {
	var requestBody map[string]any
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
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err = json.Unmarshal(body, &requestBody); err != nil {
					t.Fatalf("decode request body: %v", err)
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
	if got := stringValue(requestBody["client_prepare_state"]); got != "success" {
		t.Fatalf("client_prepare_state = %q, want success", got)
	}
	gotEncodings, ok := requestBody["supported_encodings"].([]any)
	if !ok || len(gotEncodings) != 1 || stringValue(gotEncodings[0]) != "v1" {
		t.Fatalf("supported_encodings = %#v, want [\"v1\"]", requestBody["supported_encodings"])
	}
}

func TestGenerateImageFallsBackToConversationWhenFConversationFails(t *testing.T) {
	requestBodies := make(map[string]map[string]any)
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
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				decoded := make(map[string]any)
				if err = json.Unmarshal(body, &decoded); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				switch {
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/f/conversation"):
					requestBodies["/f/conversation"] = decoded
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"boom"}`)),
					}, nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/conversation"):
					requestBodies["/conversation"] = decoded
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
	for _, path := range []string{"/f/conversation", "/conversation"} {
		body := requestBodies[path]
		if got := stringValue(body["client_prepare_state"]); got != "success" {
			t.Fatalf("%s client_prepare_state = %q, want success", path, got)
		}
		gotEncodings, ok := body["supported_encodings"].([]any)
		if !ok || len(gotEncodings) != 1 || stringValue(gotEncodings[0]) != "v1" {
			t.Fatalf("%s supported_encodings = %#v, want [\"v1\"]", path, body["supported_encodings"])
		}
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

func TestBuildConversationBodyUsesWebLikeGenerationDefaults(t *testing.T) {
	client := &ChatGPTClient{}
	body := client.buildConversationBody("draw a cat", "auto", "", "", nil)

	if got := stringValue(body["model"]); got != "auto" {
		t.Fatalf("model = %q, want auto", got)
	}
	if hints, ok := body["system_hints"].([]any); !ok || len(hints) != 0 {
		t.Fatalf("system_hints = %#v, want empty array", body["system_hints"])
	}
	messageList, ok := body["messages"].([]any)
	if !ok || len(messageList) != 1 {
		t.Fatalf("messages = %#v, want one message", body["messages"])
	}
	message, ok := messageList[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want map", messageList[0])
	}
	metadata, ok := message["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want map", message["metadata"])
	}
	if repos, ok := metadata["selected_github_repos"].([]any); !ok || len(repos) != 0 {
		t.Fatalf("selected_github_repos = %#v, want empty array", metadata["selected_github_repos"])
	}
	if selectedAll, ok := metadata["selected_all_github_repos"].(bool); !ok || selectedAll {
		t.Fatalf("selected_all_github_repos = %#v, want false", metadata["selected_all_github_repos"])
	}
	if _, ok := message["create_time"].(float64); !ok {
		t.Fatalf("create_time = %#v, want float64", message["create_time"])
	}
	clientInfo, ok := body["client_contextual_info"].(map[string]any)
	if !ok {
		t.Fatalf("client_contextual_info = %#v, want map", body["client_contextual_info"])
	}
	if got := stringValue(clientInfo["app_name"]); got != "chatgpt.com" {
		t.Fatalf("app_name = %q, want chatgpt.com", got)
	}
}

func TestConversationPollSnapshotPopulateCapturesTextPreview(t *testing.T) {
	longText := strings.Repeat("policy ", 40) + "\n\nPlease change the prompt."
	rawText, err := json.Marshal(longText)
	if err != nil {
		t.Fatalf("marshal longText: %v", err)
	}
	snapshot := conversationPollSnapshot{
		CurrentNode: "assistant-1",
	}

	snapshot.Populate(map[string]conversationNode{
		"assistant-1": {
			Message: &sseMessage{
				Status:    "finished_successfully",
				Recipient: "all",
				Author: struct {
					Role string `json:"role"`
				}{
					Role: "assistant",
				},
				Content: struct {
					ContentType string            `json:"content_type"`
					Parts       []json.RawMessage `json:"parts"`
				}{
					ContentType: "text",
					Parts: []json.RawMessage{
						rawText,
					},
				},
			},
		},
	})

	if snapshot.CurrentRole != "assistant" {
		t.Fatalf("CurrentRole = %q, want assistant", snapshot.CurrentRole)
	}
	if snapshot.CurrentContentType != "text" {
		t.Fatalf("CurrentContentType = %q, want text", snapshot.CurrentContentType)
	}
	if snapshot.CurrentTextPreview == "" {
		t.Fatal("CurrentTextPreview is empty")
	}
	if strings.Contains(snapshot.CurrentTextPreview, "\n") {
		t.Fatalf("CurrentTextPreview contains newline: %q", snapshot.CurrentTextPreview)
	}
	if len([]rune(snapshot.CurrentTextPreview)) > 200 {
		t.Fatalf("CurrentTextPreview length = %d, want <= 200", len([]rune(snapshot.CurrentTextPreview)))
	}
	if !strings.HasSuffix(snapshot.CurrentTextPreview, "...") {
		t.Fatalf("CurrentTextPreview = %q, want truncated suffix", snapshot.CurrentTextPreview)
	}
	if !snapshot.IsTerminalAssistantTextResponse() {
		t.Fatal("expected terminal assistant text response")
	}
}
