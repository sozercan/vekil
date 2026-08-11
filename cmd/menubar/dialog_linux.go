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

var (
	errNotificationDismissed = errors.New("notification dismissed")
	notifyWithActions        = dbusNotifyWithActions
	notify                   = dbusNotify
	runXDGOpen               = func(rawURL string) { _ = exec.Command("xdg-open", rawURL).Start() }
)

// confirmAction prompts the user through an xdg-desktop-portal notification
// with approve/decline actions, falling back to a legacy
// org.freedesktop.Notifications action prompt only when the portal
// notification was provably never shown. false is the safe default: once a
// prompt may have been shown by either mechanism, any decline, dismissal,
// timeout, ctx cancellation, or ambiguous delivery failure returns false
// without ever trying the other mechanism, so at most one prompt is ever
// shown. One overall timeout budget covers both the portal attempt and any
// legacy fallback.
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
			// The portal notification was never displayed; fall through to
			// the bounded legacy fallback below.
		}
	}

	if ctx.Err() != nil {
		// The shared budget above already covers the legacy attempt too:
		// never post a second prompt once it is exhausted, or once the
		// caller (e.g. a cancelled sign-in) has already moved on.
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

// portalConfirmOutcome is the three-state result of showing a portal
// confirmation notification and waiting for the user's answer.
type portalConfirmOutcome int

const (
	// portalConfirmNotShown means the notification was never actually
	// displayed (an ActionInvoked subscribe failure, or an AddNotification
	// failure portalErrorProvesNotShown recognizes as having occurred before
	// display). The caller may safely retry through the legacy fallback path.
	portalConfirmNotShown portalConfirmOutcome = iota
	// portalConfirmApproved means the user clicked the explicit approve
	// button.
	portalConfirmApproved
	// portalConfirmDeclined covers every other outcome once the notification
	// may already have been displayed: an explicit decline, any other
	// action, a timeout, a cancellation, or an ambiguous delivery failure.
	// At most one prompt is ever shown, so this is always final.
	portalConfirmDeclined
)

// portalConfirm shows a portal notification with approve/decline buttons and
// no default-action (so activating the notification body cannot approve) and
// waits for the user's answer. See portalConfirmOutcome for what each result
// means to the caller.
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

// showErrorDialog displays an urgent-priority portal notification, falling
// back to a plain legacy notification. Linux errors are notifications rather
// than modal windows because the portal exposes no generic message-dialog
// interface.
func showErrorDialog(title, message string) {
	if err := portalShow(title, message, portalPriorityUrgent); err == nil {
		return
	}
	_ = notify(title, message)
}

// showNotification displays a normal-priority portal notification, falling
// back to a plain legacy notification.
func showNotification(title, message string) {
	if err := portalShow(title, message, portalPriorityNormal); err == nil {
		return
	}
	_ = notify(title, message)
}

// portalShow calls Notification.AddNotification on the shared
// Notification-interface connection. On failure it simply returns the
// error: it never evicts or closes the shared connection, and callers fall
// back to a plain legacy notification for this one call only. Unlike
// confirmAction's at-most-once semantics, a duplicate plain toast (e.g. if
// the portal actually displayed one before an ambiguous error) is an
// acceptable, low-consequence outcome.
func portalShow(title, message, priority string) error {
	conn, err := sharedPortalNotificationConn.get()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), portalMethodCallTimeout)
	defer cancel()
	return portalAddNotification(ctx, conn, newPortalToken("notify"), title, message, priority)
}

// chooseProvidersConfigPath opens the xdg-desktop-portal file chooser. There
// is no subprocess dialog fallback: no remaining in-process mechanism
// provides an equivalent picker without reintroducing the dependency being
// removed. Pass --providers-config or edit the saved menubar config directly
// when no portal backend is available.
func chooseProvidersConfigPath() (string, error) {
	conn, err := newPortalConn()
	if err != nil {
		return "", fmt.Errorf("connect to xdg-desktop-portal: %w; install and run a desktop portal backend", err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), portalFileChooserTimeout)
	defer cancel()

	token := newPortalToken("file")
	predicted := predictRequestPath(conn.UniqueName(), token)

	resp, err := portalOpenFile(ctx, conn, token, predicted, "Choose Providers Config")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return "", errDialogCanceled
		}
		return "", fmt.Errorf("xdg-desktop-portal file chooser failed; install and run a desktop portal backend: %w", err)
	}

	switch resp.Code {
	case 0:
		// success
	case 1:
		return "", errDialogCanceled
	default:
		return "", fmt.Errorf("xdg-desktop-portal file chooser failed (response code %d); install and run a desktop portal backend", resp.Code)
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

// openURL opens an HTTP(S) URL via the xdg-desktop-portal OpenURI method,
// awaiting its Request response before deciding whether to fall back.
// Response code 0 (success) and code 1 (a deliberate user cancellation, e.g.
// dismissing an application chooser) are both final; xdg-open only runs when
// the portal cannot be reached, the method call itself fails, the response
// wait fails or times out, or the response reports any other code.
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
		// success
	case 1:
		// user canceled (e.g. dismissed an application chooser); a
		// deliberate cancel is final, not a signal to try a second
		// mechanism.
	default:
		runXDGOpen(rawURL)
	}
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

// dbusNotifyWithActions sends a notification with action buttons via the
// legacy org.freedesktop.Notifications interface and blocks until the user
// clicks an action, dismisses the notification, or ctx is done. The actions
// slice contains alternating key/label pairs: ["key1", "Label 1", "key2",
// "Label 2"]; only signals reporting one of those keys are honored. Returns
// the action key the user clicked, or an error if the notification was
// dismissed, ctx ended, or DBus is unavailable.
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

	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return "", fmt.Errorf("signal channel closed")
			}
			switch sig.Name {
			case dbusNotifyIface + ".ActionInvoked":
				if len(sig.Body) >= 2 {
					if id, ok := sig.Body[0].(uint32); ok && id == nid {
						if action, ok := sig.Body[1].(string); ok && validActions[action] {
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
		case <-ctx.Done():
			closeCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
			_ = obj.CallWithContext(closeCtx, dbusNotifyIface+".CloseNotification", 0, nid).Err
			cancel()
			return "", ctx.Err()
		}
	}
}
