package database

import (
	"testing"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestInitialMigrationIsRepeatable(t *testing.T) {
	pool := testenv.StartPostgres(t)

	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	required := []string{
		"repositories",
		"service_accounts",
		"api_tokens",
		"audit_events",
		"idempotency_records",
		"blobs",
		"upload_sessions",
		"artifacts",
		"packages",
		"releases",
		"release_artifacts",
		"publish_attempts",
		"release_manifests",
		"channels",
		"channel_revisions",
		"jobs",
		"signing_keys",
	}

	rows, err := pool.Query(t.Context(), "SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	found := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	for _, name := range required {
		if !found[name] {
			t.Errorf("required table %q is missing", name)
		}
	}
}
