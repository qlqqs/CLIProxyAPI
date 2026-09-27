package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveTokenToFile_PreservesCustomMetadata(t *testing.T) {
	tempDir := t.TempDir()
	authFilePath := filepath.Join(tempDir, "claude-test.json")

	storage := &ClaudeTokenStorage{
		Type:         "claude",
		Email:        "user@example.com",
		AccessToken:  "new-claude-access",
		RefreshToken: "new-claude-refresh",
		Expire:       "2026-12-31T23:59:59Z",
		LastRefresh:  "2026-04-14T12:00:00Z",
	}
	storage.SetMetadata(map[string]any{
		"disabled":  false,
		"prefix":    "claude-prefix",
		"note":      "claude custom note",
		"proxy_url": "http://proxy:8080",
		"weight":    float64(5),
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

	if saved["access_token"] != "new-claude-access" {
		t.Errorf("access_token = %v, want new-claude-access", saved["access_token"])
	}
	if saved["prefix"] != "claude-prefix" {
		t.Errorf("prefix = %v, want claude-prefix", saved["prefix"])
	}
	if saved["note"] != "claude custom note" {
		t.Errorf("note = %v, want claude custom note", saved["note"])
	}
	if saved["proxy_url"] != "http://proxy:8080" {
		t.Errorf("proxy_url = %v, want http://proxy:8080", saved["proxy_url"])
	}
	if saved["weight"] != float64(5) {
		t.Errorf("weight = %v, want 5", saved["weight"])
	}
}

func TestSaveTokenToFileUsesPrivateAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "synthetic.json")
	storage := &ClaudeTokenStorage{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}
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
