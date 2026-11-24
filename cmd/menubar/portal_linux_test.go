package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestPredictRequestPathUsesEscapedUniqueNameAndToken(t *testing.T) {
	got := predictRequestPath(":1.23", "tok")
	want := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_23/tok")
	if got != want {
		t.Fatalf("predictRequestPath() = %q, want %q", got, want)
	}
}

var objectPathSafeToken = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func TestNewPortalTokenIsObjectPathSafeAndDistinct(t *testing.T) {
	first := newPortalToken("req")
	second := newPortalToken("req")

	if !objectPathSafeToken.MatchString(first) {
		t.Fatalf("newPortalToken() = %q, want only object-path-safe characters", first)
	}
	if !objectPathSafeToken.MatchString(second) {
		t.Fatalf("newPortalToken() = %q, want only object-path-safe characters", second)
	}
	if first == second {
		t.Fatalf("newPortalToken() produced the same token twice: %q", first)
	}
}

func TestProviderConfigFileFiltersMarshalAsArrayOfStructArrayOfStruct(t *testing.T) {
	filters := providerConfigFileFilters()

	if got := dbus.SignatureOf(filters).String(); got != "a(sa(us))" {
		t.Fatalf("SignatureOf(providerConfigFileFilters()) = %q, want a(sa(us))", got)
	}

	var globs []string
	for _, group := range filters {
		for _, rule := range group.Rules {
			globs = append(globs, rule.Pattern)
		}
	}
	for _, want := range []string{"*.json", "*.yaml", "*.yml", "*"} {
		found := false
		for _, got := range globs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("providerConfigFileFilters() globs = %v, missing %q", globs, want)
		}
	}
}

func TestPortalNotificationBodyOmitsOptionalFields(t *testing.T) {
	body := portalNotificationBody("title", "message", portalPriorityUrgent)

	if len(body) != 3 {
		t.Fatalf("portalNotificationBody() = %v, want exactly title/body/priority", body)
	}
	for _, optional := range []string{"default-action", "buttons", "icon"} {
		if _, ok := body[optional]; ok {
			t.Fatalf("portalNotificationBody() unexpectedly set optional key %q", optional)
		}
	}
}

func TestPortalConfirmationButtonsMarshalAsArrayOfDictWithoutOptionalKeys(t *testing.T) {
	buttons := portalConfirmationButtons("Approve", "Decline")

	if got := dbus.SignatureOf(buttons).String(); got != "aa{sv}" {
		t.Fatalf("SignatureOf(portalConfirmationButtons()) = %q, want aa{sv}", got)
	}
	if len(buttons) != 2 {
		t.Fatalf("portalConfirmationButtons() returned %d buttons, want 2", len(buttons))
	}
	for _, button := range buttons {
		if len(button) != 2 {
			t.Fatalf("portal button = %v, want only label/action keys", button)
		}
	}
	if action := buttons[0]["action"].Value(); action != portalActionApprove {
		t.Fatalf("approve button action = %v, want %q", action, portalActionApprove)
	}
	if action := buttons[1]["action"].Value(); action != portalActionDecline {
		t.Fatalf("decline button action = %v, want %q", action, portalActionDecline)
	}
}

func TestPortalConfirmationNotificationHasNoDefaultActionAndOnlyApproveDeclineButtons(t *testing.T) {
	notification := portalConfirmationNotification(confirmationPrompt{
		Title: "Sign in", Message: "message", ApproveLabel: "Approve", DeclineLabel: "Decline",
	})

	if _, ok := notification["default-action"]; ok {
		t.Fatal("portalConfirmationNotification() set default-action; activating the notification body must never approve")
	}
	buttonsVal, ok := notification["buttons"]
	if !ok {
		t.Fatal("portalConfirmationNotification() did not set buttons")
	}
	buttons, ok := buttonsVal.Value().([]map[string]dbus.Variant)
	if !ok {
		t.Fatalf("portalConfirmationNotification() buttons has unexpected type %T", buttonsVal.Value())
	}
	if len(buttons) != 2 {
		t.Fatalf("portalConfirmationNotification() buttons = %d, want 2 (approve/decline only)", len(buttons))
	}
	if action := buttons[0]["action"].Value(); action != portalActionApprove {
		t.Fatalf("first button action = %v, want %q", action, portalActionApprove)
	}
	if action := buttons[1]["action"].Value(); action != portalActionDecline {
		t.Fatalf("second button action = %v, want %q", action, portalActionDecline)
	}
}

