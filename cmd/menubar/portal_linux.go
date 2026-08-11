package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Portal bus identity and interface names. All calls target the well-known
// org.freedesktop.portal.Desktop service; "sender" match options below refer
// to this well-known name so the message bus resolves the current owner.
const (
	portalBusName       = "org.freedesktop.portal.Desktop"
	portalObjectPath    = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalRequestIface  = "org.freedesktop.portal.Request"
	portalFileChooser   = "org.freedesktop.portal.FileChooser"
	portalOpenURI       = "org.freedesktop.portal.OpenURI"
	portalNotification  = "org.freedesktop.portal.Notification"
	portalResponseName  = "Response"
	portalActionInvoked = "ActionInvoked"

	portalActionApprove = "approve"
	portalActionDecline = "decline"

	portalPriorityNormal = "normal"
	portalPriorityUrgent = "urgent"

	portalRequestPathPrefix = "/org/freedesktop/portal/desktop/request/"

	portalSignalBufferSize    = 8
	portalCleanupTimeout      = 2 * time.Second
	portalMethodCallTimeout   = 5 * time.Second
	portalFileChooserTimeout  = 5 * time.Minute
	portalConfirmationTimeout = 2 * time.Minute

	// portalOpenURITimeout bounds the wait for OpenURI.OpenURI's asynchronous
	// Request.Response. openURL passes ask: false, so a chooser dialog rarely
	// appears (only when no default handler exists or the URI needs
	// disambiguation); when it does appear it is a much lighter-weight prompt
	// than the file chooser, so this is shorter than portalFileChooserTimeout.
	// Every caller of openURL (Open Dashboard, GitHub sign-in) runs on its own
	// goroutine rather than the tray's single menu-dispatch loop (see
	// main.go), so this bounded wait can never block or drop other systray
	// clicks.
	portalOpenURITimeout = 15 * time.Second
)

// errPortalNotificationNotShown marks an error that proves a portal
// notification was never displayed: subscribing to ActionInvoked failed
// before AddNotification was even attempted, or AddNotification itself
// failed in a way portalErrorProvesNotShown recognizes as safe (see its doc
// comment). Callers may retry through the legacy org.freedesktop.Notifications
// path in this case without risking a duplicate prompt. Every other
// AddNotification error, and any error from the ActionInvoked wait itself
// (timeout, cancellation, decline, or a closed signal channel), is ambiguous:
// the notification may already have been displayed before the failure, so
// callers must decline rather than show a second prompt.
var errPortalNotificationNotShown = errors.New("portal notification was not shown")

// dbusErrorNamesProvingNotShown are D-Bus error names that can only occur
// before a method call reaches the target service: the well-known portal
// service has no owner, could not be activated, or does not implement the
// method/interface/object being called. None of these are possible after
// xdg-desktop-portal has actually accepted and processed AddNotification.
var dbusErrorNamesProvingNotShown = map[string]bool{
	"org.freedesktop.DBus.Error.ServiceUnknown":   true,
	"org.freedesktop.DBus.Error.NameHasNoOwner":   true,
	"org.freedesktop.DBus.Error.UnknownMethod":    true,
	"org.freedesktop.DBus.Error.UnknownInterface": true,
	"org.freedesktop.DBus.Error.UnknownObject":    true,
}

// dbusErrorSpawnPrefix additionally covers every org.freedesktop.DBus.Error.Spawn.*
// name (e.g. Spawn.ServiceNotFound, Spawn.ChildExited): all reported by the
// bus daemon when it cannot even launch the service that would have handled
// the call.
const dbusErrorSpawnPrefix = "org.freedesktop.DBus.Error.Spawn."

