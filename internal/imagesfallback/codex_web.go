package imagesfallback

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	defaultChatGPTBaseURL       = "https://chatgpt.com"
	defaultBrowserUserAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	defaultAcceptLanguage       = "en-US,en;q=0.9"
	defaultAcceptEncoding       = "gzip, deflate, br"
	conversationAcceptLanguage  = "zh-CN,zh;q=0.9,en;q=0.8"
	defaultSecCHUA              = `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`
	defaultSecCHUAMobile        = "?0"
	defaultSecCHUAPlatform      = `"Windows"`
	defaultTimezone             = "America/Los_Angeles"
	defaultTimezoneOffsetMinute = -480
	defaultClientBuildNumber    = "5955942"
	defaultClientVersion        = "prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad"
	proofAttemptLimit           = 500000
	conversationPollInterval    = 2 * time.Second
	webRequestRetryAttempts     = 3
	webRequestRetryBackoff      = 2 * time.Second
)

var (
	homepageScriptPattern = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	homepageBuildPattern  = regexp.MustCompile(`<html[^>]*data-build="([^"]+)"`)
	filePointerPattern    = regexp.MustCompile(`(?:file-service://|sediment://)([A-Za-z0-9_-]+)`)
	processStart          = time.Now()
)

type webSession struct {
	client         webHTTPClient
	auth           *coreauth.Auth
	baseURL        string
	deviceID       string
	sessionID      string
	userAgent      string
	acceptLanguage string
	secChUA        string
	secChUAMobile  string
	secChUAPlat    string
	scriptSources  []string
	dataBuild      string
}

type uploadedImage struct {
	fileID   string
	data     []byte
	fileName string
	mimeType string
	width    int
	height   int
}

func (s *Service) execute(ctx context.Context, auth *coreauth.Auth, req Request) (*Result, error) {
	accessToken := strings.TrimSpace(AccessToken(auth))
	if accessToken == "" {
		return nil, newStatusError(http.StatusUnauthorized, "codex oauth access token is missing")
	}

	session, err := s.newWebSession(ctx, auth)
	if err != nil {
		return nil, err
	}
	if err = session.bootstrap(ctx); err != nil {
		return nil, err
	}

	switch req.Operation {
	case OperationGenerate:
		return session.generate(ctx, accessToken, req)
	case OperationEdit:
		return session.edit(ctx, accessToken, req)
	default:
		return nil, newStatusError(http.StatusBadRequest, fmt.Sprintf("unsupported image fallback operation %q", req.Operation))
	}
}

func (s *Service) newWebSession(ctx context.Context, auth *coreauth.Auth) (*webSession, error) {
	client, err := newProxyAwareClient(ctx, s.cfg, auth)
	if err != nil {
		return nil, err
	}

	baseURL := defaultChatGPTBaseURL
	return &webSession{
		client:         client,
		auth:           auth,
		baseURL:        baseURL,
		deviceID:       firstNonEmpty(authHeaderValue(auth, "oai-device-id"), metaValue(auth, "oai-device-id"), metaValue(auth, "oai_device_id"), uuid.NewString()),
		sessionID:      firstNonEmpty(authHeaderValue(auth, "oai-session-id"), metaValue(auth, "oai-session-id"), metaValue(auth, "oai_session_id")),
		userAgent:      firstNonEmpty(authHeaderValue(auth, "User-Agent"), metaValue(auth, "user-agent"), metaValue(auth, "user_agent"), defaultBrowserUserAgent),
		acceptLanguage: firstNonEmpty(authHeaderValue(auth, "Accept-Language"), defaultAcceptLanguage),
		secChUA:        firstNonEmpty(authHeaderValue(auth, "Sec-CH-UA"), metaValue(auth, "sec-ch-ua"), defaultSecCHUA),
		secChUAMobile:  firstNonEmpty(authHeaderValue(auth, "Sec-CH-UA-Mobile"), metaValue(auth, "sec-ch-ua-mobile"), defaultSecCHUAMobile),
		secChUAPlat:    firstNonEmpty(authHeaderValue(auth, "Sec-CH-UA-Platform"), metaValue(auth, "sec-ch-ua-platform"), defaultSecCHUAPlatform),
	}, nil
}

