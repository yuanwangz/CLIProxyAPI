package helps

import "context"

type failureUsageMode int

const (
	failureUsageModeDefault failureUsageMode = iota
	failureUsageModeSuppress
	failureUsageModeAllow
)

type failureUsageContextKey struct{}

// WithFailureUsageSuppressed marks the context so internal probe failures do
// not emit failure usage records. Callers may later override this for the final
// outcome with WithFailureUsageAllowed.
func WithFailureUsageSuppressed(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, failureUsageContextKey{}, failureUsageModeSuppress)
}

// WithFailureUsageAllowed clears any parent suppression for the returned
// context so callers can publish the final failure result explicitly.
func WithFailureUsageAllowed(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, failureUsageContextKey{}, failureUsageModeAllow)
}

func failureUsageSuppressedFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	mode, _ := ctx.Value(failureUsageContextKey{}).(failureUsageMode)
	return mode == failureUsageModeSuppress
}
