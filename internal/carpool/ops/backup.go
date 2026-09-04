// Package ops provides offline operational commands for the carpool database.
package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
)

const (
	manifestFormatVersion = 1
	manifestSuffix        = ".manifest.json"
	maximumManifestSize   = 64 << 10
)

// BackupManifest describes one stopped SQLite backup and its integrity data.
type BackupManifest struct {
	FormatVersion  int       `json:"format_version"`
	CreatedAt      time.Time `json:"created_at"`
	BinaryVersion  string    `json:"binary_version"`
	SchemaVersion  int       `json:"schema_version"`
	DatabaseFile   string    `json:"database_file"`
	DatabaseSize   int64     `json:"database_size"`
	DatabaseSHA256 string    `json:"database_sha256"`
}

// BackupOptions configures a stopped database backup.
type BackupOptions struct {
	DatabasePath  string
	BackupPath    string
	BinaryVersion string
	Now           func() time.Time
}

// ManifestPath returns the sidecar manifest path for a backup database.
func ManifestPath(backupPath string) string {
	return strings.TrimSpace(backupPath) + manifestSuffix
}

// RequireStopped rejects a database that still has SQLite WAL coordination files.
func RequireStopped(databasePath string) error {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" {
		return fmt.Errorf("carpool operation: database path is required")
	}
	databaseAbs, errAbs := filepath.Abs(filepath.Clean(databasePath))
	if errAbs != nil {
		return fmt.Errorf("carpool operation: resolve database path: %w", errAbs)
	}
	return requireNoSQLiteSidecars(databaseAbs)
}

// BackupStopped checkpoints and closes the source before copying its main file.
func BackupStopped(ctx context.Context, options BackupOptions) (manifest BackupManifest, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	databasePath, backupPath, errPaths := resolveDistinctPaths(options.DatabasePath, options.BackupPath)
	if errPaths != nil {
		return BackupManifest{}, errPaths
	}
	if errSource := requireRegularFile(databasePath); errSource != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: inspect source database: %w", errSource)
	}
	manifestPath := ManifestPath(backupPath)
	if errAvailable := requireDestinationAvailable(backupPath, manifestPath); errAvailable != nil {
		return BackupManifest{}, errAvailable
	}

	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: databasePath})
	if errOpen != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: open source database: %w", errOpen)
	}
	defer func() {
		if store != nil {
			if errClose := store.Close(); errClose != nil {
				err = errors.Join(err, fmt.Errorf("carpool backup: close source database: %w", errClose))
			}
		}
	}()
	schemaVersion, errSchema := store.SchemaVersion(ctx)
	if errSchema != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: read schema version: %w", errSchema)
	}
	if errCheckpoint := store.Checkpoint(ctx); errCheckpoint != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: checkpoint database: %w", errCheckpoint)
	}
	if errClose := store.Close(); errClose != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: close source database: %w", errClose)
	}
	store = nil
	if errStopped := requireNoSQLiteSidecars(databasePath); errStopped != nil {
		return BackupManifest{}, errStopped
	}

	if errDirectory := ensurePrivateDirectory(filepath.Dir(backupPath)); errDirectory != nil {
		return BackupManifest{}, errDirectory
	}
	databaseSize, checksum, errCopy := copyFileExclusive(ctx, databasePath, backupPath)
	if errCopy != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: copy database: %w", errCopy)
	}
	backupCreated := true
	manifestCreated := false
	defer func() {
		if err == nil {
			return
		}
		if manifestCreated {
			_ = os.Remove(manifestPath)
		}
		if backupCreated {
			_ = os.Remove(backupPath)
		}
	}()

	now := options.Now
	if now == nil {
		now = time.Now
	}
	binaryVersion := strings.TrimSpace(options.BinaryVersion)
	if binaryVersion == "" {
		binaryVersion = "unknown"
	}
	manifest = BackupManifest{
		FormatVersion:  manifestFormatVersion,
		CreatedAt:      now().UTC(),
		BinaryVersion:  binaryVersion,
		SchemaVersion:  schemaVersion,
		DatabaseFile:   filepath.Base(backupPath),
		DatabaseSize:   databaseSize,
		DatabaseSHA256: checksum,
	}
	if errManifest := writeManifestExclusive(manifestPath, manifest); errManifest != nil {
		return BackupManifest{}, fmt.Errorf("carpool backup: write manifest: %w", errManifest)
	}
	manifestCreated = true
	if errSync := syncDirectory(filepath.Dir(backupPath)); errSync != nil {
		return BackupManifest{}, errSync
	}
	backupCreated = false
	manifestCreated = false
	return manifest, nil
}

