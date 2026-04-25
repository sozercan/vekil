package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
)

const dbusNotifyDest = "org.freedesktop.Notifications"
const dbusNotifyPath = "/org/freedesktop/Notifications"
const dbusNotifyIface = "org.freedesktop.Notifications"

var (
	errNotificationDismissed = errors.New("notification dismissed")
	execLookPath             = exec.LookPath
	execCommand              = exec.Command
	notifyWithActions        = dbusNotifyWithActions
	notify                   = dbusNotify
)

type dialogAttemptResult int

const (
	dialogUnavailable dialogAttemptResult = iota
	dialogAccepted
	dialogCanceled
)

// showOsascriptDialog displays a dialog using zenity, kdialog, or a DBus
// notification with action buttons, and returns the button the user clicked.
// If no dialog mechanism is available, defaultButton is returned so that the
// calling flow proceeds automatically.
func showOsascriptDialog(title, message, defaultButton, secondButton string) string {
	// Try zenity first (GNOME/GTK).
	if zenity, err := execLookPath("zenity"); err == nil {
		switch runQuestionDialog(zenity,
			"--question",
			"--title="+title,
			"--text="+message,
			"--ok-label="+defaultButton,
			"--cancel-label="+secondButton,
		) {
		case dialogAccepted:
			return defaultButton
		case dialogCanceled:
			return "Cancel"
		}
	}

	// Fall back to kdialog (KDE).
	if kdialog, err := execLookPath("kdialog"); err == nil {
		switch runQuestionDialog(kdialog,
			"--title", title,
			"--yesno", message,
			"--yes-label", defaultButton,
			"--no-label", secondButton,
		) {
		case dialogAccepted:
			return defaultButton
		case dialogCanceled:
			return "Cancel"
		}
	}

	// Fall back to DBus notification with action buttons.
	action, err := notifyWithActions(title, message, []string{
		"default", defaultButton,
		"cancel", secondButton,
	})
	if err == nil {
		if action == "cancel" {
			return "Cancel"
		}
		return defaultButton
	}
	if errors.Is(err, errNotificationDismissed) {
		return "Cancel"
	}

	// No dialog mechanism available — proceed automatically.
	return defaultButton
}

// showErrorDialog displays a simple error dialog using zenity, kdialog, or a
// DBus notification.
func showErrorDialog(title, message string) {
	if zenity, err := execLookPath("zenity"); err == nil && runDialog(zenity,
		"--error",
		"--title="+title,
		"--text="+message,
	) {
		return
	}

	if kdialog, err := execLookPath("kdialog"); err == nil && runDialog(kdialog,
		"--title", title,
		"--error", message,
	) {
		return
	}

	// Fall back to a plain DBus notification (no actions needed).
	_ = notify(title, message)
}

func chooseProvidersConfigPath() (string, error) {
	if zenity, err := execLookPath("zenity"); err == nil {
		output, runErr := execCommand(zenity,
			"--file-selection",
			"--title=Choose Providers Config",
			"--file-filter=Provider config files | *.json *.yaml *.yml",
			"--file-filter=All files | *",
		).CombinedOutput()
		if runErr == nil {
			path := strings.TrimSpace(string(output))
			if path == "" {
				return "", errDialogCanceled
			}
			return path, nil
		}
		if isDialogCancel(runErr, output) {
			return "", errDialogCanceled
		}
	}

	if kdialog, err := execLookPath("kdialog"); err == nil {
		output, runErr := execCommand(kdialog,
			"--getopenfilename",
			"",
			"*.json *.yaml *.yml|Provider config files",
		).CombinedOutput()
		if runErr == nil {
			path := strings.TrimSpace(string(output))
			if path == "" {
				return "", errDialogCanceled
			}
			return path, nil
		}
		if isDialogCancel(runErr, output) {
			return "", errDialogCanceled
		}
	}

	return "", errors.New("file selection is unavailable; install zenity or kdialog")
}

