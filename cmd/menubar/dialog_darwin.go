package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// confirmAction displays a macOS confirmation dialog using osascript and
// returns true only when the user clicks the exact approve label. false is
// the safe default for a declined, cancelled, or dismissed dialog.
func confirmAction(ctx context.Context, prompt confirmationPrompt) bool {
	script := fmt.Sprintf(
		`display dialog %q with title %q buttons {%q, %q} default button %q`,
		prompt.Message, prompt.Title, prompt.DeclineLabel, prompt.ApproveLabel, prompt.ApproveLabel,
	)
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		// User clicked decline, closed the dialog, or ctx was cancelled.
		return false
	}
	// osascript returns "button returned:Open GitHub\n"
	result := strings.TrimSpace(string(out))
	result = strings.TrimPrefix(result, "button returned:")
	return result == prompt.ApproveLabel
}

// showErrorDialog displays a simple macOS error dialog with an OK button.
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

// copyToClipboard copies the given text to the macOS clipboard using pbcopy.
func copyToClipboard(text string) {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}

// openURL opens a URL in the default browser using the macOS open command.
func openURL(url string) {
	_ = exec.Command("open", url).Run()
}

// showNotification displays a macOS notification using osascript.
func showNotification(title, message string) {
	script := fmt.Sprintf(
		`display notification %q with title %q`,
		message, title,
	)
	_ = exec.Command("osascript", "-e", script).Run()
}
