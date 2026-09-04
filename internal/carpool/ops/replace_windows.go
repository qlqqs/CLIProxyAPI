//go:build windows

package ops

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func replaceFileAtomically(sourcePath, targetPath string) error {
	sourceUTF16, errSource := windows.UTF16PtrFromString(sourcePath)
	if errSource != nil {
		return fmt.Errorf("encode source path: %w", errSource)
	}
	targetUTF16, errTarget := windows.UTF16PtrFromString(targetPath)
	if errTarget != nil {
		return fmt.Errorf("encode target path: %w", errTarget)
	}
	if errMove := windows.MoveFileEx(sourceUTF16, targetUTF16, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); errMove != nil {
		return fmt.Errorf("move file: %w", errMove)
	}
	return nil
}

func syncDirectory(string) error {
	return nil
}
