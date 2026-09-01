package logger

import (
	"strings"
	"testing"
)

func TestParseLevelStrict(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  Level
	}{
		{name: "debug", value: "debug", want: LevelDebug},
		{name: "info", value: "info", want: LevelInfo},
		{name: "warn", value: "warn", want: LevelWarn},
		{name: "error", value: "error", want: LevelError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseLevelStrict(tc.value)
			if err != nil {
				t.Fatalf("ParseLevelStrict(%q) error = %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("ParseLevelStrict(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseLevelStrictRejectsUnknownLevel(t *testing.T) {
	_, err := ParseLevelStrict("trace")
	if err == nil {
		t.Fatal("ParseLevelStrict(\"trace\") error = nil, want rejection")
	}
	for _, want := range []string{"trace", "debug", "info", "warn", "error"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("ParseLevelStrict(\"trace\") error = %q, want %q", err, want)
		}
	}
}

func TestNilLoggerWarnIsNoop(t *testing.T) {
	var log *Logger
	log.Warn("ignored", F("field", "value"))
}
