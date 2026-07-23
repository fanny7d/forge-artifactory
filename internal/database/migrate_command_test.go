package database

import (
	"errors"
	"testing"

	"superfan.myasustor.com/fanchao/artifact-repository/internal/testenv"
)

func TestMigrateCommandIsRepeatableAndProducesExpectedSchemaVersion(t *testing.T) {
	pool := testenv.StartPostgres(t)
	databaseURL := pool.Config().ConnString()

	if err := RunMigrateCommand(t.Context(), databaseURL); err != nil {
		t.Fatalf("RunMigrateCommand(first) error = %v", err)
	}
	if err := RunMigrateCommand(t.Context(), databaseURL); err != nil {
		t.Fatalf("RunMigrateCommand(second) error = %v", err)
	}
	if err := CheckSchemaVersion(t.Context(), pool); err != nil {
		t.Fatalf("CheckSchemaVersion() error = %v", err)
	}
	current, expected, err := SchemaVersions(t.Context(), pool)
	if err != nil {
		t.Fatalf("SchemaVersions() error = %v", err)
	}
	if current != expected || expected != 2 {
		t.Fatalf("schema versions = current %d expected %d, want 2/2", current, expected)
	}
}

func TestMigrateCommandSchemaCheckRejectsUnmigratedAndFutureDatabase(t *testing.T) {
	pool := testenv.StartPostgres(t)
	if err := CheckSchemaVersion(t.Context(), pool); !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("CheckSchemaVersion(unmigrated) error = %v, want ErrSchemaVersionMismatch", err)
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, true)",
	); err != nil {
		t.Fatalf("insert future schema version: %v", err)
	}
	if err := CheckSchemaVersion(t.Context(), pool); !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("CheckSchemaVersion(future) error = %v, want ErrSchemaVersionMismatch", err)
	}
}
