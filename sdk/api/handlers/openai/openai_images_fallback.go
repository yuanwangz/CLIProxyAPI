package openai

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/imagesfallback"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	executorhelps "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type selectedAuthCapture struct {
	mu     sync.RWMutex
	authID string
}

func (c *selectedAuthCapture) Set(authID string) {
	c.mu.Lock()
	c.authID = strings.TrimSpace(authID)
	c.mu.Unlock()
}

func (c *selectedAuthCapture) Get() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.authID
}

type imageFallbackService interface {
	Execute(context.Context, string, imagesfallback.Request) (*imagesfallback.Result, error)
	ExecuteWithAuthManager(context.Context, imagesfallback.Request, func(string)) (*imagesfallback.Result, error)
}

type routedImageAttempt struct {
	payload      []byte
	headers      http.Header
	fallback     *imagesfallback.Result
	usedFallback bool
}

type routedImageStreamAttempt struct {
	data         <-chan []byte
	errs         <-chan *interfaces.ErrorMessage
	headers      http.Header
	firstChunk   []byte
	fallback     *imagesfallback.Result
	usedFallback bool
}

type routedImageAttemptError struct {
	statusCode int
	err        error
	headers    http.Header
}

func (e *routedImageAttemptError) Error() string {
	if e == nil || e.err == nil {
		return "image request failed"
	}
	return e.err.Error()
}

func (e *routedImageAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *routedImageAttemptError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func (e *routedImageAttemptError) Headers() http.Header {
	if e == nil || e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

var newImageFallbackService = func(cfg *sdkconfig.SDKConfig, authManager *coreauth.Manager) imageFallbackService {
	return imagesfallback.NewService(cfg, authManager)
}

func buildImageGenerationFallbackRequest(rawJSON []byte, responseFormat string) imagesfallback.Request {
	return imagesfallback.Request{
		Operation:      imagesfallback.OperationGenerate,
		Prompt:         strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String()),
		RequestedModel: firstNonEmptyString(strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String()), defaultImagesToolModel),
		ResponseFormat: responseFormat,
		Size:           strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()),
		Quality:        strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()),
		Background:     strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()),
		OutputFormat:   strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()),
	}
}

func (h *OpenAIAPIHandler) handleImageGenerationWithFallback(c *gin.Context, rawJSON, responsesReq []byte, responseFormat string, stream bool) {
	fallbackReq := buildImageGenerationFallbackRequest(rawJSON, responseFormat)
	h.executeImagesWithFallback(c, responsesReq, responseFormat, stream, "image_generation", fallbackReq)
}

func buildImageEditJSONFallbackRequest(rawJSON []byte, responseFormat string) imagesfallback.Request {
	fallbackReq := imagesfallback.Request{
		Operation:      imagesfallback.OperationEdit,
		Prompt:         strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String()),
		RequestedModel: firstNonEmptyString(strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String()), defaultImagesToolModel),
		ResponseFormat: responseFormat,
		Size:           strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()),
		Quality:        strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()),
		Background:     strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()),
		OutputFormat:   strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()),
	}

	for _, img := range gjson.GetBytes(rawJSON, "images").Array() {
		url := strings.TrimSpace(img.Get("image_url").String())
		if url == "" {
			continue
		}
		fallbackReq.Images = append(fallbackReq.Images, imagesfallback.InputImage{URL: url})
	}
	if maskURL := strings.TrimSpace(gjson.GetBytes(rawJSON, "mask.image_url").String()); maskURL != "" {
		fallbackReq.Mask = &imagesfallback.InputImage{URL: maskURL}
	}

	return fallbackReq
}

func (h *OpenAIAPIHandler) handleImageEditJSONWithFallback(c *gin.Context, rawJSON, responsesReq []byte, responseFormat string, stream bool) {
	fallbackReq := buildImageEditJSONFallbackRequest(rawJSON, responseFormat)
	h.executeImagesWithFallback(c, responsesReq, responseFormat, stream, "image_edit", fallbackReq)
}

