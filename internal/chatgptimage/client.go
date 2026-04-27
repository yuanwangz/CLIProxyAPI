package chatgptimage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	apiBaseURL                  = "https://chatgpt.com/backend-api"
	defaultPollInterval         = 3 * time.Second
	defaultPollMaxWait          = 10 * time.Minute
	defaultPollRateLimitBudget  = 2 * time.Minute
	defaultPollRateLimitBackoff = 30 * time.Second
)

type Backend struct {
	cfg *sdkconfig.SDKConfig
}

func New(cfg *sdkconfig.SDKConfig) *Backend {
	return &Backend{cfg: cfg}
}

func (b *Backend) Execute(ctx context.Context, auth *coreauth.Auth, req Request) (*Result, error) {
	client, err := newChatGPTClient(auth, b.cfg)
	if err != nil {
		return nil, err
	}

	var generated []imageResult
	switch req.Operation {
	case OperationGenerate:
		generated, err = client.GenerateImage(ctx, req.Prompt, req.Model, req.Size, req.Quality, req.Background)
	case OperationEdit:
		generated, err = client.EditImage(ctx, req.Prompt, req.Model, req.Images, req.Mask)
	default:
		err = &statusError{statusCode: http.StatusBadRequest, message: fmt.Sprintf("unsupported image operation %q", req.Operation)}
	}
	if err != nil {
		return nil, err
	}
	if len(generated) == 0 {
		return nil, &statusError{statusCode: http.StatusBadGateway, message: "no image returned from chatgpt image backend"}
	}

	result := &Result{
		CreatedAt:    time.Now().Unix(),
		Size:         strings.TrimSpace(req.Size),
		Quality:      strings.TrimSpace(req.Quality),
		Background:   strings.TrimSpace(req.Background),
		OutputFormat: strings.TrimSpace(req.OutputFormat),
		Images:       make([]GeneratedImage, 0, len(generated)),
	}

	for _, image := range generated {
		data, mimeType, errDownload := client.DownloadImage(ctx, image.URL)
		if errDownload != nil {
			return nil, errDownload
		}
		result.Images = append(result.Images, GeneratedImage{
			Data:          data,
			MIMEType:      mimeType,
			RevisedPrompt: firstNonEmpty(strings.TrimSpace(image.RevisedPrompt), strings.TrimSpace(req.Prompt)),
		})
		if result.OutputFormat == "" {
			result.OutputFormat = outputFormatFromMIME(mimeType)
		}
	}

	if result.OutputFormat == "" {
		result.OutputFormat = "png"
	}
	return result, nil
}

type ChatGPTClient struct {
	accessToken         string
	cookies             string
	oaiDeviceID         string
	userAgent           string
	apiClient           *http.Client
	streamClient        *http.Client
	plainClient         *http.Client
	pollInterval        time.Duration
	pollMaxWait         time.Duration
	pollRateLimitBudget time.Duration
}

func newChatGPTClient(auth *coreauth.Auth, cfg *sdkconfig.SDKConfig) (*ChatGPTClient, error) {
	accessToken := metadataString(auth, "access_token")
	if accessToken == "" {
		return nil, &statusError{statusCode: http.StatusUnauthorized, message: "codex oauth access token is missing"}
	}

	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	apiTransport, err := newChromeTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create chatgpt chrome transport: %w", err)
	}
	streamTransport, err := newChromeTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create chatgpt stream transport: %w", err)
	}
	plainTransport, err := newHTTPTransport(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("create plain http transport: %w", err)
	}

	return &ChatGPTClient{
		accessToken:         accessToken,
		cookies:             firstNonEmpty(metadataString(auth, "cookies"), metadataString(auth, "cookie"), attributeHeader(auth, "Cookie")),
		oaiDeviceID:         firstNonEmpty(metadataString(auth, "oai-device-id"), metadataString(auth, "oai_device_id"), attributeHeader(auth, "oai-device-id"), uuid.NewString()),
		userAgent:           defaultUserAgent,
		apiClient:           &http.Client{Transport: apiTransport},
		streamClient:        &http.Client{Transport: streamTransport},
		plainClient:         &http.Client{Transport: plainTransport},
		pollInterval:        defaultPollInterval,
		pollMaxWait:         defaultPollMaxWait,
		pollRateLimitBudget: defaultPollRateLimitBudget,
	}, nil
}

func (c *ChatGPTClient) GenerateImage(ctx context.Context, prompt, model, size, quality, background string) ([]imageResult, error) {
	fullPrompt := strings.TrimSpace(prompt)
	if fullPrompt == "" {
		return nil, &statusError{statusCode: http.StatusBadRequest, message: "prompt is required"}
	}
	if size != "" && size != "auto" && size != "1024x1024" {
		fullPrompt = fmt.Sprintf("Generate an image with size %s. %s", size, fullPrompt)
	}
	if quality == "hd" || quality == "high" {
		fullPrompt = "Generate a high-quality, detailed image: " + fullPrompt
	}
	if background == "transparent" {
		fullPrompt += " The image must have a transparent background (PNG with alpha channel)."
	}

	body := c.buildConversationBody(fullPrompt, model, "", "", nil)
	fBody := cloneConversationBody(body)
	fBody["client_prepare_state"] = "success"
	fBody["supported_encodings"] = []string{"v1"}

	images, err := c.doFConversation(ctx, fBody)
	if err == nil {
		return images, nil
	}
	if !shouldFallbackFromFConversation(err) {
		return nil, err
	}
	return c.doConversation(ctx, body)
}

