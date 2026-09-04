package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

const migration001Checksum = "4b670ee5af0ce8f92992a23c2d3d78a888c2aff34a7431e5208428c3f5ec766b"

var embeddedMigrationDefinitions = []migration{
	{
		version:  1,
		name:     "initial",
		checksum: migration001Checksum,
	},
}

func loadEmbeddedMigrations() ([]migration, error) {
	migrations := append([]migration(nil), embeddedMigrationDefinitions...)
	for i := range migrations {
		path := fmt.Sprintf("migrations/%03d_%s.sql", migrations[i].version, migrations[i].name)
		contents, errRead := migrationFiles.ReadFile(path)
		if errRead != nil {
			return nil, fmt.Errorf("sqlite store: read embedded migration %s: %w", path, errRead)
		}
		migrations[i].sql = string(contents)
	}
	return migrations, nil
}

func (s *Store) applyMigrations(ctx context.Context, migrations []migration) error {
	if _, errExec := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at INTEGER NOT NULL
		) STRICT
	`); errExec != nil {
		return fmt.Errorf("sqlite store: create migration ledger: %w", classifyError(errExec))
	}

	applied, errApplied := loadAppliedMigrations(ctx, s.db)
	if errApplied != nil {
		return errApplied
	}
	if errValidate := validateMigrationHistory(applied, migrations); errValidate != nil {
		return errValidate
	}
	for _, next := range migrations {
		if _, ok := applied[next.version]; ok {
			continue
		}
		if checksum(next.sql) != next.checksum {
			return fmt.Errorf("sqlite store: embedded migration %d checksum mismatch: %w", next.version, domain.ErrMigration)
		}
		tx, errBegin := s.db.BeginTx(ctx, nil)
		if errBegin != nil {
			return fmt.Errorf("sqlite store: begin migration %d: %w", next.version, classifyError(errBegin))
		}
		if _, errExec := tx.ExecContext(ctx, next.sql); errExec != nil {
			return rollback(tx, fmt.Errorf("sqlite store: apply migration %d: %w", next.version, classifyError(errExec)))
		}
		if _, errRecord := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			next.version, next.name, next.checksum, toDatabaseTime(s.currentTime())); errRecord != nil {
			return rollback(tx, fmt.Errorf("sqlite store: record migration %d: %w", next.version, classifyError(errRecord)))
		}
		if errCommit := tx.Commit(); errCommit != nil {
			return fmt.Errorf("sqlite store: commit migration %d: %w", next.version, classifyError(errCommit))
		}
	}
	return nil
}

func loadAppliedMigrations(ctx context.Context, db *sql.DB) (applied map[int]migration, err error) {
	rows, errQuery := db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if errQuery != nil {
		return nil, fmt.Errorf("sqlite store: query migration history: %w", classifyError(errQuery))
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("sqlite store: close migration rows: %w", errClose))
		}
	}()

	applied = make(map[int]migration)
	for rows.Next() {
		var item migration
		if errScan := rows.Scan(&item.version, &item.name, &item.checksum); errScan != nil {
			return nil, fmt.Errorf("sqlite store: scan migration history: %w", errScan)
		}
		applied[item.version] = item
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("sqlite store: iterate migration history: %w", errRows)
	}
	return applied, nil
}

func validateMigrationHistory(applied map[int]migration, available []migration) error {
	for index, item := range available {
		wantVersion := index + 1
		if item.version != wantVersion {
			return fmt.Errorf("sqlite store: migrations are not contiguous at version %d: %w", item.version, domain.ErrMigration)
		}
		if checksum(item.sql) != item.checksum {
			return fmt.Errorf("sqlite store: embedded migration %d checksum mismatch: %w", item.version, domain.ErrMigration)
		}
	}
	for version, recorded := range applied {
		if version < 1 || version > len(available) {
			return fmt.Errorf("sqlite store: database schema version %d is newer than supported version %d: %w", version, len(available), domain.ErrMigration)
		}
		want := available[version-1]
		if recorded.name != want.name || recorded.checksum != want.checksum {
			return fmt.Errorf("sqlite store: migration %d does not match embedded history: %w", version, domain.ErrMigration)
		}
		if version > 1 {
			if _, ok := applied[version-1]; !ok {
				return fmt.Errorf("sqlite store: migration history has a gap before version %d: %w", version, domain.ErrMigration)
			}
		}
	}
	return nil
}

func checksum(contents string) string {
	sum := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(sum[:])
}

func rollback(tx *sql.Tx, operationErr error) error {
	if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
		return errors.Join(operationErr, fmt.Errorf("sqlite store: rollback transaction: %w", errRollback))
	}
	return operationErr
}
