package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestConfirmActionPortalApprovalApproves(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.1")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			id := call.Args[0].(string)
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   portalObjectPath,
				Name:   portalNotification + "." + portalActionInvoked,
				Body:   []any{id, portalActionApprove},
			})
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		t.Fatal("legacy notification fallback should not be used when the portal approves")
		return "", nil
	}

	got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if !got {
		t.Fatal("confirmAction() = false, want true on portal approval")
	}
	if fake.isClosed() {
		t.Fatal("confirmAction() closed the shared notification connection; it must persist for reuse by later notification calls")
	}
}

func TestConfirmActionPortalDeclineDoesNotInvokeFallback(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.2")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			id := call.Args[0].(string)
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   portalObjectPath,
				Name:   portalNotification + "." + portalActionInvoked,
				Body:   []any{id, portalActionDecline},
			})
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		t.Fatal("legacy notification fallback should not be used after a portal decline")
		return "", nil
	}

	got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if got {
		t.Fatal("confirmAction() = true, want false on portal decline")
	}
}

func TestConfirmActionPortalTimeoutDoesNotInvokeFallback(t *testing.T) {
	restoreDialogHooks(t)

	parentCtx, cancelParent := context.WithCancel(t.Context())
	t.Cleanup(cancelParent)

	fake := newFakePortalConn(":1.3")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			// AddNotification succeeds (the notification was shown), and
			// only then does the caller's context end while still waiting
			// for an ActionInvoked signal that never arrives.
			cancelParent()
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		t.Fatal("legacy notification fallback should not be used after portal accepted the notification")
		return "", nil
	}

	got := confirmAction(parentCtx, confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if got {
		t.Fatal("confirmAction() = true, want false when cancelled after the portal accepted the notification")
	}
}

func TestConfirmActionPortalSetupFailureFallsBackToLegacyActionPath(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyWithActions = func(_ context.Context, title, message string, actions []string) (string, error) {
		if title != "Sign in" || message != "message" {
			t.Fatalf("notifyWithActions() got title=%q message=%q", title, message)
		}
		return portalActionApprove, nil
	}

	got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if !got {
		t.Fatal("confirmAction() = false, want true from legacy fallback approval")
	}
}

func TestConfirmActionLegacyDismissalDeclines(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		return "", errNotificationDismissed
	}

	if got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{Title: "t", Message: "m", ApproveLabel: "A", DeclineLabel: "D"}); got {
		t.Fatal("confirmAction() = true, want false on legacy dismissal")
	}
}

// TestWaitForLegacyNotificationActionReturnsQueuedApprovalWhenContextEndsSimultaneously
// exercises the same ctx.Done()-vs-buffered-signal race as the portal action
// path. An explicit approval already delivered to the process must win over
// the shared confirmation deadline, and the notification must not be closed.
func TestWaitForLegacyNotificationActionReturnsQueuedApprovalWhenContextEndsSimultaneously(t *testing.T) {
	const notificationID = uint32(42)
	validActions := map[string]bool{portalActionApprove: true, portalActionDecline: true}

	const iterations = 30
	for i := 0; i < iterations; i++ {
		sigCh := make(chan *dbus.Signal, 1)
		sigCh <- &dbus.Signal{
			Path: dbusNotifyPath,
			Name: dbusNotifyIface + ".ActionInvoked",
			Body: []any{notificationID, portalActionApprove},
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		closeCalls := 0
		action, err := waitForLegacyNotificationAction(ctx, sigCh, notificationID, validActions, func() {
			closeCalls++
		})

		if err != nil {
			t.Fatalf("iteration %d: waitForLegacyNotificationAction() error = %v, want queued approval honored", i, err)
		}
		if action != portalActionApprove {
			t.Fatalf("iteration %d: waitForLegacyNotificationAction() = %q, want %q", i, action, portalActionApprove)
		}
		if closeCalls != 0 {
			t.Fatalf("iteration %d: closeNotification calls = %d, want 0 after queued approval", i, closeCalls)
		}
	}
}

func TestWaitForLegacyNotificationActionCancellationClosesNotification(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	closeCalls := 0
	_, err := waitForLegacyNotificationAction(ctx, make(chan *dbus.Signal, 1), 42, map[string]bool{}, func() {
		closeCalls++
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForLegacyNotificationAction() error = %v, want context.Canceled", err)
	}
	if closeCalls != 1 {
		t.Fatalf("closeNotification calls = %d, want 1", closeCalls)
	}
}

func TestConfirmActionTotalUIUnavailabilityDeclines(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		return "", errors.New("dbus unavailable")
	}

	if got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{Title: "t", Message: "m", ApproveLabel: "A", DeclineLabel: "D"}); got {
		t.Fatal("confirmAction() = true, want false when no confirmation UI is available")
	}
}