func (c *ChatGPTClient) EditImage(ctx context.Context, prompt, model string, images []InputImage, mask *InputImage) ([]imageResult, error) {
	if len(images) == 0 {
		return nil, &statusError{statusCode: http.StatusBadRequest, message: "image is required"}
	}

	resolvedImages, err := c.resolveInputImages(ctx, images)
	if err != nil {
		return nil, err
	}

	uploads := make([]*uploadedFile, 0, len(resolvedImages))
	for index, image := range resolvedImages {
		filename := firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(index, image.MIMEType))
		upload, errUpload := c.UploadFile(ctx, image.Data, filename, firstNonEmpty(image.MIMEType, detectMIME(image.Data)))
		if errUpload != nil {
			return nil, errUpload
		}
		uploads = append(uploads, upload)
	}

	var maskUpload *uploadedFile
	if mask != nil {
		resolvedMask, errMask := c.resolveInputImage(ctx, *mask, len(resolvedImages))
		if errMask != nil {
			return nil, errMask
		}
		maskUpload, errMask = c.UploadFile(ctx, resolvedMask.Data, firstNonEmpty(strings.TrimSpace(resolvedMask.FileName), "mask.png"), firstNonEmpty(resolvedMask.MIMEType, detectMIME(resolvedMask.Data)))
		if errMask != nil {
			return nil, errMask
		}
	}

	body := c.buildMultimodalBody(strings.TrimSpace(prompt), model, uploads, maskUpload)
	return c.doConversation(ctx, body)
}

func (c *ChatGPTClient) UploadFile(ctx context.Context, data []byte, filename, mimeType string) (*uploadedFile, error) {
	if len(data) == 0 {
		return nil, &statusError{statusCode: http.StatusBadRequest, message: "upload image is empty"}
	}
	if strings.TrimSpace(mimeType) == "" {
		mimeType = detectMIME(data)
	}

	preBody, _ := json.Marshal(map[string]any{
		"file_name": filename,
		"file_size": len(data),
		"use_case":  "multimodal",
		"mime_type": mimeType,
	})
	preReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/files", bytes.NewReader(preBody))
	c.setAPIHeaders(preReq)

	preResp, err := c.apiClient.Do(preReq)
	if err != nil {
		return nil, fmt.Errorf("pre-upload request: %w", err)
	}
	defer closeBody(preResp.Body)
	if preResp.StatusCode != http.StatusOK {
		return nil, responseStatusError("pre-upload", preResp)
	}

	var preResult struct {
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err = json.NewDecoder(preResp.Body).Decode(&preResult); err != nil {
		return nil, fmt.Errorf("decode pre-upload: %w", err)
	}
	if strings.TrimSpace(preResult.UploadURL) == "" || strings.TrimSpace(preResult.FileID) == "" {
		return nil, &statusError{statusCode: http.StatusBadGateway, message: "pre-upload returned empty upload_url or file_id"}
	}

	uploadReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, preResult.UploadURL, bytes.NewReader(data))
	uploadReq.Header.Set("x-ms-blob-type", "BlockBlob")
	uploadReq.Header.Set("Content-Type", mimeType)

	uploadResp, err := c.plainClient.Do(uploadReq)
	if err != nil {
		return nil, fmt.Errorf("blob upload request: %w", err)
	}
	defer closeBody(uploadResp.Body)
	if uploadResp.StatusCode != http.StatusCreated && uploadResp.StatusCode != http.StatusOK {
		return nil, responseStatusError("blob upload", uploadResp)
	}

	confirmReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/files/"+url.PathEscape(preResult.FileID)+"/uploaded", bytes.NewReader([]byte("{}")))
	c.setAPIHeaders(confirmReq)

	confirmResp, err := c.apiClient.Do(confirmReq)
	if err != nil {
		return nil, fmt.Errorf("confirm upload request: %w", err)
	}
	defer closeBody(confirmResp.Body)
	if confirmResp.StatusCode != http.StatusOK {
		return nil, responseStatusError("confirm upload", confirmResp)
	}

	width, height := imageDimensions(data)
	return &uploadedFile{
		FileID:    preResult.FileID,
		SizeBytes: len(data),
		Width:     width,
		Height:    height,
		MIMEType:  mimeType,
	}, nil
}

func (c *ChatGPTClient) DownloadImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, "", &statusError{statusCode: http.StatusBadGateway, message: "download url is empty"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build image download request: %w", err)
	}

	client := c.plainClient
	if isChatGPTHost(rawURL) {
		client = c.apiClient
		c.setAPIHeaders(req)
	} else {
		c.setBrowserHeaders(req)
	}

	resp, err := client.Do(req)
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
		return nil, "", &statusError{statusCode: http.StatusBadGateway, message: "generated image download returned empty body"}
	}

	mimeType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return data, mimeType, nil
}