// portalErrorProvesNotShown reports whether err proves an AddNotification
// call never reached xdg-desktop-portal's notification code, so a caller may
// safely retry through the legacy fallback path without risking a duplicate
// prompt.
//
// dbus.ErrClosed is safe: with the pinned godbus version, a method call only
// produces it when the output handler observes the connection already
// closed before the message is written, i.e. before anything was sent.
//
// A narrow allowlist of D-Bus bus-level error names is also safe: they are
// only returned by the message bus itself (service routing/activation
// failures, or the method/interface/object not existing), never by the
// portal backend after it has accepted a call.
//
// Every other error -- including org.freedesktop.DBus.Error.NoReply, a
// backend-reported Failed, deadline/cancellation, or a raw transport
// read/write failure -- is ambiguous: the portal may already have displayed
// the notification before the reply was lost, so it is not safe to retry.
func portalErrorProvesNotShown(err error) bool {
	if errors.Is(err, dbus.ErrClosed) {
		return true
	}
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		if dbusErrorNamesProvingNotShown[dbusErr.Name] {
			return true
		}
		if strings.HasPrefix(dbusErr.Name, dbusErrorSpawnPrefix) {
			return true
		}
	}
	return false
}

// portalConn is the subset of a private D-Bus session-bus connection needed by
// the portal helpers below. It is replaceable in unit tests so request
// lifecycle, race, and cancellation behavior can be driven deterministically
// without a live session bus.
type portalConn interface {
	UniqueName() string
	Call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error)
	AddMatch(ctx context.Context, options ...dbus.MatchOption) error
	RemoveMatch(ctx context.Context, options ...dbus.MatchOption) error
	Signal(ch chan<- *dbus.Signal)
	RemoveSignal(ch chan<- *dbus.Signal)
	// Connected reports whether the underlying transport is still live. It is
	// a liveness check only, not a portal/backend health check: it can
	// report true even though the last method call against this connection
	// failed, and it only reports false once the transport itself has
	// actually been torn down (e.g. after Close, or an unrecoverable
	// transport error godbus itself detected).
	Connected() bool
	Close() error
}

// sessionPortalConn adapts a real *dbus.Conn to portalConn.
type sessionPortalConn struct {
	conn *dbus.Conn
}

// dialSessionPortalConn opens a fresh, private session-bus connection.
// FileChooser and OpenURI operations each dial their own connection rather
// than sharing one; this isolates signal channels between concurrent
// operations and avoids a global signal router. Notification-interface
// operations instead share one process-lifetime connection through
// sharedPortalNotificationConn below; see its doc comment for why.
func dialSessionPortalConn() (portalConn, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &sessionPortalConn{conn: conn}, nil
}

// newPortalConn is a seam so tests can substitute a fake bus connection.
var newPortalConn = dialSessionPortalConn

// sharedPortalNotificationConn is a lazily-dialed, process-lifetime-shared
// connection used only for org.freedesktop.portal.Notification operations:
// AddNotification, RemoveNotification, and the ActionInvoked wait around
// them. xdg-desktop-portal's notification backend keys pending notification
// bookkeeping by the calling app's identity (an empty app_id for every
// non-sandboxed process) and tears that identity's bookkeeping down when it
// sees a peer sharing that identity disconnect from the bus. A short-lived
// per-call connection for showErrorDialog/showNotification could therefore
// race a concurrent confirmAction wait running on its own connection and
// cause the portal daemon to discard the in-flight confirmation
// notification. FileChooser and OpenURI are unaffected by this app_id-keyed
// bookkeeping and keep their own per-call connections dialed directly via
// newPortalConn.
//
// Production code never explicitly closes or evicts this connection: no
// method or setup error is treated as proof the connection is dead, because
// doing so could close the signal channel of, or purge the portal's
// app-id-keyed notification bookkeeping for, a concurrently in-flight
// confirmation sharing the same peer. A genuine transport loss is instead
// observed by godbus itself (see Connected on portalConn), and only then does
// the next get() call redial.
var sharedPortalNotificationConn portalNotificationConn

// portalNotificationConn lazily dials and shares a single portalConn across
// every Notification-interface operation. newPortalConn is resolved fresh on
// every dial attempt (never captured once) so tests that replace that
// package var to inject a fake connection still take effect here.
type portalNotificationConn struct {
	mu   sync.Mutex
	conn portalConn
}

