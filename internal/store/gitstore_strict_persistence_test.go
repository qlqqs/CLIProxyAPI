package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestGitTokenStoreStrictSaveRetriesMatchingContentAfterLeaseConflict(t *testing.T) {
	root := t.TempDir()
	remoteDir := setupGitRemoteRepository(t, root, "master",
		testBranchSpec{name: "master", contents: "remote master branch\n"},
	)
	storeA := NewGitTokenStore(remoteDir, "", "", "")
	storeA.SetBaseDir(filepath.Join(root, "workspace-a", "auths"))
	if errEnsure := storeA.EnsureRepository(); errEnsure != nil {
		t.Fatalf("EnsureRepository A: %v", errEnsure)
	}
	storeB := NewGitTokenStore(remoteDir, "", "", "")
	storeB.SetBaseDir(filepath.Join(root, "workspace-b", "auths"))
	if errEnsure := storeB.EnsureRepository(); errEnsure != nil {
		t.Fatalf("EnsureRepository B: %v", errEnsure)
	}

	authA := &coreauth.Auth{
		ID:       "local.json",
		FileName: "local.json",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex", "access_token": "local"},
	}
	remoteAdvanced := false
	authA.Storage = &callbackTokenStorage{save: func(path string) error {
		raw, errMarshal := json.Marshal(authA.Metadata)
		if errMarshal != nil {
			return errMarshal
		}
		if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
			return errWrite
		}
		if remoteAdvanced {
			return nil
		}
		remoteAdvanced = true
		_, errSave := storeB.Save(context.Background(), &coreauth.Auth{
			ID:       "concurrent.json",
			FileName: "concurrent.json",
			Provider: "codex",
			Metadata: map[string]any{"type": "codex", "access_token": "remote"},
		})
		return errSave
	}}
	if _, errSave := storeA.Save(coreauth.WithStrictPersistence(context.Background()), authA); errSave == nil {
		t.Fatal("first Save error = nil, want lease rejection")
	}
	assertRemoteTreePath(t, remoteDir, "master", "auths/local.json", false)
	assertRemoteTreePath(t, remoteDir, "master", "auths/concurrent.json", true)

	assertLocalFileContents(t, filepath.Join(root, "workspace-a", "auths", "local.json"), `{"access_token":"local","disabled":false,"type":"codex"}`)
	authA.Storage = nil
	if _, errSave := storeA.Save(coreauth.WithStrictPersistence(context.Background()), authA); errSave != nil {
		t.Fatalf("second Save after lease rejection: %v", errSave)
	}
	assertRemoteFileContents(t, remoteDir, "master", "auths/local.json", `{"access_token":"local","disabled":false,"type":"codex"}`)
	assertRemoteTreePath(t, remoteDir, "master", "auths/concurrent.json", true)
}

func TestGitTokenStoreStrictDisableWithMissingMirror(t *testing.T) {
	root := t.TempDir()
	remoteDir := setupGitRemoteRepository(t, root, "master", testBranchSpec{name: "master", contents: "seed\n"})
	store := NewGitTokenStore(remoteDir, "", "", "")
	dir := filepath.Join(root, "workspace", "auths")
	store.SetBaseDir(dir)
	auth := &coreauth.Auth{ID: "disable.json", FileName: "disable.json", Metadata: map[string]any{"type": "test"}}
	if _, err := store.Save(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	other := NewGitTokenStore(remoteDir, "", "", "")
	other.SetBaseDir(filepath.Join(root, "other", "auths"))
	if err := other.EnsureRepository(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, auth.FileName)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	auth.Disabled = true
	if saved, err := store.Save(context.Background(), auth); err != nil || saved != "" {
		t.Fatalf("default missing mirror: path=%q err=%v", saved, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("default created mirror: %v", err)
	}
	assertRemoteFileContents(t, remoteDir, "master", "auths/disable.json", `{"disabled":false,"type":"test"}`)

	// Advance the remote after Save pulls, forcing the disable push to fail its lease.
	auth.Storage = &callbackTokenStorage{save: func(path string) error {
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return errMarshal
		}
		if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
			return errWrite
		}
		_, errSave := other.Save(context.Background(), &coreauth.Auth{ID: "other.json", FileName: "other.json", Metadata: map[string]any{"type": "test"}})
		return errSave
	}}
	ctx := coreauth.WithStrictPersistence(context.Background())
	if _, err := store.Save(ctx, auth); err == nil {
		t.Fatal("strict missing mirror disable returned success despite rejected push")
	}
	assertLocalFileContents(t, path, `{"disabled":true,"type":"test"}`)
	assertRemoteFileContents(t, remoteDir, "master", "auths/disable.json", `{"disabled":false,"type":"test"}`)
	auth.Storage = nil
	// Retry with the mirror absent again to prove successful durable disable as well.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if saved, err := store.Save(ctx, auth); err != nil || saved != path {
		t.Fatalf("strict disable retry: path=%q err=%v", saved, err)
	}
	assertRemoteFileContents(t, remoteDir, "master", "auths/disable.json", `{"disabled":true,"type":"test"}`)
}
