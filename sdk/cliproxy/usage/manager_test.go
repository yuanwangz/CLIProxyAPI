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
