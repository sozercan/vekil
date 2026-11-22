# 0008: Fall back on Linux only when the outcome permits it

Status: Accepted

## Context

The Linux tray uses desktop portals across Wayland and X11, but some environments lack a suitable backend. An RPC error does not necessarily mean an interaction never reached the user. Repeating an informational notification is acceptable; repeating an authorization prompt or overriding a user's dismissal is not.

## Decision

- Run sign-in confirmation with approve and decline actions and no default action under one deadline. Fall back to `org.freedesktop.Notifications` only when the portal prompt is known not to have appeared, such as a failed signal subscription or an error known to precede display. An ambiguous error, a timeout, cancellation, or anything other than explicit approval after possible display is final. At the deadline, honor an already-buffered matching action or dismissal before closing the legacy notification.
- Use urgent or normal non-modal notifications for errors or information; there is no generic modal message-dialog portal. An ambiguous failure may retry a plain notification via the legacy interface without discarding the shared portal connection. The legacy `Notify` call takes app name, replacement ID, icon, summary, body, alternating action keys/labels, hints, and expiry. Plain toasts use server-default expiry; confirmations use critical urgency and no server expiry, with dismissal or cleanup under the caller's deadline.
- Wait for the `OpenURI` request result. Both success and explicit user cancellation are final; a failed request or another result is eligible for `xdg-open`. Do not add a subprocess file chooser fallback. If the portal file chooser is unavailable, offer the existing command-line or saved-config paths instead.

## Consequences

Confirmation remains at most once and fail-closed when delivery is ambiguous. Informational toasts favor visibility over deduplication. See [Linux Tray](../menubar.md#linux-tray) for recovery options when a portal backend is missing.
