package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
)

func TestBackupStoppedAndRestoreStoppedRoundTrip(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "source", "carpool.db")
	backupPath := filepath.Join(directory, "backups", "carpool-20260904.db")
	targetPath := filepath.Join(directory, "restore", "carpool.db")
	sourceCarID := createDatabaseWithCar(t, databasePath, "source marker")
	targetCarID := createDatabaseWithCar(t, targetPath, "target marker")
	fixedNow := time.Date(2026, time.September, 4, 12, 30, 0, 0, time.FixedZone("test", 8*60*60))

	manifest, errBackup := BackupStopped(ctx, BackupOptions{
		DatabasePath:  databasePath,
		BackupPath:    backupPath,
		BinaryVersion: "v7.0.0-test",
		Now:           func() time.Time { return fixedNow },
	})
	if errBackup != nil {
		t.Fatalf("BackupStopped() error = %v", errBackup)
	}
	assertManifest(t, manifest, backupPath, "v7.0.0-test", fixedNow.UTC())
	assertFileMode(t, backupPath, 0o600)
	assertFileMode(t, ManifestPath(backupPath), 0o600)

	persistedManifest, errManifest := readManifest(ManifestPath(backupPath))
	if errManifest != nil {
		t.Fatalf("readManifest() error = %v", errManifest)
	}
	if persistedManifest != manifest {
		t.Fatalf("persisted manifest = %#v, want %#v", persistedManifest, manifest)
	}

	restoredManifest, errRestore := RestoreStopped(ctx, targetPath, backupPath)
	if errRestore != nil {
		t.Fatalf("RestoreStopped() error = %v", errRestore)
	}
	if restoredManifest != manifest {
		t.Fatalf("restored manifest = %#v, want %#v", restoredManifest, manifest)
	}
	assertFileMode(t, targetPath, 0o600)

	store := openDatabase(t, targetPath)
	defer closeDatabase(t, store)
	if _, errGet := store.GetCar(ctx, sourceCarID); errGet != nil {
		t.Fatalf("restored source car: %v", errGet)
	}
	if _, errGet := store.GetCar(ctx, targetCarID); !errors.Is(errGet, domain.ErrNotFound) {
		t.Fatalf("target marker GetCar() error = %v, want ErrNotFound", errGet)
	}
}

func TestBackupStoppedRecoversCommittedWAL(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	livePath := filepath.Join(directory, "live.db")
	crashPath := filepath.Join(directory, "crash-copy.db")
	backupPath := filepath.Join(directory, "backup.db")
	restorePath := filepath.Join(directory, "restored.db")

	liveStore := openDatabase(t, livePath)
	car, errCreate := liveStore.CreateCar(ctx, domain.Car{Name: "wal marker", Status: domain.CarStatusActive})
	if errCreate != nil {
		closeDatabase(t, liveStore)
		t.Fatalf("CreateCar() error = %v", errCreate)
	}
	walInfo, errWAL := os.Stat(livePath + "-wal")
	if errWAL != nil || walInfo.Size() <= 32 {
		closeDatabase(t, liveStore)
		t.Fatalf("committed WAL missing or empty: info=%v error=%v", walInfo, errWAL)
	}
	copyTestFile(t, livePath, crashPath)
	copyTestFile(t, livePath+"-wal", crashPath+"-wal")
	closeDatabase(t, liveStore)

	if _, errBackup := BackupStopped(ctx, BackupOptions{DatabasePath: crashPath, BackupPath: backupPath}); errBackup != nil {
		t.Fatalf("BackupStopped() error = %v", errBackup)
	}
	if _, errRestore := RestoreStopped(ctx, restorePath, backupPath); errRestore != nil {
		t.Fatalf("RestoreStopped() error = %v", errRestore)
	}
	restored := openDatabase(t, restorePath)
	defer closeDatabase(t, restored)
	got, errGet := restored.GetCar(ctx, car.ID)
	if errGet != nil {
		t.Fatalf("restored WAL car: %v", errGet)
	}
	if got.Name != "wal marker" {
		t.Fatalf("restored car name = %q, want wal marker", got.Name)
	}
}

