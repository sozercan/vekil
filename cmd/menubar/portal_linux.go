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

	portalOpenURITimeout = 15 * time.Second
)

var errPortalNotificationNotShown = errors.New("portal notification was not shown")

var dbusErrorNamesProvingNotShown = map[string]bool{
	"org.freedesktop.DBus.Error.ServiceUnknown":   true,
	"org.freedesktop.DBus.Error.NameHasNoOwner":   true,
	"org.freedesktop.DBus.Error.UnknownMethod":    true,
	"org.freedesktop.DBus.Error.UnknownInterface": true,
	"org.freedesktop.DBus.Error.UnknownObject":    true,
}

const dbusErrorSpawnPrefix = "org.freedesktop.DBus.Error.Spawn."

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

type portalConn interface {
	UniqueName() string
	Call(ctx context.Context, dest string, path dbus.ObjectPath, method string, args ...any) ([]any, error)
	AddMatch(ctx context.Context, options ...dbus.MatchOption) error
	RemoveMatch(ctx context.Context, options ...dbus.MatchOption) error
	Signal(ch chan<- *dbus.Signal)
	RemoveSignal(ch chan<- *dbus.Signal)
	Connected() bool
	Close() error
}

type sessionPortalConn struct {
	conn *dbus.Conn
}

func dialSessionPortalConn() (portalConn, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	return &sessionPortalConn{conn: conn}, nil
}

var newPortalConn = dialSessionPortalConn

var sharedPortalNotificationConn portalNotificationConn

type portalNotificationConn struct {
	mu   sync.Mutex
	conn portalConn
}

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

func newPortalToken(prefix string) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func requestPathNamespace(uniqueName string) dbus.ObjectPath {
	sender := strings.ReplaceAll(strings.TrimPrefix(uniqueName, ":"), ".", "_")
	return dbus.ObjectPath(portalRequestPathPrefix + sender)
}

func predictRequestPath(uniqueName, token string) dbus.ObjectPath {
	return dbus.ObjectPath(string(requestPathNamespace(uniqueName)) + "/" + token)
}

type portalResponse struct {
	Code    uint32
	Results map[string]dbus.Variant
}

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
			if resp, ok := drainQueuedPortalResponse(sigCh, predicted, handle); ok {
				return resp, nil
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), portalCleanupTimeout)
			_, _ = conn.Call(closeCtx, portalBusName, handle, portalRequestIface+".Close")
			cancel()
			return portalResponse{}, ctx.Err()
		}
	}
}

func drainQueuedPortalResponse(sigCh <-chan *dbus.Signal, predicted, handle dbus.ObjectPath) (portalResponse, bool) {
	for {
		select {
		case sig, ok := <-sigCh:
			if !ok {
				return portalResponse{}, false
			}
			if resp, matched := decodePortalResponse(sig, predicted, handle); matched {
				return resp, true
			}
		default:
			return portalResponse{}, false
		}
	}
}

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
				return "", fmt.Errorf("portal signal channel closed")
			}
			if action, matched := decodePortalActionInvoked(sig, notificationID); matched {
				return action, nil
			}
		case <-ctx.Done():
			if action, ok := drainQueuedPortalAction(sigCh, notificationID); ok {
				return action, nil
			}
			return "", ctx.Err()
		}
	}
}

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

type portalFilterRule struct {
	Type    uint32
	Pattern string
}

type portalFilterGroup struct {
	Name  string
	Rules []portalFilterRule
}

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

func portalNotificationBody(title, body, priority string) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"title":    dbus.MakeVariant(title),
		"body":     dbus.MakeVariant(body),
		"priority": dbus.MakeVariant(priority),
	}
}

func portalConfirmationButtons(approveLabel, declineLabel string) []map[string]dbus.Variant {
	return []map[string]dbus.Variant{
		{"label": dbus.MakeVariant(approveLabel), "action": dbus.MakeVariant(portalActionApprove)},
		{"label": dbus.MakeVariant(declineLabel), "action": dbus.MakeVariant(portalActionDecline)},
	}
}

func portalConfirmationNotification(prompt confirmationPrompt) map[string]dbus.Variant {
	notification := portalNotificationBody(prompt.Title, prompt.Message, portalPriorityNormal)
	notification["buttons"] = dbus.MakeVariant(portalConfirmationButtons(prompt.ApproveLabel, prompt.DeclineLabel))
	return notification
}

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

func isSupportedOpenURIScheme(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

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

func portalAddNotification(ctx context.Context, conn portalConn, id, title, message, priority string) error {
	notification := portalNotificationBody(title, message, priority)
	_, err := conn.Call(ctx, portalBusName, portalObjectPath, portalNotification+".AddNotification", id, notification)
	return err
}

func portalRemoveNotification(ctx context.Context, conn portalConn, id string) {
	_, _ = conn.Call(ctx, portalBusName, portalObjectPath, portalNotification+".RemoveNotification", id)
}
