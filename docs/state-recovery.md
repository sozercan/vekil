# Durable Provider-State Recovery

Schema-v2 explicit routes retain provider-state ownership across process
restarts by default. The existing providers JSON or YAML controls storage for
the CLI, managed launchers, and menubar app. No durability flags, environment
variables, or separate menubar settings are required. Legacy, zero-config, and
configurations without explicit routes keep their existing defaults.

Durable storage cannot reconstruct bindings already lost through restart,
expiry, or eviction in memory mode. Vekil validates and opens the store before
listening; configuration changes require a restart.

## Configure one local writer

The optional top-level block belongs in the existing schema-v2 providers file.
These are the defaults for explicit routes, including when the block is absent:

```yaml
state_bindings:
  mode: durable
  max_entries: 8388608
  # file: /absolute/path/to/bindings.db
```

Set `mode: memory` to opt out of durable recovery. Memory mode retains the
process-local index, a 24-hour absolute TTL, and LRU eviction. Its default
capacity is 262,144 entries when `max_entries` is omitted. A configured
`max_entries` must be positive; it limits logical records rather than reserving
memory or disk for every possible entry.

When `file` is omitted, Vekil uses a stable application-data path:

| Platform | Default file |
|----------|--------------|
| macOS | `~/Library/Application Support/vekil/state/bindings.db` |
| Linux with `XDG_DATA_HOME` | `$XDG_DATA_HOME/vekil/state/bindings.db` |
| Linux otherwise | `~/.local/share/vekil/state/bindings.db` |

`XDG_DATA_HOME`, when set, must be an absolute, clean path. Vekil creates missing
default-path directories with mode `0700` and syncs directory creation. It never
changes the permissions of an existing unsafe directory to make startup succeed. An
explicit `file` must be an absolute, clean path whose containing directory
already exists with mode `0700` and is owned by the service user. A missing
database file is created with mode `0600`. An existing empty, corrupt,
incompatible, unsafe, or locked file stops startup; it is never replaced and a
storage failure never causes automatic fallback to memory.

Optional [process overrides](configuration.md) can select a different file or
use `--state-bindings-mode memory` for isolated tests. A file override activates
durable mode; an explicit mode override of `memory` wins and avoids opening the
file. An omitted or zero capacity override follows the providers config.
The same resolution rules apply to the CLI, launchers, and menubar.

Linux supports local ext4, XFS, btrfs, tmpfs, and overlay filesystems. macOS
supports local writable APFS volumes. Network/shared filesystems and other
macOS filesystems are rejected. A tmpfs store survives process restart but is
lost on reboot. Container-layer or ephemeral-volume replacement also loses its
store; use a persistent local volume for host or container lifecycle continuity.

The filesystem and ownership checks use the actual opened directory and
store-file descriptors, including both the read-only validation probe and the
writable open. A file mounted from unsupported storage is rejected too. Symlink
files or containing directories, non-regular files, and hard-linked store files
are rejected. macOS also rejects extended ACLs on the file or private directory,
since ACLs can grant access despite `0600` or `0700` mode bits.

bbolt synchronously commits data pages before transaction metadata. On macOS,
its Go file sync uses `F_FULLFSYNC`; Vekil verifies support on the opened
descriptors and uses the same full-sync operation for directory barriers.
Linux retains synchronous file and directory barriers. Recovery depends on
the local filesystem and device honoring those sync operations.

Concurrent clients can share one proxy and its store. Only one proxy process
may own a file. A second writer fails startup, including an offline pruning
command while the service owns the file. Separate proxy instances must choose
distinct files or use the memory override for tests. Do not copy an active
store to give replicas independent writable copies. Cross-host failover and
shared storage remain unsupported; separate instances still need ingress
affinity for stateful traffic.

Shutdown retains the lock until a successful drain; an incomplete drain does
not admit a replacement writer while the old process can still write.
If the shutdown caller times out, finalization continues after force-closing
connections. The lock is released only after HTTP/websocket handlers and detached
workers actually finish; another `Stop` call is not required. A stuck
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
The same rule covers response IDs and state-bearing response, conversation,
output, and item fields. A single spelling such as `response.ID` is bound before
exposure; competing spellings such as `id` and `ID` are rejected. Arbitrary
message text and vendor metadata are not interpreted as ownership fields.
Durable JSON responses and stream events reject duplicate object keys before
binding or forwarding, including escaped spellings of the same key. JSON
nesting is bounded before the duplicate-key scan, including builds using the
legacy Go JSON decoder. Reusing a key in distinct objects or inside a string is
valid. Ambiguous events terminate the stream without exposing or recording
their state; memory-only passthrough is unchanged.

Durable HTTP Responses and compact requests require canonical JSON names for
`previous_response_id`, `conversation` and its `id`, `input`, input-item `type`,
and `encrypted_content` in reasoning/compaction items. Case variants, competing
spellings, and duplicate keys return `400` before any provider send. The
WebSocket bridge checks the same state paths after its existing envelope
normalization. Vendor metadata and message text are not interpreted as ownership
fields. Memory and legacy request behavior is unchanged.

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
fingerprints, issuance timestamps, and conflict tombstones. Raw continuation
tokens, conversations, account IDs, and credentials are not stored. It is not an
encrypted conversation backup. Keep the file private: its integrity key is in
the same file and does not defend against a malicious writer with the service user's
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
The default 8,388,608-entry limit does not preallocate the full capacity. The
database grows as new records are committed.
At capacity, existing proof remains usable and existing tokens can still be
marked conflicting. A batch requiring new records fails without partial
insertion or exposure. Increase the limit on restart or deliberately retire old
proof offline. bbolt pages, freelists and copy-on-write overhead require extra
disk space; pruning reuses pages but does not shrink the file.

The [dashboard](dashboard.md) and `/stats.json` expose retained entry count,
configured capacity, capacity usage, and database-file size. Warnings begin at
80% usage, become critical at 95%, and show exhaustion at the limit. Raising the
limit still requires enough disk space for future records; none of these
warnings triggers eviction or pruning.

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
Records first issued before it (or first marked conflicting before it) are
deleted atomically. Repeated observations preserve the original issuance time;
repeated collisions preserve a tombstone's first conflict time.
Pruned opaque state becomes unknown. Conversation-
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

To disable durable ownership, set `state_bindings.mode: memory` or the explicit
memory-mode override and restart. This starts an empty memory-only index and
loses access to durable proof while leaving the file intact. Removing an
optional file override restores the providers configuration and its durable
default. Downgrading to a binary that cannot read this format also loses
continuity. Fence and drain stateful traffic before such a rollback; the
memory-only 24-hour TTL is **not** a retirement period for durable records.
Ordinary restart with the same supported binary, complete store and exact owner
configuration preserves proof but still requires clients to reconnect.