func TestBackupStoppedRejectsExistingDestination(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "carpool.db")
	backupPath := filepath.Join(directory, "existing.db")
	createDatabaseWithCar(t, databasePath, "source")
	wantContents := []byte("do-not-overwrite")
	if errWrite := os.WriteFile(backupPath, wantContents, 0o600); errWrite != nil {
		t.Fatalf("write existing backup: %v", errWrite)
	}

	_, errBackup := BackupStopped(context.Background(), BackupOptions{DatabasePath: databasePath, BackupPath: backupPath})
	if errBackup == nil || !strings.Contains(errBackup.Error(), "already exists") {
		t.Fatalf("BackupStopped() error = %v, want existing destination error", errBackup)
	}
	gotContents, errRead := os.ReadFile(backupPath)
	if errRead != nil {
		t.Fatalf("read existing backup: %v", errRead)
	}
	if string(gotContents) != string(wantContents) {
		t.Fatalf("existing backup contents = %q, want %q", gotContents, wantContents)
	}
	if _, errStat := os.Stat(ManifestPath(backupPath)); !errors.Is(errStat, os.ErrNotExist) {
		t.Fatalf("manifest stat error = %v, want not exist", errStat)
	}
}

func TestRestoreStoppedRejectsTamperingWithoutChangingTarget(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, string)
	}{
		{
			name: "database checksum",
			tamper: func(t *testing.T, backupPath string) {
				t.Helper()
				file, errOpen := os.OpenFile(backupPath, os.O_WRONLY|os.O_APPEND, 0)
				if errOpen != nil {
					t.Fatalf("open backup: %v", errOpen)
				}
				if _, errWrite := file.Write([]byte("tampered")); errWrite != nil {
					_ = file.Close()
					t.Fatalf("tamper backup: %v", errWrite)
				}
				if errClose := file.Close(); errClose != nil {
					t.Fatalf("close tampered backup: %v", errClose)
				}
			},
		},
		{
			name: "manifest schema",
			tamper: func(t *testing.T, backupPath string) {
				t.Helper()
				manifestPath := ManifestPath(backupPath)
				payload, errRead := os.ReadFile(manifestPath)
				if errRead != nil {
					t.Fatalf("read manifest: %v", errRead)
				}
				var manifest BackupManifest
				if errDecode := json.Unmarshal(payload, &manifest); errDecode != nil {
					t.Fatalf("decode manifest: %v", errDecode)
				}
				manifest.SchemaVersion++
				payload, errMarshal := json.Marshal(manifest)
				if errMarshal != nil {
					t.Fatalf("encode manifest: %v", errMarshal)
				}
				if errWrite := os.WriteFile(manifestPath, payload, 0o600); errWrite != nil {
					t.Fatalf("write manifest: %v", errWrite)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			databasePath := filepath.Join(directory, "source.db")
			backupPath := filepath.Join(directory, "backup.db")
			targetPath := filepath.Join(directory, "target.db")
			createDatabaseWithCar(t, databasePath, "source")
			createDatabaseWithCar(t, targetPath, "target")
			if _, errBackup := BackupStopped(context.Background(), BackupOptions{DatabasePath: databasePath, BackupPath: backupPath}); errBackup != nil {
				t.Fatalf("BackupStopped() error = %v", errBackup)
			}
			before, errRead := os.ReadFile(targetPath)
			if errRead != nil {
				t.Fatalf("read target before restore: %v", errRead)
			}
			test.tamper(t, backupPath)

			if _, errRestore := RestoreStopped(context.Background(), targetPath, backupPath); errRestore == nil {
				t.Fatal("RestoreStopped() error = nil, want validation failure")
			}
			after, errRead := os.ReadFile(targetPath)
			if errRead != nil {
				t.Fatalf("read target after restore: %v", errRead)
			}
			if string(after) != string(before) {
				t.Fatal("RestoreStopped() changed target after validation failure")
			}
		})
	}
}

