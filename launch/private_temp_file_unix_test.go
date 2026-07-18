//go:build !windows

package launch

import (
	"os"
	"testing"
)

func TestWritePrivateTempFileUsesOwnerOnlyPermissions(t *testing.T) {
	path, cleanup, err := writePrivateTempFile("vekil-private-mode-", []byte("secret"))
	if err != nil {
		t.Fatalf("writePrivateTempFile() error = %v", err)
	}
	defer func() { _ = cleanup() }()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat private temporary file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("private temporary file mode = %#o, want 0600", got)
	}
}
