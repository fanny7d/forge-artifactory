package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/id"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestRunnerSeedsOneActiveJobAndSchedulesSuccessor(t *testing.T) {
	pool := newRunnerDatabase(t)
	now := &runnerMutableClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	calls := 0
	runner := newTestRunner(t, pool, now, Definition{
		Kind:     KindCleanupBlob,
		Interval: time.Hour,
		Handler: func(context.Context) error {
			calls++
			return nil
		},
	})
	if err := runner.enqueueScheduledJobs(t.Context()); err != nil {
		t.Fatalf("enqueueScheduledJobs(first) error = %v", err)
	}
	if err := runner.enqueueScheduledJobs(t.Context()); err != nil {
		t.Fatalf("enqueueScheduledJobs(second) error = %v", err)
	}
	assertJobCount(t, pool, "state IN ('pending', 'running')", 1)

	processed, err := runner.runOnce(t.Context())
	if err != nil {
		t.Fatalf("runOnce() error = %v", err)
	}
	if !processed || calls != 1 {
		t.Fatalf("runOnce() = processed %t calls %d", processed, calls)
	}
	assertJobCount(t, pool, "state = 'completed'", 1)
	assertJobCount(t, pool, "state = 'pending'", 1)
	var availableAt time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT available_at FROM jobs WHERE kind = $1 AND state = 'pending'", KindCleanupBlob,
	).Scan(&availableAt); err != nil {
		t.Fatalf("load successor job: %v", err)
	}
	if !availableAt.Equal(now.Now().Add(time.Hour)) {
		t.Fatalf("successor available_at = %s, want %s", availableAt, now.Now().Add(time.Hour))
	}
}

func TestRunnerRetriesFailedJobThenCompletesIt(t *testing.T) {
	pool := newRunnerDatabase(t)
	now := &runnerMutableClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	calls := 0
	runner := newTestRunner(t, pool, now, Definition{
		Kind:     KindCleanupUpload,
		Interval: time.Hour,
		Handler: func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("injected object deletion failure")
			}
			return nil
		},
	})
	if err := runner.enqueueScheduledJobs(t.Context()); err != nil {
		t.Fatalf("enqueueScheduledJobs() error = %v", err)
	}
	processed, err := runner.runOnce(t.Context())
	if err != nil {
		t.Fatalf("runOnce(failure) error = %v", err)
	}
	if !processed || calls != 1 {
		t.Fatalf("failed runOnce() = processed %t calls %d", processed, calls)
	}
	var state, failureCode string
	var attempts int32
	var availableAt time.Time
	if err := pool.QueryRow(t.Context(),
		"SELECT state, attempts, failure_code, available_at FROM jobs WHERE kind = $1", KindCleanupUpload,
	).Scan(&state, &attempts, &failureCode, &availableAt); err != nil {
		t.Fatalf("load retried job: %v", err)
	}
	if state != "pending" || attempts != 1 || failureCode != "cleanup-upload-failed" || !availableAt.Equal(now.Now().Add(time.Second)) {
		t.Fatalf("retried job = state %q attempts %d code %q available %s", state, attempts, failureCode, availableAt)
	}

	now.Advance(time.Second)
	processed, err = runner.runOnce(t.Context())
	if err != nil {
		t.Fatalf("runOnce(success) error = %v", err)
	}
	if !processed || calls != 2 {
		t.Fatalf("successful runOnce() = processed %t calls %d", processed, calls)
	}
	assertJobCount(t, pool, "state = 'completed' AND attempts = 2", 1)
	assertJobCount(t, pool, "state = 'pending' AND attempts = 0", 1)
}

