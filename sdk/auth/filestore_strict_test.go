package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestFileStoreStrictDisabledWriteRequiresDurableRecord(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	record := &coreauth.Auth{ID: "strict-missing.json", FileName: "strict-missing.json", Provider: "codex", Disabled: true, Metadata: map[string]any{"type": "codex", "access_token": "synthetic"}}
	path := filepath.Join(dir, record.FileName)
	// Legacy watcher saves must still avoid resurrecting removed disabled files.
	if saved, errSave := store.Save(context.Background(), record); errSave != nil || saved != "" {
		t.Fatalf("legacy disabled write = %q, %v", saved, errSave)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatalf("legacy save created missing file: %v", errStat)
	}
	// An explicitly acknowledged status change cannot silently skip persistence.
	saved, errSave := store.Save(coreauth.WithStrictPersistence(context.Background()), record)
	if errSave != nil || saved != path {
		t.Fatalf("strict disabled write = %q, %v", saved, errSave)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var metadata map[string]any
	if errDecode := json.Unmarshal(data, &metadata); errDecode != nil {
		t.Fatal(errDecode)
	}
	if metadata["disabled"] != true || metadata["access_token"] != "synthetic" {
		t.Fatal("strict save did not persist the disabled credential")
	}
}