// get returns the cached connection if it is still live, or dials (and
// caches) a fresh one via newPortalConn otherwise. The liveness check, dial,
// and store are one operation under a single mutex hold so concurrent
// callers can never race a dial against each other. Connected() is a
// transport-liveness check only, not a portal/backend health check: an
// ordinary method error on the returned connection is not, by itself, a
// reason to redial.
func (c *portalNotificationConn) get() (portalConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil && c.conn.Connected() {
		return c.conn, nil
	}
	conn, err := newPortalConn()
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return conn, nil
}

func (c *sessionPortalConn) UniqueName() string {
	names := c.conn.Names()
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func (c *sessionPortalConn) Call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	call := c.conn.Object(dest, path).CallWithContext(ctx, method, 0, args...)
	if call.Err != nil {
		return nil, call.Err
	}
	return call.Body, nil
}

func (c *sessionPortalConn) AddMatch(ctx context.Context, options ...dbus.MatchOption) error {
	return c.conn.AddMatchSignalContext(ctx, options...)
}

func (c *sessionPortalConn) RemoveMatch(ctx context.Context, options ...dbus.MatchOption) error {
	return c.conn.RemoveMatchSignalContext(ctx, options...)
}

func (c *sessionPortalConn) Signal(ch chan<- *dbus.Signal) {
	c.conn.Signal(ch)
}

func (c *sessionPortalConn) RemoveSignal(ch chan<- *dbus.Signal) {
	c.conn.RemoveSignal(ch)
}

func (c *sessionPortalConn) Connected() bool {
	return c.conn.Connected()
}

func (c *sessionPortalConn) Close() error {
	return c.conn.Close()
}

// newPortalToken generates a random, object-path-safe token for use as a
// portal handle_token or notification id.
func newPortalToken(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read failing is effectively unreachable on supported
		// platforms; fall back to a still-unique, path-safe value rather than
		// reusing a fixed token.
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

// requestPathNamespace returns the object path under which every
// org.freedesktop.portal.Request object created by a call from the given
// connection's unique name will appear, per the xdg-desktop-portal
// specification's sender-escaping rule: the leading colon is stripped and
// dots are replaced with underscores. Used with dbus.WithMatchPathNamespace
// so a Request.Response subscription only receives this connection's own
// outstanding portal requests rather than every application's on the session
// bus.
func requestPathNamespace(uniqueName string) dbus.ObjectPath {
	sender := strings.ReplaceAll(strings.TrimPrefix(uniqueName, ":"), ".", "_")
	return dbus.ObjectPath(portalRequestPathPrefix + sender)
}

// predictRequestPath predicts the object path of the org.freedesktop.portal.Request
// object a portal method call with the given handle_token will create.
func predictRequestPath(uniqueName, token string) dbus.ObjectPath {
	return dbus.ObjectPath(string(requestPathNamespace(uniqueName)) + "/" + token)
}

// portalResponse is the decoded body of an org.freedesktop.portal.Request.Response
// signal.
type portalResponse struct {
	Code    uint32
	Results map[string]dbus.Variant
}

// runPortalRequest performs the race-free portal Request lifecycle described in
// the design: it subscribes to Request.Response (scoped to the portal
// service, the Request interface, the Response member, and this
// connection's own request path namespace, but not yet to the specific
// predicted object path) before invoking call, accepts the response on
// either the predicted path or the handle call returns, and cleans up the
// match/signal on completion or on ctx cancellation. The path-namespace
// scope keeps this subscription from also receiving every other
// application's portal Request.Response traffic on the session bus. On
// cancellation, Request.Close is invoked best-effort with a short,
// independent cleanup context.
func runPortalRequest(ctx context.Context, conn portalConn, predicted dbus.ObjectPath, call func() (dbus.ObjectPath, error)) (portalResponse, error) {
	sigCh := make(chan *dbus.Signal, portalSignalBufferSize)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	matchOpts := []dbus.MatchOption{
		dbus.WithMatchSender(portalBusName),
		dbus.WithMatchInterface(portalRequestIface),
		dbus.WithMatchMember(portalResponseName),
		dbus.WithMatchPathNamespace(requestPathNamespace(conn.UniqueName())),
	}
	if err := conn.AddMatch(ctx, matchOpts...); err != nil {
		return portalResponse{}, fmt.Errorf("subscribe to portal request response: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		_ = conn.RemoveMatch(cleanupCtx, matchOpts...)
	}()

	handle, err := call()
	if err != nil {
		return portalResponse{}, err
	}

	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return portalResponse{}, fmt.Errorf("portal signal channel closed")
			}
			if resp, matched := decodePortalResponse(sig, predicted, handle); matched {
				return resp, nil
			}
		case <-ctx.Done():
			closeCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
			_, _ = conn.Call(closeCtx, portalBusName, handle, portalRequestIface+".Close")
			cancel()
			return portalResponse{}, ctx.Err()
		}
	}
}

