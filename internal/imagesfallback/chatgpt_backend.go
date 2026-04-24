package imagesfallback

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/chatgptimage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func (s *Service) executeWithChatGPTImage(ctx context.Context, auth *coreauth.Auth, req Request) (*Result, error) {
	backendReq := chatgptimage.Request{
		Operation:    mapOperation(req.Operation),
		Prompt:       req.Prompt,
		Model:        ResolveWebModel(auth, req.RequestedModel),
		Size:         req.Size,
		Quality:      req.Quality,
		Background:   req.Background,
		OutputFormat: req.OutputFormat,
		Images:       make([]chatgptimage.InputImage, 0, len(req.Images)),
	}

	for _, image := range req.Images {
		backendReq.Images = append(backendReq.Images, chatgptimage.InputImage{
			URL:      image.URL,
			Data:     image.Data,
			FileName: image.FileName,
			MIMEType: image.MIMEType,
		})
	}
	if req.Mask != nil {
		backendReq.Mask = &chatgptimage.InputImage{
			URL:      req.Mask.URL,
			Data:     req.Mask.Data,
			FileName: req.Mask.FileName,
			MIMEType: req.Mask.MIMEType,
		}
	}

	result, err := chatgptimage.New(s.cfg).Execute(ctx, auth, backendReq)
	if err != nil {
		return nil, err
	}

	out := &Result{
		CreatedAt:    result.CreatedAt,
		Size:         result.Size,
		Quality:      result.Quality,
		Background:   result.Background,
		OutputFormat: result.OutputFormat,
		Images:       make([]GeneratedImage, 0, len(result.Images)),
	}
	for _, image := range result.Images {
		out.Images = append(out.Images, GeneratedImage{
			Data:          image.Data,
			MIMEType:      image.MIMEType,
			RevisedPrompt: image.RevisedPrompt,
		})
	}
	return out, nil
}

func mapOperation(op Operation) chatgptimage.Operation {
	switch op {
	case OperationEdit:
		return chatgptimage.OperationEdit
	default:
		return chatgptimage.OperationGenerate
	}
}