// RestoreStopped validates a backup and atomically replaces the stopped target database.
func RestoreStopped(ctx context.Context, databasePath, backupPath string) (BackupManifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	targetPath, sourcePath, errPaths := resolveDistinctPaths(databasePath, backupPath)
	if errPaths != nil {
		return BackupManifest{}, errPaths
	}
	if errStopped := requireNoSQLiteSidecars(targetPath); errStopped != nil {
		return BackupManifest{}, errStopped
	}
	if errSource := requireRegularFile(sourcePath); errSource != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: inspect backup database: %w", errSource)
	}
	manifest, errManifest := readManifest(ManifestPath(sourcePath))
	if errManifest != nil {
		return BackupManifest{}, errManifest
	}
	if manifest.DatabaseFile != filepath.Base(sourcePath) {
		return BackupManifest{}, fmt.Errorf("carpool restore: manifest database file does not match backup")
	}
	size, checksum, errChecksum := fileChecksum(ctx, sourcePath)
	if errChecksum != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: checksum backup: %w", errChecksum)
	}
	if size != manifest.DatabaseSize || !strings.EqualFold(checksum, manifest.DatabaseSHA256) {
		return BackupManifest{}, fmt.Errorf("carpool restore: backup checksum or size mismatch")
	}

	if errDirectory := ensurePrivateDirectory(filepath.Dir(targetPath)); errDirectory != nil {
		return BackupManifest{}, errDirectory
	}
	temporary, errTemporary := os.CreateTemp(filepath.Dir(targetPath), ".carpool-restore-*")
	if errTemporary != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: create temporary database: %w", errTemporary)
	}
	temporaryPath := temporary.Name()
	if errClose := temporary.Close(); errClose != nil {
		_ = os.Remove(temporaryPath)
		return BackupManifest{}, fmt.Errorf("carpool restore: close temporary database: %w", errClose)
	}
	if errRemove := os.Remove(temporaryPath); errRemove != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: prepare temporary database: %w", errRemove)
	}
	defer removeSQLiteFiles(temporaryPath)
	if _, _, errCopy := copyFileExclusive(ctx, sourcePath, temporaryPath); errCopy != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: stage backup: %w", errCopy)
	}

	stagedStore, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: temporaryPath})
	if errOpen != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: validate staged database: %w", errOpen)
	}
	stagedVersion, errSchema := stagedStore.SchemaVersion(ctx)
	if errSchema == nil {
		errSchema = stagedStore.Checkpoint(ctx)
	}
	errClose := stagedStore.Close()
	if errSchema != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: validate staged schema: %w", errors.Join(errSchema, errClose))
	}
	if errClose != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: close staged database: %w", errClose)
	}
	if stagedVersion != manifest.SchemaVersion {
		return BackupManifest{}, fmt.Errorf("carpool restore: schema version %d does not match manifest version %d", stagedVersion, manifest.SchemaVersion)
	}
	stagedSize, stagedChecksum, errStagedChecksum := fileChecksum(ctx, temporaryPath)
	if errStagedChecksum != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: checksum staged database: %w", errStagedChecksum)
	}
	if stagedSize != manifest.DatabaseSize || !strings.EqualFold(stagedChecksum, manifest.DatabaseSHA256) {
		return BackupManifest{}, fmt.Errorf("carpool restore: staged database changed during schema validation")
	}
	if errStopped := requireNoSQLiteSidecars(targetPath); errStopped != nil {
		return BackupManifest{}, errStopped
	}
	if errMode := os.Chmod(temporaryPath, 0o600); errMode != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: set staged database permissions: %w", errMode)
	}
	if errRename := replaceFileAtomically(temporaryPath, targetPath); errRename != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: replace target database: %w", errRename)
	}
	if errSync := syncDirectory(filepath.Dir(targetPath)); errSync != nil {
		return BackupManifest{}, errSync
	}
	return manifest, nil
}

func resolveDistinctPaths(databasePath, backupPath string) (string, string, error) {
	databasePath = strings.TrimSpace(databasePath)
	backupPath = strings.TrimSpace(backupPath)
	if databasePath == "" || backupPath == "" {
		return "", "", fmt.Errorf("carpool operation: database and backup paths are required")
	}
	databaseAbs, errDatabase := filepath.Abs(filepath.Clean(databasePath))
	if errDatabase != nil {
		return "", "", fmt.Errorf("carpool operation: resolve database path: %w", errDatabase)
	}
	backupAbs, errBackup := filepath.Abs(filepath.Clean(backupPath))
	if errBackup != nil {
		return "", "", fmt.Errorf("carpool operation: resolve backup path: %w", errBackup)
	}
	if databaseAbs == backupAbs {
		return "", "", fmt.Errorf("carpool operation: database and backup paths must differ")
	}
	return databaseAbs, backupAbs, nil
}