// decodePortalResponse reports whether sig is a Request.Response signal for
// the predicted or actual request handle, decoding its body when it is.
// Signals for any other path (a concurrent, unrelated portal request) are
// reported as not matched so the caller keeps waiting.
func decodePortalResponse(sig *dbus.Signal, predicted, handle dbus.ObjectPath) (portalResponse, bool) {
	if sig.Name != portalRequestIface+"."+portalResponseName {
		return portalResponse{}, false
	}
	if sig.Path != predicted && sig.Path != handle {
		return portalResponse{}, false
	}
	if len(sig.Body) < 2 {
		return portalResponse{}, false
	}
	code, ok := sig.Body[0].(uint32)
	if !ok {
		return portalResponse{}, false
	}
	results, ok := sig.Body[1].(map[string]dbus.Variant)
	if !ok {
		return portalResponse{}, false
	}
	return portalResponse{Code: code, Results: results}, true
}

// waitForPortalAction subscribes to the portal Notification.ActionInvoked
// signal, invokes addNotification (expected to call AddNotification), and
// waits for an ActionInvoked signal carrying the given notification id.
// Unlike Request objects, the Notification object path is fixed, so no
// prediction/race handling is required.
//
// Failing to subscribe to ActionInvoked always wraps errPortalNotificationNotShown:
// no attempt to show the notification was even made yet. A failing
// addNotification wraps errPortalNotificationNotShown only when
// portalErrorProvesNotShown reports the failure could only have happened
// before xdg-desktop-portal's notification code ran; otherwise it returns an
// ordinary, unwrapped error, because the notification may already have been
// displayed. Once addNotification succeeds, every subsequent outcome (a
// decline, another action, a closed signal channel, or ctx ending) is an
// ordinary, unwrapped error or a non-approve action: the notification was
// shown, so callers must not retry through a second mechanism.
func waitForPortalAction(ctx context.Context, conn portalConn, notificationID string, addNotification func() error) (string, error) {
	sigCh := make(chan *dbus.Signal, portalSignalBufferSize)
	conn.Signal(sigCh)
	defer conn.RemoveSignal(sigCh)

	matchOpts := []dbus.MatchOption{
		dbus.WithMatchSender(portalBusName),
		dbus.WithMatchInterface(portalNotification),
		dbus.WithMatchMember(portalActionInvoked),
		dbus.WithMatchObjectPath(portalObjectPath),
	}
	if err := conn.AddMatch(ctx, matchOpts...); err != nil {
		return "", fmt.Errorf("%w: subscribe to portal notification actions: %w", errPortalNotificationNotShown, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
		defer cancel()
		_ = conn.RemoveMatch(cleanupCtx, matchOpts...)
	}()

	if err := addNotification(); err != nil {
		if portalErrorProvesNotShown(err) {
			return "", fmt.Errorf("%w: %w", errPortalNotificationNotShown, err)
		}
		return "", fmt.Errorf("portal notification delivery is ambiguous, declining without a second prompt: %w", err)
	}

	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				// A closed channel here means the notification was already
				// shown (addNotification succeeded above); this is an
				// ambiguous delivery failure, not proof of no display.
				return "", fmt.Errorf("portal signal channel closed")
			}
			if action, matched := decodePortalActionInvoked(sig, notificationID); matched {
				return action, nil
			}
		case <-ctx.Done():
			// Go's select can choose the ctx.Done() case even though a
			// matching action was already delivered and buffered on sigCh
			// before ctx ended; drain it non-blockingly before conceding no
			// answer exists so a genuinely queued answer is never lost to
			// this race.
			if action, ok := drainQueuedPortalAction(sigCh, notificationID); ok {
				return action, nil
			}
			return "", ctx.Err()
		}
	}
}

