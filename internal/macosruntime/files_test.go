package macosruntime

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEnsurePrivateDirectoryRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on some Windows hosts")
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "vekil")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	err := ensurePrivateDirectory(link)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("ensurePrivateDirectory() error = %v, want symlink rejection", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target permissions = %#o, want 0755", got)
	}
}