func buildImageEditMultipartFallbackRequest(form *multipart.Form, responseFormat string) (imagesfallback.Request, error) {
	fallbackReq := imagesfallback.Request{
		Operation:      imagesfallback.OperationEdit,
		Prompt:         strings.TrimSpace(formValue(form, "prompt")),
		RequestedModel: firstNonEmptyString(strings.TrimSpace(formValue(form, "model")), defaultImagesToolModel),
		ResponseFormat: responseFormat,
		Size:           strings.TrimSpace(formValue(form, "size")),
		Quality:        strings.TrimSpace(formValue(form, "quality")),
		Background:     strings.TrimSpace(formValue(form, "background")),
		OutputFormat:   strings.TrimSpace(formValue(form, "output_format")),
	}

	var imageFiles []*multipart.FileHeader
	if files := form.File["image[]"]; len(files) > 0 {
		imageFiles = files
	} else if files := form.File["image"]; len(files) > 0 {
		imageFiles = files
	}

	for index, fh := range imageFiles {
		image, err := multipartFileToFallbackInputImage(fh, index)
		if err != nil {
			return imagesfallback.Request{}, err
		}
		fallbackReq.Images = append(fallbackReq.Images, image)
	}

	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		mask, err := multipartFileToFallbackInputImage(maskFiles[0], 0)
		if err != nil {
			return imagesfallback.Request{}, err
		}
		fallbackReq.Mask = &mask
	}

	return fallbackReq, nil
}

func (h *OpenAIAPIHandler) handleImageEditMultipartWithFallback(c *gin.Context, form *multipart.Form, responsesReq []byte, responseFormat string, stream bool) {
	fallbackReq, err := buildImageEditMultipartFallbackRequest(form, responseFormat)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Invalid request: " + err.Error(),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	h.executeImagesWithFallback(c, responsesReq, responseFormat, stream, "image_edit", fallbackReq)
}

func (h *OpenAIAPIHandler) executeImagesWithFallback(c *gin.Context, responsesReq []byte, responseFormat string, stream bool, streamPrefix string, fallbackReq imagesfallback.Request) {
	if stream {
		h.streamImagesFromResponsesWithFallback(c, responsesReq, responseFormat, streamPrefix, fallbackReq)
		return
	}
	h.collectImagesFromResponsesWithFallback(c, responsesReq, responseFormat, fallbackReq)
}

func (h *OpenAIAPIHandler) handleRoutedImagesWithFallback(c *gin.Context, imageReq []byte, imageModel string, responseFormat string, streamPrefix string, fallbackReq imagesfallback.Request, stream bool) {
	if stream {
		h.streamRoutedImagesWithFallback(c, imageReq, imageModel, responseFormat, streamPrefix, fallbackReq)
		return
	}
	h.collectRoutedImagesWithFallback(c, imageReq, imageModel, responseFormat, fallbackReq)
}

func (h *OpenAIAPIHandler) collectRoutedImagesWithFallback(c *gin.Context, imageReq []byte, imageModel string, responseFormat string, fallbackReq imagesfallback.Request) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = coreauth.WithPreserveAuthOnUnauthorized(cliCtx)
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	primaryCtx := handlers.WithDisallowFreeAuth(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	model := strings.TrimSpace(imageModel)
	resp, upstreamHeaders, errMsg := h.ExecuteImageWithAuthManager(primaryCtx, xaiImagesHandlerType, model, imageReq, "")
	if errMsg != nil {
		if h.shouldUseImageFallbackAfterRoutedError(errMsg) {
			stopKeepAlive()
			log.WithFields(log.Fields{
				"auth_id": selectedAuth.Get(),
				"status":  errMsg.StatusCode,
				"error":   errorString(errMsg),
			}).Warn("openai images: routed image provider failed, switching to codex oauth fallback")
			stopFallbackKeepAlive := startImageFallbackJSONKeepAlive(c, cliCtx)
			attempt, fallbackErr := h.executeRoutedImageAttemptsAcrossCodexAuths(cliCtx, imageReq, model, fallbackReq, selectedAuth.Set)
			stopFallbackKeepAlive()
			if fallbackErr != nil {
				h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, true)
				log.WithFields(log.Fields{
					"auth_id": selectedAuth.Get(),
					"status":  fallbackErr.StatusCode,
					"error":   errorString(fallbackErr),
				}).Error("openai images: codex oauth fallback failed after routed image provider failure")
				h.WriteErrorResponse(c, fallbackErr)
				if fallbackErr.Error != nil {
					cliCancel(fallbackErr.Error)
				} else {
					cliCancel(nil)
				}
				return
			}
			if attempt.usedFallback {
				h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, false)
				fallbackOut, buildErr := buildFallbackImagesAPIResponse(attempt.fallback, responseFormat)
				if buildErr != nil {
					h.WriteErrorResponse(c, buildErr)
					cliCancel(buildErr.Error)
					return
				}
				_, _ = c.Writer.Write(fallbackOut)
			} else {
				handlers.WriteUpstreamHeaders(c.Writer.Header(), attempt.headers)
				_, _ = c.Writer.Write(attempt.payload)
			}
			cliCancel(nil)
			return
		}

		stopKeepAlive()
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}

	stopKeepAlive()
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel(nil)
}

