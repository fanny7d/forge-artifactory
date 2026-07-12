package idempotency

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestRunInTxCommitsMutationAndReplaysWithoutExecutingAgain(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	tokenID := seedTokenID
	service := newTestService(t, pool)
	request := fixedRequest(tokenID, "create-repository")
	calls := 0

	first, err := service.RunInTx(t.Context(), request, func(ctx context.Context, tx pgx.Tx) (Response, error) {
		calls++
		_, err := tx.Exec(ctx, "INSERT INTO repositories (key, display_name, created_by) VALUES ('releases', 'Releases', $1)", tokenID)
		return Response{Status: 201, Body: []byte(`{"key":"releases"}`)}, err
	})
	if err != nil {
		t.Fatalf("first RunInTx() error = %v", err)
	}
	second, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		calls++
		return Response{}, errors.New("callback must not run during replay")
	})
	if err != nil {
		t.Fatalf("replay RunInTx() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}
	if second.Replayed != true || !bytes.Equal(second.Body, first.Body) || second.Status != 201 {
		t.Fatalf("replay result = %+v, first = %+v", second, first)
	}
}

func TestRunInTxRollsBackMutationAndIdempotencyOnError(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	request := fixedRequest(seedTokenID, "rollback")
	wantErr := errors.New("domain failed")

	_, err := service.RunInTx(t.Context(), request, func(ctx context.Context, tx pgx.Tx) (Response, error) {
		if _, err := tx.Exec(ctx, "INSERT INTO repositories (key, display_name, created_by) VALUES ('rollback', 'Rollback', $1)", seedTokenID); err != nil {
			return Response{}, err
		}
		return Response{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunInTx() error = %v, want domain error", err)
	}

	var repositories, records int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM repositories WHERE key = 'rollback'").Scan(&repositories); err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records WHERE idempotency_key = 'rollback'").Scan(&records); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if repositories != 0 || records != 0 {
		t.Fatalf("repositories = %d, records = %d, want both 0", repositories, records)
	}
}

func TestRunInTxCompletesDeterministicErrorAndReplaysAfterServiceRestart(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	wantErr := errors.New("repository conflict")
	request := fixedRequest(seedTokenID, "terminal-conflict")
	request.RequestID = "request-terminal-conflict"
	request.ClassifyError = func(err error) (ErrorResponse, bool) {
		if errors.Is(err, wantErr) {
			return ErrorResponse{Status: 409, Title: "Conflict", Code: "conflict"}, true
		}
		return ErrorResponse{}, false
	}
	request.OnTerminal = func(ctx context.Context, tx pgx.Tx, err error) error {
		if !errors.Is(err, wantErr) {
			t.Fatalf("OnTerminal() error = %v, want conflict", err)
		}
		_, insertErr := tx.Exec(ctx, `INSERT INTO audit_events
			(action, resource_type, outcome, code, request_id, details)
			VALUES ('repository.create', 'repository', 'denied', 'conflict', $1, '{}')`, request.RequestID)
		return insertErr
	}

	_, err := service.RunInTx(t.Context(), request, func(ctx context.Context, tx pgx.Tx) (Response, error) {
		if _, insertErr := tx.Exec(ctx, "INSERT INTO repositories (key, display_name, created_by) VALUES ('terminal-rollback', 'Rollback', $1)", seedTokenID); insertErr != nil {
			return Response{}, insertErr
		}
		return Response{}, wantErr
	})
	completed, ok := CompletedErrorFrom(err)
	if !ok || !errors.Is(err, wantErr) {
		t.Fatalf("RunInTx() error = %v, want completed conflict", err)
	}
	if completed.Status != 409 || completed.Replayed {
		t.Fatalf("completed error = %+v", completed)
	}
	var problem struct {
		Code      string `json:"code"`
		RequestID string `json:"requestId"`
	}
	if decodeErr := json.Unmarshal(completed.Body, &problem); decodeErr != nil {
		t.Fatalf("decode completed problem: %v", decodeErr)
	}
	if problem.Code != "conflict" || problem.RequestID != request.RequestID {
		t.Fatalf("completed problem = %+v", problem)
	}

	var repositories, audits, completedRecords int
	if queryErr := pool.QueryRow(t.Context(), "SELECT count(*) FROM repositories WHERE key = 'terminal-rollback'").Scan(&repositories); queryErr != nil {
		t.Fatalf("count repositories: %v", queryErr)
	}
	if queryErr := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE request_id = $1", request.RequestID).Scan(&audits); queryErr != nil {
		t.Fatalf("count audits: %v", queryErr)
	}
	if queryErr := pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records WHERE idempotency_key = $1 AND state = 'completed'", request.Key).Scan(&completedRecords); queryErr != nil {
		t.Fatalf("count completed records: %v", queryErr)
	}
	if repositories != 0 || audits != 1 || completedRecords != 1 {
		t.Fatalf("repositories = %d, audits = %d, completed records = %d; want 0, 1, 1", repositories, audits, completedRecords)
	}

	restartedPool, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open restarted pool: %v", err)
	}
	defer restartedPool.Close()
	if err := restartedPool.Ping(t.Context()); err != nil {
		t.Fatalf("ping restarted pool: %v", err)
	}
	restarted := newTestService(t, restartedPool)
	called := false
	_, err = restarted.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		called = true
		return Response{}, errors.New("callback must not run after restart")
	})
	replayed, ok := CompletedErrorFrom(err)
	if !ok || !replayed.Replayed || replayed.Status != completed.Status || !bytes.Equal(replayed.Body, completed.Body) {
		t.Fatalf("replayed error = %+v, first = %+v, err = %v", replayed, completed, err)
	}
	if called {
		t.Fatal("callback ran while replaying completed deterministic error")
	}
	if queryErr := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE request_id = $1", request.RequestID).Scan(&audits); queryErr != nil {
		t.Fatalf("count replay audits: %v", queryErr)
	}
	if audits != 1 {
		t.Fatalf("audit events after replay = %d, want 1", audits)
	}
}

