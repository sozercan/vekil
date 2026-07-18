package launch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBinaryNotFound marks an agent executable lookup failure.
var ErrBinaryNotFound = errors.New("agent binary not found")

type resolvedExecutable struct {
	path       string
	prefixArgs []string
}

func resolveExecutable(override, name string, fallbackPaths []string) (resolvedExecutable, error) {
	if candidate := strings.TrimSpace(override); candidate != "" {
		path, err := exec.LookPath(candidate)
		if err != nil {
			return resolvedExecutable{}, fmt.Errorf("%w: %s", ErrBinaryNotFound, candidate)
		}
		return normalizeResolvedExecutable(path)
	}

	if path, err := exec.LookPath(name); err == nil {
		return normalizeResolvedExecutable(path)
	}

	home, _ := os.UserHomeDir()
	for _, candidate := range fallbackPaths {
		if strings.HasPrefix(candidate, "~/") && home != "" {
			candidate = filepath.Join(home, candidate[2:])
		}
		if path, err := exec.LookPath(candidate); err == nil {
			return normalizeResolvedExecutable(path)
		}
	}
	return resolvedExecutable{}, fmt.Errorf("%w: %s", ErrBinaryNotFound, name)
}
