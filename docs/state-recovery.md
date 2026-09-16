# Durable Provider-State Recovery

For explicit schema-v2 routes, Vekil can retain provider-state ownership across
a process restart. Enable this **before** issuing state that must survive. It
cannot reconstruct bindings already lost through restart, expiry, or eviction.
Memory-only operation remains the default.

## Enable one local writer

Create a private directory owned by the Vekil service user with mode `0700`, on
a supported local filesystem, then start Vekil with an absolute, clean path:

```bash
vekil --providers-config /path/to/providers.yaml \
  --state-bindings-file /path/to/private-state/bindings.db \
  --state-bindings-max-entries 262144
```

The corresponding environment variables are `STATE_BINDINGS_FILE` and
`STATE_BINDINGS_MAX_ENTRIES`. An omitted/zero entry limit means 262,144. A limit
without a file, or a negative limit, is invalid. A missing file is created with
mode `0600`; an existing empty, corrupt, incompatible, unsafe, or locked file
is rejected rather than replaced. Vekil validates the store before listening.
No runtime activation or migration of a memory-only index is performed.

Durable mode currently supports Linux only. The filesystem allowlist is ext4,
XFS, btrfs, tmpfs, and overlay; network/shared filesystems are rejected. A tmpfs
store survives process restart, **not reboot**. Container-layer/ephemeral-volume
replacement also loses its store. Use a persistent local volume for host or
container lifecycle continuity. The file and its containing directory must be
private and owned by the service user. The filesystem check uses the actual
opened directory and store-file descriptors, including both the read-only probe
and writable open; a file mounted from unsupported storage is also rejected.
Symlink files/directories, non-regular
files, and hard-linked store files are rejected.

Only one process may open a store. A second writer fails startup, including an
offline pruning command while the service owns the file. Do not copy an active
store to give several replicas independent writable copies. This is not shared
storage, cross-host failover, or a replacement for ingress affinity. Shutdown
retains the lock until a successful drain; an incomplete drain does not admit a
replacement writer while the old process can still write.
If the shutdown caller times out, finalization continues after force-closing
connections. The lock is released only after HTTP/websocket handlers and detached
workers actually finish; another `Stop` call is not required. A genuinely stuck
handler or worker continues to hold the lock, and late storage-close errors are
logged. Repeated `Stop` calls retain the original shutdown result.
Listener startup failure also triggers shutdown and store cleanup. In-process
callers must construct a new server to retry.
Concurrent starts cannot create multiple listeners. Shutdown cancels pending
startup and retains the store lock until any late listener is closed; a shutdown
deadline still uses the same background finalizer rather than releasing the lock
early or requiring another stop call.

## What is recovered

Before exposing provider-issued response IDs, encrypted reasoning/compaction
content, conversation ownership, or `X-Codex-Turn-State`, Vekil commits exact
ownership proof. JSON batches and state-bearing stream events are all-or-nothing.
Repeated proof for the same owner needs no new write. Hidden state discarded from
a failed route attempt or a normal compact/memory summary is not persisted.
Final error responses, including passthrough compaction-trigger failures, are
also exposure boundaries: their structured JSON state
and headers are committed before passthrough, preserving the upstream status.
Empty final-error bodies may carry bound headers. Nonempty malformed or
non-object JSON is withheld with a visible `502`; storage faults still use
`503`. Only the error actually returned to the client is bound, not errors
discarded during failover. Usage recording and tool optimization remain
success-only.
Native Chat and direct Anthropic final passthrough errors also bind supported
turn-state headers; their bodies are not parsed as Responses objects. Websocket
error frames bind their final projected turn-state headers using the captured
request owner, including translated precommit errors. They retain structured
error messages/codes, but durable mode replaces raw-body message fallback with
bounded HTTP status text so discarded body state cannot escape inside a string.
Durable Responses failure events also commit turn state carried in root or
nested error headers atomically with the event's body state, before either the
raw event or a projected websocket error can expose it.
Single case-variant JSON error-envelope members accepted by websocket decoding
are supported. Competing spellings such as `headers` and `Headers`, or multiple
turn-state values that websocket projection would join, are rejected before
exposure. This also applies to repeated raw HTTP turn-state headers inherited
by websocket errors, including errors after stream progress. Ordinary HTTP
responses retain their repeated-identical-header semantics. HTTP header names
retain their case-insensitive semantics.
Durable JSON responses and stream events reject duplicate object keys before
binding or forwarding, including escaped spellings of the same key. Reusing a
key in distinct objects or inside a string is valid. Ambiguous events terminate
the stream without exposing or recording their state; memory-only passthrough
is unchanged.