func TestRunInTxReplaysEncryptedSuccessAfterResponseLossAndServiceRestart(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	request := fixedRequest(seedTokenID, "token-response-after-restart")
	plaintext := []byte(`{"secret":"ar1.same-secret"}`)

	if _, err := newTestService(t, pool).RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		return Response{Status: 201, Body: plaintext, Encrypt: true}, nil
	}); err != nil {
		t.Fatalf("committed request before response loss: %v", err)
	}

	restartedPool, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open restarted pool: %v", err)
	}
	defer restartedPool.Close()
	if err := restartedPool.Ping(t.Context()); err != nil {
		t.Fatalf("ping restarted pool: %v", err)
	}
	called := false
	replayed, err := newTestService(t, restartedPool).RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		called = true
		return Response{}, errors.New("callback must not run after restart")
	})
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if called || !replayed.Replayed || replayed.Status != 201 || !bytes.Equal(replayed.Body, plaintext) {
		t.Fatalf("replayed = %+v, callback called = %v", replayed, called)
	}
}

func TestRunInTxStoresEncryptedResponseAndReplaysPlaintext(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	request := fixedRequest(seedTokenID, "token-response")
	plaintext := []byte(`{"secret":"ar1.sensitive"}`)

	first, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		return Response{Status: 201, Body: plaintext, Encrypt: true}, nil
	})
	if err != nil {
		t.Fatalf("RunInTx() error = %v", err)
	}
	if !bytes.Equal(first.Body, plaintext) {
		t.Fatalf("first body = %q", first.Body)
	}

	var stored []byte
	var encrypted bool
	if err := pool.QueryRow(t.Context(), "SELECT response_body, response_encrypted FROM idempotency_records WHERE idempotency_key = 'token-response'").Scan(&stored, &encrypted); err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !encrypted || bytes.Contains(stored, []byte("sensitive")) {
		t.Fatalf("stored encrypted = %v, body = %q", encrypted, stored)
	}

	replayed, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		return Response{}, errors.New("must not execute")
	})
	if err != nil {
		t.Fatalf("replay RunInTx() error = %v", err)
	}
	if !replayed.Replayed || !bytes.Equal(replayed.Body, plaintext) {
		t.Fatalf("replayed = %+v", replayed)
	}
}

