package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var (
	execLookPath = exec.LookPath
	execCommand  = exec.Command
)

// showOsascriptDialog displays a dialog using PowerShell and returns the button
// the user clicked. If no dialog mechanism is available, defaultButton is
// returned so that the calling flow proceeds automatically.
func showOsascriptDialog(title, message, defaultButton, secondButton string) string {
	// Use PowerShell to show a Windows message box with Yes/No buttons.
	// Map Yes -> defaultButton, No/Cancel -> "Cancel".
	script := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; `+
			`$result = [System.Windows.Forms.MessageBox]::Show('%s', '%s', 'YesNo', 'Question'); `+
			`Write-Output $result`,
		escapePowerShellString(message),
		escapePowerShellString(title),
	)

	out, err := execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return defaultButton
	}

	result := strings.TrimSpace(string(out))
	if result == "Yes" {
		return defaultButton
	}
	return "Cancel"
}

// showErrorDialog displays a simple error dialog using PowerShell.
func showErrorDialog(title, message string) {
	script := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; `+
			`[System.Windows.Forms.MessageBox]::Show('%s', '%s', 'OK', 'Error')`,
		escapePowerShellString(message),
		escapePowerShellString(title),
	)
	_ = execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

func chooseProvidersConfigPath() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; ` +
		`$f = New-Object System.Windows.Forms.OpenFileDialog; ` +
		`$f.Title = 'Choose Providers Config'; ` +
		`$f.Filter = 'Provider config files (*.json;*.yaml;*.yml)|*.json;*.yaml;*.yml|All files (*.*)|*.*'; ` +
		`if ($f.ShowDialog() -eq 'OK') { Write-Output $f.FileName } else { exit 1 }`

	out, err := execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errDialogCanceled
		}
		return "", fmt.Errorf("run file dialog: %w", err)
	}

	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errDialogCanceled
	}
	return path, nil
}

// copyToClipboard copies the given text to the Windows clipboard using clip.exe.
func copyToClipboard(text string) {
	cmd := execCommand("clip")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}

// openURL opens a URL in the default browser using cmd /c start.
func openURL(url string) {
	_ = execCommand("cmd", "/c", "start", "", url).Start()
}

// showNotification displays a Windows notification using PowerShell.
func showNotification(title, message string) {
	script := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; `+
			`$n = New-Object System.Windows.Forms.NotifyIcon; `+
			`$n.Icon = [System.Drawing.SystemIcons]::Information; `+
			`$n.Visible = $true; `+
			`$n.ShowBalloonTip(5000, '%s', '%s', 'Info'); `+
			`Start-Sleep -Milliseconds 100; `+
			`$n.Dispose()`,
		escapePowerShellString(title),
		escapePowerShellString(message),
	)
	_ = execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}

// escapePowerShellString escapes single quotes for use inside PowerShell
// single-quoted string literals.
func escapePowerShellString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
