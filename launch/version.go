package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var semanticVersionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

type minimumVersion struct {
	major int
	minor int
	patch int
}

func runContainedCommand(ctx context.Context, path string, args, environment []string) ([]byte, error) {
	var output strings.Builder
	cmd := exec.Command(path, args...)
	cmd.Env = append([]string(nil), environment...)
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = time.Second
	controller, err := newProcessController(cmd, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = controller.close() }()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := controller.afterStart(); err != nil {
		return []byte(output.String()), reapFailedContainedCommand(controller, err)
	}
	waitCh := make(chan commandOutcome, 1)
	go func() {
		waitCh <- waitAndCloseController(controller)
	}()
	select {
	case outcome := <-waitCh:
		if outcome.err != nil {
			return []byte(output.String()), outcome.err
		}
		if outcome.code != 0 {
			return []byte(output.String()), fmt.Errorf("process exited with code %d", outcome.code)
		}
		return []byte(output.String()), nil
	case <-ctx.Done():
		outcome := stopCommand(controller, waitCh, time.Second, os.Interrupt, false)
		// Forced termination can itself fail or time out, leaving the wait/pipe
		// goroutine alive. Do not read output while that goroutine may still be
		// writing to the non-concurrency-safe builder. Callers discard command
		// output whenever cancellation returns an error.
		return nil, errors.Join(ctx.Err(), outcome.err)
	}
}

func reapFailedContainedCommand(controller processController, cause error) error {
	killErr := controller.kill()
	outcome := waitAndCloseController(controller)
	return errors.Join(cause, killErr, outcome.err)
}

func validateExecutableVersion(
	executable resolvedExecutable,
	environment []string,
	displayName string,
	requiredOutput string,
	minimum minimumVersion,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(append([]string(nil), executable.prefixArgs...), "--version")
	output, err := runContainedCommand(ctx, executable.path, args, environment)
	if err != nil {
		return fmt.Errorf("check %s version: %w", displayName, err)
	}
	return validateVersionOutput(string(output), displayName, requiredOutput, minimum)
}

func validateVersionOutput(output, displayName, requiredOutput string, minimum minimumVersion) error {
	trimmedOutput := strings.TrimSpace(output)
	match := productVersionMatch(trimmedOutput, requiredOutput)
	if match == nil {
		return fmt.Errorf("check %s version: unrecognized output %q", displayName, trimmedOutput)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	if compareVersion(major, minor, patch, minimum.major, minimum.minor, minimum.patch) < 0 {
		return fmt.Errorf(
			"unsupported %s version %d.%d.%d; version %d.%d.%d or newer is required",
			displayName,
			major,
			minor,
			patch,
			minimum.major,
			minimum.minor,
			minimum.patch,
		)
	}
	return nil
}

func productVersionMatch(output, requiredOutput string) []string {
	if strings.TrimSpace(requiredOutput) == "" {
		return semanticVersionPattern.FindStringSubmatch(output)
	}
	required := strings.ToLower(strings.TrimSpace(requiredOutput))
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(strings.ToLower(line), required) {
			continue
		}
		if match := semanticVersionPattern.FindStringSubmatch(line); match != nil {
			return match
		}
	}
	return nil
}

func compareVersion(major, minor, patch, wantMajor, wantMinor, wantPatch int) int {
	for _, pair := range [][2]int{{major, wantMajor}, {minor, wantMinor}, {patch, wantPatch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}
