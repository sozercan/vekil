package macosruntime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteExclusiveFileRemovesPartialFileOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned")
	injected := errors.New("injected write failure")
	err := writeExclusiveFileWithBody(path, func(file *os.File) error {
		if _, writeErr := file.Write([]byte("partial")); writeErr != nil {
			return writeErr
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("writeExclusiveFileWithBody() error = %v, want injected failure", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial owned file remains: %v", err)
	}
}

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

func TestPrivateDirectoryOperationsStayBoundAfterPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on some Windows hosts")
	}

	parent := t.TempDir()
	path := filepath.Join(parent, "vekil")
	directory, err := openPrivateDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.close() }()

	original := filepath.Join(parent, "original")
	redirect := filepath.Join(parent, "redirect")
	if err := os.Mkdir(redirect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(redirect, path); err != nil {
		t.Fatal(err)
	}

	file, err := directory.createExclusive("pending")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("owned")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := directory.rename("pending", "installed"); err != nil {
		t.Fatal(err)
	}
	if err := directory.sync(); err != nil {
		t.Fatal(err)
	}

	if got, err := os.ReadFile(filepath.Join(original, "installed")); err != nil || string(got) != "owned" {
		t.Fatalf("descriptor-relative write = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(redirect, "installed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write followed replacement symlink: %v", err)
	}
	if err := directory.remove("installed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(original, "installed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor-relative cleanup failed: %v", err)
	}
}