func (w *webSession) bootstrap(ctx context.Context) error {
	resp, err := w.doRequestWithRetry(ctx, "bootstrap chatgpt web session", func(ctx context.Context) (*http.Request, error) {
		req, errBuild := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/", nil)
		if errBuild != nil {
			return nil, fmt.Errorf("build bootstrap request: %w", errBuild)
		}
		w.applyBaseHeaders(req)
		return req, nil
	})
	if err != nil {
		return fmt.Errorf("bootstrap chatgpt web session: %w", err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseStatusError("bootstrap chatgpt web session", resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read bootstrap response: %w", err)
	}

	w.captureDeviceID(resp)
	w.scriptSources, w.dataBuild = parseHomepageMetadata(string(body))
	return nil
}

func (w *webSession) generate(ctx context.Context, accessToken string, req Request) (*Result, error) {
	chatToken, proofToken, err := w.chatRequirements(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	upstreamModel := ResolveWebModel(w.auth, req.RequestedModel)
	resp, err := w.sendConversation(ctx, accessToken, chatToken, proofToken, map[string]any{
		"action": "next",
		"messages": []any{
			map[string]any{
				"id":     uuid.NewString(),
				"author": map[string]any{"role": "user"},
				"content": map[string]any{
					"content_type": "text",
					"parts":        []any{strings.TrimSpace(req.Prompt)},
				},
				"metadata": map[string]any{
					"attachments": []any{},
				},
			},
		},
		"parent_message_id":                    uuid.NewString(),
		"model":                                upstreamModel,
		"history_and_training_disabled":        false,
		"timezone_offset_min":                  defaultTimezoneOffsetMinute,
		"timezone":                             defaultTimezone,
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"conversation_origin":                  nil,
		"force_paragen":                        false,
		"force_paragen_model_slug":             "",
		"force_rate_limit":                     false,
		"force_use_sse":                        true,
		"paragen_cot_summary_display_override": "allow",
		"paragen_stream_type_override":         nil,
		"reset_rate_limits":                    false,
		"suggestions":                          []any{},
		"supported_encodings":                  []any{},
		"system_hints":                         []any{"picture_v2"},
		"variant_purpose":                      "comparison_implicit",
		"websocket_request_id":                 uuid.NewString(),
		"client_contextual_info":               defaultClientContextualInfo(),
	})
	if err != nil {
		return nil, err
	}
	defer closeBody(resp.Body)

	parsed, err := parseConversationSSE(resp.Body)
	if err != nil {
		return nil, err
	}

	fileIDs := parsed.fileIDs
	if parsed.conversationID != "" && len(fileIDs) == 0 {
		fileIDs, err = w.pollConversationImageIDs(ctx, accessToken, parsed.conversationID)
		if err != nil {
			return nil, err
		}
	}
	if len(fileIDs) == 0 {
		if parsed.text != "" {
			return nil, newStatusError(http.StatusBadGateway, parsed.text)
		}
		return nil, newStatusError(http.StatusBadGateway, "no image returned from chatgpt web fallback")
	}

	data, mimeType, err := w.downloadGeneratedImage(ctx, accessToken, parsed.conversationID, fileIDs[0])
	if err != nil {
		return nil, err
	}
	return &Result{
		CreatedAt:    time.Now().Unix(),
		Size:         strings.TrimSpace(req.Size),
		Quality:      strings.TrimSpace(req.Quality),
		Background:   strings.TrimSpace(req.Background),
		OutputFormat: firstNonEmpty(strings.TrimSpace(req.OutputFormat), outputFormatFromMIME(mimeType)),
		Images: []GeneratedImage{
			{
				Data:          data,
				MIMEType:      mimeType,
				RevisedPrompt: strings.TrimSpace(req.Prompt),
			},
		},
	}, nil
}

func (w *webSession) edit(ctx context.Context, accessToken string, req Request) (*Result, error) {
	if len(req.Images) == 0 {
		return nil, newStatusError(http.StatusBadRequest, "image is required")
	}

	normalizedImages, err := w.resolveInputImages(ctx, req.Images)
	if err != nil {
		return nil, err
	}
	uploaded := make([]uploadedImage, 0, len(normalizedImages))
	for index, image := range normalizedImages {
		if len(image.Data) == 0 {
			return nil, newStatusError(http.StatusBadRequest, "image is required")
		}
		fileID, errUpload := w.uploadImage(ctx, accessToken, image)
		if errUpload != nil {
			return nil, errUpload
		}
		width, height := imageDimensions(image.Data)
		fileName := image.FileName
		if strings.TrimSpace(fileName) == "" {
			fileName = defaultImageFileName(index, image.MIMEType)
		}
		uploaded = append(uploaded, uploadedImage{
			fileID:   fileID,
			data:     image.Data,
			fileName: fileName,
			mimeType: firstNonEmpty(image.MIMEType, "image/png"),
			width:    width,
			height:   height,
		})
	}

	chatToken, proofToken, err := w.chatRequirements(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	imageParts := make([]any, 0, len(uploaded))
	attachments := make([]any, 0, len(uploaded))
	inputIDs := make(map[string]struct{}, len(uploaded))
	for _, image := range uploaded {
		inputIDs[canonicalFileID(image.fileID)] = struct{}{}
		imageParts = append(imageParts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "sediment://" + image.fileID,
			"size_bytes":    len(image.data),
			"width":         image.width,
			"height":        image.height,
		})
		attachments = append(attachments, map[string]any{
			"id":           image.fileID,
			"size":         len(image.data),
			"name":         image.fileName,
			"mime_type":    image.mimeType,
			"width":        image.width,
			"height":       image.height,
			"source":       "local",
			"is_big_paste": false,
		})
	}
	imageParts = append(imageParts, strings.TrimSpace(req.Prompt))

	upstreamModel := ResolveWebModel(w.auth, req.RequestedModel)
	resp, err := w.sendConversation(ctx, accessToken, chatToken, proofToken, map[string]any{
		"action": "next",
		"messages": []any{
			map[string]any{
				"id":     uuid.NewString(),
				"author": map[string]any{"role": "user"},
				"content": map[string]any{
					"content_type": "multimodal_text",
					"parts":        imageParts,
				},
				"metadata": map[string]any{
					"attachments": attachments,
				},
			},
		},
		"parent_message_id":                    uuid.NewString(),
		"model":                                upstreamModel,
		"history_and_training_disabled":        false,
		"timezone_offset_min":                  defaultTimezoneOffsetMinute,
		"timezone":                             defaultTimezone,
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"force_paragen":                        false,
		"force_paragen_model_slug":             "",
		"force_rate_limit":                     false,
		"force_use_sse":                        true,
		"paragen_cot_summary_display_override": "allow",
		"reset_rate_limits":                    false,
		"suggestions":                          []any{},
		"supported_encodings":                  []any{},
		"system_hints":                         []any{"picture_v2"},
		"variant_purpose":                      "comparison_implicit",
		"websocket_request_id":                 uuid.NewString(),
		"client_contextual_info":               defaultClientContextualInfo(),
	})
	if err != nil {
		return nil, err
	}
	defer closeBody(resp.Body)

	parsed, err := parseConversationSSE(resp.Body)
	if err != nil {
		return nil, err
	}

	fileIDs := filterOutputFileIDs(parsed.fileIDs, inputIDs)
	if parsed.conversationID != "" && len(fileIDs) == 0 {
		fileIDs, err = w.pollConversationImageIDs(ctx, accessToken, parsed.conversationID)
		if err != nil {
			return nil, err
		}
		fileIDs = filterOutputFileIDs(fileIDs, inputIDs)
	}
	if len(fileIDs) == 0 {
		if parsed.text != "" {
			return nil, newStatusError(http.StatusBadGateway, parsed.text)
		}
		return nil, newStatusError(http.StatusBadGateway, "no image returned from chatgpt web edit fallback")
	}

	data, mimeType, err := w.downloadGeneratedImage(ctx, accessToken, parsed.conversationID, fileIDs[0])
	if err != nil {
		return nil, err
	}
	return &Result{
		CreatedAt:    time.Now().Unix(),
		Size:         strings.TrimSpace(req.Size),
		Quality:      strings.TrimSpace(req.Quality),
		Background:   strings.TrimSpace(req.Background),
		OutputFormat: firstNonEmpty(strings.TrimSpace(req.OutputFormat), outputFormatFromMIME(mimeType)),
		Images: []GeneratedImage{
			{
				Data:          data,
				MIMEType:      mimeType,
				RevisedPrompt: strings.TrimSpace(req.Prompt),
			},
		},
	}, nil
}

func (w *webSession) chatRequirements(ctx context.Context, accessToken string) (string, string, error) {
	payload := map[string]any{
		"p": buildRequirementsToken(buildProofConfig(w.userAgent, w.scriptSources, w.dataBuild)),
	}
	resp, err := w.postJSON(ctx, accessToken, "/backend-api/sentinel/chat-requirements", payload, func(req *http.Request) {
		req.Header.Set("Accept", "*/*")
		req.Header.Del("oai-language")
		req.Header.Del("oai-client-build-number")
		req.Header.Del("oai-client-version")
		req.Header.Del("Chatgpt-Account-Id")
	})
	if err != nil {
		return "", "", err
	}
	defer closeBody(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", responseStatusError("fetch chat requirements", resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read chat requirements response: %w", err)
	}

	token := strings.TrimSpace(gjson.GetBytes(body, "token").String())
	if token == "" {
		return "", "", newStatusError(http.StatusBadGateway, "chat requirements token missing from response")
	}

	if !gjson.GetBytes(body, "proofofwork.required").Bool() {
		return token, "", nil
	}

	seed := strings.TrimSpace(gjson.GetBytes(body, "proofofwork.seed").String())
	difficulty := strings.TrimSpace(gjson.GetBytes(body, "proofofwork.difficulty").String())
	if seed == "" || difficulty == "" {
		return "", "", newStatusError(http.StatusBadGateway, "chat requirements proof payload is incomplete")
	}
	proofToken := buildProofAnswerToken(seed, difficulty, buildProofConfig(w.userAgent, w.scriptSources, w.dataBuild))
	return token, proofToken, nil
}

func (w *webSession) uploadImage(ctx context.Context, accessToken string, image InputImage) (string, error) {
	fileName := firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(0, image.MIMEType))
	payload := map[string]any{
		"file_name":           fileName,
		"file_size":           len(image.Data),
		"use_case":            "multimodal",
		"timezone_offset_min": defaultTimezoneOffsetMinute,
		"reset_rate_limits":   false,
	}
	resp, err := w.postJSON(ctx, accessToken, "/backend-api/files", payload, nil)
	if err != nil {
		return "", err
	}
	defer closeBody(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", responseStatusError("initialize image upload", resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read image upload init response: %w", err)
	}

	uploadURL := strings.TrimSpace(gjson.GetBytes(body, "upload_url").String())
	fileID := strings.TrimSpace(gjson.GetBytes(body, "file_id").String())
	if uploadURL == "" || fileID == "" {
		return "", newStatusError(http.StatusBadGateway, "image upload init returned no upload_url or file_id")
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(image.Data))
	if err != nil {
		return "", fmt.Errorf("build image upload request: %w", err)
	}
	putReq.Header.Set("Content-Type", firstNonEmpty(image.MIMEType, "image/png"))
	putReq.Header.Set("x-ms-blob-type", "BlockBlob")
	putReq.Header.Set("x-ms-version", "2020-04-08")
	putResp, err := w.client.Do(putReq)
	if err != nil {
		return "", fmt.Errorf("upload image content: %w", err)
	}
	defer closeBody(putResp.Body)
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		return "", responseStatusError("upload image content", putResp)
	}

	processResp, err := w.postJSON(ctx, accessToken, "/backend-api/files/process_upload_stream", map[string]any{
		"file_id":             fileID,
		"use_case":            "multimodal",
		"index_for_retrieval": false,
		"file_name":           fileName,
	}, nil)
	if err != nil {
		return "", err
	}
	defer closeBody(processResp.Body)
	if processResp.StatusCode < 200 || processResp.StatusCode >= 300 {
		return "", responseStatusError("finalize image upload", processResp)
	}
	return fileID, nil
}

