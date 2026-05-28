package auth

import (
	"strings"
	"sync"
	"time"
)

// QuotaRoutingHint captures the latest observed quota reset metadata for a
// single auth. It is provider-neutral; producers translate provider-specific
// rate-limit headers / events into this shape via SetQuotaRoutingHint.
//
// Hints are advisory only. The scheduler uses them to prefer credentials
// whose next quota reset is the nearest within the same manual priority
// bucket. Absence of a hint, or an unusable hint, means "no preference" and
// the scheduler falls back to its prior ordering.
type QuotaRoutingHint struct {
	// ResetAt is the absolute time at which the most-recently-observed quota
	// window resets. May be in the past once the window has already rolled
	// over; consumers should use EffectiveNextReset to compute the actual
	// upcoming reset.
	ResetAt time.Time
	// Window is the length of the quota window. When non-zero,
	// EffectiveNextReset rolls ResetAt forward by Window until it is strictly
	// after now.
	Window time.Duration
	// UpdatedAt records when the hint was last set; auto-populated when zero.
	UpdatedAt time.Time
}

var quotaRoutingHintByAuth sync.Map

// SetQuotaRoutingHint stores the latest known quota reset metadata for an
// auth. Subsequent calls overwrite the prior value. Empty authID is a no-op.
func SetQuotaRoutingHint(authID string, hint QuotaRoutingHint) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if hint.UpdatedAt.IsZero() {
		hint.UpdatedAt = time.Now()
	}
	quotaRoutingHintByAuth.Store(authID, hint)
}

// GetQuotaRoutingHint returns the latest known quota reset metadata for an
// auth. The boolean reports whether a hint has ever been observed.
func GetQuotaRoutingHint(authID string) (QuotaRoutingHint, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return QuotaRoutingHint{}, false
	}
	value, ok := quotaRoutingHintByAuth.Load(authID)
	if !ok {
		return QuotaRoutingHint{}, false
	}
	hint, ok := value.(QuotaRoutingHint)
	if !ok {
		quotaRoutingHintByAuth.Delete(authID)
		return QuotaRoutingHint{}, false
	}
	return hint, true
}

// EffectiveNextReset returns the next future quota reset moment derived from
// hint, rolling ResetAt forward by Window when the recorded reset has already
// elapsed. A zero time.Time return value means the hint carries no usable
// information and callers should treat the auth as having no quota
// preference.
func EffectiveNextReset(hint QuotaRoutingHint, now time.Time) time.Time {
	if hint.ResetAt.IsZero() {
		return time.Time{}
	}
	if hint.ResetAt.After(now) {
		return hint.ResetAt
	}
	if hint.Window <= 0 {
		return time.Time{}
	}
	next := hint.ResetAt
	// Guard against pathological windows: cap the roll-forward to avoid
	// infinite loops in case ResetAt + Window overflow or stay <= now.
	for i := 0; i < 1<<20; i++ {
		next = next.Add(hint.Window)
		if next.After(now) {
			return next
		}
	}
	return time.Time{}
}

// ClearQuotaRoutingHintForTest removes the hint for the given authID. It is
// only intended for test cleanup across packages; production code should not
// call this.
func ClearQuotaRoutingHintForTest(authID string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	quotaRoutingHintByAuth.Delete(authID)
}
