package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/godbus/dbus/v5"
)

const dbusNotifyDest = "org.freedesktop.Notifications"
const dbusNotifyPath = "/org/freedesktop/Notifications"
const dbusNotifyIface = "org.freedesktop.Notifications"
const providersConfigSelectionGuidance = "install and run a desktop portal backend, run vekil --providers-config SOURCE, or edit the saved menubar config file directly"

var (
	errNotificationDismissed = errors.New("notification dismissed")
	notifyWithActions        = dbusNotifyWithActions
	notify                   = dbusNotify
	runXDGOpen               = func(rawURL string) { _ = exec.Command("xdg-open", rawURL).Start() }
)

func confirmAction(ctx context.Context, prompt confirmationPrompt) bool {
	ctx, cancel := context.WithTimeout(ctx, portalConfirmationTimeout)
	defer cancel()

	if conn, err := sharedPortalNotificationConn.get(); err == nil {
		switch portalConfirm(ctx, conn, prompt) {
		case portalConfirmApproved:
			return true
		case portalConfirmDeclined:
			return false
		case portalConfirmNotShown:
		}
	}

	if ctx.Err() != nil {
		return false
	}

	action, err := notifyWithActions(ctx, prompt.Title, prompt.Message, []string{
		portalActionApprove, prompt.ApproveLabel,
		portalActionDecline, prompt.DeclineLabel,
	})
	if err != nil {
		return false
	}
	return action == portalActionApprove
}

type portalConfirmOutcome int

const (
	portalConfirmNotShown portalConfirmOutcome = iota
	portalConfirmApproved
	portalConfirmDeclined
)

func portalConfirm(ctx context.Context, conn portalConn, prompt confirmationPrompt) portalConfirmOutcome {
	id := newPortalToken("confirm")
	notification := portalConfirmationNotification(prompt)

	action, err := waitForPortalAction(ctx, conn, id, func() error {
		callCtx, callCancel := context.WithTimeout(ctx, portalMethodCallTimeout)
		defer callCancel()
		_, callErr := conn.Call(callCtx, portalBusName, portalObjectPath, portalNotification+".AddNotification", id, notification)
		return callErr
	})

	removeCtx, removeCancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
	portalRemoveNotification(removeCtx, conn, id)
	removeCancel()

	if err != nil {
		if errors.Is(err, errPortalNotificationNotShown) {
			return portalConfirmNotShown
		}
		return portalConfirmDeclined
	}
	if action == portalActionApprove {
		return portalConfirmApproved
	}
	return portalConfirmDeclined
}

func showErrorDialog(title, message string) {
	if err := portalShow(title, message, portalPriorityUrgent); err == nil {
		return
	}
	_ = notify(title, message)
}

func showNotification(title, message string) {
	if err := portalShow(title, message, portalPriorityNormal); err == nil {
		return
	}
	_ = notify(title, message)
}

func portalShow(title, message, priority string) error {
	conn, err := sharedPortalNotificationConn.get()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), portalMethodCallTimeout)
	defer cancel()
	return portalAddNotification(ctx, conn, newPortalToken("notify"), title, message, priority)
}

func chooseProvidersConfigPath() (string, error) {
	conn, err := newPortalConn()
	if err != nil {
		return "", fmt.Errorf("connect to xdg-desktop-portal: %w; %s", err, providersConfigSelectionGuidance)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), portalFileChooserTimeout)
	defer cancel()

	token := newPortalToken("file")
	predicted := predictRequestPath(conn.UniqueName(), token)

	resp, err := portalOpenFile(ctx, conn, token, predicted, "Choose Providers Config")
	if err != nil {
		return "", fmt.Errorf("xdg-desktop-portal file chooser failed: %w; %s", err, providersConfigSelectionGuidance)
	}

	switch resp.Code {
	case 0:
	case 1:
		return "", errDialogCanceled
	default:
		return "", fmt.Errorf("xdg-desktop-portal file chooser failed (response code %d); %s", resp.Code, providersConfigSelectionGuidance)
	}

	uris, err := decodePortalStringArray(resp.Results, "uris")
	if err != nil {
		return "", fmt.Errorf("xdg-desktop-portal file chooser returned a successful response without usable file selection results: %w", err)
	}
	if len(uris) == 0 {
		return "", fmt.Errorf("xdg-desktop-portal file chooser returned a successful response with no selected files")
	}

	return decodePortalFileURI(uris[0])
}

