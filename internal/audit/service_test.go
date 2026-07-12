package audit

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestRecordParticipatesInCallerTransactionAndListReturnsEvent(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	serviceAccountID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	tokenID := uuid.MustParse("bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	if _, err := pool.Exec(t.Context(), "INSERT INTO service_accounts (id, name) VALUES ($1, 'audit-admin')", serviceAccountID); err != nil {
		t.Fatalf("insert service account: %v", err)
	}
	if _, err := pool.Exec(
		t.Context(),
		`INSERT INTO api_tokens
		 (id, service_account_id, secret_hmac, scopes, repository_ids, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tokenID,
		serviceAccountID,
		bytes.Repeat([]byte{1}, 32),
		[]string{"admin"},
		[]uuid.UUID{},
		time.Date(2027, 7, 11, 0, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	service := NewService(pool)
	event := Event{
		ActorTokenID: tokenID,
		Action:       "service-account.create",
		ResourceType: "service_account",
		ResourceID:   "cccccccc-dddd-4eee-8fff-000000000000",
		Outcome:      OutcomeSuccess,
		RequestID:    "request-123",
		Details:      map[string]any{"name": "edgecli-ci"},
	}

	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	if _, err := service.Record(t.Context(), tx, event); err != nil {
		t.Fatalf("Record() before rollback error = %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}

	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin commit transaction: %v", err)
	}
	created, err := service.Record(t.Context(), tx, event)
	if err != nil {
		t.Fatalf("Record() before commit error = %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	tx, err = pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin second transaction: %v", err)
	}
	event.Action = "token.create"
	newer, err := service.Record(t.Context(), tx, event)
	if err != nil {
		t.Fatalf("second Record() error = %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit second transaction: %v", err)
	}

	page, err := service.List(t.Context(), ListRequest{Limit: 1})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != newer.ID || page.Next == nil {
		t.Fatalf("List() page = %+v", page)
	}
	if page.Items[0].Details["name"] != "edgecli-ci" {
		t.Fatalf("event details = %#v", page.Items[0].Details)
	}

	next, err := service.List(t.Context(), ListRequest{Limit: 1, After: page.Next})
	if err != nil {
		t.Fatalf("List() next page error = %v", err)
	}
	if len(next.Items) != 1 || next.Items[0].ID != created.ID || next.Next != nil {
		t.Fatalf("List() next page = %+v", next)
	}
}