func (w *webSession) sendConversation(ctx context.Context, accessToken, chatToken, proofToken string, payload map[string]any) (*http.Response, error) {
	resp, err := w.postJSON(ctx, accessToken, "/backend-api/conversation", payload, func(req *http.Request) {
		req.Header.Set("oai-language", "zh-CN")
		req.Header.Set("oai-client-build-number", defaultClientBuildNumber)
		req.Header.Set("oai-client-version", defaultClientVersion)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Language", conversationAcceptLanguage)
		req.Header.Set("openai-sentinel-chat-requirements-token", chatToken)
		if strings.TrimSpace(proofToken) != "" {
			req.Header.Set("openai-sentinel-proof-token", proofToken)
		}
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer closeBody(resp.Body)
		return nil, responseStatusError("send image conversation", resp)
	}
	return resp, nil
}

func (w *webSession) pollConversationImageIDs(ctx context.Context, accessToken, conversationID string) ([]string, error) {
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/backend-api/conversation/"+url.PathEscape(conversationID), nil)
		if err != nil {
			return nil, fmt.Errorf("build conversation poll request: %w", err)
		}
		w.applyBearerHeaders(req, accessToken)
		req.Header.Set("Accept", "*/*")

		resp, err := w.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll conversation for generated images: %w", err)
		}
		body, errRead := io.ReadAll(resp.Body)
		closeBody(resp.Body)
		if errRead != nil {
			return nil, fmt.Errorf("read conversation poll response: %w", errRead)
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, newStatusError(resp.StatusCode, string(body))
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			fileIDs := extractConversationFileIDs(body)
			if len(fileIDs) > 0 {
				return fileIDs, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(conversationPollInterval):
		}
	}
}

