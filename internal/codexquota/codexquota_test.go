package codexquota

import (
	"net/http"
	"testing"
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