func TestDecodePortalFileURIAcceptsPercentEncodedAndLocalhostPaths(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"percent-encoded path", "file:///home/user/My%20Config.yaml", "/home/user/My Config.yaml"},
		{"localhost authority", "file://localhost/etc/vekil/providers.yaml", "/etc/vekil/providers.yaml"},
		{"document portal mount path", "file:///run/user/1000/doc/abcdef/providers.json", "/run/user/1000/doc/abcdef/providers.json"},
	}
	for _, c := range cases {
		got, err := decodePortalFileURI(c.uri)
		if err != nil {
			t.Fatalf("%s: decodePortalFileURI(%q) error = %v", c.name, c.uri, err)
		}
		if got != c.want {
			t.Fatalf("%s: decodePortalFileURI(%q) = %q, want %q", c.name, c.uri, got, c.want)
		}
	}
}

func TestDecodePortalFileURIRejectsNonFileRemoteMalformedAndRelativeURIs(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{"non-file scheme", "https://example.com/providers.yaml"},
		{"remote authority", "file://remotehost/etc/providers.yaml"},
		{"malformed percent-encoding", "file://%zz/providers.yaml"},
		{"relative (opaque) uri", "file:providers.yaml"},
	}
	for _, c := range cases {
		if _, err := decodePortalFileURI(c.uri); err == nil {
			t.Fatalf("%s: decodePortalFileURI(%q) error = nil, want rejection", c.name, c.uri)
		}
	}
}

func TestIsSupportedOpenURISchemeAcceptsOnlyHTTPAndHTTPS(t *testing.T) {
	accepted := []string{"http://example.com", "https://example.com"}
	for _, u := range accepted {
		if !isSupportedOpenURIScheme(u) {
			t.Fatalf("isSupportedOpenURIScheme(%q) = false, want true", u)
		}
	}
	rejected := []string{"ftp://example.com", "file:///etc/passwd", "not a url"}
	for _, u := range rejected {
		if isSupportedOpenURIScheme(u) {
			t.Fatalf("isSupportedOpenURIScheme(%q) = true, want false", u)
		}
	}
}

func TestRunPortalRequestRetainsResponseEmittedOnPredictedPathBeforeMethodReturns(t *testing.T) {
	conn := newFakePortalConn(":1.1")
	predicted := predictRequestPath(conn.unique, "tok")

	call := func() (dbus.ObjectPath, error) {
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   predicted,
			Name:   portalRequestIface + "." + portalResponseName,
			Body:   []any{uint32(0), map[string]dbus.Variant{"uris": dbus.MakeVariant([]string{"file:///a"})}},
		})
		return predicted, nil
	}

	resp, err := runPortalRequest(withPortalTestTimeout(t), conn, predicted, call)
	if err != nil {
		t.Fatalf("runPortalRequest() error = %v", err)
	}
	if resp.Code != 0 {
		t.Fatalf("runPortalRequest() code = %d, want 0", resp.Code)
	}
	if conn.addMatchCalls() != 1 || conn.removeMatchCalls() != 1 {
		t.Fatalf("runPortalRequest() match add/remove = %d/%d, want 1/1", conn.addMatchCalls(), conn.removeMatchCalls())
	}
}

func TestRunPortalRequestReturnsQueuedResponseWhenContextEndsSimultaneously(t *testing.T) {
	const iterations = 30
	for i := 0; i < iterations; i++ {
		conn := newFakePortalConn(":1.33")
		predicted := predictRequestPath(conn.unique, "tok")
		ctx, cancel := context.WithCancel(t.Context())

		resp, err := runPortalRequest(ctx, conn, predicted, func() (dbus.ObjectPath, error) {
			conn.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   predicted,
				Name:   portalRequestIface + "." + portalResponseName,
				Body:   []any{uint32(0), map[string]dbus.Variant{}},
			})
			cancel()
			return predicted, nil
		})

		if err != nil {
			t.Fatalf("iteration %d: runPortalRequest() error = %v, want queued response honored despite simultaneous cancellation", i, err)
		}
		if resp.Code != 0 {
			t.Fatalf("iteration %d: runPortalRequest() code = %d, want 0", i, resp.Code)
		}
		if closeCalls := conn.callsMatching(portalRequestIface + ".Close"); len(closeCalls) != 0 {
			t.Fatalf("iteration %d: runPortalRequest() Request.Close calls = %d, want 0 after completed response", i, len(closeCalls))
		}
	}
}