func TestRunnerRunReturnsCleanlyWhenContextIsCanceled(t *testing.T) {
	pool := newRunnerDatabase(t)
	now := &runnerMutableClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	runner := newTestRunner(t, pool, now, Definition{
		Kind: KindRecoverPublish, Interval: time.Minute, Handler: func(context.Context) error { return nil },
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run(canceled) error = %v", err)
	}
}

func TestRunnerReapsExpiredExhaustedJobAndSchedulesSuccessor(t *testing.T) {
	pool := newRunnerDatabase(t)
	now := &runnerMutableClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	handlerCalls := 0
	runner := newTestRunner(t, pool, now, Definition{
		Kind:     KindCleanupBlob,
		Interval: time.Hour,
		Handler: func(context.Context) error {
			handlerCalls++
			return nil
		},
	})
	if err := runner.enqueueScheduledJobs(t.Context()); err != nil {
		t.Fatalf("enqueueScheduledJobs() error = %v", err)
	}
	if _, err := pool.Exec(t.Context(), "UPDATE jobs SET max_attempts = 1 WHERE kind = $1", KindCleanupBlob); err != nil {
		t.Fatalf("set max attempts: %v", err)
	}
	owner := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	claimed, err := db.New(pool).ClaimJob(t.Context(), db.ClaimJobParams{
		Kind: string(KindCleanupBlob), LeaseOwner: &owner,
		LeaseExpiresAt: dbTimestamptz(now.Now().Add(time.Minute)), UpdatedAt: dbTimestamptz(now.Now()),
	})
	if err != nil {
		t.Fatalf("ClaimJob(last attempt) error = %v", err)
	}
	if claimed.Attempts != 1 || claimed.MaxAttempts != 1 {
		t.Fatalf("claimed exhausted job = attempts %d max %d", claimed.Attempts, claimed.MaxAttempts)
	}
	now.Advance(2 * time.Minute)
	processed, err := runner.runOnce(t.Context())
	if err != nil {
		t.Fatalf("runOnce(reap) error = %v", err)
	}
	if processed || handlerCalls != 0 {
		t.Fatalf("reap runOnce() = processed %t handlerCalls %d", processed, handlerCalls)
	}
	assertJobCount(t, pool, "state = 'failed' AND failure_code = 'job-attempts-exhausted'", 1)
	assertJobCount(t, pool, "state = 'pending' AND attempts = 0", 1)
}

func TestRunnerIgnoresLeaseLossAfterHandlerTakeover(t *testing.T) {
	pool := newRunnerDatabase(t)
	now := &runnerMutableClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	otherOwner := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	runner := newTestRunner(t, pool, now, Definition{
		Kind:     KindCleanupIdempotency,
		Interval: time.Hour,
		Handler: func(ctx context.Context) error {
			if _, err := pool.Exec(ctx,
				`UPDATE jobs
				 SET lease_owner = $1, lease_generation = lease_generation + 1
				 WHERE kind = $2 AND state = 'running'`,
				otherOwner, KindCleanupIdempotency,
			); err != nil {
				return err
			}
			return nil
		},
	})
	if err := runner.enqueueScheduledJobs(t.Context()); err != nil {
		t.Fatalf("enqueueScheduledJobs() error = %v", err)
	}
	processed, err := runner.runOnce(t.Context())
	if err != nil {
		t.Fatalf("runOnce() error = %v, want benign lease race", err)
	}
	if !processed {
		t.Fatal("runOnce() processed = false, want true")
	}
}

func dbTimestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

type runnerMutableClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *runnerMutableClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *runnerMutableClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func newRunnerDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return pool
}

func newTestRunner(t *testing.T, pool *pgxpool.Pool, now *runnerMutableClock, definition Definition) *Runner {
	t.Helper()
	runner, err := NewRunner(RunnerOptions{
		Pool: pool, Clock: now, IDs: id.UUIDGenerator{}, Definitions: []Definition{definition},
		Lease: time.Minute, PollInterval: 10 * time.Millisecond, RetryBase: time.Second,
		RetryMax: time.Minute, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("NewRunner() error = %v", err)
	}
	return runner
}

func assertJobCount(t *testing.T, pool *pgxpool.Pool, predicate string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM jobs WHERE "+predicate).Scan(&got); err != nil {
		t.Fatalf("count jobs matching %q: %v", predicate, err)
	}
	if got != want {
		t.Fatalf("jobs matching %q = %d, want %d", predicate, got, want)
	}
}