// copyToClipboard copies the given text to the clipboard, trying Wayland and
// X11 tools in order.
func copyToClipboard(text string) {
	// Wayland
	if wlcopy, err := exec.LookPath("wl-copy"); err == nil {
		cmd := exec.Command(wlcopy)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}

	// X11 - xclip
	if xclip, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command(xclip, "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}

	// X11 - xsel
	if xsel, err := exec.LookPath("xsel"); err == nil {
		cmd := exec.Command(xsel, "--clipboard", "--input")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
	}
}

// openURL opens a URL in the default browser using xdg-open.
func openURL(url string) {
	_ = exec.Command("xdg-open", url).Start()
}

// showNotification displays a desktop notification. It tries DBus directly
// first, falling back to notify-send.
func showNotification(title, message string) {
	if err := notify(title, message); err == nil {
		return
	}
	_ = execCommand("notify-send", title, message).Run()
}

func runQuestionDialog(command string, args ...string) dialogAttemptResult {
	output, err := execCommand(command, args...).CombinedOutput()
	if err == nil {
		return dialogAccepted
	}
	if isDialogCancel(err, output) {
		return dialogCanceled
	}
	return dialogUnavailable
}

func runDialog(command string, args ...string) bool {
	return execCommand(command, args...).Run() == nil
}

func isDialogCancel(err error, output []byte) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() != 1 {
		return false
	}
	return len(bytes.TrimSpace(output)) == 0
}

// dbusNotify sends a simple notification (no action buttons) via DBus.
func dbusNotify(summary, body string) error {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return err
	}
	defer func() {
		_ = conn.Close()
	}()

	obj := conn.Object(dbusNotifyDest, dbusNotifyPath)
	call := obj.Call(dbusNotifyIface+".Notify", 0,
		"Vekil",                   // app_name
		uint32(0),                 // replaces_id
		"",                        // app_icon
		summary,                   // summary
		body,                      // body
		[]string{},                // actions
		map[string]dbus.Variant{}, // hints
		int32(-1),                 // expire_timeout: server default
	)
	return call.Err
}

// dbusNotifyWithActions sends a notification with action buttons via DBus and
// blocks until the user clicks an action or dismisses the notification.
// The actions slice contains alternating key/label pairs: ["key1", "Label 1", "key2", "Label 2"].
// Returns the action key the user clicked, or an error if the notification was
// dismissed or if DBus is unavailable.
func dbusNotifyWithActions(summary, body string, actions []string) (string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", err
	}
	defer func() {
		_ = conn.Close()
	}()

	obj := conn.Object(dbusNotifyDest, dbusNotifyPath)

	call := obj.Call(dbusNotifyIface+".Notify", 0,
		"Vekil",   // app_name
		uint32(0), // replaces_id
		"",        // app_icon
		summary,   // summary
		body,      // body
		actions,   // actions
		map[string]dbus.Variant{
			"urgency": dbus.MakeVariant(byte(2)), // critical: persist until acknowledged
		},
		int32(0), // expire_timeout: 0 = never expire
	)
	if call.Err != nil {
		return "", call.Err
	}

	var nid uint32
	if err := call.Store(&nid); err != nil {
		return "", err
	}

	// Subscribe to notification signals.
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(dbusNotifyPath),
		dbus.WithMatchInterface(dbusNotifyIface),
	); err != nil {
		return "", err
	}

	sigCh := make(chan *dbus.Signal, 10)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	for sig := range sigCh {
		switch sig.Name {
		case dbusNotifyIface + ".ActionInvoked":
			if len(sig.Body) >= 2 {
				if id, ok := sig.Body[0].(uint32); ok && id == nid {
					if action, ok := sig.Body[1].(string); ok {
						return action, nil
					}
				}
			}
		case dbusNotifyIface + ".NotificationClosed":
			if len(sig.Body) >= 1 {
				if id, ok := sig.Body[0].(uint32); ok && id == nid {
					return "", errNotificationDismissed
				}
			}
		}
	}

	return "", fmt.Errorf("signal channel closed")
}
