package launch

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const privateTempFileAttempts = 100

func writePrivateTempFile(prefix string, data []byte) (string, func() error, error) {
	for range privateTempFileAttempts {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", nil, fmt.Errorf("generate private temporary filename: %w", err)
		}
		path := filepath.Join(os.TempDir(), prefix+hex.EncodeToString(random))
		file, err := openPrivateTempFile(path)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", nil, err
		}
		cleanup := func() error {
			err := os.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			_ = cleanup()
			return "", nil, fmt.Errorf("write private temporary file: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = cleanup()
			return "", nil, fmt.Errorf("close private temporary file: %w", err)
		}
		return path, cleanup, nil
	}
	return "", nil, fmt.Errorf("create private temporary file: exhausted %d attempts", privateTempFileAttempts)
}