func TestRunPortalRequestRetainsResponseOnReturnedHandleDifferentFromPrediction(t *testing.T) {
	conn := newFakePortalConn(":1.2")
	predicted := predictRequestPath(conn.unique, "tok")
	actualHandle := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_2/other_token")

	call := func() (dbus.ObjectPath, error) {
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   actualHandle,
			Name:   portalRequestIface + "." + portalResponseName,
			Body:   []any{uint32(1), map[string]dbus.Variant{}},
		})
		return actualHandle, nil
	}

	resp, err := runPortalRequest(withPortalTestTimeout(t), conn, predicted, call)
	if err != nil {
		t.Fatalf("runPortalRequest() error = %v", err)
	}
	if resp.Code != 1 {
		t.Fatalf("runPortalRequest() code = %d, want 1 (retained despite handle/prediction mismatch)", resp.Code)
	}
}

func TestRunPortalRequestIgnoresUnrelatedResponsePaths(t *testing.T) {
	conn := newFakePortalConn(":1.3")
	predicted := predictRequestPath(conn.unique, "tok")
	unrelated := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/9_9/unrelated")

	call := func() (dbus.ObjectPath, error) {
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   unrelated,
			Name:   portalRequestIface + "." + portalResponseName,
			Body:   []any{uint32(0), map[string]dbus.Variant{}},
		})
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   predicted,
			Name:   portalRequestIface + "." + portalResponseName,
			Body:   []any{uint32(2), map[string]dbus.Variant{}},
		})
		return predicted, nil
	}

	resp, err := runPortalRequest(withPortalTestTimeout(t), conn, predicted, call)
	if err != nil {
		t.Fatalf("runPortalRequest() error = %v", err)
	}
	if resp.Code != 2 {
		t.Fatalf("runPortalRequest() code = %d, want 2 (the response for our own path, not the unrelated one)", resp.Code)
	}
}

func TestRunPortalRequestCancellationClosesRequestAndCleansUp(t *testing.T) {
	conn := newFakePortalConn(":1.4")
	predicted := predictRequestPath(conn.unique, "tok")

	ctx, cancel := context.WithCancel(t.Context())

	call := func() (dbus.ObjectPath, error) {
		cancel()
		return predicted, nil
	}

	_, err := runPortalRequest(ctx, conn, predicted, call)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runPortalRequest() error = %v, want context.Canceled", err)
	}

	closeCalls := conn.callsMatching(portalRequestIface + ".Close")
	if len(closeCalls) != 1 {
		t.Fatalf("runPortalRequest() Request.Close calls = %d, want 1", len(closeCalls))
	}
	if closeCalls[0].Path != predicted {
		t.Fatalf("Request.Close path = %q, want %q", closeCalls[0].Path, predicted)
	}
	if conn.removeMatchCalls() != 1 {
		t.Fatalf("runPortalRequest() left %d match subscriptions registered, want 0", conn.removeMatchCalls())
	}
	if conn.signalRegistered() {
		t.Fatal("runPortalRequest() left the signal channel registered after cancellation")
	}
}

func TestRunPortalRequestConcurrentOperationsDoNotConsumeEachOthersResponses(t *testing.T) {
	const operations = 10

	var wg sync.WaitGroup
	errs := make([]error, operations)
	codes := make([]uint32, operations)

	for i := 0; i < operations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			conn := newFakePortalConn(":1." + string(rune('a'+i)))
			predicted := predictRequestPath(conn.unique, "tok")
			wantCode := uint32(i)

			call := func() (dbus.ObjectPath, error) {
				conn.emit(&dbus.Signal{
					Sender: portalBusName,
					Path:   predicted,
					Name:   portalRequestIface + "." + portalResponseName,
					Body:   []any{wantCode, map[string]dbus.Variant{}},
				})
				return predicted, nil
			}

			resp, err := runPortalRequest(withPortalTestTimeout(t), conn, predicted, call)
			errs[i] = err
			codes[i] = resp.Code
		}(i)
	}
	wg.Wait()

	for i := 0; i < operations; i++ {
		if errs[i] != nil {
			t.Fatalf("operation %d: runPortalRequest() error = %v", i, errs[i])
		}
		if codes[i] != uint32(i) {
			t.Fatalf("operation %d: runPortalRequest() code = %d, want %d (no cross-operation consumption)", i, codes[i], i)
		}
	}
}

