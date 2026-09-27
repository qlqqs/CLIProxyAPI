package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveTokenToFile_PreservesCustomMetadata(t *testing.T) {
	tempDir := t.TempDir()
	authFilePath := filepath.Join(tempDir, "codex-test.json")

	storage := &CodexTokenStorage{
		Type:         "codex",
		Email:        "user@example.com",
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		IDToken:      "new-id-token",
		AccountID:    "new-account",
		Expire:       "2026-12-31T23:59:59Z",
		LastRefresh:  "2026-04-14T12:00:00Z",
	}
	storage.SetMetadata(map[string]any{
		"disabled":   false,
		"prefix":     "my-prefix",
		"websockets": false,
		"note":       "my important note",
		"proxy_url":  "http://proxy:8080",
		"weight":     float64(42),
	})

	if errSave := storage.SaveTokenToFile(authFilePath); errSave != nil {
		t.Fatalf("SaveTokenToFile() error = %v", errSave)
	}

	savedRaw, errRead := os.ReadFile(authFilePath)
	if errRead != nil {
		t.Fatalf("os.ReadFile error = %v", errRead)
	}

	var saved map[string]any
	if errUnmarshal := json.Unmarshal(savedRaw, &saved); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal error = %v", errUnmarshal)
	}

	// Verify updated OAuth token fields
	if saved["access_token"] != "new-access-token" {
		t.Errorf("access_token = %v, want new-access-token", saved["access_token"])
	}
	if saved["refresh_token"] != "new-refresh-token" {
		t.Errorf("refresh_token = %v, want new-refresh-token", saved["refresh_token"])
	}
	if saved["id_token"] != "new-id-token" {
		t.Errorf("id_token = %v, want new-id-token", saved["id_token"])
	}
	if saved["account_id"] != "new-account" {
		t.Errorf("account_id = %v, want new-account", saved["account_id"])
	}

	// Verify custom fields in metadata
	if saved["prefix"] != "my-prefix" {
		t.Errorf("prefix = %v, want my-prefix", saved["prefix"])
	}
	if saved["websockets"] != false {
		t.Errorf("websockets = %v, want false", saved["websockets"])
	}
	if saved["note"] != "my important note" {
		t.Errorf("note = %v, want my important note", saved["note"])
	}
	if saved["proxy_url"] != "http://proxy:8080" {
		t.Errorf("proxy_url = %v, want http://proxy:8080", saved["proxy_url"])
	}
	if saved["weight"] != float64(42) {
		t.Errorf("weight = %v, want 42", saved["weight"])
	}
}

func TestSaveTokenToFileUsesPrivateAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "synthetic.json")
	storage := &CodexTokenStorage{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.WriteFile(path, []byte(`{"access_token":"old"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := storage.SaveTokenToFile(path); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("token file mode %o, want 0600", info.Mode().Perm())
		}
		var payload map[string]any
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(data, &payload) != nil || payload["access_token"] != "synthetic-access" {
			t.Fatal("token replacement corrupted the payload")
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	storage.SetMetadata(map[string]any{"unsupported": make(chan struct{})})
	if err := storage.SaveTokenToFile(path); err == nil {
		t.Fatal("unsupported metadata accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed save changed the existing credential")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary credential files leaked: count=%d error=%v", len(files), err)
	}
}
