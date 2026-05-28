package auth

import (
	"sync"
	"testing"
	"time"
)

func TestSetGetQuotaRoutingHint_RoundTrip(t *testing.T) {
	defer quotaRoutingHintByAuth.Delete("rt-1")
	reset := time.Now().Add(30 * time.Minute)
	SetQuotaRoutingHint("rt-1", QuotaRoutingHint{
		ResetAt: reset,
		Window:  5 * time.Hour,
	})
	got, ok := GetQuotaRoutingHint("rt-1")
	if !ok {
		t.Fatalf("expected hint to be stored")
	}
	if !got.ResetAt.Equal(reset) {
		t.Fatalf("ResetAt = %v, want %v", got.ResetAt, reset)
	}
	if got.Window != 5*time.Hour {
		t.Fatalf("Window = %v, want 5h", got.Window)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt should auto-populate when zero")
	}
}

func TestSetQuotaRoutingHint_EmptyAuthIDNoOp(t *testing.T) {
	SetQuotaRoutingHint("   ", QuotaRoutingHint{ResetAt: time.Now()})
	if _, ok := GetQuotaRoutingHint(""); ok {
		t.Fatalf("empty authID must not be stored")
	}
}

func TestGetQuotaRoutingHint_Missing(t *testing.T) {
	if _, ok := GetQuotaRoutingHint("does-not-exist"); ok {
		t.Fatalf("expected miss for unknown authID")
	}
}

func TestEffectiveNextReset_FutureResetReturnedAsIs(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	hint := QuotaRoutingHint{
		ResetAt: now.Add(15 * time.Minute),
		Window:  5 * time.Hour,
	}
	got := EffectiveNextReset(hint, now)
	want := now.Add(15 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestEffectiveNextReset_PastResetRollsForward(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	hint := QuotaRoutingHint{
		ResetAt: now.Add(-30 * time.Minute),
		Window:  5 * time.Hour,
	}
	got := EffectiveNextReset(hint, now)
	want := now.Add(-30*time.Minute + 5*time.Hour) // = now + 4h30m
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !got.After(now) {
		t.Fatalf("rolled-forward reset must be in the future, got %v", got)
	}
}

func TestEffectiveNextReset_PastResetRollsForwardMultipleWindows(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	hint := QuotaRoutingHint{
		ResetAt: now.Add(-23 * time.Hour), // four 5h windows passed
		Window:  5 * time.Hour,
	}
	got := EffectiveNextReset(hint, now)
	if !got.After(now) {
		t.Fatalf("expected future reset, got %v", got)
	}
	if got.Sub(now) > 5*time.Hour {
		t.Fatalf("rolled-forward reset must lie within one window of now, got delta=%v", got.Sub(now))
	}
}

func TestEffectiveNextReset_PastResetNoWindowReturnsZero(t *testing.T) {
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	hint := QuotaRoutingHint{
		ResetAt: now.Add(-30 * time.Minute),
	}
	got := EffectiveNextReset(hint, now)
	if !got.IsZero() {
		t.Fatalf("expected zero time for past reset with zero window, got %v", got)
	}
}

func TestEffectiveNextReset_MissingResetReturnsZero(t *testing.T) {
	if got := EffectiveNextReset(QuotaRoutingHint{}, time.Now()); !got.IsZero() {
		t.Fatalf("expected zero time for empty hint, got %v", got)
	}
}

func TestSetQuotaRoutingHint_ConcurrentSafe(t *testing.T) {
	const workers = 8
	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				SetQuotaRoutingHint("concurrent", QuotaRoutingHint{
					ResetAt: time.Now().Add(time.Duration(id*iterations+i) * time.Millisecond),
					Window:  time.Hour,
				})
				_, _ = GetQuotaRoutingHint("concurrent")
			}
		}(w)
	}
	wg.Wait()
	defer quotaRoutingHintByAuth.Delete("concurrent")
	if _, ok := GetQuotaRoutingHint("concurrent"); !ok {
		t.Fatalf("expected hint to persist after concurrent writes")
	}
}