func (h *OpenAIAPIHandler) executeRoutedImageAttemptsAcrossCodexAuths(ctx context.Context, imageReq []byte, model string, fallbackReq imagesfallback.Request, selectedCallback func(string)) (*routedImageAttempt, *interfaces.ErrorMessage) {
	if h == nil || h.AuthManager == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("auth manager is unavailable")}
	}

	metadata := map[string]any{
		coreexecutor.RequestedModelMetadataKey:         model,
		coreexecutor.SkipSelectedAuthResultMetadataKey: true,
	}
	if selectedCallback != nil {
		metadata[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	service := newImageFallbackService(h.Cfg, h.AuthManager)
	value, errExecute := h.AuthManager.ExecuteSelectedAuth(ctx, []string{"codex"}, imagesfallback.TextAuthSelectionModel, coreexecutor.Options{Metadata: metadata}, func(execCtx context.Context, auth *coreauth.Auth, _ string) (any, error) {
		isOAuth := imagesfallback.IsCodexOAuthAuth(auth)
		if !isOAuth || !imagesfallback.IsFreePlan(auth) {
			primaryCtx := handlers.WithPinnedAuthID(handlers.WithDisallowFreeAuth(execCtx), auth.ID)
			payload, headers, errMsg := h.ExecuteImageWithAuthManager(primaryCtx, xaiImagesHandlerType, model, imageReq, "")
			if errMsg == nil {
				return &routedImageAttempt{payload: payload, headers: headers}, nil
			}
			if !isOAuth || !h.shouldUseImageFallbackAfterRoutedError(errMsg) {
				return nil, routedImageError(errMsg)
			}
			log.WithFields(log.Fields{
				"auth_id": auth.ID,
				"status":  errMsg.StatusCode,
				"error":   errorString(errMsg),
			}).Warn("openai images: selected codex primary path failed, trying web image fallback")
		}

		fallbackResult, errFallback := service.Execute(execCtx, auth.ID, fallbackReq)
		if errFallback != nil {
			return nil, errFallback
		}
		h.AuthManager.MarkResult(execCtx, coreauth.Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true})
		return &routedImageAttempt{fallback: fallbackResult, usedFallback: true}, nil
	})
	if errExecute != nil {
		status := imagesfallback.StatusCode(errExecute)
		if status <= 0 {
			status = http.StatusBadGateway
		}
		var headers http.Header
		if withHeaders, ok := errExecute.(interface{ Headers() http.Header }); ok && withHeaders != nil {
			headers = withHeaders.Headers()
		}
		return nil, &interfaces.ErrorMessage{StatusCode: status, Error: errExecute, Addon: headers}
	}
	attempt, ok := value.(*routedImageAttempt)
	if !ok || attempt == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("image attempt returned an invalid result")}
	}
	return attempt, nil
}

func routedImageError(errMsg *interfaces.ErrorMessage) error {
	if errMsg == nil {
		return &routedImageAttemptError{statusCode: http.StatusBadGateway, err: errors.New("image request failed")}
	}
	err := errMsg.Error
	if err == nil {
		err = errors.New(http.StatusText(errMsg.StatusCode))
	}
	return &routedImageAttemptError{statusCode: errMsg.StatusCode, err: err, headers: errMsg.Addon}
}

