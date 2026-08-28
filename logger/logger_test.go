package logger

import "testing"

func TestNilLoggerWarnIsNoop(t *testing.T) {
	var log *Logger
	log.Warn("ignored", F("field", "value"))
}
