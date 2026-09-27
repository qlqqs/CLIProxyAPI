package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

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
const migration002Checksum = "60df97c5bb326f4e2216e54f47910e5654dd56bdd83cac0cfd0802efe1a77423"
const migration003Checksum = "eb1af0994dc20b154360ccbe69b9365c50a9f3c7a9a71ac5a2382882015fb8d5"

const migration004Checksum = "9d5cfedf1bbaab5240f9dd5e4522f6ed69c44f6502240ee8dfc149e978f04134"
const migration005Checksum = "0b882060587a09d09ae4ba6b04626babb28aad57a912253d977c4c13c0917395"
const migration007Checksum = "6115184df6f1e3211f570f03d9ed1ea81bca617ee87786f82ac07a90fb9936c5"
const migration006Checksum = "c3c2f4e5d12ee836422ebf142257d69b3674b5ad49e5caf3bb8505611fae82c8"

var embeddedMigrationDefinitions = []migration{
	{
		version:  1,
		name:     "initial",
		checksum: migration001Checksum,
	},
	{
		version:  2,
		name:     "billing",
		checksum: migration002Checksum,
	},
	{
		version:  3,
		name:     "retention",
		checksum: migration003Checksum,
	},
	{version: 4, name: "quota", checksum: migration004Checksum},
	{version: 5, name: "api_key_tokens", checksum: migration005Checksum},
	{version: 6, name: "local_quotas", checksum: migration006Checksum},
	{version: 7, name: "quota_window_fees", checksum: migration007Checksum},
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
		if next.version == 6 {
			if errBackfill := backfillLocalQuotaWindows(ctx, tx, s.currentTime(), s.location); errBackfill != nil {
				return rollback(tx, fmt.Errorf("sqlite store: backfill local quota windows: %w", errBackfill))
			}
		}
		if next.version == 7 {
			if errBackfill := backfillQuotaWindowFees(ctx, tx); errBackfill != nil {
				return rollback(tx, fmt.Errorf("sqlite store: backfill quota window fees: %w", errBackfill))
			}
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

func backfillLocalQuotaWindows(ctx context.Context, tx *sql.Tx, at time.Time, location *time.Location) error {
	if location == nil {
		location = time.UTC
	}
	for _, item := range []struct {
		kind   string
		column string
	}{{domain.QuotaWindowFiveHour, "five_hour_window_id"}, {domain.QuotaWindowWeekly, "weekly_window_id"}} {
		query := `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,unknown_cost_events,updated_at)
			SELECT f.membership_id,?,MIN(w.window_from),MAX(w.reset_at),SUM(f.confirmed_nano_usd),SUM(f.unknown_cost_events),?
			FROM quota_request_fees f JOIN quota_windows w ON w.id=f.` + item.column + ` AND w.kind=?
			WHERE w.reset_at>?
			GROUP BY f.membership_id`
		if _, errExec := tx.ExecContext(ctx, query, item.kind, toDatabaseTime(at), item.kind, toDatabaseTime(at)); errExec != nil {
			return classifyError(errExec)
		}
		if errBackfill := backfillUnboundQuotaFees(ctx, tx, item.kind, item.column, at, location); errBackfill != nil {
			return errBackfill
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