func (h *OpenAIAPIHandler) collectImagesFromResponsesWithFallback(c *gin.Context, responsesReq []byte, responseFormat string, fallbackReq imagesfallback.Request) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = coreauth.WithPreserveAuthOnUnauthorized(cliCtx)
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	cliCtx = executorhelps.WithFailureUsageSuppressed(cliCtx)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	dataChan, upstreamHeaders, errChan := h.imageProbeHandler().ExecuteStreamWithAuthManager(cliCtx, "openai-response", defaultImagesMainModel, responsesReq, "")
	out, errMsg := collectImagesFromResponsesStream(cliCtx, dataChan, errChan, responseFormat)

	if errMsg != nil {
		if h.shouldUseImageFallback(errMsg, selectedAuth.Get()) {
			stopKeepAlive()
			log.WithFields(log.Fields{
				"auth_id": selectedAuth.Get(),
				"status":  errMsg.StatusCode,
				"error":   errorString(errMsg),
			}).Warn("openai images: primary responses path failed, switching to codex oauth fallback")
			stopFallbackKeepAlive := startImageFallbackJSONKeepAlive(c, cliCtx)
			fallbackOut, fallbackErr := h.executeImageFallbackAsJSON(cliCtx, selectedAuth.Set, responseFormat, fallbackReq)
			stopFallbackKeepAlive()
			if fallbackErr != nil {
				h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, true)
				log.WithFields(log.Fields{
					"auth_id": selectedAuth.Get(),
					"status":  fallbackErr.StatusCode,
					"error":   errorString(fallbackErr),
				}).Error("openai images: codex oauth fallback failed")
				h.WriteErrorResponse(c, fallbackErr)
				if fallbackErr.Error != nil {
					cliCancel(fallbackErr.Error)
				} else {
					cliCancel(nil)
				}
				return
			}
			h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, false)
			_, _ = c.Writer.Write(fallbackOut)
			cliCancel()
			return
		}

		stopKeepAlive()
		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}

	stopKeepAlive()
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
}

func startImageFallbackJSONKeepAlive(c *gin.Context, ctx context.Context) func() {
	if c == nil {
		return func() {}
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	const heartbeatInterval = 15 * time.Second
	stopChan := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
			}
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stopChan)
		})
		wg.Wait()
	}
}

func (h *OpenAIAPIHandler) streamImagesFromResponsesWithFallback(c *gin.Context, responsesReq []byte, responseFormat string, streamPrefix string, fallbackReq imagesfallback.Request) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = coreauth.WithPreserveAuthOnUnauthorized(cliCtx)
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	cliCtx = executorhelps.WithFailureUsageSuppressed(cliCtx)
	dataChan, upstreamHeaders, errChan := h.imageProbeHandler().ExecuteStreamWithAuthManager(cliCtx, "openai-response", defaultImagesMainModel, responsesReq, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	writeEvent := func(eventName string, dataJSON []byte) {
		if strings.TrimSpace(eventName) != "" {
			_, _ = c.Writer.Write([]byte("event: " + eventName + "\n"))
		}
		_, _ = c.Writer.Write([]byte("data: "))
		_, _ = c.Writer.Write(dataJSON)
		_, _ = c.Writer.Write([]byte("\n\n"))
		flusher.Flush()
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, okRead := <-errChan:
			if !okRead {
				errChan = nil
				continue
			}
			if errMsg == nil {
				errChan = nil
				continue
			}
			if h.shouldUseImageFallback(errMsg, selectedAuth.Get()) {
				log.WithFields(log.Fields{
					"auth_id": selectedAuth.Get(),
					"status":  errMsg.StatusCode,
					"error":   errorString(errMsg),
				}).Warn("openai images: primary responses stream failed, switching to codex oauth fallback")
				fallbackErr := h.writeImageFallbackStream(cliCtx, selectedAuth.Set, responseFormat, streamPrefix, fallbackReq, setSSEHeaders, writeEvent)
				if fallbackErr != nil {
					h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, true)
					log.WithFields(log.Fields{
						"auth_id": selectedAuth.Get(),
						"status":  fallbackErr.StatusCode,
						"error":   errorString(fallbackErr),
					}).Error("openai images: codex oauth stream fallback failed")
					h.WriteErrorResponse(c, fallbackErr)
					if fallbackErr.Error != nil {
						cliCancel(fallbackErr.Error)
					} else {
						cliCancel(nil)
					}
					return
				}
				h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, false)
				cliCancel(nil)
				return
			}
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, okRead := <-dataChan:
			if !okRead {
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			h.forwardImagesStream(cliCtx, c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, chunk, responseFormat, streamPrefix, writeEvent)
			return
		}
	}
}

