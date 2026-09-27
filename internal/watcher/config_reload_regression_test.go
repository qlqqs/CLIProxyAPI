package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func writeReloadTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(contents), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}

func reloadTestConfig(authDir string, port int) string {
	return fmt.Sprintf("auth-dir: %q\nport: %d\n", authDir, port)
}

func newReloadTestWatcher(t *testing.T, configPath, authDir string, callback func(*config.Config)) *Watcher {
	t.Helper()
	w, errNew := NewWatcher(configPath, authDir, callback)
	if errNew != nil {
		t.Fatal(errNew)
	}
	t.Cleanup(func() {
		if errStop := w.Stop(); errStop != nil {
			t.Error(errStop)
		}
	})
	cfg, errLoad := config.LoadConfig(configPath)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	w.SetConfig(cfg)
	return w
}

func startReloadTestWatcher(t *testing.T, w *Watcher) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if errStart := w.Start(ctx); errStart != nil {
		t.Fatal(errStart)
	}
}

func awaitReloadTestPort(t *testing.T, reloads <-chan int, want int) {
	t.Helper()
	select {
	case got := <-reloads:
		if got != want {
			t.Fatalf("reloaded port = %d, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("config reload to port %d did not arrive", want)
	}
}

func awaitReloadTestAuth(t *testing.T, updates <-chan AuthUpdate) AuthUpdate {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(5 * time.Second):
		t.Fatal("auth update did not arrive")
		return AuthUpdate{}
	}
}

func assertReloadTestWatch(t *testing.T, w *Watcher, path string, want bool) {
	t.Helper()
	found := false
	for _, watched := range w.watcher.WatchList() {
		if w.sameWatchPath(watched, path) {
			found = true
		}
	}
	if found != want {
		t.Fatalf("watch %q present = %v, want %v; watches = %v", path, found, want, w.watcher.WatchList())
	}
}

func TestConfigAtomicReplacementRemainsWatched(t *testing.T) {
	root := t.TempDir()
	authDir := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1001))
	reloads := make(chan int, 8)
	w := newReloadTestWatcher(t, path, authDir, func(cfg *config.Config) { reloads <- cfg.Port })
	startReloadTestWatcher(t, w)
	awaitReloadTestPort(t, reloads, 1001)

	for _, port := range []int{1002, 1003} {
		next := path + ".new"
		writeReloadTestFile(t, next, reloadTestConfig(authDir, port))
		if errRename := os.Rename(next, path); errRename != nil {
			t.Fatal(errRename)
		}
		awaitReloadTestPort(t, reloads, port)
		assertReloadTestWatch(t, w, root, true)
	}
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1004))
	awaitReloadTestPort(t, reloads, 1004)
}

func TestAuthDirReloadMovesWatchAndSnapshot(t *testing.T) {
	for _, scenario := range []string{"separate", "old-shares-config", "new-shares-relative-config"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			oldDir, newDir := t.TempDir(), t.TempDir()
			if scenario == "old-shares-config" {
				oldDir = root
			}
			if scenario == "new-shares-relative-config" {
				newDir = root
			}
			oldPath := filepath.Join(oldDir, "old.json")
			writeReloadTestFile(t, oldPath, `{"type":"codex","access_token":"synthetic-old"}`)
			writeReloadTestFile(t, filepath.Join(newDir, "new.json"), `{"type":"codex","access_token":"synthetic-new"}`)
			path := filepath.Join(root, "config.yaml")
			if scenario == "new-shares-relative-config" {
				cwd, errCwd := os.Getwd()
				if errCwd != nil {
					t.Fatal(errCwd)
				}
				relativePath, errRel := filepath.Rel(cwd, path)
				if errRel != nil {
					t.Fatal(errRel)
				}
				path = relativePath
			}
			writeReloadTestFile(t, path, reloadTestConfig(oldDir, 1001))
			reloads := make(chan int, 8)
			var w *Watcher
			w = newReloadTestWatcher(t, path, oldDir, func(cfg *config.Config) {
				// The service callback installs its plugin parser and may read
				// auth snapshots; neither operation may deadlock during reload.
				w.SetPluginAuthParser(nil)
				if auths := w.SnapshotCoreAuths(); len(auths) == 0 {
					t.Error("callback observed an empty auth snapshot")
				}
				reloads <- cfg.Port
			})
			updates := make(chan AuthUpdate, 16)
			w.SetAuthUpdateQueue(updates)
			startReloadTestWatcher(t, w)
			awaitReloadTestPort(t, reloads, 1001)
			initial := awaitReloadTestAuth(t, updates)
			if initial.Action != AuthUpdateActionAdd || initial.ID != "old.json" {
				t.Fatalf("unexpected initial auth: %+v", initial)
			}

			writeReloadTestFile(t, path, reloadTestConfig(newDir, 1002))
			w.ReloadConfigIfChanged()
			awaitReloadTestPort(t, reloads, 1002)
			changes := make(map[string]AuthUpdateAction)
			for range 2 {
				update := awaitReloadTestAuth(t, updates)
				changes[update.ID] = update.Action
			}
			if changes["old.json"] != AuthUpdateActionDelete || changes["new.json"] != AuthUpdateActionAdd {
				t.Fatalf("directory switch updates = %v", changes)
			}
			auths := w.SnapshotCoreAuths()
			if len(auths) != 1 || auths[0].ID != "new.json" {
				t.Fatalf("directory switch retained wrong auth snapshot: %+v", auths)
			}
			assertReloadTestWatch(t, w, newDir, true)
			assertReloadTestWatch(t, w, root, true)
			assertReloadTestWatch(t, w, oldDir, scenario == "old-shares-config")

			// A queued event from the retired auth directory must be ignored.
			w.handleEvent(fsnotify.Event{Name: oldPath, Op: fsnotify.Write})
			w.clientsMutex.RLock()
			_, retainedOldHash := w.lastAuthHashes[w.normalizeAuthPath(oldPath)]
			_, retainedOldAuth := w.currentAuths["old.json"]
			w.clientsMutex.RUnlock()
			if retainedOldHash || retainedOldAuth {
				t.Fatal("retired directory event reintroduced old auth state")
			}

			writeReloadTestFile(t, filepath.Join(newDir, "added.json"), `{"type":"codex","access_token":"synthetic-added"}`)
			added := awaitReloadTestAuth(t, updates)
			if added.Action != AuthUpdateActionAdd || added.ID != "added.json" {
				t.Fatalf("new directory is not delivering auth events: %+v", added)
			}
			writeReloadTestFile(t, path, reloadTestConfig(newDir, 1003))
			awaitReloadTestPort(t, reloads, 1003)
		})
	}
}