func TestRequestPathNamespaceUsesEscapedUniqueName(t *testing.T) {
	got := requestPathNamespace(":1.23")
	want := dbus.ObjectPath("/org/freedesktop/portal/desktop/request/1_23")
	if got != want {
		t.Fatalf("requestPathNamespace() = %q, want %q", got, want)
	}
}

func TestRunPortalRequestScopesMatchToOwnRequestPathNamespace(t *testing.T) {
	conn := newFakePortalConn(":1.5")
	predicted := predictRequestPath(conn.unique, "tok")

	call := func() (dbus.ObjectPath, error) {
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   predicted,
			Name:   portalRequestIface + "." + portalResponseName,
			Body:   []any{uint32(0), map[string]dbus.Variant{}},
		})
		return predicted, nil
	}

	if _, err := runPortalRequest(withPortalTestTimeout(t), conn, predicted, call); err != nil {
		t.Fatalf("runPortalRequest() error = %v", err)
	}

	want := dbus.WithMatchPathNamespace(requestPathNamespace(conn.unique))
	if !matchRuleContains(conn.lastMatchRule(), want) {
		t.Fatalf("runPortalRequest() match options = %v, want a path_namespace scoped to this connection's own requests", conn.lastMatchRule())
	}
}

func TestPortalErrorProvesNotShownClassifiesDBusErrorsByAllowlist(t *testing.T) {
	t.Run("safe: dbus.ErrClosed proves not shown", func(t *testing.T) {
		if !portalErrorProvesNotShown(dbus.ErrClosed) {
			t.Fatal("portalErrorProvesNotShown(dbus.ErrClosed) = false, want true")
		}
	})
	t.Run("safe: wrapped dbus.ErrClosed still proves not shown", func(t *testing.T) {
		wrapped := fmt.Errorf("call failed: %w", dbus.ErrClosed)
		if !portalErrorProvesNotShown(wrapped) {
			t.Fatal("portalErrorProvesNotShown(wrapped dbus.ErrClosed) = false, want true")
		}
	})

	safeAllowlistedNames := []string{
		"org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.UnknownMethod",
		"org.freedesktop.DBus.Error.UnknownInterface",
		"org.freedesktop.DBus.Error.UnknownObject",
		"org.freedesktop.DBus.Error.Spawn.ServiceNotFound",
		"org.freedesktop.DBus.Error.Spawn.ChildExited",
	}
	for _, name := range safeAllowlistedNames {
		t.Run("safe: allowlisted D-Bus error name "+name, func(t *testing.T) {
			err := dbus.Error{Name: name, Body: []any{"boom"}}
			if !portalErrorProvesNotShown(err) {
				t.Fatalf("portalErrorProvesNotShown(%v) = false, want true", err)
			}
		})
	}

	ambiguousCases := []struct {
		name string
		err  error
	}{
		{"D-Bus NoReply", dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply", Body: []any{"boom"}}},
		{"D-Bus Failed", dbus.Error{Name: "org.freedesktop.DBus.Error.Failed", Body: []any{"boom"}}},
		{"D-Bus InvalidArgs", dbus.Error{Name: "org.freedesktop.DBus.Error.InvalidArgs", Body: []any{"boom"}}},
		{"D-Bus AccessDenied", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"boom"}}},
		{"context deadline exceeded", context.DeadlineExceeded},
		{"context canceled", context.Canceled},
		{"raw transport error", errors.New("write unix ->: broken pipe")},
	}
	for _, c := range ambiguousCases {
		t.Run("ambiguous: "+c.name, func(t *testing.T) {
			if portalErrorProvesNotShown(c.err) {
				t.Fatalf("portalErrorProvesNotShown(%v) = true, want false (ambiguous)", c.err)
			}
		})
	}
}

