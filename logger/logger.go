// Package logger provides structured JSON logging with level filtering.
package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Level represents the severity of a log message.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelError
	LevelFatal
)

var levelNames = map[Level]string{
	LevelDebug: "debug",
	LevelInfo:  "info",
	LevelError: "error",
	LevelFatal: "fatal",
}

// ParseLevel parses a log level string. Defaults to info for unknown values.
func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return LevelDebug
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger writes structured JSON log entries to stderr.
type Logger struct {
	level Level
	mu    sync.Mutex
}

// New creates a Logger that emits messages at or above the given level.
func New(level Level) *Logger {
	return &Logger{level: level}
}

func (l *Logger) log(level Level, msg string, fields map[string]interface{}) {
	if level < l.level {
		return
	}

	e := make(map[string]interface{}, len(fields)+3)
	e["time"] = time.Now().UTC().Format(time.RFC3339)
	e["level"] = levelNames[level]
	e["msg"] = msg
	for k, v := range fields {
		e[k] = v
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_ = json.NewEncoder(os.Stderr).Encode(e)
}

// Debug logs a message at debug level.
func (l *Logger) Debug(msg string, fields ...Field) {
	l.log(LevelDebug, msg, toMap(fields))
}

// Info logs a message at info level.
func (l *Logger) Info(msg string, fields ...Field) {
	l.log(LevelInfo, msg, toMap(fields))
}

// Error logs a message at error level.
func (l *Logger) Error(msg string, fields ...Field) {
	l.log(LevelError, msg, toMap(fields))
}

// Fatal logs a message at fatal level and exits with code 1.
func (l *Logger) Fatal(msg string, fields ...Field) {
	l.log(LevelFatal, msg, toMap(fields))
	os.Exit(1)
}

// Field is a key-value pair for structured log context.
type Field struct {
	Key   string
	Value interface{}
}

// F creates a log field.
func F(key string, value interface{}) Field {
	return Field{Key: key, Value: value}
}

// Err creates an error log field.
func Err(err error) Field {
	return Field{Key: "error", Value: fmt.Sprintf("%v", err)}
}

func toMap(fields []Field) map[string]interface{} {
	if len(fields) == 0 {
		return nil
	}
	m := make(map[string]interface{}, len(fields))
	for _, f := range fields {
		m[f.Key] = f.Value
	}
	return m
}