func TestRunInTxRejectsReusedKeyWithDifferentFingerprint(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	request := fixedRequest(seedTokenID, "fingerprint-conflict")

	if _, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		return Response{Status: 201, Body: []byte(`{"id":"first"}`)}, nil
	}); err != nil {
		t.Fatalf("first RunInTx() error = %v", err)
	}

	request.Fingerprint = bytes.Repeat([]byte{0x22}, 32)
	called := false
	_, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		called = true
		return Response{}, nil
	})
	if !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("RunInTx() error = %v, want ErrKeyConflict", err)
	}
	if called {
		t.Fatal("callback ran for conflicting idempotency key")
	}
}

func TestRunInTxReportsCommittedPendingRecordAsInProgress(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	request := fixedRequest(seedTokenID, "pending")
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO idempotency_records
		 (token_id, http_method, canonical_resource, idempotency_key, request_fingerprint, state, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'pending', $6)`,
		request.TokenID,
		request.Method,
		request.CanonicalResource,
		request.Key,
		request.Fingerprint,
		time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert pending idempotency record: %v", err)
	}

	called := false
	_, err := service.RunInTx(t.Context(), request, func(context.Context, pgx.Tx) (Response, error) {
		called = true
		return Response{}, nil
	})
	if !errors.Is(err, ErrInProgress) {
		t.Fatalf("RunInTx() error = %v, want ErrInProgress", err)
	}
	if called {
		t.Fatal("callback ran for pending idempotency record")
	}
}

func TestRunInTxWithoutKeyRunsAndCommitsNormally(t *testing.T) {
	pool := testenv.StartPostgres(t)
	migrateAndSeedToken(t, pool)
	service := newTestService(t, pool)
	request := fixedRequest(seedTokenID, "")

	result, err := service.RunInTx(t.Context(), request, func(ctx context.Context, tx pgx.Tx) (Response, error) {
		_, err := tx.Exec(ctx, "INSERT INTO repositories (key, display_name, created_by) VALUES ('unkeyed', 'Unkeyed', $1)", seedTokenID)
		return Response{Status: 204}, err
	})
	if err != nil {
		t.Fatalf("RunInTx() error = %v", err)
	}
	if result.Status != 204 || result.Replayed {
		t.Fatalf("RunInTx() result = %+v", result)
	}

	var repositories, records int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM repositories WHERE key = 'unkeyed'").Scan(&repositories); err != nil {
		t.Fatalf("count repositories: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM idempotency_records").Scan(&records); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if repositories != 1 || records != 0 {
		t.Fatalf("repositories = %d, records = %d, want 1 and 0", repositories, records)
	}
}

var seedTokenID = uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")

func migrateAndSeedToken(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	serviceAccountID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	if _, err := pool.Exec(
		t.Context(),
		"INSERT INTO service_accounts (id, name) VALUES ($1, $2)",
		serviceAccountID,
		"test-admin",
	); err != nil {
		t.Fatalf("insert service account: %v", err)
	}

	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		seedTokenID,
		serviceAccountID,
		bytes.Repeat([]byte{1}, 32),
		[]string{"admin"},
		[]uuid.UUID{},
		time.Date(2027, 7, 11, 0, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}
}

func newTestService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()
	sealer, err := NewSealer(bytes.Repeat([]byte{0x42}, 32), bytes.NewReader(bytes.Repeat([]byte{0x24}, 1024)))
	if err != nil {
		t.Fatalf("NewSealer() error = %v", err)
	}
	return NewService(pool, sealer, func() time.Time {
		return time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	})
}

func fixedRequest(tokenID uuid.UUID, key string) Request {
	return Request{
		TokenID:           tokenID,
		Method:            "POST",
		CanonicalResource: "/api/v1/repositories",
		Key:               key,
		Fingerprint:       bytes.Repeat([]byte{0x11}, 32),
		TTL:               24 * time.Hour,
	}
}