func (h *OpenAIAPIHandler) streamRoutedImagesWithFallback(c *gin.Context, imageReq []byte, imageModel string, responseFormat string, streamPrefix string, fallbackReq imagesfallback.Request) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = coreauth.WithPreserveAuthOnUnauthorized(cliCtx)
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	primaryCtx := handlers.WithDisallowFreeAuth(cliCtx)
	model := strings.TrimSpace(imageModel)
	execution, streamStarted, canceled := h.waitImagesStreamExecution(c, flusher, func() imagesStreamExecutionResult {
		dataChan, upstreamHeaders, errChan := h.ExecuteImageStreamWithAuthManager(primaryCtx, xaiImagesHandlerType, model, imageReq, "")
		return imagesStreamExecutionResult{Data: dataChan, UpstreamHeaders: upstreamHeaders, Errs: errChan}
	})
	if canceled {
		cliCancel(c.Request.Context().Err())
		return
	}
	dataChan := execution.Data
	upstreamHeaders := execution.UpstreamHeaders
	errChan := execution.Errs
	keepAlive, keepAliveC := h.newImagesStreamKeepAliveTicker()
	stopKeepAlive := func() {
		if keepAlive != nil {
			keepAlive.Stop()
			keepAlive = nil
			keepAliveC = nil
		}
	}
	defer stopKeepAlive()

	writeError := func(errMsg *interfaces.ErrorMessage) {
		if streamStarted {
			writeImagesStreamErrorEvent(c, errMsg)
			flusher.Flush()
		} else {
			h.WriteErrorResponse(c, errMsg)
		}
		if errMsg != nil && errMsg.Error != nil {
			cliCancel(errMsg.Error)
			return
		}
		cliCancel(nil)
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, okRead := <-errChan:
			if !okRead {
				errChan = nil
				continue
			}
			if errMsg == nil {
				errChan = nil
				continue
			}
			if h.shouldUseImageFallbackAfterRoutedError(errMsg) {
				stopKeepAlive()
				log.WithFields(log.Fields{
					"auth_id": selectedAuth.Get(),
					"status":  errMsg.StatusCode,
					"error":   errorString(errMsg),
				}).Warn("openai images: routed image stream failed, switching to codex oauth fallback")
				attempt, fallbackErr := h.executeRoutedImageStreamAttemptsAcrossCodexAuths(cliCtx, imageReq, model, fallbackReq, selectedAuth.Set)
				if fallbackErr == nil && attempt.usedFallback {
					h.writeImageFallbackResultStream(attempt.fallback, responseFormat, streamPrefix, func() {
						setImagesSSEHeaders(c)
					}, func(eventName string, dataJSON []byte) {
						if strings.TrimSpace(eventName) != "" {
							_, _ = c.Writer.Write([]byte("event: " + eventName + "\n"))
						}
						_, _ = c.Writer.Write([]byte("data: "))
						_, _ = c.Writer.Write(dataJSON)
						_, _ = c.Writer.Write([]byte("\n\n"))
						flusher.Flush()
					})
				} else if fallbackErr == nil {
					setImagesSSEHeaders(c)
					handlers.WriteUpstreamHeaders(c.Writer.Header(), attempt.headers)
					if len(attempt.firstChunk) > 0 {
						_, _ = c.Writer.Write(attempt.firstChunk)
						flusher.Flush()
					}
					h.forwardRawImageStream(cliCtx, c, func(err error) { cliCancel(err) }, attempt.data, attempt.errs)
					return
				}
				if fallbackErr != nil {
					h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, true)
					log.WithFields(log.Fields{
						"auth_id": selectedAuth.Get(),
						"status":  fallbackErr.StatusCode,
						"error":   errorString(fallbackErr),
					}).Error("openai images: codex oauth stream fallback failed after routed image provider failure")
					writeError(fallbackErr)
					return
				}
				h.publishImageFallbackFinalUsage(cliCtx, selectedAuth.Get(), fallbackReq.RequestedModel, false)
				cliCancel(nil)
				return
			}
			writeError(errMsg)
			return
		case chunk, okRead := <-dataChan:
			if !okRead {
				stopKeepAlive()
				setImagesSSEHeaders(c)
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			stopKeepAlive()
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			_, _ = c.Writer.Write(chunk)
			flusher.Flush()
			streamStarted = true
			h.forwardRawImageStream(cliCtx, c, func(err error) { cliCancel(err) }, dataChan, errChan)
			return
		case <-keepAliveC:
			setImagesSSEHeaders(c)
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			writeImagesStreamKeepAlive(c, flusher)
			streamStarted = true
		}
	}
}

