package logger

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

var levelNames = map[Level]string{
	LevelDebug: "debug",
	LevelInfo:  "info",
	LevelWarn:  "warn",
	LevelError: "error",
	LevelFatal: "fatal",
}

func ParseLevel(s string) Level {
	switch s {
	case "debug":
		return LevelDebug
	case "warn":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

type Logger struct {
	level  Level
	writer io.Writer
	mu     sync.Mutex
}

func New(level Level) *Logger {
	return NewWithWriter(level, os.Stderr)
}

func NewWithWriter(level Level, w io.Writer) *Logger {
	if w == nil {
		w = os.Stderr
	}
	return &Logger{level: level, writer: w}
}

func (l *Logger) Enabled(level Level) bool {
	return l != nil && level >= l.level
}

func (l *Logger) log(level Level, msg string, fields map[string]interface{}) {
	if level < l.level {
		return
	}

	e := make(map[string]interface{})
	e["time"] = time.Now().UTC().Format(time.RFC3339)
	e["level"] = levelNames[level]
	e["msg"] = msg
	for k, v := range fields {
		e[k] = v
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_ = json.NewEncoder(l.writer).Encode(e)
}

func (l *Logger) Debug(msg string, fields ...Field) {
	if !l.Enabled(LevelDebug) {
		return
	}
	l.log(LevelDebug, msg, toMap(fields))
}

func (l *Logger) Info(msg string, fields ...Field) {
	if !l.Enabled(LevelInfo) {
		return
	}
	l.log(LevelInfo, msg, toMap(fields))
}

func (l *Logger) Warn(msg string, fields ...Field) {
	if !l.Enabled(LevelWarn) {
		return
	}
	l.log(LevelWarn, msg, toMap(fields))
}

func (l *Logger) Error(msg string, fields ...Field) {
	if !l.Enabled(LevelError) {
		return
	}
	l.log(LevelError, msg, toMap(fields))
}

func (l *Logger) Fatal(msg string, fields ...Field) {
	l.log(LevelFatal, msg, toMap(fields))
	os.Exit(1)
}

type Field struct {
	Key   string
	Value interface{}
}

func F(key string, value interface{}) Field {
	return Field{Key: key, Value: value}
}

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
