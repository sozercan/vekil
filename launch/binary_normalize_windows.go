//go:build windows

package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var npmWindowsShimScriptPattern = regexp.MustCompile(`(?i)%(?:~dp0|dp0%)[\\/]([^"\r\n]+?\.js)`)

func normalizeResolvedExecutable(path string) (resolvedExecutable, error) {
	extension := strings.ToLower(filepath.Ext(path))
	if extension != ".cmd" && extension != ".bat" {
		return resolvedExecutable{path: path}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return resolvedExecutable{}, fmt.Errorf("read Windows command shim %q: %w", path, err)
	}
	matches := npmWindowsShimScriptPattern.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return resolvedExecutable{}, fmt.Errorf(
			"unsupported Windows command shim %q: install a native agent executable or pass --binary",
			path,
		)
	}
	relativeScript := strings.ReplaceAll(string(matches[len(matches)-1][1]), `\`, string(filepath.Separator))
	scriptPath := filepath.Clean(filepath.Join(filepath.Dir(path), relativeScript))
	if _, err := os.Stat(scriptPath); err != nil {
		return resolvedExecutable{}, fmt.Errorf("resolve npm command shim script %q: %w", scriptPath, err)
	}

	nodeCandidates := []string{filepath.Join(filepath.Dir(path), "node.exe"), "node.exe", "node"}
	for _, candidate := range nodeCandidates {
		if nodePath, err := exec.LookPath(candidate); err == nil {
			return resolvedExecutable{path: nodePath, prefixArgs: []string{scriptPath}}, nil
		}
	}
	return resolvedExecutable{}, fmt.Errorf("resolve npm command shim %q: node executable not found", path)
}
