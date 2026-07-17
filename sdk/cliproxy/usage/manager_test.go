package usage

import (
	"context"
	"testing"
	"time"
)

type capturePlugin struct {
	ch chan Record
}

func (p *capturePlugin) HandleUsage(_ context.Context, record Record) {
	select {
	case p.ch <- record:
	default:
	}
}

func TestManagerPublishSkipsSuppressedContext(t *testing.T) {
	manager := NewManager(4)
	plugin := &capturePlugin{ch: make(chan Record, 1)}
	manager.Register(plugin)
	manager.Publish(WithUsageSuppressed(context.Background()), Record{Provider: "openai", Model: "warmup"})

	select {
	case record := <-plugin.ch:
		t.Fatalf("unexpected suppressed usage record: %+v", record)
	case <-time.After(100 * time.Millisecond):
	}
	manager.Stop()
}

func TestManagerPublishDeliversNormalContext(t *testing.T) {
	manager := NewManager(4)
	plugin := &capturePlugin{ch: make(chan Record, 1)}
	manager.Register(plugin)
	manager.Publish(context.Background(), Record{Provider: "openai", Model: "normal"})

	select {
	case record := <-plugin.ch:
		if record.Model != "normal" {
			t.Fatalf("model = %q, want normal", record.Model)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
	manager.Stop()
}

func TestGenerateEnabledDefaultsNilToTrue(t *testing.T) {
	if !GenerateEnabled(nil) {
		t.Fatalf("GenerateEnabled(nil) = false, want true")
	}
}

func TestGenerateEnabledHonorsExplicitFalse(t *testing.T) {
	if GenerateEnabled(GenerateFlag(false)) {
		t.Fatalf("GenerateEnabled(false) = true, want false")
	}
}

func TestGenerateEnabledHonorsExplicitTrue(t *testing.T) {
	if !GenerateEnabled(GenerateFlag(true)) {
		t.Fatalf("GenerateEnabled(true) = false, want true")
	}
}

func TestGenerateFromContextDefaultsMissingToTrue(t *testing.T) {
	if !GenerateFromContext(context.Background()) {
		t.Fatalf("GenerateFromContext(background) = false, want true")
	}
}

func TestGenerateFromContextHonorsExplicitFalse(t *testing.T) {
	ctx := WithGenerate(context.Background(), false)
	if GenerateFromContext(ctx) {
		t.Fatalf("GenerateFromContext(false) = true, want false")
	}
}

func TestRecordOmittedGenerateIsEnabled(t *testing.T) {
	// Existing callers construct Record without setting Generate.
	// Omission must remain distinguishable from explicit false and default to true.
	record := Record{
		Provider: "openai",
		Model:    "gpt-5.4",
	}
	if record.Generate != nil {
		t.Fatalf("Record.Generate = %v, want nil for omitted field", record.Generate)
	}
	if !GenerateEnabled(record.Generate) {
		t.Fatalf("GenerateEnabled(omitted) = false, want true")
	}
}