func TestWaitForPortalActionWrapsSubscribeFailureAsNotShown(t *testing.T) {
	conn := newFakePortalConn(":1.20")
	conn.addMatchErr = errors.New("subscribe failed")

	_, err := waitForPortalAction(withPortalTestTimeout(t), conn, "id", func() error {
		t.Fatal("addNotification should not be invoked when subscribing failed")
		return nil
	})

	if !errors.Is(err, errPortalNotificationNotShown) {
		t.Fatalf("waitForPortalAction() error = %v, want errors.Is(err, errPortalNotificationNotShown)", err)
	}
}

func TestWaitForPortalActionWrapsAllowlistedAddNotificationFailureAsNotShown(t *testing.T) {
	conn := newFakePortalConn(":1.21")
	addNotificationErr := dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{"boom"}}

	_, err := waitForPortalAction(withPortalTestTimeout(t), conn, "id", func() error {
		return addNotificationErr
	})

	if !errors.Is(err, errPortalNotificationNotShown) {
		t.Fatalf("waitForPortalAction() error = %v, want errors.Is(err, errPortalNotificationNotShown)", err)
	}
	var gotDBusErr dbus.Error
	if !errors.As(err, &gotDBusErr) || gotDBusErr.Name != addNotificationErr.Name {
		t.Fatalf("waitForPortalAction() error = %v, want the underlying D-Bus error %v preserved", err, addNotificationErr)
	}
}

func TestWaitForPortalActionDoesNotWrapAmbiguousAddNotificationFailureAsNotShown(t *testing.T) {
	conn := newFakePortalConn(":1.22")
	addNotificationErr := errors.New("add notification failed ambiguously")

	_, err := waitForPortalAction(withPortalTestTimeout(t), conn, "id", func() error {
		return addNotificationErr
	})

	if errors.Is(err, errPortalNotificationNotShown) {
		t.Fatalf("waitForPortalAction() error = %v, want an ambiguous (non-NotShown) outcome, not errPortalNotificationNotShown", err)
	}
	if !errors.Is(err, addNotificationErr) {
		t.Fatalf("waitForPortalAction() error = %v, want the underlying AddNotification error preserved", err)
	}
}