// drainQueuedPortalAction non-blockingly drains any ActionInvoked signals
// already buffered on sigCh, looking for one matching notificationID. See
// waitForPortalAction's ctx.Done() case for why this is needed.
func drainQueuedPortalAction(sigCh <-chan *dbus.Signal, notificationID string) (string, bool) {
	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return "", false
			}
			if action, matched := decodePortalActionInvoked(sig, notificationID); matched {
				return action, true
			}
		default:
			return "", false
		}
	}
}

func decodePortalActionInvoked(sig *dbus.Signal, notificationID string) (string, bool) {
	if sig.Name != portalNotification+"."+portalActionInvoked {
		return "", false
	}
	if len(sig.Body) < 2 {
		return "", false
	}
	id, ok := sig.Body[0].(string)
	if !ok || id != notificationID {
		return "", false
	}
	action, ok := sig.Body[1].(string)
	if !ok {
		return "", false
	}
	return action, true
}

// portalFilterRule is one (type, glob) pair in a FileChooser filter group. The
// zero value for Type (0) is a glob pattern per the portal specification.
type portalFilterRule struct {
	Type    uint32
	Pattern string
}

// portalFilterGroup is one named group of filter rules. Marshalled together
// as a slice, portalFilterGroup produces the "a(sa(us))" signature the
// FileChooser "filters" option requires.
type portalFilterGroup struct {
	Name  string
	Rules []portalFilterRule
}

// providerConfigFileFilters returns the FileChooser filters offered when
// choosing a providers config file.
func providerConfigFileFilters() []portalFilterGroup {
	return []portalFilterGroup{
		{
			Name: "Provider config files",
			Rules: []portalFilterRule{
				{Pattern: "*.json"},
				{Pattern: "*.yaml"},
				{Pattern: "*.yml"},
			},
		},
		{
			Name:  "All files",
			Rules: []portalFilterRule{{Pattern: "*"}},
		},
	}
}

// portalNotificationBody builds the required a{sv} fields of a portal
// notification. Optional fields (default-action, buttons) are added by
// callers that need them rather than populated here with incorrectly typed
// zero values.
func portalNotificationBody(title, body, priority string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"title":    dbus.MakeVariant(title),
		"body":     dbus.MakeVariant(body),
		"priority": dbus.MakeVariant(priority),
	}
}

// portalConfirmationButtons builds the aa{sv} button collection for a
// confirmation notification, using non-reserved action keys.
func portalConfirmationButtons(approveLabel, declineLabel string) []map[string]dbus.Variant {
	return []map[string]dbus.Variant{
		{"label": dbus.MakeVariant(approveLabel), "action": dbus.MakeVariant(portalActionApprove)},
		{"label": dbus.MakeVariant(declineLabel), "action": dbus.MakeVariant(portalActionDecline)},
	}
}

// portalConfirmationNotification builds the a{sv} notification dictionary
// for a confirmation prompt: approve/decline buttons and deliberately no
// default-action, so activating the notification body itself can never
// approve. Only an explicit click on the approve button can approve; a body
// click, dismissal, or a backend that never delivers a button action all
// produce no ActionInvoked signal and therefore an eventual timeout decline.
func portalConfirmationNotification(prompt confirmationPrompt) map[string]dbus.Variant {
	notification := portalNotificationBody(prompt.Title, prompt.Message, portalPriorityNormal)
	notification["buttons"] = dbus.MakeVariant(portalConfirmationButtons(prompt.ApproveLabel, prompt.DeclineLabel))
	return notification
}