func TestShowErrorDialogUsesUrgentPortalPriority(t *testing.T) {
	restoreDialogHooks(t)

	var gotPriority string
	fake := newFakePortalConn(":1.4")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			notification := call.Args[1].(map[string]dbus.Variant)
			gotPriority = notification["priority"].Value().(string)
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notify = func(string, string) error {
		t.Fatal("legacy notify should not be used when the portal call succeeds")
		return nil
	}

	showErrorDialog("title", "message")

	if gotPriority != portalPriorityUrgent {
		t.Fatalf("showErrorDialog() priority = %q, want %q", gotPriority, portalPriorityUrgent)
	}
}

func TestShowNotificationUsesNormalPortalPriority(t *testing.T) {
	restoreDialogHooks(t)

	var gotPriority string
	fake := newFakePortalConn(":1.5")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			notification := call.Args[1].(map[string]dbus.Variant)
			gotPriority = notification["priority"].Value().(string)
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notify = func(string, string) error {
		t.Fatal("legacy notify should not be used when the portal call succeeds")
		return nil
	}

	showNotification("title", "message")

	if gotPriority != portalPriorityNormal {
		t.Fatalf("showNotification() priority = %q, want %q", gotPriority, portalPriorityNormal)
	}
}

func TestShowNotificationFallsBackToLegacyNotifyNeverNotifySend(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyCalled := false
	notify = func(title, message string) error {
		notifyCalled = true
		if title != "title" || message != "message" {
			t.Fatalf("notify() got title=%q message=%q", title, message)
		}
		return nil
	}

	showNotification("title", "message")

	if !notifyCalled {
		t.Fatal("showNotification() did not fall back to the legacy notify path on portal failure")
	}
}

func TestShowErrorDialogFallsBackToLegacyNotifyNeverNotifySend(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyCalled := false
	notify = func(title, message string) error {
		notifyCalled = true
		if title != "title" || message != "message" {
			t.Fatalf("notify() got title=%q message=%q", title, message)
		}
		return nil
	}

	showErrorDialog("title", "message")

	if !notifyCalled {
		t.Fatal("showErrorDialog() did not fall back to the legacy notify path on portal failure")
	}
}

func TestChooseProvidersConfigPathReturnsDecodedPath(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.6")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body: []any{uint32(0), map[string]dbus.Variant{
					"uris": dbus.MakeVariant([]string{"file:///home/user/providers.yaml"}),
				}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	got, err := chooseProvidersConfigPath()
	if err != nil {
		t.Fatalf("chooseProvidersConfigPath() error = %v", err)
	}
	if got != "/home/user/providers.yaml" {
		t.Fatalf("chooseProvidersConfigPath() = %q, want /home/user/providers.yaml", got)
	}
}

func TestChooseProvidersConfigPathCancellationReturnsErrDialogCanceled(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.7")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(1), map[string]dbus.Variant{}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if !errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want errDialogCanceled", err)
	}
}

func TestChooseProvidersConfigPathTimeoutIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.34")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a descriptive timeout error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want context.DeadlineExceeded preserved", err)
	}
	requireProvidersConfigSelectionGuidance(t, err)
}

func TestChooseProvidersConfigPathSetupFailureIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a descriptive setup error", err)
	}
	requireProvidersConfigSelectionGuidance(t, err)
}

func TestChooseProvidersConfigPathOpenFailureIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.18")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			return nil, errors.New("open failed")
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a descriptive open error", err)
	}
	requireProvidersConfigSelectionGuidance(t, err)
}

func TestChooseProvidersConfigPathErrorResponseIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.19")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(2), map[string]dbus.Variant{}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a descriptive portal response error", err)
	}
	requireProvidersConfigSelectionGuidance(t, err)
}

func TestOpenURLSuccessfulPortalCallDoesNotInvokeXDGOpen(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.8")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalOpenURI+".OpenURI" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(0), map[string]dbus.Variant{}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	runXDGOpen = func(string) {
		t.Fatal("xdg-open should not run when the portal reports a successful response")
	}

	openURL("https://example.com")

	calls := fake.callsMatching(portalOpenURI + ".OpenURI")
	if len(calls) != 1 {
		t.Fatalf("OpenURI.OpenURI calls = %d, want 1", len(calls))
	}
}

func TestOpenURLPortalMethodFailureInvokesXDGOpen(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.9")
	fake.callFn = func(fakePortalCall) ([]any, error) { return nil, errors.New("method call failed") }
	newPortalConn = func() (portalConn, error) { return fake, nil }

	xdgOpenCalled := false
	runXDGOpen = func(rawURL string) {
		xdgOpenCalled = true
		if rawURL != "https://example.com" {
			t.Fatalf("runXDGOpen() got %q", rawURL)
		}
	}

	openURL("https://example.com")

	if !xdgOpenCalled {
		t.Fatal("openURL() did not fall back to xdg-open after the portal method call failed")
	}
}

func TestOpenURLUserCanceledResponseDoesNotInvokeXDGOpen(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.10")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalOpenURI+".OpenURI" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(1), map[string]dbus.Variant{}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	runXDGOpen = func(string) {
		t.Fatal("xdg-open should not run after a deliberate user cancel (response code 1)")
	}

	openURL("https://example.com")
}

func TestOpenURLOtherResponseCodeInvokesXDGOpen(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.11")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalOpenURI+".OpenURI" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(2), map[string]dbus.Variant{}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	xdgOpenCalled := false
	runXDGOpen = func(string) { xdgOpenCalled = true }

	openURL("https://example.com")

	if !xdgOpenCalled {
		t.Fatal("openURL() did not fall back to xdg-open for a non-success, non-cancel response code")
	}
}

func TestOpenURLResponseWaitSetupFailureInvokesXDGOpen(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.12")
	fake.addMatchErr = errors.New("subscribe failed")
	newPortalConn = func() (portalConn, error) { return fake, nil }

	xdgOpenCalled := false
	runXDGOpen = func(string) { xdgOpenCalled = true }

	openURL("https://example.com")

	if !xdgOpenCalled {
		t.Fatal("openURL() did not fall back to xdg-open when subscribing to the portal response failed")
	}
}

func TestSharedNotificationConnectionIsReusedAcrossCalls(t *testing.T) {
	restoreDialogHooks(t)

	dialCount := 0
	fake := newFakePortalConn(":1.13")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			id := call.Args[0].(string)
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   portalObjectPath,
				Name:   portalNotification + "." + portalActionInvoked,
				Body:   []any{id, portalActionApprove},
			})
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) {
		dialCount++
		return fake, nil
	}

	showNotification("title", "message")
	showErrorDialog("title", "message")
	confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})

	if dialCount != 1 {
		t.Fatalf("newPortalConn() called %d times across three notification operations, want 1 (shared connection)", dialCount)
	}
	if fake.isClosed() {
		t.Fatal("shared notification connection was closed; it must persist for the process lifetime")
	}
}

