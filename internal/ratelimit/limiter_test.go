package ratelimit

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLimiterSeparatesTokensAndEndpointClasses(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, Options{
		Read: Rate{PerSecond: 1, Burst: 1}, Mutation: Rate{PerSecond: 1, Burst: 1},
		Upload: Rate{PerSecond: 1, Burst: 1}, UploadConcurrency: 1, IdleTTL: 15 * time.Minute,
	})
	tokenA := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	tokenB := uuid.MustParse("22222222-3333-4444-8555-666666666666")

	if decision := limiter.Acquire(tokenA, ClassRead); !decision.Allowed {
		t.Fatalf("first token A read decision = %+v", decision)
	}
	if decision := limiter.Acquire(tokenA, ClassRead); decision.Allowed || decision.RetryAfter != time.Second {
		t.Fatalf("second token A read decision = %+v, want one-second retry", decision)
	}
	if decision := limiter.Acquire(tokenB, ClassRead); !decision.Allowed {
		t.Fatalf("token B read decision = %+v", decision)
	}
	if decision := limiter.Acquire(tokenA, ClassMutation); !decision.Allowed {
		t.Fatalf("token A mutation decision = %+v", decision)
	}
}

func TestLimiterRefillsAndReturnsPreciseRetryDelay(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, Options{
		Read: Rate{PerSecond: 2, Burst: 1}, Mutation: Rate{PerSecond: 1, Burst: 1},
		Upload: Rate{PerSecond: 1, Burst: 1}, UploadConcurrency: 1, IdleTTL: 15 * time.Minute,
	})
	token := uuid.MustParse("11111111-2222-4333-8444-555555555555")

	if decision := limiter.Acquire(token, ClassRead); !decision.Allowed {
		t.Fatalf("first decision = %+v", decision)
	}
	if decision := limiter.Acquire(token, ClassRead); decision.Allowed || decision.RetryAfter != 500*time.Millisecond {
		t.Fatalf("limited decision = %+v, want 500ms retry", decision)
	}
	now = now.Add(500 * time.Millisecond)
	if decision := limiter.Acquire(token, ClassRead); !decision.Allowed {
		t.Fatalf("refilled decision = %+v", decision)
	}
}

func TestLimiterCapsConcurrentUploadsUntilRelease(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, Options{
		Read: Rate{PerSecond: 1, Burst: 1}, Mutation: Rate{PerSecond: 1, Burst: 1},
		Upload: Rate{PerSecond: 100, Burst: 10}, UploadConcurrency: 2, IdleTTL: 15 * time.Minute,
	})
	token := uuid.MustParse("11111111-2222-4333-8444-555555555555")

	first := limiter.Acquire(token, ClassUpload)
	second := limiter.Acquire(token, ClassUpload)
	if !first.Allowed || !second.Allowed || first.Release == nil || second.Release == nil {
		t.Fatalf("upload decisions = first %+v second %+v", first, second)
	}
	if third := limiter.Acquire(token, ClassUpload); third.Allowed || third.RetryAfter != time.Second {
		t.Fatalf("third upload decision = %+v, want concurrency denial", third)
	}
	first.Release()
	third := limiter.Acquire(token, ClassUpload)
	if !third.Allowed || third.Release == nil {
		t.Fatalf("upload after release = %+v", third)
	}
	second.Release()
	third.Release()
}

func TestLimiterEvictsIdleTokenBuckets(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	limiter := newTestLimiter(t, &now, Options{
		Read: Rate{PerSecond: 1, Burst: 1}, Mutation: Rate{PerSecond: 1, Burst: 1},
		Upload: Rate{PerSecond: 1, Burst: 1}, UploadConcurrency: 1, IdleTTL: 15 * time.Minute,
	})
	tokenA := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	tokenB := uuid.MustParse("22222222-3333-4444-8555-666666666666")

	if decision := limiter.Acquire(tokenA, ClassRead); !decision.Allowed {
		t.Fatalf("token A decision = %+v", decision)
	}
	now = now.Add(15*time.Minute + time.Nanosecond)
	if decision := limiter.Acquire(tokenB, ClassRead); !decision.Allowed {
		t.Fatalf("token B decision = %+v", decision)
	}
	if len(limiter.entries) != 1 {
		t.Fatalf("limiter entries = %d, want one live token", len(limiter.entries))
	}
	if _, ok := limiter.entries[tokenB]; !ok {
		t.Fatalf("live token B entry was not retained")
	}
}

func TestNewRejectsInvalidLimits(t *testing.T) {
	_, err := New(Options{
		Read: Rate{PerSecond: 0, Burst: 1}, Mutation: Rate{PerSecond: 1, Burst: 1},
		Upload: Rate{PerSecond: 1, Burst: 1}, UploadConcurrency: 1, IdleTTL: 15 * time.Minute,
	})
	if err == nil {
		t.Fatalf("New() accepted a zero read rate")
	}
}

func newTestLimiter(t *testing.T, now *time.Time, options Options) *Limiter {
	t.Helper()
	options.Now = func() time.Time { return *now }
	limiter, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return limiter
}