Ownership includes route/target/provider identity, the effective endpoint and
query, physical model/deployment, and authenticated account/tenant scope from
the **actual outbound request**. Reusing configuration labels does not authorize
another owner. Changed endpoints, deployments, API keys, tenant headers, or
principals reject retained state before inference; Vekil never guesses a target
or silently drops context to recover. Copilot service-token refresh preserves
ownership when its source-credential fingerprint is stable. Legacy caches
without that provenance cannot promise refresh continuity. Entra requires
issuer, tenant and object identity from its acquired token; token scope is also
bound. Malformed or unavailable principal evidence fails before dispatch.

The provider/endpoint support matrix is unchanged. In particular, this does not
enable `openai-codex` as an explicit schema-v2 target. Codex **clients** using
supported explicit Responses routes benefit from the store. Legacy/zero-config
provider routes retain their existing behavior.

The file contains a versioned key, typed keyed token digests, keyed owner
fingerprints, issuance timestamps, and conflict tombstones—not raw continuation
tokens, conversations, account IDs, or credentials. It is not an encrypted
conversation backup. Keep the file private: its integrity key is in the same
file and does not defend against a malicious writer with the service user's
filesystem access. Preserve the complete file for an offline backup, including
its key and tombstones. Restoring an older backup loses proof issued afterward.

Websocket connection history and upstream connections are **not** recovered.
After reconnect, resend full client-held input without an old connection-local
`previous_response_id`; retained encrypted input is still checked against its
durable owner. HTTP `previous_response_id` retains its usual provider contract.
Provider-side expiry/deletion may still reject state whose local ownership is
known. Responses-backed Chat tool replay remains a separate process-local
store; this feature does not persist it or migrate state across targets.

## Retention, capacity and explicit pruning

Durable records never expire or evict automatically. The configured limit counts
logical fixed-size records, **including tombstones**, not physical file bytes.
At capacity, existing proof remains usable and existing tokens can still be
marked conflicting. A batch requiring new records fails without partial
insertion or exposure. Increase the limit on restart or deliberately retire old
proof offline. bbolt pages, freelists and copy-on-write overhead require extra
disk space; pruning reuses pages but does not shrink the file.

Pruning is continuity-breaking. Stop and drain the owning service, preserve a
backup, choose the cutoff deliberately, then explicitly confirm:

```bash
vekil state prune --file /path/to/private-state/bindings.db \
  --before 2026-01-01T00:00:00Z --confirm
```

The cutoff must be in the past and on a whole-second boundary, matching the
store's timestamp precision. Omit the fractional-second component in CLI input;
the pruning API rejects cutoffs with nonzero nanoseconds. Both validate before
opening the store. Records from the cutoff second are retained.
Records issued before it (or subsequently marked conflicting before it) are
deleted atomically; repeated same-owner observations
do not refresh issuance time. Pruned opaque state becomes unknown. Conversation-
only IDs retain their narrow deterministic first-use bootstrap rule, so pruning
their proof removes Vekil's ability to distinguish old use from first use.
No provider credentials are loaded and no inference is performed by pruning.

## Failures and rollback

Capacity failures use HTTP `503`, code `state_binding_capacity_exceeded`.
Storage failures use `503`, code `state_binding_storage_unavailable`; after
streaming starts, a bounded SSE or websocket error terminates the turn before
unrecorded state is exposed. The failing process freezes use of an uncertain
store until operator repair and reopen. A storage failure after inference never
authorizes Vekil to retry that inference or switch targets. Previously exposed,
committed state remains recorded even when a later event fails.
Direct Anthropic Messages and Count Tokens use their native `type: error`
envelope with `overloaded_error` and the same bounded storage-failure message,
rather than OpenAI-specific error fields.
The same message is returned before dispatch when the store is frozen or closed.

These are local infrastructure failures, not model safety decisions or native
tool-approval verdicts. Do not bypass approvals or replay a refused tool to test
recovery. Preserve the store, check permissions/free space/ownership, resolve the
storage fault, and reopen the same file and owner configuration. Never delete or
reinitialize it merely to silence the error.

Removing the flag returns to an empty memory-only index and loses access to
durable proof. Downgrading to a binary that cannot read this format has the same
continuity cost. Fence and drain stateful traffic before such a rollback; the
memory-only 24-hour TTL is **not** a retirement period for durable records.
Ordinary restart with the same supported binary, complete store and exact owner
configuration preserves proof but still requires clients to reconnect.
