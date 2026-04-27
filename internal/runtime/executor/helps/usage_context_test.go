package helps

import (
	"context"
	"errors"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type captureUsagePlugin struct {
	ch chan coreusage.Record
}

func (p *captureUsagePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	select {
	case p.ch <- record:
	default:
	}
}

func TestUsageReporterPublishFailureRespectsSuppressedContext(t *testing.T) {
	plugin := &captureUsagePlugin{ch: make(chan coreusage.Record, 1)}
	coreusage.RegisterPlugin(plugin)

	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-5.4-mini",
		requestedAt: time.Now(),
	}
	reporter.PublishFailure(WithFailureUsageSuppressed(context.Background()))

	select {
	case record := <-plugin.ch:
		t.Fatalf("unexpected usage record: %+v", record)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUsageReporterTrackFailurePublishesWhenAllowed(t *testing.T) {
	plugin := &captureUsagePlugin{ch: make(chan coreusage.Record, 1)}
	coreusage.RegisterPlugin(plugin)

	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-5.4-mini",
		requestedAt: time.Now(),
	}
	errValue := errors.New("boom")
	reporter.TrackFailure(WithFailureUsageAllowed(context.Background()), &errValue)

	select {
	case record := <-plugin.ch:
		if !record.Failed {
			t.Fatalf("failed = false, want true")
		}
		if record.Model != "gpt-5.4-mini" {
			t.Fatalf("model = %q, want %q", record.Model, "gpt-5.4-mini")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterPublishFailureAllowedOverridesParentSuppression(t *testing.T) {
	plugin := &captureUsagePlugin{ch: make(chan coreusage.Record, 1)}
	coreusage.RegisterPlugin(plugin)

	reporter := &UsageReporter{
		provider:    "codex",
		model:       "gpt-image-2",
		requestedAt: time.Now(),
	}
	reporter.PublishFailure(WithFailureUsageAllowed(WithFailureUsageSuppressed(context.Background())))

	select {
	case record := <-plugin.ch:
		if !record.Failed {
			t.Fatalf("failed = false, want true")
		}
		if record.Model != "gpt-image-2" {
			t.Fatalf("model = %q, want %q", record.Model, "gpt-image-2")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}
