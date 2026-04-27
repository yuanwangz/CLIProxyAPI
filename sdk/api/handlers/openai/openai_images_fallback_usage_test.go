package openai

import (
	"context"
	"testing"
	"time"

	executorhelps "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type usageCapturePlugin struct {
	ch chan coreusage.Record
}

func (p *usageCapturePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	select {
	case p.ch <- record:
	default:
	}
}

func waitForUsageRecord(t *testing.T, ch <-chan coreusage.Record) coreusage.Record {
	t.Helper()
	select {
	case record := <-ch:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
		return coreusage.Record{}
	}
}

func TestPublishImageFallbackFinalUsagePublishesMainAndToolRecordsOnSuccess(t *testing.T) {
	plugin := &usageCapturePlugin{ch: make(chan coreusage.Record, 2)}
	coreusage.RegisterPlugin(plugin)

	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-codex",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "tester@example.com",
		},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewOpenAIAPIHandler(base)
	handler.publishImageFallbackFinalUsage(executorhelps.WithFailureUsageSuppressed(context.Background()), auth.ID, "gpt-image-2", false)

	recordA := waitForUsageRecord(t, plugin.ch)
	recordB := waitForUsageRecord(t, plugin.ch)
	records := map[string]coreusage.Record{
		recordA.Model: recordA,
		recordB.Model: recordB,
	}

	mainRecord, ok := records[defaultImagesMainModel]
	if !ok {
		t.Fatalf("missing main model record %q", defaultImagesMainModel)
	}
	toolRecord, ok := records["gpt-image-2"]
	if !ok {
		t.Fatalf("missing tool model record %q", "gpt-image-2")
	}

	for _, record := range []coreusage.Record{mainRecord, toolRecord} {
		if record.Provider != "codex" {
			t.Fatalf("provider = %q, want %q", record.Provider, "codex")
		}
		if record.AuthID != auth.ID {
			t.Fatalf("auth id = %q, want %q", record.AuthID, auth.ID)
		}
		if record.Failed {
			t.Fatalf("failed = true, want false for model %q", record.Model)
		}
	}
}

func TestPublishImageFallbackFinalUsagePublishesOnlyMainFailureRecord(t *testing.T) {
	plugin := &usageCapturePlugin{ch: make(chan coreusage.Record, 1)}
	coreusage.RegisterPlugin(plugin)

	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-codex-failure",
		Provider: "codex",
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler := NewOpenAIAPIHandler(base)
	handler.publishImageFallbackFinalUsage(executorhelps.WithFailureUsageSuppressed(context.Background()), auth.ID, "gpt-image-2", true)

	record := waitForUsageRecord(t, plugin.ch)
	if record.Model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q", record.Model, defaultImagesMainModel)
	}
	if !record.Failed {
		t.Fatalf("failed = false, want true")
	}
}
