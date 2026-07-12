package bootstrap

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/auth"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/clock"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/database"
	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestBootstrapAdminOnlySucceedsOnce(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	tokenID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	pepper := bytes.Repeat([]byte{0x44}, 32)
	firstOutput := new(bytes.Buffer)
	result, err := BootstrapAdmin(t.Context(), AdminOptions{
		Pool: pool, Name: "bootstrap-admin", Pepper: pepper,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)),
		IDs:    &fixedIDs{values: []uuid.UUID{tokenID}},
		Clock:  clock.Fixed{Time: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)},
		Output: firstOutput,
	})
	if err != nil {
		t.Fatalf("BootstrapAdmin(first) error = %v", err)
	}
	if result.ServiceAccountID == uuid.Nil || result.TokenID != tokenID || strings.TrimSpace(firstOutput.String()) != result.Bearer {
		t.Fatalf("bootstrap result = %+v output = %q", result, firstOutput.String())
	}
	parsedID, secret, err := auth.ParseBearer(result.Bearer)
	var storedHMAC []byte
	if queryErr := pool.QueryRow(t.Context(), "SELECT secret_hmac FROM api_tokens WHERE id = $1", tokenID).Scan(&storedHMAC); queryErr != nil {
		t.Fatalf("load bootstrap token HMAC: %v", queryErr)
	}
	if err != nil || parsedID != tokenID || !auth.VerifySecret(pepper, parsedID, secret, storedHMAC) {
		t.Fatalf("bootstrap bearer verification: id=%s err=%v", parsedID, err)
	}
	var accounts, tokens, audits int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM service_accounts").Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM api_tokens WHERE id = $1 AND 'admin' = ANY(scopes)", tokenID).Scan(&tokens); err != nil {
		t.Fatalf("count admin token: %v", err)
	}
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_events WHERE action = 'bootstrap-admin' AND outcome = 'success'").Scan(&audits); err != nil {
		t.Fatalf("count bootstrap audit: %v", err)
	}
	if accounts != 1 || tokens != 1 || audits != 1 {
		t.Fatalf("bootstrap counts = accounts %d tokens %d audits %d", accounts, tokens, audits)
	}

	secondOutput := new(bytes.Buffer)
	_, err = BootstrapAdmin(t.Context(), AdminOptions{
		Pool: pool, Name: "second-admin", Pepper: pepper,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x66}, 32)), IDs: &fixedIDs{values: []uuid.UUID{uuid.New(), uuid.New()}},
		Clock: clock.Fixed{Time: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}, Output: secondOutput,
	})
	if !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("BootstrapAdmin(second) error = %v, want ErrAlreadyBootstrapped", err)
	}
	if secondOutput.Len() != 0 {
		t.Fatalf("second bootstrap output = %q, want empty", secondOutput.String())
	}
}

type fixedIDs struct {
	values []uuid.UUID
	index  int
}

func (g *fixedIDs) New() uuid.UUID {
	if g.index >= len(g.values) {
		return uuid.Nil
	}
	value := g.values[g.index]
	g.index++
	return value
}