func (h *OpenAIAPIHandler) executeRoutedImageStreamAttemptsAcrossCodexAuths(ctx context.Context, imageReq []byte, model string, fallbackReq imagesfallback.Request, selectedCallback func(string)) (*routedImageStreamAttempt, *interfaces.ErrorMessage) {
	if h == nil || h.AuthManager == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("auth manager is unavailable")}
	}

	metadata := map[string]any{
		coreexecutor.RequestedModelMetadataKey:         model,
		coreexecutor.SkipSelectedAuthResultMetadataKey: true,
	}
	if selectedCallback != nil {
		metadata[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	service := newImageFallbackService(h.Cfg, h.AuthManager)
	value, errExecute := h.AuthManager.ExecuteSelectedAuth(ctx, []string{"codex"}, imagesfallback.TextAuthSelectionModel, coreexecutor.Options{Metadata: metadata}, func(execCtx context.Context, auth *coreauth.Auth, _ string) (any, error) {
		isOAuth := imagesfallback.IsCodexOAuthAuth(auth)
		if !isOAuth || !imagesfallback.IsFreePlan(auth) {
			primaryCtx := handlers.WithPinnedAuthID(handlers.WithDisallowFreeAuth(execCtx), auth.ID)
			data, headers, errs := h.ExecuteImageStreamWithAuthManager(primaryCtx, xaiImagesHandlerType, model, imageReq, "")
			for data != nil || errs != nil {
				select {
				case <-execCtx.Done():
					return nil, execCtx.Err()
				case errMsg, okRead := <-errs:
					if !okRead {
						errs = nil
						continue
					}
					if errMsg == nil {
						continue
					}
					if !isOAuth || !h.shouldUseImageFallbackAfterRoutedError(errMsg) {
						return nil, routedImageError(errMsg)
					}
					log.WithFields(log.Fields{
						"auth_id": auth.ID,
						"status":  errMsg.StatusCode,
						"error":   errorString(errMsg),
					}).Warn("openai images: selected codex primary stream failed, trying web image fallback")
					data = nil
					errs = nil
				case chunk, okRead := <-data:
					if !okRead {
						data = nil
						continue
					}
					return &routedImageStreamAttempt{data: data, errs: errs, headers: headers, firstChunk: chunk}, nil
				}
			}
			if !isOAuth {
				return nil, &routedImageAttemptError{statusCode: http.StatusBadGateway, err: io.ErrUnexpectedEOF, headers: headers}
			}
		}

		fallbackResult, errFallback := service.Execute(execCtx, auth.ID, fallbackReq)
		if errFallback != nil {
			return nil, errFallback
		}
		h.AuthManager.MarkResult(execCtx, coreauth.Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true})
		return &routedImageStreamAttempt{fallback: fallbackResult, usedFallback: true}, nil
	})
	if errExecute != nil {
		status := imagesfallback.StatusCode(errExecute)
		if status <= 0 {
			status = http.StatusBadGateway
		}
		var headers http.Header
		if withHeaders, ok := errExecute.(interface{ Headers() http.Header }); ok && withHeaders != nil {
			headers = withHeaders.Headers()
		}
		return nil, &interfaces.ErrorMessage{StatusCode: status, Error: errExecute, Addon: headers}
	}
	attempt, ok := value.(*routedImageStreamAttempt)
	if !ok || attempt == nil {
		return nil, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("image stream attempt returned an invalid result")}
	}
	return attempt, nil
}

