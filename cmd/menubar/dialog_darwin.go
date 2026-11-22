package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func confirmAction(ctx context.Context, prompt confirmationPrompt) bool {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {%q, %q} default button %q`,
		prompt.Message, prompt.Title, prompt.DeclineLabel, prompt.ApproveLabel, prompt.ApproveLabel,
	)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return false
	}
	result := strings.TrimSpace(string(out))
	result = strings.TrimPrefix(result, "button returned:")
	return result == prompt.ApproveLabel
}

func showErrorDialog(title, message string) {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {"OK"} default button "OK" with icon stop`,
		message, title,
	)
	_ = exec.Command("osascript", "-e", script).Run()
}

func chooseProvidersConfigPath() (string, error) {
	script := `POSIX path of (choose file with prompt "Choose a providers config JSON or YAML file")`
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		if isOsascriptCancel(err, out) {
			return "", errDialogCanceled
		}
		message := strings.TrimSpace(string(out))
		if message == "" {
			return "", fmt.Errorf("run osascript file chooser: %w", err)
		}
		return "", fmt.Errorf("run osascript file chooser: %w: %s", err, message)
	}

	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errDialogCanceled
	}

	return path, nil
}

func isOsascriptCancel(err error, output []byte) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return exitErr.ExitCode() == 1 && strings.Contains(string(output), "(-128)")
}

func copyToClipboard(text string) {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}

func openURL(url string) {
	_ = exec.Command("open", url).Run()
}

func showNotification(title, message string) {
	script := fmt.Sprintf(
		`display notification %q with title %q`,
		message, title,
	)
	_ = exec.Command("osascript", "-e", script).Run()
}