func TestWaitForPortalActionDoesNotWrapWaitPhaseCancellationAsNotShown(t *testing.T) {
	conn := newFakePortalConn(":1.23")

	ctx, cancel := context.WithCancel(t.Context())

	_, err := waitForPortalAction(ctx, conn, "id", func() error {
		cancel()
		return nil
	})

	if errors.Is(err, errPortalNotificationNotShown) {
		t.Fatalf("waitForPortalAction() error = %v, want a wait-phase outcome, not errPortalNotificationNotShown", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForPortalAction() error = %v, want context.Canceled", err)
	}
}

func TestWaitForPortalActionReturnsQueuedActionEvenWhenContextEndsSimultaneously(t *testing.T) {
	const iterations = 30
	for i := 0; i < iterations; i++ {
		conn := newFakePortalConn(":1.30")
		ctx, cancel := context.WithCancel(t.Context())

		action, err := waitForPortalAction(ctx, conn, "id", func() error {
			conn.emit(&dbus.Signal{
				Sender: portalBusName,
				Path:   portalObjectPath,
				Name:   portalNotification + "." + portalActionInvoked,
				Body:   []any{"id", portalActionApprove},
			})
			cancel()
			return nil
		})

		if err != nil {
			t.Fatalf("iteration %d: waitForPortalAction() error = %v, want the queued action honored despite simultaneous cancellation", i, err)
		}
		if action != portalActionApprove {
			t.Fatalf("iteration %d: waitForPortalAction() = %q, want %q", i, action, portalActionApprove)
		}
	}
}

func TestWaitForPortalActionReturnsBufferedActionBeforeObservingChannelClose(t *testing.T) {
	conn := newFakePortalConn(":1.31")

	action, err := waitForPortalAction(withPortalTestTimeout(t), conn, "id", func() error {
		conn.emit(&dbus.Signal{
			Sender: portalBusName,
			Path:   portalObjectPath,
			Name:   portalNotification + "." + portalActionInvoked,
			Body:   []any{"id", portalActionApprove},
		})
		_ = conn.Close()
		return nil
	})

	if err != nil {
		t.Fatalf("waitForPortalAction() error = %v, want the buffered action honored before the channel close is observed", err)
	}
	if action != portalActionApprove {
		t.Fatalf("waitForPortalAction() = %q, want %q", action, portalActionApprove)
	}
}

func TestWaitForPortalActionClosedChannelWithNoBufferedActionIsAmbiguous(t *testing.T) {
	conn := newFakePortalConn(":1.32")

	_, err := waitForPortalAction(withPortalTestTimeout(t), conn, "id", func() error {
		_ = conn.Close()
		return nil
	})

	if err == nil {
		t.Fatal("waitForPortalAction() error = nil, want a closed-channel error")
	}
	if errors.Is(err, errPortalNotificationNotShown) {
		t.Fatalf("waitForPortalAction() error = %v, want an ambiguous decline, not errPortalNotificationNotShown (the notification was already shown)", err)
	}
}

type fakePortalCall struct {
	Dest   string
	Path   dbus.ObjectPath
	Method string
	Args   []any
}

type fakePortalConn struct {
	unique string

	mu            sync.Mutex
	sigs          map[chan<- *dbus.Signal]struct{}
	closed        bool
	calls         []fakePortalCall
	addMatchCount int
	removeMatch   int
	callFn        func(fakePortalCall) ([]any, error)
	addMatchErr   error
	matchRules    [][]dbus.MatchOption
}

func newFakePortalConn(unique string) *fakePortalConn {
	return &fakePortalConn{unique: unique, sigs: make(map[chan<- *dbus.Signal]struct{})}
}

func (f *fakePortalConn) UniqueName() string { return f.unique }

func (f *fakePortalConn) Call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, dbus.ErrClosed
	}
	call := fakePortalCall{Dest: dest, Path: path, Method: method, Args: args}
	f.calls = append(f.calls, call)
	fn := f.callFn
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(call)
}

func (f *fakePortalConn) AddMatch(ctx context.Context, options ...dbus.MatchOption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return dbus.ErrClosed
	}
	f.addMatchCount++
	f.matchRules = append(f.matchRules, options)
	return f.addMatchErr
}

func (f *fakePortalConn) RemoveMatch(context.Context, ...dbus.MatchOption) error {
	f.mu.Lock()
	f.removeMatch++
	f.mu.Unlock()
	return nil
}

func (f *fakePortalConn) Signal(ch chan<- *dbus.Signal) {
	f.mu.Lock()
	f.sigs[ch] = struct{}{}
	f.mu.Unlock()
}

func (f *fakePortalConn) RemoveSignal(ch chan<- *dbus.Signal) {
	f.mu.Lock()
	delete(f.sigs, ch)
	f.mu.Unlock()
}

func (f *fakePortalConn) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.closed
}

func (f *fakePortalConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	for ch := range f.sigs {
		close(ch)
	}
	f.sigs = make(map[chan<- *dbus.Signal]struct{})
	return nil
}

func (f *fakePortalConn) emit(sig *dbus.Signal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	for ch := range f.sigs {
		ch <- sig
	}
}

func (f *fakePortalConn) addMatchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addMatchCount
}

func (f *fakePortalConn) removeMatchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.removeMatch
}

func (f *fakePortalConn) signalRegistered() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sigs) > 0
}

func (f *fakePortalConn) callsMatching(method string) []fakePortalCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePortalCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakePortalConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakePortalConn) lastMatchRule() []dbus.MatchOption {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.matchRules) == 0 {
		return nil
	}
	return f.matchRules[len(f.matchRules)-1]
}

func matchRuleContains(opts []dbus.MatchOption, want dbus.MatchOption) bool {
	for _, opt := range opts {
		if opt == want {
			return true
		}
	}
	return false
}

func withPortalTestTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func resetSharedPortalNotificationConnForTest() {
	sharedPortalNotificationConn.mu.Lock()
	conn := sharedPortalNotificationConn.conn
	sharedPortalNotificationConn.conn = nil
	sharedPortalNotificationConn.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