func (h *OpenAIAPIHandler) shouldUseImageFallback(errMsg *interfaces.ErrorMessage, authID string) bool {
	if errMsg == nil || h == nil || h.AuthManager == nil {
		return false
	}
	auth, ok := h.AuthManager.GetByID(strings.TrimSpace(authID))
	if !ok || auth == nil {
		return false
	}
	return imagesfallback.ShouldUseCodexOAuthFallback(errMsg.StatusCode, errMsg.Error, auth)
}

func (h *OpenAIAPIHandler) shouldUseImageFallbackAfterRoutedError(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || !h.hasCodexOAuthImageFallbackAuth() {
		return false
	}
	if imagesfallback.IsMissingImageGenerationToolError(errMsg.Error) {
		return true
	}
	text := strings.ToLower(imagesfallback.ErrorText(errMsg.Error))
	if strings.Contains(text, "upstream did not return image output") {
		return true
	}
	if strings.Contains(text, "stream disconnected before completion") {
		return true
	}
	switch errMsg.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return errMsg.StatusCode >= http.StatusInternalServerError
}

func (h *OpenAIAPIHandler) hasCodexOAuthImageFallbackAuth() bool {
	if h == nil || h.AuthManager == nil {
		return false
	}
	for _, auth := range h.AuthManager.List() {
		if auth == nil || auth.Archived || auth.Status == coreauth.StatusArchived || auth.Disabled {
			continue
		}
		if imagesfallback.IsCodexOAuthAuth(auth) {
			return true
		}
	}
	return false
}

func (h *OpenAIAPIHandler) executeImageFallback(ctx context.Context, selectedCallback func(string), fallbackReq imagesfallback.Request) (*imagesfallback.Result, *interfaces.ErrorMessage) {
	service := newImageFallbackService(h.Cfg, h.AuthManager)
	result, err := service.ExecuteWithAuthManager(ctx, fallbackReq, selectedCallback)
	if err == nil {
		return result, nil
	}

	status := imagesfallback.StatusCode(err)
	if status <= 0 {
		status = http.StatusBadGateway
	}
	return nil, &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      err,
	}
}

func (h *OpenAIAPIHandler) executeImageFallbackAsJSON(ctx context.Context, selectedCallback func(string), responseFormat string, fallbackReq imagesfallback.Request) ([]byte, *interfaces.ErrorMessage) {
	result, errMsg := h.executeImageFallback(ctx, selectedCallback, fallbackReq)
	if errMsg != nil {
		return nil, errMsg
	}
	return buildFallbackImagesAPIResponse(result, responseFormat)
}

func (h *OpenAIAPIHandler) writeImageFallbackStream(ctx context.Context, selectedCallback func(string), responseFormat string, streamPrefix string, fallbackReq imagesfallback.Request, setSSEHeaders func(), writeEvent func(string, []byte)) *interfaces.ErrorMessage {
	result, errMsg := h.executeImageFallback(ctx, selectedCallback, fallbackReq)
	if errMsg != nil {
		return errMsg
	}
	h.writeImageFallbackResultStream(result, responseFormat, streamPrefix, setSSEHeaders, writeEvent)
	return nil
}