func TestStoppedOperationsRejectSQLiteSidecars(t *testing.T) {
	t.Run("backup rejects active source", func(t *testing.T) {
		directory := t.TempDir()
		databasePath := filepath.Join(directory, "source.db")
		backupPath := filepath.Join(directory, "backup.db")
		store := openDatabase(t, databasePath)
		defer closeDatabase(t, store)
		if _, errCreate := store.CreateCar(context.Background(), domain.Car{Name: "active", Status: domain.CarStatusActive}); errCreate != nil {
			t.Fatalf("CreateCar() error = %v", errCreate)
		}

		_, errBackup := BackupStopped(context.Background(), BackupOptions{DatabasePath: databasePath, BackupPath: backupPath})
		if errBackup == nil || !strings.Contains(strings.ToLower(errBackup.Error()), "sidecar") {
			t.Fatalf("BackupStopped() error = %v, want active sidecar error", errBackup)
		}
		if _, errStat := os.Stat(backupPath); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("backup stat error = %v, want not exist", errStat)
		}
	})

	t.Run("restore rejects target sidecar", func(t *testing.T) {
		directory := t.TempDir()
		databasePath := filepath.Join(directory, "source.db")
		backupPath := filepath.Join(directory, "backup.db")
		targetPath := filepath.Join(directory, "target.db")
		createDatabaseWithCar(t, databasePath, "source")
		wantContents := []byte("target-sentinel")
		if errWrite := os.WriteFile(targetPath, wantContents, 0o600); errWrite != nil {
			t.Fatalf("write target: %v", errWrite)
		}
		if _, errBackup := BackupStopped(context.Background(), BackupOptions{DatabasePath: databasePath, BackupPath: backupPath}); errBackup != nil {
			t.Fatalf("BackupStopped() error = %v", errBackup)
		}
		if errWrite := os.WriteFile(targetPath+"-wal", []byte("active"), 0o600); errWrite != nil {
			t.Fatalf("write sidecar: %v", errWrite)
		}

		_, errRestore := RestoreStopped(context.Background(), targetPath, backupPath)
		if errRestore == nil || !strings.Contains(strings.ToLower(errRestore.Error()), "sidecar") {
			t.Fatalf("RestoreStopped() error = %v, want sidecar error", errRestore)
		}
		gotContents, errRead := os.ReadFile(targetPath)
		if errRead != nil {
			t.Fatalf("read target: %v", errRead)
		}
		if string(gotContents) != string(wantContents) {
			t.Fatalf("target contents = %q, want %q", gotContents, wantContents)
		}
	})
}

func assertManifest(t *testing.T, manifest BackupManifest, backupPath, binaryVersion string, createdAt time.Time) {
	t.Helper()
	if manifest.FormatVersion != manifestFormatVersion || manifest.BinaryVersion != binaryVersion ||
		manifest.SchemaVersion != 3 || manifest.DatabaseFile != filepath.Base(backupPath) ||
		!manifest.CreatedAt.Equal(createdAt) {
		t.Fatalf("manifest metadata = %#v", manifest)
	}
	payload, errRead := os.ReadFile(backupPath)
	if errRead != nil {
		t.Fatalf("read backup: %v", errRead)
	}
	digest := sha256.Sum256(payload)
	if manifest.DatabaseSize != int64(len(payload)) || manifest.DatabaseSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("manifest integrity = size %d checksum %q, want size %d checksum %q",
			manifest.DatabaseSize, manifest.DatabaseSHA256, len(payload), hex.EncodeToString(digest[:]))
	}
}

func createDatabaseWithCar(t *testing.T, path, name string) string {
	t.Helper()
	store := openDatabase(t, path)
	car, errCreate := store.CreateCar(context.Background(), domain.Car{Name: name, Status: domain.CarStatusActive})
	if errCreate != nil {
		closeDatabase(t, store)
		t.Fatalf("CreateCar() error = %v", errCreate)
	}
	closeDatabase(t, store)
	return car.ID
}

func openDatabase(t *testing.T, path string) *carpoolsqlite.Store {
	t.Helper()
	store, errOpen := carpoolsqlite.Open(context.Background(), carpoolsqlite.Config{Path: path})
	if errOpen != nil {
		t.Fatalf("sqlite.Open() error = %v", errOpen)
	}
	return store
}

func closeDatabase(t *testing.T, store *carpoolsqlite.Store) {
	t.Helper()
	if errClose := store.Close(); errClose != nil {
		t.Fatalf("Store.Close() error = %v", errClose)
	}
}

func copyTestFile(t *testing.T, sourcePath, targetPath string) {
	t.Helper()
	payload, errRead := os.ReadFile(sourcePath)
	if errRead != nil {
		t.Fatalf("read %s: %v", filepath.Base(sourcePath), errRead)
	}
	if errWrite := os.WriteFile(targetPath, payload, 0o600); errWrite != nil {
		t.Fatalf("write %s: %v", filepath.Base(targetPath), errWrite)
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat %s: %v", path, errStat)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %o, want %o", path, got, want)
	}
}