func (w *webSession) downloadGeneratedImage(ctx context.Context, accessToken, conversationID, fileID string) ([]byte, string, error) {
	downloadURL, err := w.fetchDownloadURL(ctx, accessToken, conversationID, fileID)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build image download request: %w", err)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download generated image: %w", err)
	}
	defer closeBody(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", responseStatusError("download generated image", resp)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read generated image body: %w", err)
	}
	if len(data) == 0 {
		return nil, "", newStatusError(http.StatusBadGateway, "generated image download returned empty body")
	}

	mimeType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return data, mimeType, nil
}

func (w *webSession) fetchDownloadURL(ctx context.Context, accessToken, conversationID, fileID string) (string, error) {
	rawID := canonicalFileID(fileID)
	endpoint := "/backend-api/files/" + url.PathEscape(rawID) + "/download"
	if strings.HasPrefix(strings.TrimSpace(fileID), "sed:") {
		endpoint = "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(rawID) + "/download"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build download-url request: %w", err)
	}
	w.applyBearerHeaders(req, accessToken)

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch download url: %w", err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", responseStatusError("fetch generated image download url", resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read download-url response: %w", err)
	}
	downloadURL := strings.TrimSpace(gjson.GetBytes(body, "download_url").String())
	if downloadURL == "" {
		return "", newStatusError(http.StatusBadGateway, "download_url missing from generated image response")
	}
	return downloadURL, nil
}