func TestSharedNotificationConnectionSurvivesTransientAddNotificationFailure(t *testing.T) {
	restoreDialogHooks(t)

	dialCount := 0
	fake := newFakePortalConn(":1.14")
	fake.callFn = func(fakePortalCall) ([]any, error) { return nil, errors.New("add notification failed") }
	newPortalConn = func() (portalConn, error) {
		dialCount++
		return fake, nil
	}
	notify = func(string, string) error { return nil }

	showNotification("title", "message")
	showNotification("title", "message")

	if dialCount != 1 {
		t.Fatalf("newPortalConn() called %d times after two transient AddNotification failures, want 1: a transient method failure must never evict or redial the shared connection", dialCount)
	}
	if fake.isClosed() {
		t.Fatal("a transient AddNotification failure closed the shared notification connection; production must never do this")
	}
}

func TestSharedNotificationConnectionRedialsOnlyAfterGenuineDisconnect(t *testing.T) {
	restoreDialogHooks(t)

	dialCount := 0
	var current *fakePortalConn
	newPortalConn = func() (portalConn, error) {
		dialCount++
		current = newFakePortalConn(":1.14")
		return current, nil
	}
	notify = func(string, string) error { return nil }

	showNotification("title", "message")
	if dialCount != 1 {
		t.Fatalf("newPortalConn() called %d times on first use, want 1", dialCount)
	}

	// Simulate the peer genuinely disconnecting: Connected() now reports
	// false, the only condition production treats as a reason to redial.
	_ = current.Close()

	showNotification("title", "message")
	if dialCount != 2 {
		t.Fatalf("newPortalConn() called %d times after a genuine disconnect, want 2 (redial)", dialCount)
	}
}

func TestSharedConnectionTransientAddNotificationFailureDoesNotDisruptConcurrentConfirmation(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.25")
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notify = func(string, string) error { return nil }

	var confirmSubscribed sync.WaitGroup
	confirmSubscribed.Add(1)
	var confirmSubscribedOnce sync.Once

	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method != portalNotification+".AddNotification" {
			return nil, nil
		}
		id, _ := call.Args[0].(string)
		if strings.HasPrefix(id, "confirm_") {
			confirmSubscribedOnce.Do(confirmSubscribed.Done)
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   portalObjectPath,
				Name:   portalNotification + "." + portalActionInvoked,
				Body:   []any{id, portalActionApprove},
			})
			return nil, nil
		}
		// The concurrent plain-notification call fails transiently; this
		// must never disrupt the still-pending confirmation sharing this
		// connection.
		confirmSubscribed.Wait()
		return nil, errors.New("transient add notification failure")
	}

	var wg sync.WaitGroup
	var confirmResult bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		confirmResult = confirmAction(withPortalTestTimeout(t), confirmationPrompt{
			Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
		})
	}()

	showNotification("title", "message")
	wg.Wait()

	if !confirmResult {
		t.Fatal("confirmAction() = false, want true: a concurrent transient AddNotification failure on the shared connection must not disrupt a pending confirmation")
	}
	if fake.isClosed() {
		t.Fatal("a transient AddNotification failure closed the shared connection out from under a concurrent confirmation")
	}
}

func TestConfirmActionAmbiguousAddNotificationTimeoutDeclinesWithoutFallback(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.15")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalNotification+".AddNotification" {
			// AddNotification's own sub-timeout elapsed. context.DeadlineExceeded
			// is not in the safe "proves not shown" allowlist -- the deadline
			// can fire after the portal already accepted and displayed the
			// request -- so this must be an ambiguous delivery failure, not
			// proof the notification was never shown.
			return nil, context.DeadlineExceeded
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notifyWithActions = func(_ context.Context, title, message string, actions []string) (string, error) {
		t.Fatal("legacy notification fallback should not be used for an ambiguous AddNotification failure")
		return "", nil
	}

	got := confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if got {
		t.Fatal("confirmAction() = true, want false: an ambiguous AddNotification timeout must decline, not fall back to a second prompt")
	}
}

func TestConfirmActionLegacyFallbackReceivesABoundedContext(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }

	gotDeadline := false
	notifyWithActions = func(ctx context.Context, title, message string, actions []string) (string, error) {
		_, gotDeadline = ctx.Deadline()
		return portalActionApprove, nil
	}

	confirmAction(withPortalTestTimeout(t), confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})

	if !gotDeadline {
		t.Fatal("confirmAction() invoked the legacy fallback with a context that has no deadline; one shared confirmation timeout must bound both the portal attempt and any legacy fallback")
	}
}

