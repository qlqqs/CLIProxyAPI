package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestOpenAppliesMigrationAndConnectionSettings(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "private", "carpool.db")
	store, errOpen := Open(ctx, Config{
		Path:               databasePath,
		BusyTimeout:        1234 * time.Millisecond,
		MaxOpenConnections: 4,
	})
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Errorf("Close() error = %v", errClose)
		}
	})

	version, errVersion := store.SchemaVersion(ctx)
	if errVersion != nil {
		t.Fatalf("SchemaVersion() error = %v", errVersion)
	}
	if version != 3 {
		t.Fatalf("SchemaVersion() = %d, want 3", version)
	}
	var recordedChecksum string
	if errQuery := store.db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version = 1").Scan(&recordedChecksum); errQuery != nil {
		t.Fatalf("read migration checksum: %v", errQuery)
	}
	if recordedChecksum != migration001Checksum {
		t.Fatalf("migration checksum = %q, want %q", recordedChecksum, migration001Checksum)
	}

	connections := make([]*sql.Conn, 0, 4)
	defer func() {
		for _, conn := range connections {
			if errClose := conn.Close(); errClose != nil {
				t.Errorf("connection Close() error = %v", errClose)
			}
		}
	}()
	for i := 0; i < 4; i++ {
		conn, errConn := store.db.Conn(ctx)
		if errConn != nil {
			t.Fatalf("db.Conn(%d) error = %v", i, errConn)
		}
		connections = append(connections, conn)
		assertPragma(t, ctx, conn, "foreign_keys", "1")
		assertPragma(t, ctx, conn, "journal_mode", "wal")
		assertPragma(t, ctx, conn, "busy_timeout", "1234")
		assertPragma(t, ctx, conn, "synchronous", "1")
	}

	wantTables := []string{
		"audit_events", "billing_periods", "car_auth_assignments", "carpool_billing_settings", "cars", "memberships", "pricing_catalogs", "proxy_request_auth_scopes",
		"proxy_requests", "retention_jobs", "schema_migrations", "sessions", "usage_events", "user_api_keys", "users",
	}
	rows, errQuery := connections[0].QueryContext(ctx, `
		SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name
	`)
	if errQuery != nil {
		t.Fatalf("list schema tables: %v", errQuery)
	}
	var gotTables []string
	for rows.Next() {
		var name string
		if errScan := rows.Scan(&name); errScan != nil {
			t.Fatalf("scan schema table: %v", errScan)
		}
		gotTables = append(gotTables, name)
	}
	if errRows := rows.Err(); errRows != nil {
		t.Fatalf("iterate schema tables: %v", errRows)
	}
	if errClose := rows.Close(); errClose != nil {
		t.Fatalf("close schema table rows: %v", errClose)
	}
	if !equalStrings(gotTables, wantTables) {
		t.Fatalf("schema tables = %v, want %v", gotTables, wantTables)
	}

	assertMode(t, filepath.Dir(databasePath), 0o700)
	assertMode(t, databasePath, 0o600)
}

func TestMigrationValidationRejectsChangedAndNewerHistory(t *testing.T) {
	t.Run("checksum mismatch", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "carpool.db")
		store := openTestStore(t, path, nil)
		if _, errExec := store.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'changed' WHERE version = 1"); errExec != nil {
			t.Fatalf("corrupt migration checksum: %v", errExec)
		}
		closeTestStore(t, store)

		_, errOpen := Open(ctx, Config{Path: path})
		if !errors.Is(errOpen, domain.ErrMigration) {
			t.Fatalf("Open() error = %v, want ErrMigration", errOpen)
		}
	})

	t.Run("newer schema", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "carpool.db")
		store := openTestStore(t, path, nil)
		if _, errExec := store.db.ExecContext(ctx, `
			INSERT INTO schema_migrations(version, name, checksum, applied_at)
			VALUES (4, 'future', 'future', ?)
		`, toDatabaseTime(time.Now())); errExec != nil {
			t.Fatalf("insert future migration: %v", errExec)
		}
		closeTestStore(t, store)

		_, errOpen := Open(ctx, Config{Path: path})
		if !errors.Is(errOpen, domain.ErrMigration) {
			t.Fatalf("Open() error = %v, want ErrMigration", errOpen)
		}
	})
}

func TestFailedMigrationIsAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "carpool.db")
	if errPrepare := prepareDatabaseFile(path); errPrepare != nil {
		t.Fatalf("prepareDatabaseFile() error = %v", errPrepare)
	}
	db, errOpen := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if errOpen != nil {
		t.Fatalf("sql.Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := db.Close(); errClose != nil {
			t.Errorf("db.Close() error = %v", errClose)
		}
	})
	store := &Store{db: db, now: time.Now}
	brokenSQL := "CREATE TABLE should_rollback (id INTEGER PRIMARY KEY) STRICT; INSERT INTO missing_table(id) VALUES (1);"
	errMigrate := store.applyMigrations(ctx, []migration{{
		version: 1, name: "broken", checksum: checksum(brokenSQL), sql: brokenSQL,
	}})
	if errMigrate == nil {
		t.Fatal("applyMigrations() error = nil, want failure")
	}
	var tableCount int
	if errQuery := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_rollback'
	`).Scan(&tableCount); errQuery != nil {
		t.Fatalf("query rolled back table: %v", errQuery)
	}
	if tableCount != 0 {
		t.Fatalf("rolled back table count = %d, want 0", tableCount)
	}
	var migrationCount int
	if errQuery := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); errQuery != nil {
		t.Fatalf("query migration ledger: %v", errQuery)
	}
	if migrationCount != 0 {
		t.Fatalf("migration ledger count = %d, want 0", migrationCount)
	}
}

func TestCheckpointStoppedCopyAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "carpool.db")
	backupPath := filepath.Join(dir, "backup.db")
	store := openTestStore(t, path, nil)
	car, errCreate := store.CreateCar(ctx, domain.Car{Name: "backup marker", Status: domain.CarStatusActive})
	if errCreate != nil {
		t.Fatalf("CreateCar() error = %v", errCreate)
	}
	if errCheckpoint := store.Checkpoint(ctx); errCheckpoint != nil {
		t.Fatalf("Checkpoint() error = %v", errCheckpoint)
	}
	closeTestStore(t, store)
	copyFile(t, path, backupPath)

	restored := openTestStore(t, backupPath, nil)
	defer closeTestStore(t, restored)
	got, errGet := restored.GetCar(ctx, car.ID)
	if errGet != nil {
		t.Fatalf("restored GetCar() error = %v", errGet)
	}
	if got.Name != "backup marker" {
		t.Fatalf("restored car name = %q, want backup marker", got.Name)
	}
}

func TestUsageSchemaDoesNotDuplicateCarOrUser(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)
	rows, errQuery := store.db.QueryContext(context.Background(), "PRAGMA table_info(usage_events)")
	if errQuery != nil {
		t.Fatalf("PRAGMA table_info error = %v", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			t.Errorf("rows.Close() error = %v", errClose)
		}
	}()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if errScan := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); errScan != nil {
			t.Fatalf("scan usage column: %v", errScan)
		}
		if name == "car_id" || name == "user_id" {
			t.Fatalf("usage_events unexpectedly contains %s", name)
		}
	}
	if errRows := rows.Err(); errRows != nil {
		t.Fatalf("iterate usage columns: %v", errRows)
	}
}

func assertPragma(t *testing.T, ctx context.Context, conn *sql.Conn, pragma, want string) {
	t.Helper()
	var got string
	if errQuery := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); errQuery != nil {
		t.Fatalf("query PRAGMA %s: %v", pragma, errQuery)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %q, want %q", pragma, got, want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("os.Stat(%q) error = %v", path, errStat)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %q = %o, want %o", path, got, want)
	}
}

func equalStrings(got, want []string) bool {
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	sourceFile, errOpen := os.Open(source)
	if errOpen != nil {
		t.Fatalf("open backup source: %v", errOpen)
	}
	defer func() {
		if errClose := sourceFile.Close(); errClose != nil {
			t.Errorf("source Close() error = %v", errClose)
		}
	}()
	destinationFile, errCreate := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errCreate != nil {
		t.Fatalf("create backup destination: %v", errCreate)
	}
	if _, errCopy := io.Copy(destinationFile, sourceFile); errCopy != nil {
		_ = destinationFile.Close()
		t.Fatalf("copy backup: %v", errCopy)
	}
	if errClose := destinationFile.Close(); errClose != nil {
		t.Fatalf("destination Close() error = %v", errClose)
	}
}

func openTestStore(t *testing.T, path string, now func() time.Time) *Store {
	t.Helper()
	store, errOpen := Open(context.Background(), Config{Path: path, Now: now})
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	return store
}

func closeTestStore(t *testing.T, store *Store) {
	t.Helper()
	if errClose := store.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
}
