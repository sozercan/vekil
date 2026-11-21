package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func codexConfigDirectory(args []string) string {
	var directory string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--":
			return directory
		case arg == "-C" || arg == "--cd":
			if index+1 < len(args) {
				index++
				directory = args[index]
			}
		case strings.HasPrefix(arg, "--cd="):
			directory = strings.TrimPrefix(arg, "--cd=")
		case strings.HasPrefix(arg, "-C"):
			directory = strings.TrimPrefix(strings.TrimPrefix(arg, "-C"), "=")
		case codexOptionRequiresValue(arg):
			index++
		}
	}
	return directory
}

func checkCodexLaunchProvider(executable resolvedExecutable, environment []string, directory string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path, err := filepath.Abs(executable.path)
	if err != nil {
		return fmt.Errorf("resolve Codex executable path: %w", err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve Codex launch directory: %w", err)
	}
	for index := len(environment) - 1; index >= 0; index-- {
		key, home := splitEnvironmentEntry(environment[index])
		if canonicalEnvironmentKey(key) != "CODEX_HOME" {
			continue
		}
		if home != "" && !filepath.IsAbs(home) {
			absoluteHome, err := filepath.Abs(home)
			if err != nil {
				return fmt.Errorf("resolve Codex home directory: %w", err)
			}
			environment = applyEnvironment(environment, nil, map[string]string{"CODEX_HOME": absoluteHome})
		}
		break
	}
	args := append(append([]string(nil), executable.prefixArgs...), "app-server")
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env, cmd.Dir = environment, directory
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdout.Close() }()
	controller, err := newProcessController(cmd, nil)
	if err != nil {
		return err
	}
	defer func() { _ = controller.close() }()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Codex configuration check: %w", err)
	}
	if err := controller.afterStart(); err != nil {
		return reapFailedContainedCommand(controller, err)
	}
	defer func() {
		_ = controller.kill()
		_ = cmd.Wait()
	}()
	stopRead := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	defer stopRead()

	const initialize = `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"vekil","version":"0"}}}
{"method":"initialized"}
`
	request := initialize + `{"id":2,"method":"config/read","params":{"includeLayers":false,"cwd":` + configString(directory) + "}}\n"
	if _, err := io.WriteString(stdin, request); err != nil {
		return fmt.Errorf("request Codex configuration: %w", err)
	}
	decoder := json.NewDecoder(io.LimitReader(stdout, 8<<20))
	for {
		var reply struct {
			ID     json.RawMessage `json:"id"`
			Result *struct {
				Config *struct {
					Providers map[string]json.RawMessage `json:"model_providers"`
				} `json:"config"`
			} `json:"result"`
		}
		if err := decoder.Decode(&reply); err != nil {
			return errors.New("could not read effective Codex configuration; check that Codex can start in the launch directory")
		}
		if string(reply.ID) != "2" {
			continue
		}
		if reply.Result == nil || reply.Result.Config == nil {
			return errors.New("missing effective configuration in Codex response")
		}
		if _, exists := reply.Result.Config.Providers[codexProviderID]; exists {
			return fmt.Errorf("provider %q is reserved by Vekil launch; rename or remove it from Codex configuration", codexProviderID)
		}
		return nil
	}
}
