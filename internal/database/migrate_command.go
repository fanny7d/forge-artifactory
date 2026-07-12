package database

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrSchemaVersionMismatch = errors.New("database schema version mismatch")

func RunMigrateCommand(ctx context.Context, databaseURL string) error {
	if strings.TrimSpace(databaseURL) == "" {
		return fmt.Errorf("run migrate command: database URL is required")
	}
	pool, err := Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		return err
	}
	if err := CheckSchemaVersion(ctx, pool); err != nil {
		return fmt.Errorf("verify migrated schema: %w", err)
	}
	return nil
}

func CheckSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	current, err := appliedSchemaVersions(ctx, pool)
	if err != nil {
		return err
	}
	expected, err := embeddedSchemaVersions()
	if err != nil {
		return err
	}
	if !slices.Equal(current, expected) {
		return fmt.Errorf("%w: database has versions %v; binary requires %v", ErrSchemaVersionMismatch, current, expected)
	}
	return nil
}

func SchemaVersions(ctx context.Context, pool *pgxpool.Pool) (current, expected int64, err error) {
	currentVersions, err := appliedSchemaVersions(ctx, pool)
	if err != nil {
		return 0, 0, err
	}
	expectedVersions, err := embeddedSchemaVersions()
	if err != nil {
		return 0, 0, err
	}
	if len(currentVersions) != 0 {
		current = currentVersions[len(currentVersions)-1]
	}
	if len(expectedVersions) != 0 {
		expected = expectedVersions[len(expectedVersions)-1]
	}
	return current, expected, nil
}

func appliedSchemaVersions(ctx context.Context, pool *pgxpool.Pool) ([]int64, error) {
	if pool == nil {
		return nil, fmt.Errorf("check schema version: pool is nil")
	}
	var versionTableExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.goose_db_version') IS NOT NULL").Scan(&versionTableExists); err != nil {
		return nil, fmt.Errorf("check schema version table: %w", err)
	}
	if !versionTableExists {
		return []int64{}, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT version_id
		FROM (
			SELECT DISTINCT ON (version_id) version_id, is_applied
			FROM goose_db_version
			WHERE version_id > 0
			ORDER BY version_id, id DESC
		) latest
		WHERE is_applied
		ORDER BY version_id`)
	if err != nil {
		return nil, fmt.Errorf("query applied schema versions: %w", err)
	}
	defer rows.Close()
	versions := make([]int64, 0)
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied schema version: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied schema versions: %w", err)
	}
	return versions, nil
}

func embeddedSchemaVersions() ([]int64, error) {
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list embedded migrations: %w", err)
	}
	versions := make([]int64, 0, len(files))
	seen := make(map[int64]struct{}, len(files))
	for _, filename := range files {
		base := path.Base(filename)
		prefix, _, found := strings.Cut(base, "_")
		if !found || prefix == "" {
			return nil, fmt.Errorf("parse embedded migration filename %q", filename)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("parse embedded migration version %q", prefix)
		}
		if _, exists := seen[version]; exists {
			return nil, fmt.Errorf("duplicate embedded migration version %d", version)
		}
		seen[version] = struct{}{}
		versions = append(versions, version)
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("no embedded migrations found")
	}
	sort.Slice(versions, func(left, right int) bool { return versions[left] < versions[right] })
	return versions, nil
}