func (c *ChatGPTClient) doConversation(ctx context.Context, body map[string]any) ([]imageResult, error) {
	return c.doConversationRequest(ctx, body, "/conversation", "conversation request")
}

func (c *ChatGPTClient) doFConversation(ctx context.Context, body map[string]any) ([]imageResult, error) {
	return c.doConversationRequest(ctx, body, "/f/conversation", "f conversation request")
}

func (c *ChatGPTClient) doConversationRequest(ctx context.Context, body map[string]any, requestPath, action string) ([]imageResult, error) {
	requestContext := extractConversationRequestContext(body)

	chatToken, proofToken, err := c.getSentinelTokens(ctx)
	if err != nil {
		return nil, fmt.Errorf("sentinel tokens: %w", err)
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal conversation body: %w", err)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+requestPath, bytes.NewReader(jsonBody))
	c.setAPIHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("openai-sentinel-chat-requirements-token", chatToken)
	if strings.TrimSpace(proofToken) != "" {
		req.Header.Set("openai-sentinel-proof-token", proofToken)
	}

	client := c.streamClient
	if client == nil {
		client = c.apiClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, responseStatusError(action, resp)
	}

	return c.parseSSE(ctx, resp.Body, requestContext)
}

func (c *ChatGPTClient) getSentinelTokens(ctx context.Context) (string, string, error) {
	reqBody, _ := json.Marshal(map[string]string{"p": generateRequirementsToken()})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiBaseURL+"/sentinel/chat-requirements", bytes.NewReader(reqBody))
	c.setAPIHeaders(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("chat-requirements request: %w", err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", responseStatusError("chat-requirements", resp)
	}

	var result struct {
		Token       string `json:"token"`
		ProofOfWork struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", "", fmt.Errorf("decode chat-requirements: %w", err)
	}
	if strings.TrimSpace(result.Token) == "" {
		return "", "", &statusError{statusCode: http.StatusBadGateway, message: "chat-requirements token is empty"}
	}
	if !result.ProofOfWork.Required {
		return result.Token, "", nil
	}

	proofToken, err := solvePoW(result.ProofOfWork.Seed, result.ProofOfWork.Difficulty)
	if err != nil {
		return "", "", fmt.Errorf("solve proof-of-work: %w", err)
	}
	return result.Token, proofToken, nil
}

func (c *ChatGPTClient) parseSSE(ctx context.Context, reader io.Reader, requestContext conversationRequestContext) ([]imageResult, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	var (
		conversationID string
		asyncMode      bool
		images         []imageResult
	)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		if !strings.HasPrefix(data, "{") {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &raw); err != nil {
			continue
		}
		if rawCID, ok := raw["conversation_id"]; ok {
			var cid string
			if json.Unmarshal(rawCID, &cid) == nil && cid != "" {
				conversationID = cid
			}
		}
		if rawType, ok := raw["type"]; ok {
			var eventType string
			if json.Unmarshal(rawType, &eventType) == nil &&
				eventType == "message_stream_complete" &&
				asyncMode &&
				conversationID != "" {
				log.WithField("conversation_id", conversationID).Debug("chatgpt image: async message stream completed, polling conversation")
				return c.pollForImages(ctx, conversationID, requestContext.SubmittedMessageID)
			}
		}
		if rawAS, ok := raw["async_status"]; ok {
			var status int
			if json.Unmarshal(rawAS, &status) == nil && status > 0 {
				asyncMode = true
			}
		}

		var event sseEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil || event.Message == nil {
			continue
		}

		msg := event.Message
		if requestContext.ParentMessageID != "" && msg.ID == requestContext.ParentMessageID {
			continue
		}
		if isAsyncImagePendingMessage(msg) {
			asyncMode = true
			if conversationID != "" {
				log.WithField("conversation_id", conversationID).Debug("chatgpt image: async image placeholder received")
			}
		}
		images = append(images, c.extractImages(ctx, msg, conversationID)...)
	}

	if conversationID == "" {
		conversationID = strings.TrimSpace(requestContext.ConversationID)
	}
	if len(images) > 0 {
		return images, nil
	}
	if err := scanner.Err(); err != nil {
		if conversationID != "" {
			log.WithError(err).WithField("conversation_id", conversationID).Warn("chatgpt image: sse stream failed, polling conversation")
			recovered, pollErr := c.pollForImages(ctx, conversationID, requestContext.SubmittedMessageID)
			if pollErr == nil && len(recovered) > 0 {
				return recovered, nil
			}
			if pollErr != nil {
				log.WithError(pollErr).WithField("conversation_id", conversationID).Warn("chatgpt image: failed to recover after sse read error")
			}
		}
		return nil, fmt.Errorf("sse read error: %w", err)
	}
	if asyncMode && conversationID != "" {
		return c.pollForImages(ctx, conversationID, requestContext.SubmittedMessageID)
	}
	if conversationID != "" {
		log.WithField("conversation_id", conversationID).Debug("chatgpt image: stream ended without inline images, polling conversation for delayed image results")
		recovered, pollErr := c.pollForImages(ctx, conversationID, requestContext.SubmittedMessageID)
		if pollErr == nil && len(recovered) > 0 {
			return recovered, nil
		}
		if pollErr != nil {
			log.WithError(pollErr).WithField("conversation_id", conversationID).Debug("chatgpt image: empty stream recovery did not produce images")
			return nil, pollErr
		}
	}
	return nil, &statusError{statusCode: http.StatusBadGateway, message: "no images generated"}
}

