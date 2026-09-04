//go:build !windows

package ops

import (
	"errors"
	"fmt"
	"os"
)

func replaceFileAtomically(sourcePath, targetPath string) error {
	return os.Rename(sourcePath, targetPath)
}

func syncDirectory(path string) (err error) {
	directory, errOpen := os.Open(path)
	if errOpen != nil {
		return fmt.Errorf("carpool operation: open destination directory: %w", errOpen)
	}
	defer func() {
		if errClose := directory.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("carpool operation: close destination directory: %w", errClose))
		}
	}()
	if errSync := directory.Sync(); errSync != nil {
		return fmt.Errorf("carpool operation: sync destination directory: %w", errSync)
	}
	return nil
}
