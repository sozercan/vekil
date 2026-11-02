package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServeEarlyFileArgs(t *testing.T) {
	envFile, logFile := serveEarlyFileArgs([]string{"serve", "--port", "1", "--env-file", "a.env", "-log-file=b.log", "--", "--env-file=ignored"})
	if envFile != "a.env" || logFile != "b.log" {
		t.Fatalf("got env=%q log=%q", envFile, logFile)
	}
}

func TestParseServeEnvFile(t *testing.T) {
	got, err := parseServeEnvFile(strings.NewReader("# comment\n\nA=1\nexport B = \"two words\"\nC='x=y'\nD=\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"A", "1"}, {"B", "two words"}, {"C", "x=y"}, {"D", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"A=1\nnope\n", "1A=x\n", "A-B=x\n"} {
		if _, err := parseServeEnvFile(strings.NewReader(bad)); err == nil {
			t.Errorf("parseServeEnvFile(%q) succeeded", bad)
		}
	}
}

func TestLoadServeEnvFileOverridesEnvironment(t *testing.T) {
	t.Setenv("VEKIL_TEST_ENV_FILE_A", "from-process")
	t.Setenv("VEKIL_TEST_ENV_FILE_B", "kept")
	path := filepath.Join(t.TempDir(), "service.env")
	if err := os.WriteFile(path, []byte("VEKIL_TEST_ENV_FILE_A=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadServeEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("VEKIL_TEST_ENV_FILE_A"); got != "from-file" {
		t.Fatalf("A = %q, want from-file", got)
	}
	if got := os.Getenv("VEKIL_TEST_ENV_FILE_B"); got != "kept" {
		t.Fatalf("B = %q, want kept", got)
	}
}
