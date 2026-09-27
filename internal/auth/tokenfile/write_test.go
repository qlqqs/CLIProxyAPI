package tokenfile

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteJSONRenameFailurePreservesTargetAndCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(target, map[string]any{"access_token": "synthetic"}); err == nil {
		t.Fatal("directory replaced by credential file")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "original" {
		t.Fatal("failed replacement damaged original target")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "token.json" {
		t.Fatal("temporary secret file leaked")
	}
}

func TestWriteJSONReplacesRatherThanTruncating(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("open-handle replacement semantics differ on Windows")
	}
	path := filepath.Join(t.TempDir(), "token.json")
	if err := WriteJSON(path, map[string]string{"access_token": "synthetic-old"}); err != nil {
		t.Fatal(err)
	}
	previous, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() {
		if errClose := previous.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	if err := WriteJSON(path, map[string]string{"access_token": "synthetic-new"}); err != nil {
		t.Fatal(err)
	}
	oldData, errRead := io.ReadAll(previous)
	if errRead != nil || string(oldData) != "{\"access_token\":\"synthetic-old\"}\n" {
		t.Fatalf("previous reader lost its complete generation: %q, %v", oldData, errRead)
	}
	newData, errRead := os.ReadFile(path)
	if errRead != nil || string(newData) != "{\"access_token\":\"synthetic-new\"}\n" {
		t.Fatalf("replacement is incomplete: %q, %v", newData, errRead)
	}
}
