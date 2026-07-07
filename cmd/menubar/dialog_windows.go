//go:build windows

package main

import (
	"os/exec"
	"strings"
)

var powershellCommand = exec.Command

func showOsascriptDialog(title, message, defaultButton, secondButton string) string {
	// Use WinForms because it is present on stock Windows desktop installs where
	// the tray app runs. If PowerShell is unavailable, proceed with the default
	// action so sign-in can continue via the browser.
	script := `& { param($title, $message) Add-Type -AssemblyName System.Windows.Forms; $result = [System.Windows.Forms.MessageBox]::Show($message, $title, [System.Windows.Forms.MessageBoxButtons]::OKCancel, [System.Windows.Forms.MessageBoxIcon]::Information); if ($result -eq [System.Windows.Forms.DialogResult]::OK) { "OK" } else { "Cancel" } }`
	out, err := powershellCommand("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script, title, message).Output()
	if err != nil {
		return defaultButton
	}
	if strings.EqualFold(strings.TrimSpace(string(out)), "Cancel") {
		return secondButton
	}
	return defaultButton
}

func showErrorDialog(title, message string) {
	showWindowsMessageBox(title, message, "Error")
}

func showWindowsMessageBox(title, message, icon string) {
	script := `& { param($title, $message, $icon) Add-Type -AssemblyName System.Windows.Forms; $boxIcon = [System.Windows.Forms.MessageBoxIcon]::$icon; [System.Windows.Forms.MessageBox]::Show($message, $title, [System.Windows.Forms.MessageBoxButtons]::OK, $boxIcon) | Out-Null }`
	_ = powershellCommand("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script, title, message, icon).Run()
}

func chooseProvidersConfigPath() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.OpenFileDialog; $d.Title = 'Choose Providers Config'; $d.Filter = 'Provider config files (*.json;*.yaml;*.yml)|*.json;*.yaml;*.yml|All files (*.*)|*.*'; if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { $d.FileName }`
	out, err := powershellCommand("powershell", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-Command", script).Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", errDialogCanceled
	}
	return path, nil
}

func copyToClipboard(text string) {
	cmd := powershellCommand("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", `& { param($text) Set-Clipboard -Value $text }`, text)
	_ = cmd.Run()
}

func openURL(rawURL string) {
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
}

func showNotification(title, message string) {
	// Windows toast notifications require an AppUserModelID/shortcut. For this
	// first tray app, use a lightweight message box fallback rather than silently
	// failing important status feedback.
	if strings.TrimSpace(title) == "" && strings.TrimSpace(message) == "" {
		return
	}
	showWindowsMessageBox(title, message, "Information")
}
