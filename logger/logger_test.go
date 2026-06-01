package logger

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stderr = w

	fn()

	_ = w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}

	return buf.String()
}

func TestLoggerLevelErrorSuppressesDebugAndInfo(t *testing.T) {
	output := captureStderr(t, func() {
		log := New(LevelError)
		log.Debug("debug message")
		log.Info("info message")
		log.Error("error message")
	})

	if strings.Contains(output, `"level":"debug"`) {
		t.Fatalf("output should not contain debug logs, got %q", output)
	}
	if strings.Contains(output, `"level":"info"`) {
		t.Fatalf("output should not contain info logs, got %q", output)
	}
	if !strings.Contains(output, `"level":"error"`) {
		t.Fatalf("output should contain error logs, got %q", output)
	}
}

func TestLoggerLevelErrorStillEmitsFatal(t *testing.T) {
	output := captureStderr(t, func() {
		log := New(LevelError)
		log.log(LevelFatal, "fatal message", nil)
	})

	if !strings.Contains(output, `"level":"fatal"`) {
		t.Fatalf("output should contain fatal logs, got %q", output)
	}
}