func (w *webSession) resolveInputImages(ctx context.Context, images []InputImage) ([]InputImage, error) {
	out := make([]InputImage, 0, len(images))
	for index, image := range images {
		switch {
		case len(image.Data) > 0:
			image.MIMEType = firstNonEmpty(strings.TrimSpace(image.MIMEType), http.DetectContentType(image.Data))
			image.FileName = firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(index, image.MIMEType))
			out = append(out, image)
		case strings.HasPrefix(strings.TrimSpace(image.URL), "data:"):
			data, mimeType, err := decodeDataURL(image.URL)
			if err != nil {
				return nil, err
			}
			out = append(out, InputImage{
				Data:     data,
				MIMEType: firstNonEmpty(image.MIMEType, mimeType),
				FileName: firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(index, mimeType)),
			})
		case strings.TrimSpace(image.URL) != "":
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(image.URL), nil)
			if err != nil {
				return nil, fmt.Errorf("build input image download request: %w", err)
			}
			req.Header.Set("User-Agent", w.userAgent)
			resp, err := w.client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("download input image: %w", err)
			}
			data, errRead := io.ReadAll(resp.Body)
			closeBody(resp.Body)
			if errRead != nil {
				return nil, fmt.Errorf("read input image body: %w", errRead)
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, newStatusError(resp.StatusCode, string(data))
			}
			mimeType := firstNonEmpty(strings.TrimSpace(resp.Header.Get("Content-Type")), http.DetectContentType(data))
			fileName := strings.TrimSpace(image.FileName)
			if fileName == "" {
				fileName = fileNameFromURL(strings.TrimSpace(image.URL))
			}
			if fileName == "" {
				fileName = defaultImageFileName(index, mimeType)
			}
			out = append(out, InputImage{
				Data:     data,
				MIMEType: mimeType,
				FileName: fileName,
			})
		default:
			return nil, newStatusError(http.StatusBadRequest, "image is required")
		}
	}
	return out, nil
}

func (w *webSession) postJSON(ctx context.Context, accessToken, endpoint string, payload any, mutate func(*http.Request)) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request body for %s: %w", endpoint, err)
	}
	resp, err := w.doRequestWithRetry(ctx, "post "+endpoint, func(ctx context.Context) (*http.Request, error) {
		req, errBuild := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+endpoint, bytes.NewReader(body))
		if errBuild != nil {
			return nil, fmt.Errorf("build request for %s: %w", endpoint, errBuild)
		}
		w.applyBearerHeaders(req, accessToken)
		req.Header.Set("Content-Type", "application/json")
		if mutate != nil {
			mutate(req)
		}
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("post %s: %w", endpoint, err)
	}
	return resp, nil
}

func (w *webSession) doRequestWithRetry(ctx context.Context, action string, build func(context.Context) (*http.Request, error)) (*http.Response, error) {
	for attempt := 1; attempt <= webRequestRetryAttempts; attempt++ {
		req, err := build(ctx)
		if err != nil {
			return nil, err
		}

		resp, err := w.client.Do(req)
		if !shouldRetryWebRequest(resp, err) || attempt == webRequestRetryAttempts {
			return resp, err
		}

		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
			closeBody(resp.Body)
		}

		log.WithFields(log.Fields{
			"action":  action,
			"attempt": attempt,
			"status":  statusCode,
			"error":   strings.TrimSpace(errorStringValue(err)),
		}).Warn("images fallback: retrying chatgpt web request")

		if errWait := sleepWithContext(ctx, time.Duration(attempt)*webRequestRetryBackoff); errWait != nil {
			if err != nil {
				return nil, err
			}
			return resp, errWait
		}
	}

	return nil, nil
}

func shouldRetryWebRequest(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}

	switch resp.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *webSession) applyBaseHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("User-Agent", w.userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", defaultAcceptEncoding)
	req.Header.Set("Accept-Language", w.acceptLanguage)
	req.Header.Set("Origin", w.baseURL)
	req.Header.Set("Referer", w.baseURL+"/")
	req.Header.Set("Sec-CH-UA", w.secChUA)
	req.Header.Set("Sec-CH-UA-Mobile", w.secChUAMobile)
	req.Header.Set("Sec-CH-UA-Platform", w.secChUAPlat)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("oai-device-id", w.deviceID)
	if strings.TrimSpace(w.sessionID) != "" {
		req.Header.Set("oai-session-id", w.sessionID)
	}
	util.ApplyCustomHeadersFromAttrs(req, w.auth.Attributes)
	if req.Header.Get("oai-device-id") != "" {
		w.deviceID = strings.TrimSpace(req.Header.Get("oai-device-id"))
	}
	if req.Header.Get("oai-session-id") != "" {
		w.sessionID = strings.TrimSpace(req.Header.Get("oai-session-id"))
	}
	applyBrowserHeaderOrder(req)
}