func (c *ChatGPTClient) extractImages(ctx context.Context, msg *sseMessage, conversationID string) []imageResult {
	if msg == nil {
		return nil
	}
	if msg.Author.Role == "user" || msg.Author.Role == "system" {
		return nil
	}
	if msg.Content.ContentType != "multimodal_text" || msg.Status != "finished_successfully" {
		return nil
	}

	var images []imageResult
	for _, rawPart := range msg.Content.Parts {
		var part sseImagePart
		if err := json.Unmarshal(rawPart, &part); err != nil {
			continue
		}
		if part.ContentType != "image_asset_pointer" || part.AssetPointer == "" {
			continue
		}

		fileID := extractFileID(part.AssetPointer)
		if fileID == "" {
			continue
		}

		var (
			downloadURL string
			err         error
		)
		if strings.HasPrefix(part.AssetPointer, "sediment://") {
			downloadURL, err = c.getAttachmentURL(ctx, fileID, conversationID)
		} else {
			downloadURL, err = c.getDownloadURL(ctx, fileID, conversationID)
		}
		if err != nil {
			log.WithError(err).WithField("file_id", fileID).Warn("chatgpt image: failed to fetch download url")
			continue
		}

		images = append(images, imageResult{
			URL:            downloadURL,
			FileID:         fileID,
			GenID:          part.Metadata.Dalle.GenID,
			ConversationID: conversationID,
			ParentMsgID:    msg.ID,
			RevisedPrompt:  part.Metadata.Dalle.Prompt,
		})
	}
	return images
}

func (c *ChatGPTClient) pollForImages(ctx context.Context, conversationID, rootMessageID string) ([]imageResult, error) {
	return c.pollForImagesWithWait(ctx, conversationID, rootMessageID, c.pollMaxWait)
}

func (c *ChatGPTClient) pollForImagesWithWait(ctx context.Context, conversationID, rootMessageID string, maxWait time.Duration) ([]imageResult, error) {
	if maxWait <= 0 {
		maxWait = c.pollMaxWait
	}
	deadline := time.Now().Add(maxWait)
	pollDelay := normalizedPollInterval(c.pollInterval)
	rateLimitBudget := c.pollRateLimitBudget
	if rateLimitBudget <= 0 {
		rateLimitBudget = defaultPollRateLimitBudget
	}
	var rateLimitedSince time.Time
	lastSnapshot := conversationPollSnapshot{}
	for time.Now().Before(deadline) {
		if err := waitForNextConversationPoll(ctx, deadline, pollDelay); err != nil {
			return nil, err
		}

		images, snapshot, err := c.fetchConversationImagesDetailed(ctx, conversationID, rootMessageID)
		if err != nil {
			if statusCode(err) == http.StatusTooManyRequests {
				if rateLimitedSince.IsZero() {
					rateLimitedSince = time.Now()
				}
				pollDelay = nextConversationPollDelay(err, pollDelay)
				if time.Since(rateLimitedSince) >= rateLimitBudget {
					return nil, &statusError{
						statusCode: http.StatusTooManyRequests,
						message:    "chatgpt image conversation polling rate limited for too long",
						retryAfter: pollDelay,
					}
				}
				log.WithError(err).WithFields(log.Fields{
					"conversation_id": conversationID,
					"next_delay":      pollDelay.String(),
				}).Warn("chatgpt image: conversation poll rate limited, backing off")
				continue
			}
			rateLimitedSince = time.Time{}
			pollDelay = normalizedPollInterval(c.pollInterval)
			log.WithError(err).WithField("conversation_id", conversationID).Warn("chatgpt image: poll conversation failed")
		} else if len(images) > 0 {
			return images, nil
		} else {
			rateLimitedSince = time.Time{}
			pollDelay = normalizedPollInterval(c.pollInterval)
			if snapshot.Signature() != "" && snapshot.Signature() != lastSnapshot.Signature() {
				log.WithFields(snapshot.LogFields(conversationID)).Info("chatgpt image: conversation state updated without image output")
				lastSnapshot = snapshot
			}
		}
	}
	return nil, &statusError{statusCode: http.StatusGatewayTimeout, message: "timed out waiting for async image generation"}
}

func (c *ChatGPTClient) fetchConversationImages(ctx context.Context, conversationID, rootMessageID string) ([]imageResult, error) {
	images, _, err := c.fetchConversationImagesDetailed(ctx, conversationID, rootMessageID)
	return images, err
}