func (h *OpenAIAPIHandler) writeImageFallbackResultStream(result *imagesfallback.Result, responseFormat string, streamPrefix string, setSSEHeaders func(), writeEvent func(string, []byte)) {
	setSSEHeaders()
	eventName := streamPrefix + ".completed"
	for _, image := range result.Images {
		payload := []byte(`{"type":""}`)
		payload, _ = sjson.SetBytes(payload, "type", eventName)
		dataURL := "data:" + firstNonEmptyString(strings.TrimSpace(image.MIMEType), "image/png") + ";base64," + base64.StdEncoding.EncodeToString(image.Data)
		if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
			payload, _ = sjson.SetBytes(payload, "url", dataURL)
		} else {
			payload, _ = sjson.SetBytes(payload, "b64_json", base64.StdEncoding.EncodeToString(image.Data))
		}
		writeEvent(eventName, payload)
	}
}

func buildFallbackImagesAPIResponse(result *imagesfallback.Result, responseFormat string) ([]byte, *interfaces.ErrorMessage) {
	if result == nil || len(result.Images) == 0 {
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      io.ErrUnexpectedEOF,
		}
	}

	callResults := make([]imageCallResult, 0, len(result.Images))
	for _, image := range result.Images {
		outputFormat := strings.TrimSpace(result.OutputFormat)
		if outputFormat == "" {
			switch strings.ToLower(strings.TrimSpace(image.MIMEType)) {
			case "image/jpeg":
				outputFormat = "jpeg"
			case "image/webp":
				outputFormat = "webp"
			default:
				outputFormat = "png"
			}
		}
		callResults = append(callResults, imageCallResult{
			Result:        base64.StdEncoding.EncodeToString(image.Data),
			RevisedPrompt: strings.TrimSpace(image.RevisedPrompt),
			OutputFormat:  outputFormat,
			Size:          strings.TrimSpace(result.Size),
			Background:    strings.TrimSpace(result.Background),
			Quality:       strings.TrimSpace(result.Quality),
		})
	}

	out, err := buildImagesAPIResponse(callResults, result.CreatedAt, nil, callResults[0], responseFormat)
	if err != nil {
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      err,
		}
	}
	return out, nil
}

func multipartFileToFallbackInputImage(fileHeader *multipart.FileHeader, index int) (imagesfallback.InputImage, error) {
	if fileHeader == nil {
		return imagesfallback.InputImage{}, http.ErrMissingFile
	}
	f, err := fileHeader.Open()
	if err != nil {
		return imagesfallback.InputImage{}, err
	}
	defer func() {
		_ = f.Close()
	}()

	data, err := io.ReadAll(f)
	if err != nil {
		return imagesfallback.InputImage{}, err
	}
	mimeType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	fileName := strings.TrimSpace(fileHeader.Filename)
	if fileName == "" {
		fileName = "image"
	}
	return imagesfallback.InputImage{
		Data:     data,
		MIMEType: mimeType,
		FileName: firstNonEmptyString(fileName, "image"),
	}, nil
}

func formValue(form *multipart.Form, key string) string {
	if form == nil || form.Value == nil {
		return ""
	}
	values := form.Value[key]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func errorString(errMsg *interfaces.ErrorMessage) string {
	if errMsg == nil || errMsg.Error == nil {
		return ""
	}
	return strings.TrimSpace(errMsg.Error.Error())
}

func (h *OpenAIAPIHandler) imageProbeHandler() *OpenAIAPIHandler {
	if h == nil || h.AuthManager == nil {
		return h
	}
	return NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(h.Cfg, h.AuthManager.CloneForProbe()))
}

func (h *OpenAIAPIHandler) publishImageFallbackFinalUsage(ctx context.Context, authID string, requestedModel string, failed bool) {
	authID = strings.TrimSpace(authID)

	var auth *coreauth.Auth
	if h != nil && h.AuthManager != nil && authID != "" {
		if current, ok := h.AuthManager.GetByID(authID); ok && current != nil {
			auth = current
		}
	}

	usageCtx := executorhelps.WithFailureUsageAllowed(ctx)
	reporter := executorhelps.NewUsageReporter(usageCtx, "codex", defaultImagesMainModel, auth)
	if reporter == nil {
		return
	}
	if failed {
		reporter.PublishFailure(usageCtx)
		return
	}
	reporter.EnsurePublished(usageCtx)
	reporter.PublishAdditionalModelEvent(usageCtx, firstNonEmptyString(strings.TrimSpace(requestedModel), defaultImagesToolModel))
}
