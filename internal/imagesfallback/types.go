package imagesfallback

import "strings"

type Operation string

const (
	OperationGenerate Operation = "generate"
	OperationEdit     Operation = "edit"
)

type Request struct {
	Operation      Operation
	Prompt         string
	RequestedModel string
	ResponseFormat string
	Images         []InputImage
	Mask           *InputImage
	Size           string
	Quality        string
	Background     string
	OutputFormat   string
}

type InputImage struct {
	URL      string
	Data     []byte
	FileName string
	MIMEType string
}

type Result struct {
	CreatedAt    int64
	Images       []GeneratedImage
	Size         string
	Quality      string
	Background   string
	OutputFormat string
}

type GeneratedImage struct {
	Data          []byte
	MIMEType      string
	RevisedPrompt string
}

func (r Request) NormalizedResponseFormat() string {
	format := strings.ToLower(strings.TrimSpace(r.ResponseFormat))
	if format == "" {
		return "b64_json"
	}
	return format
}
