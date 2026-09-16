package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatePruneCommandRejectsInvalidInputWithoutCreatingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, args := range [][]string{
		nil, {"unknown"}, {"prune"},
		{"prune", "--file", path, "--before", past},
		{"prune", "--file", path, "--before", future, "--confirm"},
		{"prune", "--file", path, "--before", "invalid", "--confirm"},
		{"prune", "--file", path, "--before", "2020-01-01T00:00:00.500Z", "--confirm"},
		{"prune", "--file", path, "--before", "2020-01-01T01:00:00.000000001+01:00", "--confirm"},
		{"prune", "--file", path, "--before", "2020-01-01T00:00:00.0000000001Z", "--confirm"},
		{"prune", "--file", path, "--before", "2020-01-01T00:00:00,0000000001Z", "--confirm"},
		{"prune", "--file", path, "--before", "2020-01-01T00:00:00.000Z", "--confirm"},
		{"prune", "--file", path, "--before", past, "--confirm", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if status := runState(args, &stdout, &stderr); status != 2 {
			t.Fatalf("args %v: status %d", args, status)
		}
		if stdout.Len() != 0 {
			t.Fatal("invalid prune reported success")
		}
	}
	var stdout, stderr bytes.Buffer
	if status := runState([]string{"prune", "--file", path, "--before", past, "--confirm"}, &stdout, &stderr); status != 1 {
		t.Fatalf("missing file status = %d", status)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prune created missing authority")
	}
}
