package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type strictFailStore struct{ countingStore }

func (*strictFailStore) Save(context.Context, *Auth) (string, error) {
	return "", errors.New("synthetic save failure")
}

type strictUpdateHook struct {
	NoopHook
	updates int
}

func (h *strictUpdateHook) OnAuthUpdated(context.Context, *Auth) { h.updates++ }

func TestStrictPersistenceFailureLeavesRuntimeAndHooksUntouched(t *testing.T) {
	hook := &strictUpdateHook{}
	manager := NewManager(&strictFailStore{}, nil, hook)
	initial, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "strict", Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "token", "disabled": false}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	changed := initial.Clone()
	changed.Disabled = true
	changed.Status = StatusDisabled
	changed.Metadata["disabled"] = true
	if _, errUpdate := manager.Update(WithStrictPersistence(context.Background()), changed); errUpdate == nil {
		t.Fatal("strict Update acknowledged failed save")
	}
	live, _ := manager.GetByID(initial.ID)
	if live.Disabled || live.Metadata["disabled"] != false || live.Generation != initial.Generation || hook.updates != 0 {
		t.Fatal("failed strict update published state or hooks")
	}
	// Ordinary refresh/watcher-era updates retain the existing best-effort contract.
	if _, errUpdate := manager.Update(context.Background(), changed); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	live, _ = manager.GetByID(initial.ID)
	if !live.Disabled || hook.updates != 1 {
		t.Fatal("default update semantics changed")
	}
}

func TestStrictPersistenceSavesExactlyOnce(t *testing.T) {
	store := &countingStore{}
	manager := NewManager(store, nil, nil)
	initial, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "strict", Provider: "codex", Metadata: map[string]any{"access_token": "token"}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	stale := initial.Clone()
	initial.Disabled = true
	if _, errUpdate := manager.Update(WithStrictPersistence(context.Background()), initial); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errStale := manager.Update(WithStrictPersistence(context.Background()), stale); !errors.Is(errStale, ErrStrictPersistenceConflict) {
		t.Fatalf("stale snapshot accepted: %v", errStale)
	}
	manager.Remove(context.Background(), initial.ID)
	if _, errRemoved := manager.Update(WithStrictPersistence(context.Background()), initial); !errors.Is(errRemoved, ErrStrictPersistenceConflict) {
		t.Fatalf("removed account update acknowledged: %v", errRemoved)
	}
	if store.saveCount.Load() != 1 {
		t.Fatal("strict update duplicated persistence")
	}
}

// blockingStrictStore blocks only the first strict save, with no wall-clock
// ordering assumptions. Other saves behave like independent remote requests.
type blockingStrictStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	saved   map[string]*Auth
}

func (s *blockingStrictStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (s *blockingStrictStore) Save(ctx context.Context, auth *Auth) (string, error) {
	if StrictPersistenceRequired(ctx) {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	s.mu.Lock()
	s.saved[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}
func (s *blockingStrictStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.saved, id)
	s.mu.Unlock()
	return nil
}
func (s *blockingStrictStore) snapshot(id string) *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[id].Clone()
}

func TestStrictRemoteSaveDoesNotBlockUnrelatedReadsOrSelection(t *testing.T) {
	store := &blockingStrictStore{entered: make(chan struct{}), release: make(chan struct{}), saved: make(map[string]*Auth)}
	manager := NewManager(store, nil, nil)
	manager.RegisterExecutor(schedulerTestExecutor{provider: "codex"})
	var slow *Auth
	for _, id := range []string{"slow", "other"} {
		registered, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: "codex", Status: StatusActive, Metadata: map[string]any{"access_token": "token"}})
		if errRegister != nil {
			t.Fatal(errRegister)
		}
		if id == "slow" {
			slow = registered
		}
	}
	done := make(chan error, 1)
	go func() {
		slow.Disabled = true
		_, errUpdate := manager.Update(WithStrictPersistence(context.Background()), slow)
		done <- errUpdate
	}()
	<-store.entered
	checks := make(chan error, 1)
	go func() {
		if _, ok := manager.GetByID("other"); !ok {
			checks <- errors.New("unrelated GetByID missing")
			return
		}
		if len(manager.List()) != 2 {
			checks <- errors.New("unrelated List missing")
			return
		}
		selected, _, errPick := manager.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, map[string]struct{}{"slow": {}})
		if errPick != nil {
			checks <- errPick
			return
		}
		if selected == nil || selected.ID != "other" {
			checks <- errors.New("unrelated selection failed")
			return
		}
		other, _ := manager.GetByID("other")
		other.Label = "updated"
		_, errUpdate := manager.Update(WithSkipPersist(context.Background()), other)
		checks <- errUpdate
	}()
	select {
	case errChecks := <-checks:
		close(store.release)
		if errChecks != nil {
			t.Fatal(errChecks)
		}
	case <-time.After(3 * time.Second):
		close(store.release)
		<-done
		t.Fatal("remote strict Save held the global manager lock")
	}
	if errUpdate := <-done; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	live, _ := manager.GetByID("slow")
	if !live.Disabled || !store.snapshot("slow").Disabled {
		t.Fatal("strict update not published after save")
	}
}

func TestStrictRemoteSaveConflictReconcilesLatestVersion(t *testing.T) {
	for _, operation := range []string{"update", "remove", "replace"} {
		t.Run(operation, func(t *testing.T) {
			store := &blockingStrictStore{entered: make(chan struct{}), release: make(chan struct{}), saved: make(map[string]*Auth)}
			manager := NewManager(store, nil, nil)
			initial, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: "target", Provider: "codex", Metadata: map[string]any{"access_token": "old"}})
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			done := make(chan error, 1)
			go func() {
				changed := initial.Clone()
				changed.Disabled = true
				_, errUpdate := manager.Update(WithStrictPersistence(context.Background()), changed)
				done <- errUpdate
			}()
			<-store.entered
			switch operation {
			case "update":
				fresh, _ := manager.GetByID("target")
				fresh.Metadata["access_token"] = "new"
				if _, errUpdate := manager.Update(context.Background(), fresh); errUpdate != nil {
					t.Fatal(errUpdate)
				}
			case "remove":
				manager.Remove(context.Background(), "target")
			case "replace":
				manager.Remove(context.Background(), "target")
				if _, errReplace := manager.Register(context.Background(), &Auth{ID: "target", Provider: "codex", Metadata: map[string]any{"access_token": "replacement"}}); errReplace != nil {
					t.Fatal(errReplace)
				}
			}
			close(store.release)
			if errUpdate := <-done; !errors.Is(errUpdate, ErrStrictPersistenceConflict) {
				t.Fatalf("stale strict result = %v", errUpdate)
			}
			live, exists := manager.GetByID("target")
			disk := store.snapshot("target")
			if operation == "remove" {
				if exists || disk != nil {
					t.Fatal("stale save resurrected removed credential")
				}
				return
			}
			if !exists || live.Disabled || disk == nil || disk.Disabled || disk.Metadata["access_token"] != live.Metadata["access_token"] {
				t.Fatal("stale save replaced fresh runtime or persisted credentials")
			}
		})
	}
}
