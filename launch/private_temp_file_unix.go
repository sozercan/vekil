//go:build !windows

package launch

import (
	"fmt"
	"os"
)

func openPrivateTempFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("restrict private temporary file: %w", err)
	}
	return file, nil
}