func copyToClipboard(text string) {
	if wlcopy, err := exec.LookPath("wl-copy"); err == nil {
		cmd := exec.Command(wlcopy)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}

	if xclip, err := exec.LookPath("xclip"); err == nil {
		cmd := exec.Command(xclip, "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return
		}
	}

	if xsel, err := exec.LookPath("xsel"); err == nil {
		cmd := exec.Command(xsel, "--clipboard", "--input")
		cmd.Stdin = strings.NewReader(text)
		_ = cmd.Run()
	}
}

func openURL(rawURL string) {
	if !isSupportedOpenURIScheme(rawURL) {
		runXDGOpen(rawURL)
		return
	}

	conn, err := newPortalConn()
	if err != nil {
		runXDGOpen(rawURL)
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), portalOpenURITimeout)
	defer cancel()

	token := newPortalToken("open")
	predicted := predictRequestPath(conn.UniqueName(), token)
	activationToken := os.Getenv("XDG_ACTIVATION_TOKEN")

	resp, err := portalOpenURICall(ctx, conn, token, predicted, activationToken, rawURL)
	if err != nil {
		runXDGOpen(rawURL)
		return
	}

	switch resp.Code {
	case 0:
	case 1:
	default:
		runXDGOpen(rawURL)
	}
}

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
		"Vekil",
		uint32(0),
		"",
		summary,
		body,
		[]string{},
		map[string]dbus.Variant{},
		int32(-1),
	)
	return call.Err
}

func dbusNotifyWithActions(ctx context.Context, summary, body string, actions []string) (string, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return "", err
	}
	defer func() {
		_ = conn.Close()
	}()

	validActions := make(map[string]bool, len(actions)/2)
	for i := 0; i+1 < len(actions); i += 2 {
		validActions[actions[i]] = true
	}

	sigCh := make(chan *dbus.Signal, 10)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	if err := conn.AddMatchSignalContext(ctx,
		dbus.WithMatchSender(dbusNotifyDest),
		dbus.WithMatchObjectPath(dbusNotifyPath),
		dbus.WithMatchInterface(dbusNotifyIface),
	); err != nil {
		return "", err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		_ = conn.RemoveMatchSignalContext(cleanupCtx,
			dbus.WithMatchSender(dbusNotifyDest),
			dbus.WithMatchObjectPath(dbusNotifyPath),
			dbus.WithMatchInterface(dbusNotifyIface),
		)
	}()

	obj := conn.Object(dbusNotifyDest, dbusNotifyPath)
	call := obj.CallWithContext(ctx, dbusNotifyIface+".Notify", 0,
		"Vekil",
		uint32(0),
		"",
		summary,
		body,
		actions,
		map[string]dbus.Variant{
			"urgency": dbus.MakeVariant(byte(2)),
		},
		int32(0),
	)
	if call.Err != nil {
		return "", call.Err
	}

	var nid uint32
	if err := call.Store(&nid); err != nil {
		return "", err
	}

	return waitForLegacyNotificationAction(ctx, sigCh, nid, validActions, func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		_ = obj.CallWithContext(closeCtx, dbusNotifyIface+".CloseNotification", 0, nid).Err
		cancel()
	})
}

func waitForLegacyNotificationAction(ctx context.Context, sigCh <-chan *dbus.Signal, notificationID uint32, validActions map[string]bool, closeNotification func()) (string, error) {
	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return "", fmt.Errorf("signal channel closed")
			}
			if action, err, matched := decodeLegacyNotificationResult(sig, notificationID, validActions); matched {
				return action, err
			}
		case <-ctx.Done():
			if action, err, matched := drainQueuedLegacyNotificationResult(sigCh, notificationID, validActions); matched {
				return action, err
			}
			closeNotification()
			return "", ctx.Err()
		}
	}
}

func drainQueuedLegacyNotificationResult(sigCh <-chan *dbus.Signal, notificationID uint32, validActions map[string]bool) (string, error, bool) {
	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return "", nil, false
			}
			if action, err, matched := decodeLegacyNotificationResult(sig, notificationID, validActions); matched {
				return action, err, true
			}
		default:
			return "", nil, false
		}
	}
}

func decodeLegacyNotificationResult(sig *dbus.Signal, notificationID uint32, validActions map[string]bool) (string, error, bool) {
	switch sig.Name {
	case dbusNotifyIface + ".ActionInvoked":
		if len(sig.Body) >= 2 {
			if id, ok := sig.Body[0].(uint32); ok && id == notificationID {
				if action, ok := sig.Body[1].(string); ok && validActions[action] {
					return action, nil, true
				}
			}
		}
	case dbusNotifyIface + ".NotificationClosed":
		if len(sig.Body) >= 1 {
			if id, ok := sig.Body[0].(uint32); ok && id == notificationID {
				return "", errNotificationDismissed, true
			}
		}
	}
	return "", nil, false
}
