//go:build windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")

	// Testable function vars (same pattern as dialog_linux.go).
	execLookPath = exec.LookPath
	execCommand  = exec.Command
	messageBox   = messageBoxW
)

// MessageBox button/icon constants.
const (
	mbOK              = 0x00000000
	mbOKCancel        = 0x00000001
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconQuestion    = 0x00000020
	mbIconInformation = 0x00000040

	idOK  = 1
	idYes = 6
	idNo  = 7
)

// messageBoxW calls the Windows MessageBoxW API.
func messageBoxW(title, message string, flags uint32) int {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)
	ret, _, _ := procMessageBoxW.Call(
		0, // hWnd: NULL (no owner)
		uintptr(unsafe.Pointer(msgPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
	return int(ret)
}

// showOsascriptDialog displays a two-button dialog using the native Windows
// MessageBox and returns the button the user clicked.
func showOsascriptDialog(title, message, defaultButton, secondButton string) string {
	ret := messageBox(title, message, mbYesNo|mbIconQuestion)
	if ret == idYes {
		return defaultButton
	}
	return "Cancel"
}

// showErrorDialog displays a native error dialog.
func showErrorDialog(title, message string) {
	messageBox(title, message, mbOK|mbIconError)
}

// chooseProvidersConfigPath opens a file selection dialog via PowerShell's
// OpenFileDialog and returns the chosen path.
func chooseProvidersConfigPath() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms
$d = New-Object System.Windows.Forms.OpenFileDialog
$d.Title = 'Choose Providers Config'
$d.Filter = 'Provider config files (*.json;*.yaml;*.yml)|*.json;*.yaml;*.yml|All files (*.*)|*.*'
if ($d.ShowDialog() -eq 'OK') { $d.FileName } else { exit 1 }`

	cmd := execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", errDialogCanceled
		}
		return "", fmt.Errorf("file dialog failed: %w", err)
	}

	path := strings.TrimSpace(string(output))
	if path == "" {
		return "", errDialogCanceled
	}
	return path, nil
}

// copyToClipboard copies text to the Windows clipboard using clip.exe.
func copyToClipboard(text string) {
	cmd := execCommand("clip")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}

// openURL opens a URL in the default browser.
func openURL(url string) {
	_ = execCommand("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// showNotification displays a toast notification via PowerShell.
func showNotification(title, message string) {
	script := fmt.Sprintf(`
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType = WindowsRuntime] | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml('<toast><visual><binding template="ToastGeneric"><text>%s</text><text>%s</text></binding></visual></toast>')
$toast = [Windows.UI.Notifications.ToastNotification]::new($xml)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('Vekil').Show($toast)
`, escapeXML(title), escapeXML(message))

	cmd := execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err := cmd.Run(); err != nil {
		// Fallback to a simpler BalloonTip notification.
		fallback := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.ShowBalloonTip(5000, '%s', '%s', 'Info')
Start-Sleep -Milliseconds 100
$n.Dispose()
`, escapePowerShellString(title), escapePowerShellString(message))
		_ = execCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", fallback).Run()
	}
}

// escapeXML escapes a string for safe embedding in XML content.
func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// escapePowerShellString escapes single quotes for PowerShell string literals.
func escapePowerShellString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
