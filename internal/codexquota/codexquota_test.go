package codexquota

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagepersist"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestQuotaStateFromHeadersReadsDefaultAndAdditionalLimits(t *testing.T) {
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "12.5")
	headers.Set("x-codex-primary-window-minutes", "60")
	headers.Set("x-codex-primary-reset-at", "1704069000")
	headers.Set("x-codex-bengalfox-primary-used-percent", "80")
	headers.Set("x-codex-bengalfox-primary-window-minutes", "1440")
	headers.Set("x-codex-bengalfox-primary-reset-at", "1704074400")
	headers.Set("x-codex-bengalfox-limit-name", "gpt-5.2-codex-sonic")
	headers.Set("x-codex-credits-has-credits", "true")
	headers.Set("x-codex-credits-unlimited", "false")
	headers.Set("x-codex-credits-balance", "123")

	state, ok := quotaStateFromHeaders(headers)
	if !ok {
		t.Fatal("expected quota state from headers")
	}
	if state.Status != "success" {
		t.Fatalf("status = %q, want success", state.Status)
	}
	if state.Credits == nil || !state.Credits.HasCredits || state.Credits.Unlimited {
		t.Fatalf("credits = %+v, want limited credits", state.Credits)
	}
	if state.Credits.Balance == nil || *state.Credits.Balance != "123" {
		t.Fatalf("credits balance = %+v, want 123", state.Credits)
	}
	if len(state.Windows) != 2 {
		t.Fatalf("windows = %d, want 2: %+v", len(state.Windows), state.Windows)
	}
	first := state.Windows[0]
	if first.ID != "observed-primary" || first.UsedPercent == nil || *first.UsedPercent != 12.5 {
		t.Fatalf("default window = %+v, want observed primary 12.5", first)
	}
	if first.ResetAt == nil || *first.ResetAt != 1704069000 {
		t.Fatalf("default reset = %+v, want 1704069000", first.ResetAt)
	}
	second := state.Windows[1]
	if second.ID != "codex_bengalfox-primary" || second.LabelParams["name"] != "gpt-5.2-codex-sonic" {
		t.Fatalf("additional window = %+v, want named codex_bengalfox", second)
	}
}

func TestQuotaStateFromEventReadsCodexRateLimits(t *testing.T) {
	payload := []byte(`{
		"type": "codex.rate_limits",
		"plan_type": "plus",
		"rate_limits": {
			"allowed": true,
			"limit_reached": false,
			"primary": {
				"used_percent": 42,
				"window_minutes": 60,
				"reset_at": 1700000000
			},
			"secondary": null
		},
		"credits": {
			"has_credits": true,
			"unlimited": false,
			"balance": "123"
		}
	}`)

	state, ok := quotaStateFromEvent(payload)
	if !ok {
		t.Fatal("expected quota state from event")
	}
	if state.PlanType != "plus" {
		t.Fatalf("plan type = %q, want plus", state.PlanType)
	}
	if len(state.Windows) != 1 {
		t.Fatalf("windows = %d, want 1", len(state.Windows))
	}
	window := state.Windows[0]
	if window.UsedPercent == nil || *window.UsedPercent != 42 {
		t.Fatalf("used percent = %+v, want 42", window.UsedPercent)
	}
	if window.WindowMinutes == nil || *window.WindowMinutes != 60 {
		t.Fatalf("window minutes = %+v, want 60", window.WindowMinutes)
	}
	if state.Credits == nil || state.Credits.Balance == nil || *state.Credits.Balance != "123" {
		t.Fatalf("credits = %+v, want balance 123", state.Credits)
	}
}

func TestQuotaStateFromEmptyHeadersIsIgnored(t *testing.T) {
	_, ok := quotaStateFromHeaders(http.Header{})
	if ok {
		t.Fatal("empty headers should not create a quota state")
	}
}