func (c *ChatGPTClient) fetchConversationImagesDetailed(ctx context.Context, conversationID, rootMessageID string) ([]imageResult, conversationPollSnapshot, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/conversation/"+url.PathEscape(conversationID), nil)
	c.setAPIHeaders(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, conversationPollSnapshot{}, err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, conversationPollSnapshot{}, responseStatusError("fetch conversation", resp)
	}

	var conv struct {
		CurrentNode string                      `json:"current_node"`
		AsyncStatus int                         `json:"async_status"`
		Mapping     map[string]conversationNode `json:"mapping"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return nil, conversationPollSnapshot{}, fmt.Errorf("decode conversation: %w", err)
	}

	var (
		images []imageResult
		seen   map[string]struct{}
	)
	snapshot := conversationPollSnapshot{
		AsyncStatus: conv.AsyncStatus,
		CurrentNode: strings.TrimSpace(conv.CurrentNode),
		NodeCount:   len(conv.Mapping),
	}
	var visit func(string)
	visit = func(nodeID string) {
		if nodeID == "" {
			return
		}
		if _, ok := seen[nodeID]; ok {
			return
		}
		node, ok := conv.Mapping[nodeID]
		if !ok {
			return
		}
		seen[nodeID] = struct{}{}
		if node.Message != nil {
			images = append(images, c.extractImages(ctx, node.Message, conversationID)...)
		}
		for _, childID := range node.Children {
			visit(childID)
		}
	}

	if rootMessageID != "" {
		seen = make(map[string]struct{})
		visit(rootMessageID)
		if len(images) > 0 {
			return images, snapshot, nil
		}
		log.WithFields(log.Fields{
			"conversation_id": conversationID,
			"root_message_id": rootMessageID,
		}).Debug("chatgpt image: submitted branch did not yield images, scanning full conversation")
	}

	images = nil
	seen = make(map[string]struct{})
	for nodeID := range conv.Mapping {
		visit(nodeID)
	}
	snapshot.Populate(conv.Mapping)
	return images, snapshot, nil
}

func (c *ChatGPTClient) getAttachmentURL(ctx context.Context, fileID, conversationID string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBaseURL+"/conversation/"+url.PathEscape(conversationID)+"/attachment/"+url.PathEscape(fileID)+"/download", nil)
	c.setAPIHeaders(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", responseStatusError("fetch attachment download url", resp)
	}

	var result struct {
		DownloadURL string `json:"download_url"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.DownloadURL) == "" {
		return "", &statusError{statusCode: http.StatusBadGateway, message: fmt.Sprintf("empty download_url for attachment %s", fileID)}
	}
	return result.DownloadURL, nil
}

