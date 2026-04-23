package openai

import (
	"context"
	"encoding/base64"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/imagesfallback"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
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

func (h *OpenAIAPIHandler) handleImageGenerationWithFallback(c *gin.Context, rawJSON, responsesReq []byte, responseFormat string, stream bool) {
	fallbackReq := imagesfallback.Request{
		Operation:      imagesfallback.OperationGenerate,
		Prompt:         strings.TrimSpace(gjson.GetBytes(rawJSON, "prompt").String()),
		RequestedModel: firstNonEmptyString(strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String()), defaultImagesToolModel),
		ResponseFormat: responseFormat,
		Size:           strings.TrimSpace(gjson.GetBytes(rawJSON, "size").String()),
		Quality:        strings.TrimSpace(gjson.GetBytes(rawJSON, "quality").String()),
		Background:     strings.TrimSpace(gjson.GetBytes(rawJSON, "background").String()),
		OutputFormat:   strings.TrimSpace(gjson.GetBytes(rawJSON, "output_format").String()),
	}
	h.executeImagesWithFallback(c, responsesReq, responseFormat, stream, "image_generation", fallbackReq)
}

func (h *OpenAIAPIHandler) handleImageEditJSONWithFallback(c *gin.Context, rawJSON, responsesReq []byte, responseFormat string, stream bool) {
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

	h.executeImagesWithFallback(c, responsesReq, responseFormat, stream, "image_edit", fallbackReq)
}

func (h *OpenAIAPIHandler) handleImageEditMultipartWithFallback(c *gin.Context, form *multipart.Form, responsesReq []byte, responseFormat string, stream bool) {
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
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: " + err.Error(),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		fallbackReq.Images = append(fallbackReq.Images, image)
	}

	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		mask, err := multipartFileToFallbackInputImage(maskFiles[0], 0)
		if err != nil {
			c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "Invalid request: " + err.Error(),
					Type:    "invalid_request_error",
				},
			})
			return
		}
		fallbackReq.Mask = &mask
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

func (h *OpenAIAPIHandler) collectImagesFromResponsesWithFallback(c *gin.Context, responsesReq []byte, responseFormat string, fallbackReq imagesfallback.Request) {
	c.Header("Content-Type", "application/json")

	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", defaultImagesMainModel, responsesReq, "")
	out, errMsg := collectImagesFromResponsesStream(cliCtx, dataChan, errChan, responseFormat)
	stopKeepAlive()

	if errMsg != nil {
		if h.shouldUseImageFallback(errMsg, selectedAuth.Get()) {
			fallbackOut, fallbackErr := h.executeImageFallbackAsJSON(cliCtx, selectedAuth.Get(), responseFormat, fallbackReq)
			if fallbackErr != nil {
				h.WriteErrorResponse(c, fallbackErr)
				if fallbackErr.Error != nil {
					cliCancel(fallbackErr.Error)
				} else {
					cliCancel(nil)
				}
				return
			}
			_, _ = c.Writer.Write(fallbackOut)
			cliCancel()
			return
		}

		h.WriteErrorResponse(c, errMsg)
		if errMsg.Error != nil {
			cliCancel(errMsg.Error)
		} else {
			cliCancel(nil)
		}
		return
	}

	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(out)
	cliCancel()
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
	selectedAuth := &selectedAuthCapture{}
	cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, selectedAuth.Set)
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, "openai-response", defaultImagesMainModel, responsesReq, "")

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
				fallbackErr := h.writeImageFallbackStream(cliCtx, selectedAuth.Get(), responseFormat, streamPrefix, fallbackReq, setSSEHeaders, writeEvent)
				if fallbackErr != nil {
					h.WriteErrorResponse(c, fallbackErr)
					if fallbackErr.Error != nil {
						cliCancel(fallbackErr.Error)
					} else {
						cliCancel(nil)
					}
					return
				}
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

func (h *OpenAIAPIHandler) executeImageFallback(ctx context.Context, authID string, fallbackReq imagesfallback.Request) (*imagesfallback.Result, *interfaces.ErrorMessage) {
	service := imagesfallback.NewService(h.Cfg, h.AuthManager)
	result, err := service.Execute(ctx, authID, fallbackReq)
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

func (h *OpenAIAPIHandler) executeImageFallbackAsJSON(ctx context.Context, authID string, responseFormat string, fallbackReq imagesfallback.Request) ([]byte, *interfaces.ErrorMessage) {
	result, errMsg := h.executeImageFallback(ctx, authID, fallbackReq)
	if errMsg != nil {
		return nil, errMsg
	}
	return buildFallbackImagesAPIResponse(result, responseFormat)
}

func (h *OpenAIAPIHandler) writeImageFallbackStream(ctx context.Context, authID string, responseFormat string, streamPrefix string, fallbackReq imagesfallback.Request, setSSEHeaders func(), writeEvent func(string, []byte)) *interfaces.ErrorMessage {
	result, errMsg := h.executeImageFallback(ctx, authID, fallbackReq)
	if errMsg != nil {
		return errMsg
	}

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
	return nil
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
