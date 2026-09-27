// Package tokenfile persists OAuth credentials without exposing partial or permissive files.
package tokenfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteJSON atomically replaces a token file using a private temporary file in the same directory.
func WriteJSON(path string, value any) (err error) {
	data, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return fmt.Errorf("marshal token file: %w", errMarshal)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return fmt.Errorf("create token directory: %w", errMkdir)
	}
	file, errCreate := os.CreateTemp(dir, ".oauth-token-*")
	if errCreate != nil {
		return fmt.Errorf("create private token file: %w", errCreate)
	}
	temporaryPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			if errClose := file.Close(); errClose != nil {
				err = errors.Join(err, fmt.Errorf("close private token file: %w", errClose))
			}
		}
		if errRemove := os.Remove(temporaryPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove private token file: %w", errRemove))
		}
	}()

	// CreateTemp creates a 0600 file; chmod also preserves this contract under restrictive umasks.
	if errChmod := file.Chmod(0o600); errChmod != nil {
		return fmt.Errorf("set token file permissions: %w", errChmod)
	}
	if _, errWrite := file.Write(data); errWrite != nil {
		return fmt.Errorf("write private token file: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		return fmt.Errorf("sync private token file: %w", errSync)
	}
	errClose := file.Close()
	closed = true
	if errClose != nil {
		return fmt.Errorf("close private token file: %w", errClose)
	}
	// Never remove the original on rename failure: it must remain usable.
	if errRename := os.Rename(temporaryPath, path); errRename != nil {
		return fmt.Errorf("replace token file: %w", errRename)
	}
	return nil
}