func (c *ChatGPTClient) getDownloadURL(ctx context.Context, fileID, conversationID string) (string, error) {
	downloadURL := apiBaseURL + "/files/download/" + url.PathEscape(fileID) + "?conversation_id=" + url.QueryEscape(conversationID) + "&inline=false"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	c.setAPIHeaders(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return "", err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", responseStatusError("fetch file download url", resp)
	}

	var result struct {
		DownloadURL string `json:"download_url"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if strings.TrimSpace(result.DownloadURL) == "" {
		return "", &statusError{statusCode: http.StatusBadGateway, message: fmt.Sprintf("empty download_url for file %s", fileID)}
	}
	return result.DownloadURL, nil
}

func (c *ChatGPTClient) buildConversationBody(prompt, model, conversationID, parentMsgID string, dalleOp map[string]any) map[string]any {
	msgID := uuid.NewString()
	if parentMsgID == "" {
		parentMsgID = "client-created-root"
	}
	model = firstNonEmpty(strings.TrimSpace(model), "gpt-5.4-mini")
	now := time.Now()
	timezoneOffsetMin, timezoneName := webTimezone(now)

	metadata := map[string]any{
		"serialization_metadata": map[string]any{
			"custom_symbol_offsets": []any{},
		},
		"selected_github_repos":     []any{},
		"selected_all_github_repos": false,
	}
	if dalleOp != nil {
		metadata["dalle"] = map[string]any{
			"from_client": map[string]any{
				"operation": dalleOp,
			},
		}
	}

	msg := map[string]any{
		"id":          msgID,
		"author":      map[string]any{"role": "user"},
		"create_time": float64(now.UnixMilli()) / 1000,
		"content": map[string]any{
			"content_type": "text",
			"parts":        []string{prompt},
		},
		"metadata": metadata,
	}

	body := map[string]any{
		"action":                               "next",
		"messages":                             []any{msg},
		"parent_message_id":                    parentMsgID,
		"model":                                model,
		"timezone_offset_min":                  timezoneOffsetMin,
		"timezone":                             timezoneName,
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []any{},
		"supports_buffering":                   true,
		"supported_encodings":                  []string{},
		"client_contextual_info":               defaultWebClientContext(),
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
	}
	if conversationID != "" {
		body["conversation_id"] = conversationID
	}
	return body
}

func (c *ChatGPTClient) buildMultimodalBody(prompt, model string, uploads []*uploadedFile, maskUpload *uploadedFile) map[string]any {
	msgID := uuid.NewString()
	model = firstNonEmpty(strings.TrimSpace(model), "gpt-5.4-mini")

	parts := []any{prompt}
	attachments := []any{}
	for index, up := range uploads {
		imgPart := map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + up.FileID,
			"size_bytes":    up.SizeBytes,
			"mime_type":     up.MIMEType,
		}
		if up.Width > 0 && up.Height > 0 {
			imgPart["width"] = up.Width
			imgPart["height"] = up.Height
		}
		parts = append(parts, imgPart)
		attachments = append(attachments, map[string]any{
			"id":       up.FileID,
			"name":     fmt.Sprintf("image_%d%s", index, extensionFromMIME(up.MIMEType)),
			"size":     up.SizeBytes,
			"mimeType": up.MIMEType,
			"width":    up.Width,
			"height":   up.Height,
		})
	}

	if maskUpload != nil {
		maskPart := map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + maskUpload.FileID,
			"size_bytes":    maskUpload.SizeBytes,
			"mime_type":     maskUpload.MIMEType,
		}
		if maskUpload.Width > 0 && maskUpload.Height > 0 {
			maskPart["width"] = maskUpload.Width
			maskPart["height"] = maskUpload.Height
		}
		parts = append(parts, maskPart)
		attachments = append(attachments, map[string]any{
			"id":       maskUpload.FileID,
			"name":     "mask" + extensionFromMIME(maskUpload.MIMEType),
			"size":     maskUpload.SizeBytes,
			"mimeType": maskUpload.MIMEType,
			"width":    maskUpload.Width,
			"height":   maskUpload.Height,
		})
	}

	msg := map[string]any{
		"id":     msgID,
		"author": map[string]any{"role": "user"},
		"content": map[string]any{
			"content_type": "multimodal_text",
			"parts":        parts,
		},
		"metadata": map[string]any{
			"attachments":  attachments,
			"system_hints": []string{"picture_v2"},
			"serialization_metadata": map[string]any{
				"custom_symbol_offsets": []any{},
			},
		},
	}

	return map[string]any{
		"action":                   "next",
		"messages":                 []any{msg},
		"parent_message_id":        "client-created-root",
		"model":                    model,
		"timezone_offset_min":      420,
		"timezone":                 "America/Los_Angeles",
		"conversation_mode":        map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             []string{"picture_v2"},
		"supports_buffering":       true,
		"supported_encodings":      []string{},
		"client_contextual_info": map[string]any{
			"is_dark_mode":      true,
			"time_since_loaded": 1000,
			"page_height":       717,
			"page_width":        1200,
			"pixel_ratio":       2,
			"screen_height":     878,
			"screen_width":      1352,
			"app_name":          "chatgpt.com",
		},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
	}
}

func (c *ChatGPTClient) setAPIHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OAI-Device-Id", c.oaiDeviceID)
	req.Header.Set("OAI-Language", "en-US")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Sec-CH-UA", `"Chromium";v="146", "Google Chrome";v="146", "Not?A_Brand";v="99"`)
	req.Header.Set("Sec-CH-UA-Mobile", "?0")
	req.Header.Set("Sec-CH-UA-Platform", `"macOS"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", c.userAgent)
	if c.cookies != "" {
		req.Header.Set("Cookie", c.cookies)
	}
}

func (c *ChatGPTClient) setBrowserHeaders(req *http.Request) {
	if req == nil {
		return
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", c.userAgent)
}

func (c *ChatGPTClient) resolveInputImages(ctx context.Context, images []InputImage) ([]InputImage, error) {
	out := make([]InputImage, 0, len(images))
	for index, image := range images {
		resolved, err := c.resolveInputImage(ctx, image, index)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

func (c *ChatGPTClient) resolveInputImage(ctx context.Context, image InputImage, index int) (InputImage, error) {
	switch {
	case len(image.Data) > 0:
		image.MIMEType = firstNonEmpty(strings.TrimSpace(image.MIMEType), detectMIME(image.Data))
		image.FileName = firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(index, image.MIMEType))
		return image, nil
	case strings.HasPrefix(strings.TrimSpace(image.URL), "data:"):
		data, mimeType, err := decodeDataURL(image.URL)
		if err != nil {
			return InputImage{}, err
		}
		return InputImage{
			Data:     data,
			MIMEType: firstNonEmpty(strings.TrimSpace(image.MIMEType), mimeType),
			FileName: firstNonEmpty(strings.TrimSpace(image.FileName), defaultImageFileName(index, mimeType)),
		}, nil
	case strings.TrimSpace(image.URL) != "":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(image.URL), nil)
		if err != nil {
			return InputImage{}, fmt.Errorf("build input image request: %w", err)
		}
		c.setBrowserHeaders(req)
		resp, err := c.plainClient.Do(req)
		if err != nil {
			return InputImage{}, fmt.Errorf("download input image: %w", err)
		}
		defer closeBody(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return InputImage{}, responseStatusError("download input image", resp)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return InputImage{}, fmt.Errorf("read input image body: %w", err)
		}
		mimeType := firstNonEmpty(strings.TrimSpace(resp.Header.Get("Content-Type")), detectMIME(data))
		fileName := firstNonEmpty(strings.TrimSpace(image.FileName), fileNameFromURL(strings.TrimSpace(image.URL)), defaultImageFileName(index, mimeType))
		return InputImage{
			Data:     data,
			MIMEType: mimeType,
			FileName: fileName,
		}, nil
	default:
		return InputImage{}, &statusError{statusCode: http.StatusBadRequest, message: "image is required"}
	}
}

func responseStatusError(action string, resp *http.Response) error {
	if resp == nil {
		return &statusError{statusCode: http.StatusBadGateway, message: action + " failed"}
	}
	retryAfter := parseRetryAfterHeader(resp.Header.Get("Retry-After"))
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	if action != "" {
		message = fmt.Sprintf("%s returned %d: %s", action, resp.StatusCode, message)
	}
	return &statusError{statusCode: resp.StatusCode, message: message, retryAfter: retryAfter}
}

func waitForNextConversationPoll(ctx context.Context, deadline time.Time, delay time.Duration) error {
	if delay <= 0 {
		delay = defaultPollInterval
	}
	if remaining := time.Until(deadline); remaining < delay {
		delay = remaining
	}
	if delay <= 0 {
		return &statusError{statusCode: http.StatusGatewayTimeout, message: "timed out waiting for async image generation"}
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

func normalizedPollInterval(delay time.Duration) time.Duration {
	if delay <= 0 {
		return defaultPollInterval
	}
	return delay
}

func nextConversationPollDelay(err error, previous time.Duration) time.Duration {
	if retryAfter := retryAfterDuration(err); retryAfter > 0 {
		return clampConversationPollDelay(retryAfter)
	}
	if previous <= 0 {
		previous = defaultPollInterval
	}
	return clampConversationPollDelay(previous * 2)
}

func clampConversationPollDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return defaultPollInterval
	}
	if delay > defaultPollRateLimitBackoff {
		return defaultPollRateLimitBackoff
	}
	return delay
}

func retryAfterDuration(err error) time.Duration {
	type retryAfterCoder interface {
		RetryAfter() time.Duration
	}

	var coded retryAfterCoder
	if errors.As(err, &coded) {
		return coded.RetryAfter()
	}
	return 0
}

func parseRetryAfterHeader(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func closeBody(body io.Closer) {
	if body == nil {
		return
	}
	if err := body.Close(); err != nil {
		log.WithError(err).Warn("chatgpt image: close body failed")
	}
}

func metadataString(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if value, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func attributeHeader(auth *coreauth.Auth, headerName string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	for key, value := range auth.Attributes {
		if !strings.HasPrefix(key, "header:") {
			continue
		}
		if strings.EqualFold(strings.TrimPrefix(key, "header:"), headerName) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isChatGPTHost(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

func decodeDataURL(raw string) ([]byte, string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return nil, "", &statusError{statusCode: http.StatusBadRequest, message: "unsupported data url"}
	}
	header, data, found := strings.Cut(raw, ",")
	if !found {
		return nil, "", &statusError{statusCode: http.StatusBadRequest, message: "invalid data url"}
	}
	header = strings.TrimPrefix(header, "data:")
	mimeType := "image/png"
	if mediaType, _, err := mime.ParseMediaType(header); err == nil && strings.TrimSpace(mediaType) != "" {
		mimeType = mediaType
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, "", &statusError{statusCode: http.StatusBadRequest, message: "invalid base64 image data"}
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
	return fmt.Sprintf("image-%d%s", index+1, extensionFromMIME(mimeType))
}

func extensionFromMIME(mimeType string) string {
	exts, _ := mime.ExtensionsByType(strings.TrimSpace(mimeType))
	if len(exts) > 0 && strings.TrimSpace(exts[0]) != "" {
		return exts[0]
	}
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

func detectMIME(data []byte) string {
	if len(data) == 0 {
		return "image/png"
	}
	return http.DetectContentType(data)
}

func imageDimensions(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil && cfg.Width > 0 && cfg.Height > 0 {
		return cfg.Width, cfg.Height
	}
	return 0, 0
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

func extractFileID(pointer string) string {
	for _, prefix := range []string{"file-service://", "sediment://"} {
		if strings.HasPrefix(pointer, prefix) {
			return strings.TrimPrefix(pointer, prefix)
		}
	}
	return ""
}

func cloneConversationBody(body map[string]any) map[string]any {
	if len(body) == 0 {
		return map[string]any{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return map[string]any{}
	}
	cloned := map[string]any{}
	if err = json.Unmarshal(raw, &cloned); err != nil {
		return map[string]any{}
	}
	return cloned
}

func shouldFallbackFromFConversation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	if strings.Contains(message, "f conversation request:") {
		return true
	}
	status := statusCode(err)
	return status >= http.StatusInternalServerError &&
		status < 600 &&
		strings.Contains(message, "f conversation request returned")
}

func statusCode(err error) int {
	type statusCoder interface {
		StatusCode() int
	}

	var coded statusCoder
	if errors.As(err, &coded) {
		return coded.StatusCode()
	}
	return 0
}

type uploadedFile struct {
	FileID    string
	SizeBytes int
	Width     int
	Height    int
	MIMEType  string
}

type imageResult struct {
	URL            string
	FileID         string
	GenID          string
	ConversationID string
	ParentMsgID    string
	RevisedPrompt  string
}

type conversationRequestContext struct {
	ConversationID     string
	SubmittedMessageID string
	ParentMessageID    string
}

func extractConversationRequestContext(body map[string]any) conversationRequestContext {
	ctx := conversationRequestContext{
		ConversationID:  strings.TrimSpace(stringValue(body["conversation_id"])),
		ParentMessageID: strings.TrimSpace(stringValue(body["parent_message_id"])),
	}

	rawMessages, ok := body["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return ctx
	}
	firstMessage, ok := rawMessages[0].(map[string]any)
	if !ok {
		return ctx
	}
	ctx.SubmittedMessageID = strings.TrimSpace(stringValue(firstMessage["id"]))
	return ctx
}

func stringValue(raw any) string {
	if value, ok := raw.(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func defaultWebClientContext() map[string]any {
	return map[string]any{
		"is_dark_mode":      false,
		"time_since_loaded": 25,
		"page_height":       1138,
		"page_width":        526,
		"pixel_ratio":       3,
		"screen_height":     844,
		"screen_width":      390,
		"app_name":          "chatgpt.com",
	}
}

func webTimezone(now time.Time) (int, string) {
	if now.IsZero() {
		now = time.Now()
	}
	location := strings.TrimSpace(now.Location().String())
	if location == "" || location == "Local" {
		location = "Asia/Shanghai"
	}
	_, offsetSeconds := now.Zone()
	return -offsetSeconds / 60, location
}

func isAsyncImagePendingMessage(msg *sseMessage) bool {
	if msg == nil || len(msg.Metadata) == 0 {
		return false
	}
	for _, key := range []string{"image_gen_async", "trigger_async_ux"} {
		raw, ok := msg.Metadata[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var value bool
		if json.Unmarshal(raw, &value) == nil && value {
			return true
		}
	}
	if rawTaskID, ok := msg.Metadata["image_gen_task_id"]; ok {
		var taskID string
		if json.Unmarshal(rawTaskID, &taskID) == nil && strings.TrimSpace(taskID) != "" {
			return true
		}
	}
	return false
}

type sseEvent struct {
	ConversationID string      `json:"conversation_id"`
	Message        *sseMessage `json:"message"`
}

type sseMessage struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Recipient string `json:"recipient"`
	Channel   string `json:"channel"`
	Author    struct {
		Role string `json:"role"`
	} `json:"author"`
	Content struct {
		ContentType string            `json:"content_type"`
		Parts       []json.RawMessage `json:"parts"`
	} `json:"content"`
	Metadata map[string]json.RawMessage `json:"metadata"`
}

type conversationNode struct {
	Message  *sseMessage `json:"message"`
	Children []string    `json:"children"`
}

type sseImagePart struct {
	ContentType  string `json:"content_type"`
	AssetPointer string `json:"asset_pointer"`
	Metadata     struct {
		Dalle struct {
			GenID  string `json:"gen_id"`
			Prompt string `json:"prompt"`
		} `json:"dalle"`
	} `json:"metadata"`
}

type conversationPollSnapshot struct {
	AsyncStatus             int
	CurrentNode             string
	NodeCount               int
	CurrentRole             string
	CurrentContentType      string
	CurrentStatus           string
	CurrentRecipient        string
	CurrentChannel          string
	CurrentHasImagePointer  bool
	CurrentAsyncPlaceholder bool
}

func (s *conversationPollSnapshot) Populate(mapping map[string]conversationNode) {
	if s == nil || len(mapping) == 0 {
		return
	}
	node, ok := mapping[s.CurrentNode]
	if !ok || node.Message == nil {
		return
	}
	s.CurrentRole = strings.TrimSpace(node.Message.Author.Role)
	s.CurrentContentType = strings.TrimSpace(node.Message.Content.ContentType)
	s.CurrentStatus = strings.TrimSpace(node.Message.Status)
	s.CurrentRecipient = strings.TrimSpace(node.Message.Recipient)
	s.CurrentChannel = strings.TrimSpace(node.Message.Channel)
	s.CurrentAsyncPlaceholder = isAsyncImagePendingMessage(node.Message)
	for _, rawPart := range node.Message.Content.Parts {
		var part sseImagePart
		if err := json.Unmarshal(rawPart, &part); err == nil &&
			strings.TrimSpace(part.ContentType) == "image_asset_pointer" &&
			strings.TrimSpace(part.AssetPointer) != "" {
			s.CurrentHasImagePointer = true
			break
		}
	}
}

func (s conversationPollSnapshot) Signature() string {
	return strings.Join([]string{
		strconv.Itoa(s.AsyncStatus),
		s.CurrentNode,
		s.CurrentRole,
		s.CurrentContentType,
		s.CurrentStatus,
		s.CurrentRecipient,
		s.CurrentChannel,
		strconv.FormatBool(s.CurrentHasImagePointer),
		strconv.FormatBool(s.CurrentAsyncPlaceholder),
		strconv.Itoa(s.NodeCount),
	}, "|")
}

func (s conversationPollSnapshot) LogFields(conversationID string) log.Fields {
	return log.Fields{
		"conversation_id":           conversationID,
		"async_status":              s.AsyncStatus,
		"current_node":              s.CurrentNode,
		"current_role":              s.CurrentRole,
		"current_content_type":      s.CurrentContentType,
		"current_status":            s.CurrentStatus,
		"current_recipient":         s.CurrentRecipient,
		"current_channel":           s.CurrentChannel,
		"current_has_image_pointer": s.CurrentHasImagePointer,
		"current_async_placeholder": s.CurrentAsyncPlaceholder,
		"node_count":                s.NodeCount,
	}
}
