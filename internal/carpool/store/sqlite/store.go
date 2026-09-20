package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	moderncsqlite "modernc.org/sqlite"
)

const (
	defaultBusyTimeout        = 5 * time.Second
	defaultMaxOpenConnections = 4
)

type Config struct {
	Path               string
	BusyTimeout        time.Duration
	MaxOpenConnections int
	Now                func() time.Time
	Location           *time.Location
}

type Store struct {
	db       *sql.DB
	now      func() time.Time
	location *time.Location
}

func Open(ctx context.Context, cfg Config) (*Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil, fmt.Errorf("sqlite store: database path: %w", domain.ErrInvalid)
	}
	absPath, errAbs := filepath.Abs(path)
	if errAbs != nil {
		return nil, fmt.Errorf("sqlite store: resolve database path: %w", errAbs)
	}

	busyTimeout := cfg.BusyTimeout
	if busyTimeout == 0 {
		busyTimeout = defaultBusyTimeout
	}
	if busyTimeout < time.Millisecond {
		return nil, fmt.Errorf("sqlite store: busy timeout must be at least one millisecond: %w", domain.ErrInvalid)
	}
	maxOpenConnections := cfg.MaxOpenConnections
	if maxOpenConnections == 0 {
		maxOpenConnections = defaultMaxOpenConnections
	}
	if maxOpenConnections < 1 {
		return nil, fmt.Errorf("sqlite store: max open connections must be positive: %w", domain.ErrInvalid)
	}
	if errPrepare := prepareDatabaseFile(absPath); errPrepare != nil {
		return nil, errPrepare
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	dsn := sqliteDSN(absPath, busyTimeout)
	db, errOpen := sql.Open("sqlite", dsn)
	if errOpen != nil {
		return nil, fmt.Errorf("sqlite store: open database: %w", classifyError(errOpen))
	}
	db.SetMaxOpenConns(maxOpenConnections)
	db.SetMaxIdleConns(maxOpenConnections)

	location := cfg.Location
	if location == nil {
		location = time.UTC
	}
	store := &Store{db: db, now: now, location: location}
	if errPing := db.PingContext(ctx); errPing != nil {
		return nil, closeAfterOpenError(db, fmt.Errorf("sqlite store: ping database: %w", classifyError(errPing)))
	}
	if errMode := os.Chmod(absPath, 0o600); errMode != nil {
		return nil, closeAfterOpenError(db, fmt.Errorf("sqlite store: set database permissions: %w", errMode))
	}
	if errPragmas := store.verifyConnectionSettings(ctx, busyTimeout); errPragmas != nil {
		return nil, closeAfterOpenError(db, errPragmas)
	}
	migrations, errMigrations := loadEmbeddedMigrations()
	if errMigrations != nil {
		return nil, closeAfterOpenError(db, errMigrations)
	}
	if errMigrate := store.applyMigrations(ctx, migrations); errMigrate != nil {
		return nil, closeAfterOpenError(db, errMigrate)
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if errClose := s.db.Close(); errClose != nil {
		return fmt.Errorf("sqlite store: close database: %w", errClose)
	}
	return nil
}

func (s *Store) Checkpoint(ctx context.Context) error {
	if errReady := s.ready(); errReady != nil {
		return errReady
	}
	var busy, logFrames, checkpointedFrames int
	if errQuery := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); errQuery != nil {
		return fmt.Errorf("sqlite store: checkpoint WAL: %w", classifyError(errQuery))
	}
	if busy != 0 {
		return fmt.Errorf("sqlite store: checkpoint WAL reported busy: %w", domain.ErrBusy)
	}
	return nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	if errReady := s.ready(); errReady != nil {
		return 0, errReady
	}
	var version sql.NullInt64
	if errQuery := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&version); errQuery != nil {
		return 0, fmt.Errorf("sqlite store: read schema version: %w", classifyError(errQuery))
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

func (s *Store) ready() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sqlite store: not initialized: %w", domain.ErrInvalid)
	}
	return nil
}

func (s *Store) currentTime() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func sqliteDSN(path string, busyTimeout time.Duration) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("_busy_timeout", strconv.FormatInt(busyTimeout.Milliseconds(), 10))
	query.Set("_defensive", "1")
	query.Set("_dqs", "0")
	query.Set("_foreign_keys", "1")
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "NORMAL")
	query.Set("_txlock", "immediate")
	u.RawQuery = query.Encode()
	return u.String()
}

func prepareDatabaseFile(path string) error {
	parent := filepath.Dir(path)
	createdParent := false
	if _, errStat := os.Stat(parent); errors.Is(errStat, os.ErrNotExist) {
		createdParent = true
	} else if errStat != nil {
		return fmt.Errorf("sqlite store: inspect database directory: %w", errStat)
	}
	if errMkdir := os.MkdirAll(parent, 0o700); errMkdir != nil {
		return fmt.Errorf("sqlite store: create database directory: %w", errMkdir)
	}
	if createdParent {
		if errMode := os.Chmod(parent, 0o700); errMode != nil {
			return fmt.Errorf("sqlite store: set database directory permissions: %w", errMode)
		}
	}
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if errOpen != nil {
		return fmt.Errorf("sqlite store: create database file: %w", errOpen)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("sqlite store: close database file: %w", errClose)
	}
	if errMode := os.Chmod(path, 0o600); errMode != nil {
		return fmt.Errorf("sqlite store: set database file permissions: %w", errMode)
	}
	return nil
}

func (s *Store) verifyConnectionSettings(ctx context.Context, busyTimeout time.Duration) (err error) {
	conn, errConn := s.db.Conn(ctx)
	if errConn != nil {
		return fmt.Errorf("sqlite store: acquire connection: %w", classifyError(errConn))
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("sqlite store: close verified connection: %w", errClose))
		}
	}()

	checks := []struct {
		pragma string
		want   string
	}{
		{pragma: "foreign_keys", want: "1"},
		{pragma: "journal_mode", want: "wal"},
		{pragma: "busy_timeout", want: strconv.FormatInt(busyTimeout.Milliseconds(), 10)},
		{pragma: "synchronous", want: "1"},
	}
	for _, check := range checks {
		var got string
		if errQuery := conn.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&got); errQuery != nil {
			return fmt.Errorf("sqlite store: read %s pragma: %w", check.pragma, classifyError(errQuery))
		}
		if !strings.EqualFold(got, check.want) {
			return fmt.Errorf("sqlite store: %s pragma is %q, want %q: %w", check.pragma, got, check.want, domain.ErrInvalid)
		}
	}
	return nil
}

func closeAfterOpenError(db *sql.DB, operationErr error) error {
	if errClose := db.Close(); errClose != nil {
		return errors.Join(operationErr, fmt.Errorf("sqlite store: close after open failure: %w", errClose))
	}
	return operationErr
}

func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return err
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6:
		return fmt.Errorf("%w: %v", domain.ErrBusy, err)
	case 19:
		return fmt.Errorf("%w: %v", domain.ErrConflict, err)
	default:
		return err
	}
}
