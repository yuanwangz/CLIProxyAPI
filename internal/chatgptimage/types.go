package chatgptimage

import "time"

type Operation string

const (
	OperationGenerate Operation = "generate"
	OperationEdit     Operation = "edit"
)

type Request struct {
	Operation    Operation
	Prompt       string
	Model        string
	Images       []InputImage
	Mask         *InputImage
	Size         string
	Quality      string
	Background   string
	OutputFormat string
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

type statusError struct {
	statusCode int
	message    string
	retryAfter time.Duration
}

func (e *statusError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *statusError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.statusCode
}

func (e *statusError) RetryAfter() time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return 0
	}
	return e.retryAfter
}