// portalOpenFile invokes FileChooser.OpenFile with the given handle_token and
// provider filters and waits for its Request response.
func portalOpenFile(ctx context.Context, conn portalConn, token string, predicted dbus.ObjectPath, title string) (portalResponse, error) {
	return runPortalRequest(ctx, conn, predicted, func() (dbus.ObjectPath, error) {
		options := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(token),
			"modal":        dbus.MakeVariant(true),
			"multiple":     dbus.MakeVariant(false),
			"directory":    dbus.MakeVariant(false),
			"filters":      dbus.MakeVariant(providerConfigFileFilters()),
		}
		body, err := conn.Call(ctx, portalBusName, portalObjectPath, portalFileChooser+".OpenFile", "", title, options)
		if err != nil {
			return "", err
		}
		if len(body) == 0 {
			return "", fmt.Errorf("file chooser did not return a request handle")
		}
		handle, ok := body[0].(dbus.ObjectPath)
		if !ok {
			return "", fmt.Errorf("file chooser returned unexpected handle type %T", body[0])
		}
		return handle, nil
	})
}

// decodePortalStringArray reads a string-array result out of a portal
// Response's results dictionary.
func decodePortalStringArray(results map[string]dbus.Variant, key string) ([]string, error) {
	v, ok := results[key]
	if !ok {
		return nil, fmt.Errorf("portal response missing %q", key)
	}
	values, ok := v.Value().([]string)
	if !ok {
		return nil, fmt.Errorf("portal response %q has unexpected type %T", key, v.Value())
	}
	return values, nil
}

// decodePortalFileURI accepts a local absolute file:// URI (including
// document-portal mount paths) and returns its decoded filesystem path. Other
// schemes, remote authorities, malformed URIs, and relative paths are
// rejected.
func decodePortalFileURI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse file uri %q: %w", raw, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported file uri scheme %q", u.Scheme)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("unsupported file uri host %q", u.Host)
	}
	if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
		return "", fmt.Errorf("file uri %q is not an absolute local path", raw)
	}
	return u.Path, nil
}

// isSupportedOpenURIScheme reports whether rawURL is an HTTP(S) URL, the only
// scheme forwarded to the portal OpenURI method.
func isSupportedOpenURIScheme(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// portalOpenURICall invokes OpenURI.OpenURI and awaits its Request response
// using the same race-free lifecycle portalOpenFile uses. The method call
// replying only means the request was accepted, not that opening succeeded;
// the real outcome arrives asynchronously on Request.Response.
func portalOpenURICall(ctx context.Context, conn portalConn, token string, predicted dbus.ObjectPath, activationToken, rawURL string) (portalResponse, error) {
	return runPortalRequest(ctx, conn, predicted, func() (dbus.ObjectPath, error) {
		options := map[string]dbus.Variant{
			"handle_token": dbus.MakeVariant(token),
			"ask":          dbus.MakeVariant(false),
		}
		if activationToken != "" {
			options["activation_token"] = dbus.MakeVariant(activationToken)
		}
		body, err := conn.Call(ctx, portalBusName, portalObjectPath, portalOpenURI+".OpenURI", "", rawURL, options)
		if err != nil {
			return "", err
		}
		if len(body) == 0 {
			return "", fmt.Errorf("open uri did not return a request handle")
		}
		handle, ok := body[0].(dbus.ObjectPath)
		if !ok {
			return "", fmt.Errorf("open uri returned unexpected handle type %T", body[0])
		}
		return handle, nil
	})
}

// portalAddNotification invokes Notification.AddNotification with the given
// priority.
func portalAddNotification(ctx context.Context, conn portalConn, id, title, message, priority string) error {
	notification := portalNotificationBody(title, message, priority)
	_, err := conn.Call(ctx, portalBusName, portalObjectPath, portalNotification+".AddNotification", id, notification)
	return err
}

// portalRemoveNotification invokes Notification.RemoveNotification best-effort.
func portalRemoveNotification(ctx context.Context, conn portalConn, id string) {
	_, _ = conn.Call(ctx, portalBusName, portalObjectPath, portalNotification+".RemoveNotification", id)
}