func (w *webSession) applyAuthorizedHeaders(req *http.Request, accessToken string) {
	w.applyBearerHeaders(req, accessToken)
}

func (w *webSession) captureDeviceID(resp *http.Response) {
	if resp == nil {
		return
	}
	for _, cookie := range resp.Cookies() {
		if cookie == nil {
			continue
		}
		if cookie.Name == "oai-did" && strings.TrimSpace(cookie.Value) != "" {
			w.deviceID = strings.TrimSpace(cookie.Value)
			return
		}
	}
	if resp.Request != nil && resp.Request.URL != nil && w.client != nil {
		for _, cookie := range w.client.Cookies(resp.Request.URL) {
			if cookie == nil {
				continue
			}
			if cookie.Name == "oai-did" && strings.TrimSpace(cookie.Value) != "" {
				w.deviceID = strings.TrimSpace(cookie.Value)
				return
			}
		}
	}
}

type conversationParseResult struct {
	conversationID string
	fileIDs        []string
	text           string
}

func parseConversationSSE(body io.Reader) (conversationParseResult, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	result := conversationParseResult{}
	seenFileIDs := make(map[string]struct{})

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		for _, match := range filePointerPattern.FindAllStringSubmatch(payload, -1) {
			if len(match) < 2 {
				continue
			}
			fileID := strings.TrimSpace(match[1])
			if fileID == "" {
				continue
			}
			value := fileID
			if strings.Contains(payload, "sediment://"+fileID) {
				value = "sed:" + fileID
			}
			if _, ok := seenFileIDs[value]; ok {
				continue
			}
			seenFileIDs[value] = struct{}{}
			result.fileIDs = append(result.fileIDs, value)
		}
		if !json.Valid([]byte(payload)) {
			continue
		}

		if conversationID := strings.TrimSpace(gjson.Get(payload, "conversation_id").String()); conversationID != "" {
			result.conversationID = conversationID
		}
		if conversationID := strings.TrimSpace(gjson.Get(payload, "v.conversation_id").String()); conversationID != "" {
			result.conversationID = conversationID
		}
		if contentType := strings.TrimSpace(gjson.Get(payload, "message.content.content_type").String()); contentType == "text" {
			if part := strings.TrimSpace(gjson.Get(payload, "message.content.parts.0").String()); part != "" {
				if result.text == "" {
					result.text = part
				} else {
					result.text += part
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read conversation event stream: %w", err)
	}
	return result, nil
}

func extractConversationFileIDs(payload []byte) []string {
	if !json.Valid(payload) {
		return nil
	}
	mapping := gjson.GetBytes(payload, "mapping")
	if !mapping.Exists() || !mapping.IsObject() {
		return nil
	}

	out := make([]string, 0)
	seen := make(map[string]struct{})
	for key, node := range mapping.Map() {
		_ = key
		if strings.TrimSpace(node.Get("message.author.role").String()) != "tool" {
			continue
		}
		if strings.TrimSpace(node.Get("message.metadata.async_task_type").String()) != "image_gen" {
			continue
		}
		if strings.TrimSpace(node.Get("message.content.content_type").String()) != "multimodal_text" {
			continue
		}
		for _, part := range node.Get("message.content.parts").Array() {
			pointer := strings.TrimSpace(part.Get("asset_pointer").String())
			switch {
			case strings.HasPrefix(pointer, "file-service://"):
				fileID := strings.TrimPrefix(pointer, "file-service://")
				if _, ok := seen[fileID]; !ok && fileID != "" {
					seen[fileID] = struct{}{}
					out = append(out, fileID)
				}
			case strings.HasPrefix(pointer, "sediment://"):
				fileID := "sed:" + strings.TrimPrefix(pointer, "sediment://")
				if _, ok := seen[fileID]; !ok && fileID != "sed:" {
					seen[fileID] = struct{}{}
					out = append(out, fileID)
				}
			}
		}
	}
	return out
}

func filterOutputFileIDs(fileIDs []string, inputIDs map[string]struct{}) []string {
	if len(inputIDs) == 0 {
		return fileIDs
	}
	out := make([]string, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		if _, ok := inputIDs[canonicalFileID(fileID)]; ok {
			continue
		}
		out = append(out, fileID)
	}
	return out
}

func canonicalFileID(fileID string) string {
	fileID = strings.TrimSpace(fileID)
	if strings.HasPrefix(fileID, "sed:") {
		return strings.TrimPrefix(fileID, "sed:")
	}
	return fileID
}

func parseHomepageMetadata(html string) ([]string, string) {
	scriptMatches := homepageScriptPattern.FindAllStringSubmatch(html, -1)
	scripts := make([]string, 0, len(scriptMatches))
	dpl := ""
	for _, match := range scriptMatches {
		if len(match) < 2 {
			continue
		}
		src := strings.TrimSpace(match[1])
		if src == "" {
			continue
		}
		scripts = append(scripts, src)
		if dpl == "" {
			if submatch := proofBuildPattern.FindString(src); submatch != "" {
				dpl = strings.TrimSpace(submatch)
			}
		}
	}
	if len(scripts) == 0 {
		scripts = append(scripts, defaultChatGPTBaseURL+"/backend-api/sentinel/sdk.js")
	}

	if dpl == "" {
		if match := homepageBuildPattern.FindStringSubmatch(html); len(match) >= 2 {
			dpl = strings.TrimSpace(match[1])
		}
	}
	return scripts, dpl
}

func buildProofConfig(userAgent string, scriptSources []string, dataBuild string) []any {
	nowMs := float64(time.Now().UnixMilli())
	perfMs := float64(time.Since(processStart).Milliseconds())

	return []any{
		choiceInt(proofScreenSizes),
		easternTimeString(),
		4294705152,
		0,
		firstNonEmpty(strings.TrimSpace(userAgent), defaultBrowserUserAgent),
		choiceString(scriptSources),
		strings.TrimSpace(dataBuild),
		"en-US",
		"en-US,es-US,en,es",
		0,
		choiceString(proofNavigatorKeys),
		choiceString(proofDocumentKeys),
		choiceString(proofWindowKeys),
		perfMs,
		uuid.NewString(),
		"",
		choiceInt(proofCoreChoices),
		nowMs - perfMs,
	}
}

func buildRequirementsToken(config []any) string {
	return "gAAAAAC" + solveProofToken(formatSeed(time.Now().UnixNano()), "0fffff", config)
}

func buildProofAnswerToken(seed string, difficulty string, config []any) string {
	return "gAAAAAB" + solveProofToken(seed, difficulty, config)
}

func solveProofToken(seed string, difficulty string, config []any) string {
	target, err := hex.DecodeString(strings.TrimSpace(difficulty))
	if err != nil || len(target) == 0 {
		return fallbackProofToken(seed)
	}

	prefix1, err := marshalProofPrefix(config[:3], false, true)
	if err != nil {
		return fallbackProofToken(seed)
	}
	prefix2, err := marshalProofPrefix(config[4:9], true, true)
	if err != nil {
		return fallbackProofToken(seed)
	}
	prefix3, err := marshalProofPrefix(config[10:], true, false)
	if err != nil {
		return fallbackProofToken(seed)
	}

	seedBytes := []byte(seed)
	for attempt := 0; attempt < proofAttemptLimit; attempt++ {
		left := strconv.Itoa(attempt)
		right := strconv.Itoa(attempt >> 1)

		var buffer bytes.Buffer
		buffer.Grow(len(prefix1) + len(left) + len(prefix2) + len(right) + len(prefix3))
		buffer.Write(prefix1)
		buffer.WriteString(left)
		buffer.Write(prefix2)
		buffer.WriteString(right)
		buffer.Write(prefix3)

		encoded := base64.StdEncoding.EncodeToString(buffer.Bytes())
		sum := sha3.Sum512(append(seedBytes, []byte(encoded)...))
		if bytes.Compare(sum[:len(target)], target) <= 0 {
			return encoded
		}
	}

	return fallbackProofToken(seed)
}

func marshalProofPrefix(values []any, leadingComma bool, trailingComma bool) ([]byte, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if len(raw) < 2 {
		return nil, fmt.Errorf("invalid proof config prefix")
	}
	start := 1
	end := len(raw) - 1
	var buffer bytes.Buffer
	if leadingComma {
		buffer.WriteByte(',')
		buffer.Write(raw[start:end])
	} else {
		buffer.Write(raw[:end])
	}
	if trailingComma {
		buffer.WriteByte(',')
	}
	return buffer.Bytes(), nil
}

func fallbackProofToken(seed string) string {
	return "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString([]byte(strconv.Quote(seed)))
}

func easternTimeString() string {
	loc := time.FixedZone("EST", -5*60*60)
	return time.Now().In(loc).Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

func formatSeed(value int64) string {
	seed := strconv.FormatFloat(float64(value%1_000_000_000)/1_000_000_000, 'f', 9, 64)
	seed = strings.TrimRight(seed, "0")
	seed = strings.TrimRight(seed, ".")
	if seed == "" {
		return "0"
	}
	return seed
}

func responseStatusError(action string, resp *http.Response) error {
	if resp == nil {
		return newStatusError(http.StatusBadGateway, action+" failed")
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	if json.Valid(body) {
		for _, path := range []string{"error.message", "message", "detail.error", "detail"} {
			if text := strings.TrimSpace(gjson.GetBytes(body, path).String()); text != "" {
				message = text
				break
			}
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	if strings.TrimSpace(action) != "" {
		message = fmt.Sprintf("%s failed with status %d: %s", action, resp.StatusCode, message)
	}
	return &statusError{
		statusCode: resp.StatusCode,
		message:    message,
	}
}

func errorStringValue(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func decodeDataURL(raw string) ([]byte, string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return nil, "", newStatusError(http.StatusBadRequest, "unsupported data url")
	}
	header, data, found := strings.Cut(raw, ",")
	if !found {
		return nil, "", newStatusError(http.StatusBadRequest, "invalid data url")
	}
	header = strings.TrimPrefix(header, "data:")
	mimeType := "image/png"
	if mediaType, _, err := mime.ParseMediaType(header); err == nil && strings.TrimSpace(mediaType) != "" {
		mimeType = mediaType
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, "", newStatusError(http.StatusBadRequest, "invalid base64 image data")
	}
	return decoded, mimeType, nil
}

func fileNameFromURL(raw string) string {
	if parsed, err := url.Parse(raw); err == nil {
		if base := path.Base(parsed.Path); base != "." && base != "/" {
			return base
		}
	}
	return ""
}

func defaultImageFileName(index int, mimeType string) string {
	exts, _ := mime.ExtensionsByType(strings.TrimSpace(mimeType))
	ext := ".png"
	if len(exts) > 0 && strings.TrimSpace(exts[0]) != "" {
		ext = exts[0]
	}
	return fmt.Sprintf("image-%d%s", index+1, ext)
}

func imageDimensions(data []byte) (int, int) {
	if len(data) >= 24 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		width := int(binaryBigEndian(data[16:20]))
		height := int(binaryBigEndian(data[20:24]))
		if width > 0 && height > 0 {
			return width, height
		}
	}
	if len(data) >= 4 && data[0] == 0xff && data[1] == 0xd8 {
		for i := 2; i+9 < len(data); {
			if data[i] != 0xff {
				break
			}
			marker := data[i+1]
			if marker == 0xc0 || marker == 0xc1 || marker == 0xc2 {
				height := int(binaryBigEndian(data[i+5 : i+7]))
				width := int(binaryBigEndian(data[i+7 : i+9]))
				if width > 0 && height > 0 {
					return width, height
				}
				break
			}
			if i+4 > len(data) {
				break
			}
			segmentLen := int(binaryBigEndian(data[i+2 : i+4]))
			if segmentLen <= 0 {
				break
			}
			i += 2 + segmentLen
		}
	}
	return 1024, 1024
}

func binaryBigEndian(data []byte) uint32 {
	var value uint32
	for _, b := range data {
		value = value<<8 | uint32(b)
	}
	return value
}

func outputFormatFromMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return "jpeg"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}

func closeBody(body io.Closer) {
	if body == nil {
		return
	}
	if err := body.Close(); err != nil {
		log.Errorf("images fallback: close response body error: %v", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func choiceString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[int(time.Now().UnixNano()%int64(len(values)))]
}

func choiceInt(values []int) int {
	if len(values) == 0 {
		return 0
	}
	return values[int(time.Now().UnixNano()%int64(len(values)))]
}

func defaultClientContextualInfo() map[string]any {
	now := time.Now().UnixNano()
	return map[string]any{
		"is_dark_mode":      false,
		"time_since_loaded": 50 + int(now%450),
		"page_height":       700 + int(now%300),
		"page_width":        1200 + int(now%600),
		"pixel_ratio":       1.2,
		"screen_height":     900 + int(now%300),
		"screen_width":      1400 + int(now%800),
	}
}

func authHeaderValue(auth *coreauth.Auth, headerName string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	for key, value := range auth.Attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		name := strings.TrimPrefix(key, "header:")
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(headerName)) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func metaValue(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if value, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func (w *webSession) applyBearerHeaders(req *http.Request, accessToken string) {
	w.applyBaseHeaders(req)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
}

func applyBrowserHeaderOrder(req *http.Request) {
	if req == nil {
		return
	}
	req.Header["Header-Order:"] = []string{
		"authorization",
		"accept",
		"accept-encoding",
		"accept-language",
		"content-type",
		"oai-device-id",
		"oai-session-id",
		"oai-language",
		"oai-client-build-number",
		"oai-client-version",
		"openai-sentinel-chat-requirements-token",
		"openai-sentinel-proof-token",
		"origin",
		"referer",
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"sec-fetch-dest",
		"sec-fetch-mode",
		"sec-fetch-site",
		"user-agent",
		"chatgpt-account-id",
	}
	req.Header["PHeader-Order:"] = []string{":method", ":authority", ":scheme", ":path"}
}