func TestConfirmActionNeverPostsLegacyFallbackWhenParentContextAlreadyDone(t *testing.T) {
	restoreDialogHooks(t)

	newPortalConn = func() (portalConn, error) { return nil, errors.New("no session bus") }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		t.Fatal("legacy notification fallback must not run once the caller's context has already ended")
		return "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := confirmAction(ctx, confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if got {
		t.Fatal("confirmAction() = true, want false when the caller's context is already done")
	}
}

func TestConfirmActionNeverPostsLegacyFallbackAfterNotShownWhenContextAlreadyCanceled(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.26")
	newPortalConn = func() (portalConn, error) { return fake, nil }
	notifyWithActions = func(context.Context, string, string, []string) (string, error) {
		t.Fatal("legacy notification fallback must not run once the caller's context has already ended, even after a NotShown portal outcome")
		return "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := confirmAction(ctx, confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Open GitHub", DeclineLabel: "Cancel",
	})
	if got {
		t.Fatal("confirmAction() = true, want false when the caller's context is already done")
	}
}

func TestChooseProvidersConfigPathMissingURIsInSuccessResponseIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.16")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(0), map[string]dbus.Variant{}}, // success, but no "uris" key
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a distinct actionable error, not errDialogCanceled", err)
	}
}

func TestChooseProvidersConfigPathEmptyURIsInSuccessResponseIsActionable(t *testing.T) {
	restoreDialogHooks(t)

	fake := newFakePortalConn(":1.17")
	fake.callFn = func(call fakePortalCall) ([]any, error) {
		if call.Method == portalFileChooser+".OpenFile" {
			handle := predictRequestPath(fake.unique, extractHandleToken(t, call))
			go fake.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   handle,
				Name:   portalRequestIface + "." + portalResponseName,
				Body: []any{uint32(0), map[string]dbus.Variant{
					"uris": dbus.MakeVariant([]string{}),
				}},
			})
			return []any{handle}, nil
		}
		return nil, nil
	}
	newPortalConn = func() (portalConn, error) { return fake, nil }

	_, err := chooseProvidersConfigPath()
	if err == nil || errors.Is(err, errDialogCanceled) {
		t.Fatalf("chooseProvidersConfigPath() error = %v, want a distinct actionable error, not errDialogCanceled", err)
	}
}

func requireProvidersConfigSelectionGuidance(t *testing.T, err error) {
	t.Helper()

	for _, want := range []string{
		"vekil --providers-config SOURCE",
		"edit the saved menubar config file directly",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("chooseProvidersConfigPath() error = %q, want it to contain %q", err, want)
		}
	}
}

func restoreDialogHooks(t *testing.T) {
	t.Helper()

	oldNewPortalConn := newPortalConn
	oldNotifyWithActions := notifyWithActions
	oldNotify := notify
	oldRunXDGOpen := runXDGOpen

	// Each test case must not observe another test's cached shared
	// notification connection.
	resetSharedPortalNotificationConnForTest()

	t.Cleanup(func() {
		newPortalConn = oldNewPortalConn
		notifyWithActions = oldNotifyWithActions
		notify = oldNotify
		runXDGOpen = oldRunXDGOpen
		resetSharedPortalNotificationConnForTest()
	})
}

// extractHandleToken pulls the handle_token option back out of an OpenFile
// call's options argument so tests can predict the same request path the
// portal_linux.go implementation predicted.
func extractHandleToken(t *testing.T, call fakePortalCall) string {
	t.Helper()
	if len(call.Args) < 3 {
		t.Fatalf("OpenFile call args = %v, want at least 3", call.Args)
	}
	options, ok := call.Args[2].(map[string]dbus.Variant)
	if !ok {
		t.Fatalf("OpenFile options arg has unexpected type %T", call.Args[2])
	}
	token, ok := options["handle_token"].Value().(string)
	if !ok {
		t.Fatal("OpenFile options missing handle_token")
	}
	return token
}