func TestAuthDirReloadFailureKeepsPreviousConfigAndWatch(t *testing.T) {
	for _, invalidKind := range []string{"missing", "file"} {
		t.Run(invalidKind, func(t *testing.T) {
			root := t.TempDir()
			oldDir := t.TempDir()
			writeReloadTestFile(t, filepath.Join(oldDir, "old.json"), `{"type":"codex"}`)
			path := filepath.Join(root, "config.yaml")
			writeReloadTestFile(t, path, reloadTestConfig(oldDir, 1001))
			w := newReloadTestWatcher(t, path, oldDir, nil)
			startReloadTestWatcher(t, w)
			w.ReloadConfigIfChanged()
			w.clientsMutex.RLock()
			previousHash := w.lastConfigHash
			w.clientsMutex.RUnlock()
			invalidDir := filepath.Join(root, "invalid")
			if invalidKind == "file" {
				writeReloadTestFile(t, invalidDir, "not a directory")
			}
			writeReloadTestFile(t, path, reloadTestConfig(invalidDir, 1002))
			if w.reloadConfig() {
				t.Fatal("invalid auth directory was accepted")
			}
			w.clientsMutex.RLock()
			unchanged := w.config.Port == 1001 && w.config.AuthDir == oldDir && w.authDir == oldDir && w.lastConfigHash == previousHash
			w.clientsMutex.RUnlock()
			if !unchanged {
				t.Fatal("failed directory switch changed the applied config or hash")
			}
			assertReloadTestWatch(t, w, oldDir, true)
			if auths := w.SnapshotCoreAuths(); len(auths) != 1 || auths[0].ID != "old.json" {
				t.Fatalf("failed switch changed auth snapshot: %+v", auths)
			}
			if invalidKind == "file" {
				if errRemove := os.Remove(invalidDir); errRemove != nil {
					t.Fatal(errRemove)
				}
			}
			if errMkdir := os.Mkdir(invalidDir, 0o700); errMkdir != nil {
				t.Fatal(errMkdir)
			}
			w.ReloadConfigIfChanged()
			assertReloadTestWatch(t, w, invalidDir, true)
			assertReloadTestWatch(t, w, oldDir, false)
		})
	}
}

func TestReloadDoesNotAcknowledgeConfigWrittenDuringCallback(t *testing.T) {
	root := t.TempDir()
	authDir := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1001))
	started := make(chan struct{})
	release := make(chan struct{})
	callbacks := 0
	w := newReloadTestWatcher(t, path, authDir, func(*config.Config) {
		callbacks++
		if callbacks == 1 {
			close(started)
			<-release
		}
	})
	done := make(chan struct{})
	go func() {
		w.ReloadConfigIfChanged()
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("reload callback did not start")
	}
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1002))
	close(release)
	<-done
	w.ReloadConfigIfChanged()
	if w.config.Port != 1002 || callbacks != 2 {
		t.Fatalf("newer config skipped: port=%d callbacks=%d", w.config.Port, callbacks)
	}
}

func TestReloadManagementSecretHashDoesNotCauseExtraReload(t *testing.T) {
	root := t.TempDir()
	authDir := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1001))
	callbacks := 0
	w := newReloadTestWatcher(t, path, authDir, func(*config.Config) { callbacks++ })
	writeReloadTestFile(t, path, reloadTestConfig(authDir, 1001)+"remote-management:\n  secret-key: synthetic-secret\n")
	w.ReloadConfigIfChanged()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var persisted config.Config
	if errParse := yaml.Unmarshal(data, &persisted); errParse != nil {
		t.Fatal(errParse)
	}
	if errCompare := bcrypt.CompareHashAndPassword([]byte(persisted.RemoteManagement.SecretKey), []byte("synthetic-secret")); errCompare != nil {
		t.Fatalf("loader did not persist the management secret hash: %v", errCompare)
	}
	w.ReloadConfigIfChanged()
	if callbacks != 1 {
		t.Fatalf("loader secret rewrite triggered %d callbacks, want 1", callbacks)
	}
	// A real edit to the loader-rewritten file must still apply.
	writeReloadTestFile(t, path, strings.Replace(string(data), "port: 1001", "port: 1002", 1))
	w.ReloadConfigIfChanged()
	if callbacks != 2 || w.config.Port != 1002 {
		t.Fatalf("edit after secret hashing skipped: callbacks=%d port=%d", callbacks, w.config.Port)
	}
}
