package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sozercan/vekil/logger"
)

const (
	serveEnvFileFlag = "env-file"
	serveLogFileFlag = "log-file"
)

// serveEarlyFileArgs finds --env-file and --log-file before normal flag
// parsing, because the env file must be loaded before flag defaults read the
// environment, and the log file must be open to report env-file errors.
func serveEarlyFileArgs(args []string) (envFile, logFile string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !strings.HasPrefix(arg, "-") || (name != serveEnvFileFlag && name != serveLogFileFlag) {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				continue
			}
			i++
			value = args[i]
		}
		if name == serveEnvFileFlag {
			envFile = value
		} else {
			logFile = value
		}
	}
	return envFile, logFile
}

// loadServeEnvFile sets each KEY=value from path in the process environment.
// Values in the file override existing variables, matching systemd's
// EnvironmentFile precedence.
func loadServeEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer func() { _ = file.Close() }()
	vars, err := parseServeEnvFile(file)
	if err != nil {
		return fmt.Errorf("env file %s: %w", path, err)
	}
	for _, kv := range vars {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return fmt.Errorf("env file %s: set %s: %w", path, kv[0], err)
		}
	}
	return nil
}

// parseServeEnvFile reads KEY=value lines. Blank lines and lines starting with
// # are ignored, an optional "export " prefix is accepted, and one pair of
// matching surrounding single or double quotes is removed from the value.
func parseServeEnvFile(r io.Reader) ([][2]string, error) {
	var vars [][2]string
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || !validServeEnvKey(key) {
			return nil, fmt.Errorf("line %d: expected KEY=value", line)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars = append(vars, [2]string{key, value})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return vars, nil
}

func validServeEnvKey(key string) bool {
	if key == "" || (key[0] >= '0' && key[0] <= '9') {
		return false
	}
	for _, r := range key {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// prepareServeFiles opens --log-file and loads --env-file. It returns the log
// writer, which is nil when logs should go to stderr.
func prepareServeFiles(args []string) (io.Writer, error) {
	envFile, logFile := serveEarlyFileArgs(args)
	var writer io.Writer
	if logFile != "" {
		file, err := logger.OpenRotatingFile(logFile, logger.DefaultRotatingFileMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("open log file: %w", err)
		}
		writer = file
		// Logs now go to the file, so a console created only for this
		// process (for example by a Windows logon task) is not needed.
		detachOwnedConsole()
	}
	if envFile != "" {
		if err := loadServeEnvFile(envFile); err != nil {
			return writer, err
		}
	}
	return writer, nil
}