func requireRegularFile(path string) error {
	info, errStat := os.Lstat(path)
	if errStat != nil {
		return errStat
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file")
	}
	return nil
}

func requireDestinationAvailable(paths ...string) error {
	for _, path := range paths {
		_, errStat := os.Lstat(path)
		switch {
		case errStat == nil:
			return fmt.Errorf("carpool backup: destination already exists: %s", path)
		case errors.Is(errStat, os.ErrNotExist):
			continue
		default:
			return fmt.Errorf("carpool backup: inspect destination %s: %w", path, errStat)
		}
	}
	return nil
}

func requireNoSQLiteSidecars(databasePath string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := databasePath + suffix
		_, errStat := os.Lstat(sidecar)
		switch {
		case errStat == nil:
			return fmt.Errorf("carpool operation: SQLite sidecar %s exists; stop the service before continuing", filepath.Base(sidecar))
		case errors.Is(errStat, os.ErrNotExist):
			continue
		default:
			return fmt.Errorf("carpool operation: inspect SQLite sidecar: %w", errStat)
		}
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if errMkdir := os.MkdirAll(path, 0o700); errMkdir != nil {
		return fmt.Errorf("carpool operation: create destination directory: %w", errMkdir)
	}
	return nil
}

func copyFileExclusive(ctx context.Context, sourcePath, destinationPath string) (size int64, checksum string, err error) {
	source, errOpenSource := os.Open(sourcePath)
	if errOpenSource != nil {
		return 0, "", errOpenSource
	}
	defer func() {
		if errClose := source.Close(); errClose != nil {
			err = errors.Join(err, errClose)
		}
	}()
	destination, errOpenDestination := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpenDestination != nil {
		return 0, "", errOpenDestination
	}
	destinationCreated := true
	defer func() {
		if destination != nil {
			err = errors.Join(err, destination.Close())
		}
		if err != nil && destinationCreated {
			_ = os.Remove(destinationPath)
		}
	}()
	hash := sha256.New()
	written, errCopy := io.Copy(io.MultiWriter(destination, hash), &contextReader{ctx: ctx, reader: source})
	if errCopy != nil {
		return 0, "", errCopy
	}
	if errSync := destination.Sync(); errSync != nil {
		return 0, "", errSync
	}
	if errClose := destination.Close(); errClose != nil {
		destination = nil
		return 0, "", errClose
	}
	destination = nil
	destinationCreated = false
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

func fileChecksum(ctx context.Context, path string) (size int64, checksum string, err error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return 0, "", errOpen
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			err = errors.Join(err, errClose)
		}
	}()
	hash := sha256.New()
	size, errCopy := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if errCopy != nil {
		return 0, "", errCopy
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if errContext := r.ctx.Err(); errContext != nil {
		return 0, errContext
	}
	return r.reader.Read(buffer)
}

func writeManifestExclusive(path string, manifest BackupManifest) (err error) {
	payload, errMarshal := json.MarshalIndent(manifest, "", "  ")
	if errMarshal != nil {
		return errMarshal
	}
	payload = append(payload, '\n')
	file, errOpen := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, errWrite := file.Write(payload); errWrite != nil {
		return errWrite
	}
	return file.Sync()
}

func readManifest(path string) (BackupManifest, error) {
	if errRegular := requireRegularFile(path); errRegular != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: inspect manifest: %w", errRegular)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: stat manifest: %w", errStat)
	}
	if info.Size() > maximumManifestSize {
		return BackupManifest{}, fmt.Errorf("carpool restore: manifest is too large")
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: open manifest: %w", errOpen)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, maximumManifestSize+1))
	decoder.DisallowUnknownFields()
	var manifest BackupManifest
	if errDecode := decoder.Decode(&manifest); errDecode != nil {
		return BackupManifest{}, fmt.Errorf("carpool restore: decode manifest: %w", errDecode)
	}
	if errTrailing := decoder.Decode(&struct{}{}); !errors.Is(errTrailing, io.EOF) {
		return BackupManifest{}, fmt.Errorf("carpool restore: manifest has trailing data")
	}
	checksumBytes, errChecksum := hex.DecodeString(manifest.DatabaseSHA256)
	if manifest.FormatVersion != manifestFormatVersion || manifest.CreatedAt.IsZero() ||
		strings.TrimSpace(manifest.BinaryVersion) == "" || manifest.SchemaVersion < 1 ||
		strings.TrimSpace(manifest.DatabaseFile) == "" || manifest.DatabaseSize <= 0 ||
		errChecksum != nil || len(checksumBytes) != sha256.Size {
		return BackupManifest{}, fmt.Errorf("carpool restore: invalid manifest")
	}
	return manifest, nil
}

func removeSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
