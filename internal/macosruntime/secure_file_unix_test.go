//go:build !windows

package macosruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadOwnedFileRejectsReplacedDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	stateDirectory := filepath.Join(root, "state")
	statePath := filepath.Join(stateDirectory, "menubar.json")
	if err := writeAtomicFile(statePath, []byte("trusted")); err != nil {
		t.Fatal(err)
	}

	originalDirectory := filepath.Join(root, "state-original")
	if err := os.Rename(stateDirectory, originalDirectory); err != nil {
		t.Fatal(err)
	}
	attackerDirectory := filepath.Join(root, "attacker")
	if err := os.Mkdir(attackerDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attackerDirectory, "menubar.json"), []byte("attacker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attackerDirectory, stateDirectory); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readOwnedFile(statePath, MaxStateBytes); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("readOwnedFile() error = %v, want symlink rejection", err)
	}
	body, _, err := readSecureFile(statePath, MaxStateBytes)
	if err != nil {
		t.Fatalf("path-based external read: %v", err)
	}
	if string(body) != "attacker" {
		t.Fatalf("path-based external read = %q, want attacker fixture", body)
	}
}

func TestSecureFileReadsRejectFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	for name, read := range map[string]func(string, int64) ([]byte, fileIdentity, error){
		"external": readSecureFile,
		"owned":    readOwnedFile,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := read(path, MaxConfigBytes); err == nil ||
				!strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("read() error = %v, want regular-file rejection", err)
			}
		})
	}
}
