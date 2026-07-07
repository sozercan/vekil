package proxy

import (
	"os"
	"strings"
	"testing"
)

func TestExpandEnvVars_Present(t *testing.T) {
	t.Setenv("VEKIL_TEST_KEY", "secret123")

	input := []byte(`{"api_key": "${env:VEKIL_TEST_KEY}"}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"api_key": "secret123"}`
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_MultipleVars(t *testing.T) {
	t.Setenv("VEKIL_TEST_HOST", "https://example.com")
	t.Setenv("VEKIL_TEST_TOKEN", "tok_abc")

	input := []byte(`base_url: ${env:VEKIL_TEST_HOST}
key: ${env:VEKIL_TEST_TOKEN}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "base_url: https://example.com\nkey: tok_abc"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_SameVarMultipleTimes(t *testing.T) {
	t.Setenv("VEKIL_TEST_VAL", "x")

	input := []byte(`a: ${env:VEKIL_TEST_VAL}, b: ${env:VEKIL_TEST_VAL}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "a: x, b: x"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_Missing(t *testing.T) {
	if err := os.Unsetenv("VEKIL_TEST_MISSING_VAR"); err != nil {
		t.Fatal(err)
	}

	input := []byte(`{"key": "${env:VEKIL_TEST_MISSING_VAR}"}`)
	_, err := expandEnvVars(input)
	if err == nil {
		t.Fatal("expected error for missing env var, got nil")
	}
	if !strings.Contains(err.Error(), "VEKIL_TEST_MISSING_VAR") {
		t.Errorf("error should mention variable name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "undefined environment variable") {
		t.Errorf("error should mention undefined, got: %v", err)
	}
}

func TestExpandEnvVars_MultipleMissing(t *testing.T) {
	if err := os.Unsetenv("VEKIL_TEST_MISS_A"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("VEKIL_TEST_MISS_B"); err != nil {
		t.Fatal(err)
	}

	input := []byte(`a: ${env:VEKIL_TEST_MISS_A}, b: ${env:VEKIL_TEST_MISS_B}`)
	_, err := expandEnvVars(input)
	if err == nil {
		t.Fatal("expected error for missing env vars, got nil")
	}
	if !strings.Contains(err.Error(), "VEKIL_TEST_MISS_A") {
		t.Errorf("error should mention VEKIL_TEST_MISS_A, got: %v", err)
	}
	if !strings.Contains(err.Error(), "VEKIL_TEST_MISS_B") {
		t.Errorf("error should mention VEKIL_TEST_MISS_B, got: %v", err)
	}
}

func TestExpandEnvVars_Escaped(t *testing.T) {
	// Backslash-escaped reference should not be expanded.
	input := []byte(`literal: \${env:SOME_VAR}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `literal: ${env:SOME_VAR}`
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_EscapedAndUnescapedMixed(t *testing.T) {
	t.Setenv("VEKIL_TEST_REAL", "realval")

	input := []byte(`a: \${env:LITERAL}, b: ${env:VEKIL_TEST_REAL}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `a: ${env:LITERAL}, b: realval`
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_NoPatterns(t *testing.T) {
	input := []byte(`{"key": "plain-value", "num": 42}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(input) {
		t.Errorf("content should be unchanged: got %q, want %q", string(got), string(input))
	}
}

func TestExpandEnvVars_EmptyInput(t *testing.T) {
	got, err := expandEnvVars([]byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty output, got %q", string(got))
	}
}

func TestExpandEnvVars_EmptyValue(t *testing.T) {
	// An env var that is set but empty should substitute to empty string.
	t.Setenv("VEKIL_TEST_EMPTY", "")

	input := []byte(`key: ${env:VEKIL_TEST_EMPTY}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "key: "
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}

func TestExpandEnvVars_UnderscoreAndDigits(t *testing.T) {
	t.Setenv("_VAR_123", "works")

	input := []byte(`val: ${env:_VAR_123}`)
	got, err := expandEnvVars(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "val: works"
	if string(got) != want {
		t.Errorf("got %q, want %q", string(got), want)
	}
}