func TestPublishRoutingHint_PicksEarliestResetAcrossWindows(t *testing.T) {
	const authID = "codex-hint-1"
	t.Cleanup(func() { cliproxyauth.ClearQuotaRoutingHintForTest(authID) })

	earlier := int64(1_700_000_000)
	later := int64(1_700_003_600)
	primaryWindowMinutes := int64(60)
	secondaryWindowMinutes := int64(1440)
	state := quotaState{
		Status: "success",
		Windows: []quotaWindow{
			{ID: "observed-secondary", ResetAt: &later, WindowMinutes: &secondaryWindowMinutes},
			{ID: "observed-primary", ResetAt: &earlier, WindowMinutes: &primaryWindowMinutes},
		},
	}
	publishRoutingHint(&cliproxyauth.Auth{ID: authID}, state)

	hint, ok := cliproxyauth.GetQuotaRoutingHint(authID)
	if !ok {
		t.Fatalf("expected routing hint to be published")
	}
	wantReset := time.Unix(earlier, 0).UTC()
	if !hint.ResetAt.Equal(wantReset) {
		t.Fatalf("ResetAt = %v, want %v (earliest window)", hint.ResetAt, wantReset)
	}
	if hint.Window != time.Hour {
		t.Fatalf("Window = %v, want 1h (primary window minutes)", hint.Window)
	}
}

func TestPublishRoutingHint_NoUsableWindowIsNoop(t *testing.T) {
	const authID = "codex-hint-2"
	t.Cleanup(func() { cliproxyauth.ClearQuotaRoutingHintForTest(authID) })

	zero := int64(0)
	state := quotaState{
		Windows: []quotaWindow{{ResetAt: nil}, {ResetAt: &zero}},
	}
	publishRoutingHint(&cliproxyauth.Auth{ID: authID}, state)
	if _, ok := cliproxyauth.GetQuotaRoutingHint(authID); ok {
		t.Fatalf("expected no hint when no window has a positive reset_at")
	}
}

func TestPublishRoutingHint_LatestObservationOverwritesEarlier(t *testing.T) {
	const authID = "codex-hint-3"
	t.Cleanup(func() { cliproxyauth.ClearQuotaRoutingHintForTest(authID) })

	first := int64(1_700_000_000)
	second := int64(1_700_007_200)
	window := int64(60)
	publishRoutingHint(&cliproxyauth.Auth{ID: authID}, quotaState{
		Windows: []quotaWindow{{ResetAt: &first, WindowMinutes: &window}},
	})
	publishRoutingHint(&cliproxyauth.Auth{ID: authID}, quotaState{
		Windows: []quotaWindow{{ResetAt: &second, WindowMinutes: &window}},
	})
	hint, ok := cliproxyauth.GetQuotaRoutingHint(authID)
	if !ok {
		t.Fatalf("expected hint to be present after second publish")
	}
	if !hint.ResetAt.Equal(time.Unix(second, 0).UTC()) {
		t.Fatalf("ResetAt = %v, want second observation %v", hint.ResetAt, time.Unix(second, 0).UTC())
	}
}

func TestWarmupRoutingHints_PrimesFromPersistedSnapshots(t *testing.T) {
	const authID = "codex-warmup-1"
	t.Cleanup(func() { cliproxyauth.ClearQuotaRoutingHintForTest(authID) })

	dir := t.TempDir()
	if err := usagepersist.Init(filepath.Join(dir, "usage.sqlite"), true); err != nil {
		t.Fatalf("init usage persistence: %v", err)
	}
	t.Cleanup(func() {
		usagepersist.SetEnabled(false)
		_ = os.Remove(filepath.Join(dir, "usage.sqlite"))
	})

	resetUnix := time.Now().Add(45 * time.Minute).Unix()
	windowMinutes := int64(60)
	snapshotJSON := `{"status":"success","windows":[{"id":"observed-primary","resetAt":` +
		strconv.FormatInt(resetUnix, 10) + `,"windowMinutes":` + strconv.FormatInt(windowMinutes, 10) + `}]}`
	if _, err := usagepersist.UpsertQuotaSnapshot(context.Background(), usagepersist.QuotaSnapshot{
		Provider:  providerCodex,
		AuthID:    authID,
		AuthIndex: authID,
		QuotaJSON: snapshotJSON,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	cliproxyauth.ClearQuotaRoutingHintForTest(authID)
	WarmupRoutingHints(context.Background())

	hint, ok := cliproxyauth.GetQuotaRoutingHint(authID)
	if !ok {
		t.Fatalf("expected warmup to populate hint")
	}
	if !hint.ResetAt.Equal(time.Unix(resetUnix, 0).UTC()) {
		t.Fatalf("ResetAt = %v, want %v", hint.ResetAt, time.Unix(resetUnix, 0).UTC())
	}
	if hint.Window != time.Hour {
		t.Fatalf("Window = %v, want 1h", hint.Window)
	}
}
