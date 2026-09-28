package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingLogFileKeepsOneBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "menubar.log")
	// An existing readable file becomes private when it is opened.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := openRotatingLogFile(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first line\n", "second line\n", "third line\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != "third line\n" {
		t.Fatalf("current log = %q, %v", current, err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil || string(backup) != "second line\n" {
		t.Fatalf("backup log = %q, %v", backup, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v, %v", info.Mode().Perm(), err)
	}
	// Reopening appends and continues counting from the existing size.
	reopened, err := openRotatingLogFile(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Write([]byte("4th\n")); err != nil {
		t.Fatal(err)
	}
	if current, _ = os.ReadFile(path); string(current) != "third line\n4th\n" {
		t.Fatalf("reopened log = %q", current)
	}
}
