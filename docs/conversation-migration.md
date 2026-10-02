# Responses conversation migration

Conversation migration lets a Codex Responses conversation continue on another
Azure resource or GitHub Copilot after a confirmed failure before execution. It
preserves visible instructions, messages, local tool calls and their recorded
results. The client process and local files stay in place. A completed file edit
is context for the next generation; Vekil does not execute it again.

[Durable ownership](state-recovery.md) remembers which resource issued a token
after Vekil restarts. Migration additionally saves the readable conversation
needed to reconstruct a request when that resource is unavailable. Ownership
alone cannot do that. Migration is disabled unless configured explicitly.

## Enable selected routes

Add this block to the existing schema-v2 providers JSON or YAML. The CLI,
launchers and menubar use the same initialization and configuration. No migration
environment variable, flag or separate app setting is needed.

```yaml
conversation_migration:
  routes: [azure-coding]
  max_history_bytes: 8388608
  max_total_bytes: 268435456
  max_snapshots: 4096
```

`routes` contains operational `model_routes[].id` values. Each route must be
public, advertise native `/responses`, contain at least two targets using
`azure-openai` or `copilot`, use `routing.mode: priority_failover`, and allow at
least two target attempts and upstream sends. Configure the same underlying model
and compatible tools and reasoning settings on all targets. Azure deployment
names may differ from the Copilot model ID. Existing
[provider configuration](provider-routing.md) supplies endpoints and provider
authentication. Copilot uses the existing GitHub credential, and startup validates
that the configured Copilot model advertises `/responses`.

For east, west, then Copilot, register a `type: copilot` provider and append it to
the existing route's targets. Allow three target attempts and upstream sends:

```yaml
targets:
  - id: east
    provider: azure-eastus2
    upstream_model: gpt-6-astra
  - id: west
    provider: azure-westus3
    upstream_model: gpt-6-astra
  - id: copilot
    provider: copilot
    upstream_model: gpt-6-astra
routing:
  mode: priority_failover
  max_target_attempts: 3
  max_upstream_sends: 3
```

The saved history must fit the destination's context and request-size limits.
A history within Vekil's storage quota can still exceed Copilot's input limits.
Migration does not truncate or summarize it to make it fit.

The `conversation_migration` limits shown above are defaults. All are positive
integers. Upper limits are 64 MiB per snapshot, 16 GiB total logical bytes, and
1,000,000 snapshots.
`max_total_bytes` must be at least `max_history_bytes`. Durable `state_bindings`
is required; a memory-mode process override also prevents startup. Removing the
migration block restores ordinary ownership pinning and leaves saved history on
disk. Routes outside the block retain their existing behavior.

## Continuation and completeness

Vekil saves a complete immutable snapshot for each successful response. It accepts
these forms of input on an enabled route:

| Request | Recovery history |
|---------|------------------|
| New request with user, system or developer messages and no earlier provider state | Starts a new recorded conversation |
| `previous_response_id` plus only new messages or tool results | Loads that response's saved snapshot without contacting the original resource |
| Full input including previously returned item IDs or encrypted reasoning | Matches saved output, then verifies the entire ordered visible prefix |
| Full visible input without provider anchors | Requires the same `session_id`, `session-id` or `thread-id` header to match a saved prefix |
| Independently supplied complete history with `X-Vekil-History-Complete: true` | Starts a separate conversation using exactly the supplied history and instructions |

An explicit import asserts that the supplied visible messages and completed tool
results are sufficient. It discards provider-private state and does not inherit
another conversation's instructions. Use it deliberately when importing a
conversation or recovering after history deletion. Several messages or a lone
tool result do not establish completeness. A request without a saved match,
with a changed/missing prefix, or with only an unknown response ID fails before
inference.

Every function/custom tool result must have its original `call_id` and a matching
earlier call. Results for parallel calls may arrive in any order, but all pending
calls must have results before the next generation. Duplicate calls, unmatched
results and partially returned tool groups fail. Results remain tool-output
items with their original role; Vekil never promotes them to instructions.
Tool definitions and instructions are saved and inherited when omitted on a
continuation. An explicit replacement, including `null`, takes precedence.
Responses Lite `additional_tools` catalogs support the same local function,
custom and namespace tools. Vekil saves them separately from visible messages,
inherits them when omitted, and uses supplied catalogs as replacements. Their
IDs do not establish conversation lineage. `client_metadata` attribution is
accepted as a string map of up to 64 entries and 16 KiB per request.
Item-level turn metadata is excluded from visible history and reconstruction.

The supported history is text, function/custom tools executed by the client,
tool namespaces, and hosted web search. A `web_search` or `web_search_preview`
tool definition and completed `web_search_call` items are visible history.
A saved call keeps the prefixed item ID, `completed` status and the action's
`type`, `query`, `queries`, `url` and `pattern` fields, which is what Codex
replays; result sources and `url_citation` annotations on the cited text are
accepted and dropped because Codex does not retain them either. Copilot issues
opaque item IDs, which Codex drops on replay, so a call without a prefixed ID
is saved and replayed without one. Azure rejects a foreign search ID that does
not start with `ws` or exceeds 64 characters, and it accepts an ID-less call.
Only a target whose provider declares the capability can receive the call
items, so a conversation that uses web search skips undeclared targets during
migration:

```yaml
providers:
  - id: azure-eastus2
    type: azure-openai
    hosted_tools: [web_search]
```

Images, audio, file references, other hosted tools, provider
`conversation`/`prompt` state, background generation, automatic truncation and
compaction are unsupported. A request or completion carrying them is not
rejected: Vekil forwards the turn unprotected on the normal route, logs the
reason, and saves no history for it. After a switch, a full-history client
still replays the earlier owner's encrypted reasoning. When that mixed state
would fail ownership validation, the unprotected turn instead runs only on the
saved owner of the conversation's latest snapshot, without failover. It keeps
only that owner's encrypted reasoning, drops the turn-state header, and sends
readable items without their earlier provider IDs.
`/v1/responses/compact` follows the same rule, and its output is never saved
as history. Later failover cannot reconstruct an unprotected turn, and a
response-ID continuation from an unprotected completion returns
`conversation_history_unavailable`. Encrypted compaction or a summary cannot
prove that the original conversation is complete. Supply the original visible
history; Vekil does not invent a checkpoint. Automatic HTTP/WebSocket compaction
and the legacy encrypted-content retry are disabled for protected turns.

## A switch and later turns

The original owner must still match the configured endpoint, deployment and
authenticated identity. A dial/TLS failure before request delivery, a local
deployment cooldown, or an adapter-certified rejection before generation can
permit migration. Partial output, ambiguous delivery, cancellation, shutdown,
an exhausted deadline or a storage failure cannot. Generic error statuses alone
are insufficient proof that a request did not execute.

Vekil reconstructs a request on the next eligible target using the same operation
deadline and attempt/send budgets. A confirmed failure before execution on that
backup can advance to another untried target. Migration never returns to an
already attempted target. It removes the old response ID, turn-state header and
private reasoning before dispatch. Each target uses its own authentication.
Long histories consume more input tokens and can increase latency and cost;
no automatic summarization reduces that history.

Successful west output belongs to west. Response-ID continuations use the new
ID when the response was stored upstream. For `store: false`, Vekil reconstructs
from its local snapshot. Full-history clients can retain west-issued reasoning
whose ownership is verified while earlier east reasoning is removed. This does
not transfer east's private reasoning or promise identical future answers.

Copilot rejects `store: true`. Migration-enabled routes send `store: false` to
Copilot when the client requested storage, and always reconstruct Copilot
response-ID continuations from Vekil's local snapshots. This also works after
restart. The original Azure response IDs
keep their Azure ownership. Refreshing a Copilot bearer preserves ownership;
changing the source GitHub credential does not.

Older east IDs keep their original ownership and immutable history. Branching
from one cannot acquire later west messages. Different conversations proceed
independently; concurrent turns on one saved conversation return
`conversation_turn_in_progress` rather than racing region changes. So does a
turn that resumes from output an active turn has streamed but not saved yet,
such as right after an interrupt, so the client can retry it. A Codex turn from
another thread, such as a `/side` conversation or a subagent forked from a
thread that is still working, starts its own conversation from the saved
history instead of waiting. A retry from that thread reaches the same branch,
so it still waits for that thread's active turn. Vekil tells threads apart by
Codex's `thread-id` header and applies this only to the Codex releases
described below.

The proxy-owned WebSocket bridge follows the same rules. Reconnect by sending
`response.create` with the saved `previous_response_id` and only new input, or
send matching full history. This also works after a Vekil restart. An independent
import uses per-turn `headers: {"X-Vekil-History-Complete": "true"}`. A
`generate: false` staging ID remains connection-local and is not a saved
generation. Migration-enabled routes always use upstream HTTP, including when
the native Copilot WebSocket option is enabled for other routes. Experimental
direct Copilot connections are outside this feature.

## Storage, diagnostics and deletion

Snapshots contain conversation text, source code and tool output in plaintext
inside the existing private bbolt database. The same macOS APFS/Linux filesystem,
ownership, `0700` directory, `0600` file, one-writer and synchronous commit rules
apply. Integrity checks detect corruption; they are not encryption or protection
against a writer with the service user's filesystem access. Logs and dashboard
metrics contain no saved conversation text.

Each snapshot stands for the complete visible history through one response.
Successive turns repeat the earlier items and the whole tool catalog, so Vekil
stores each distinct item, instructions text and tool catalog once, and
snapshots reference them. A turn adds its new items and a small record.
Snapshots saved by earlier Vekil versions keep their own copies until pruned.
Those versions refuse to start with a store that holds snapshots saved this way.
History limits are separate from ownership `max_entries`. Per-snapshot bytes
measure the complete history a snapshot stands for, including serialized
metadata and the integrity tag. Total logical bytes count each stored value
once, plus records, indexes and unresolved attempts. bbolt pages and
transaction overhead require additional disk space. Nothing expires or evicts
automatically. Capacity errors preserve existing snapshots.

Before dispatch, Vekil commits an attempt marker. Before exposing a completed
response, it atomically saves history and clears that marker. If a crash or
incomplete stream leaves execution uncertain, later continuation is blocked to
avoid duplicate work. A stream that ends while the client stays connected is not
uncertain: the client received exactly what Vekil handed off. That holds when
the upstream ends it, with a terminal event (`response.failed`, `error`,
`response.incomplete` or `response.cancelled`) or by closing or resetting the
stream, and when Vekil ends it, through a processing error, its streaming
deadline or a shutdown. Reasoning, assistant messages and tool calls that
arrived only in part cannot run anything, so the next turn may continue from the
previous response. Vekil saves the completed reasoning and messages it
delivered, so a client that resends them, as Codex does, continues from verified
history too. A completed tool call or hosted tool call may have run, so
Vekil first saves every item it delivered as a snapshot of that response, as
after a client interrupt. Codex runs a delivered call and resends it with its
output, and that continuation matches the saved history. Only a turn that
continues from those items is admitted; a turn that branches from the earlier
history still gets the `409`, because it could repeat the call. After a
completed tool call, Vekil settles a terminal event only once the client reads
it, because the call may still be on its way to the client when the event
arrives. A client that disconnects first owns the outcome, as after an
interrupt. Execution stays
uncertain when a tool call's complete arguments arrived without the finished
call, when executable output arrived inside the failure event or a `keepalive`,
when an event Vekil does not recognize was handed off, or when the delivered
items cannot be saved. When Vekil ends the stream, it cannot tell whether the
client received the last events it handed off, so a completed tool call among
them also stays uncertain. It also stays uncertain after an ambiguous delivery
before commitment, a failed save, a reused response ID or a crash. Before the
stream is committed, a certified failure may instead
[fail over](provider-routing.md) to another target. After commitment, Vekil
forwards that failure unchanged, so the client can retry the turn. After only
reasoning and messages, it clears the marker. After a completed tool call, it
keeps the marker pointed at the saved items until a turn continues from them.
A stream Vekil cannot settle ends with `response.failed` carrying
`conversation_execution_uncertain`. A saved completion survives restart.

Codex 0.157 and later record every item they complete, run a completed tool
call even when its stream then fails, and resend both, with the tool's output,
in their next request. A Codex request therefore carries every item it received
from an earlier attempt, and the model sees each resent call's output, so it
cannot repeat the call. Vekil admits such a request past an unresolved attempt
and replaces that attempt's marker. This covers crashes, ambiguous deliveries,
streams Vekil could not settle and markers written by earlier Vekil versions.
History validation still requires the saved history as a prefix and a result
for every call. After that prefix, Vekil accepts model output it never saved
only from Codex. That output can come from such an attempt or from the last
items of an interrupted stream, which Vekil could not confirm as delivered.
Vekil recognizes Codex by a User-Agent that starts with
`codex_cli_rs/`, `codex-tui/`, `codex_exec/` or `codex_vscode/` followed by a
release version, such as `codex_exec/0.157.1 (...)`. Pre-release and
development builds do not count. Other clients keep the marker. Probes of
opencode 1.18.30 and GitHub Copilot CLI 1.0.81 found neither eligible. opencode
runs a delivered tool call, then retries the original request without it.
Copilot CLI sends the stock OpenAI SDK User-Agent, so Vekil cannot recognize it.

A client that disconnects or cancels its own request, for example by
interrupting an agent mid-turn, owns that turn's outcome. Vekil saves the
completed output items it delivered before the disconnect, in output order, as a
snapshot of that response, then clears the marker. An item counts as delivered
once a later event of the stream has been sent after it. The next turn may include those items and their
tool results, or omit them and branch from the earlier history. When at least
one completed item was delivered, a response-ID continuation of the interrupted
response is rebuilt from those items. Otherwise, continue from the previous
response.
Items the client did not receive from Vekil still fail as incomplete history.
Unfinished messages that arrived only as deltas are not saved. Vekil does not
repeat the request automatically. A crash is not a client decision and leaves
execution uncertain. A shutdown settles the turn like any stream Vekil ends,
counting delivered items by the same rule as a disconnect.

If an upstream reuses a saved response ID, Vekil withholds the new completion
and leaves that turn uncertain. The collision does not disable the shared store.

HTTP responses report `X-Vekil-Conversation-Recovery: recording` during streaming
and `saved` for a completed JSON response. A completed JSON response whose
output could not be saved reports `unprotected` instead. The authoritative
completion contains `vekil: {"history":"saved","target":"west"}`, with
`"migration":"completed"` only on the turn that switched; an unprotected
completion has no `vekil` field. WebSocket completion objects carry the same
fields. Fixed-content logs distinguish `attempted`, `completed`, `blocked`,
`unprotected`, `failed`, `interrupted`, `released` and `branched` recovery.
`failed` records a settled attempt, with reason `failure_event` for a terminal event,
`stream_ended` for a stream the upstream ended without one, `proxy_ended` for a
stream Vekil ended, or `delivered_history_saved` when a completed tool call was
saved first. `interrupted` records a client disconnect and whether delivered
history was saved. `released` with reason `client_resends_delivered` records a
Codex request admitted past an unresolved attempt. `branched` with reason
`concurrent_turn` records a Codex turn from another thread that started its own
conversation while the one it continues was busy. A `recording` header alone
is not a saved completion. The menubar app keeps these logs in a file; see
[Menubar](menubar.md).

Errors use `conversation_history_unavailable`, `conversation_history_incomplete`
or `conversation_tools_pending` for invalid recovery input. Uncertain
execution returns `409` with `conversation_execution_uncertain`. Storage and
capacity failures return `503` with `conversation_history_storage_unavailable`
or `conversation_history_capacity_exceeded`. After stream commitment, a terminal
SSE/WebSocket error reports the failure. Ownership-store failures retain their
existing `state_binding_*` codes.

To delete history, stop and drain the proxy, choose a past whole-second cutoff,
and run:

```bash
vekil state prune-history --file /path/to/private-state/bindings.db \
  --before 2026-01-01T00:00:00Z --confirm
```

This deletes snapshots older than the cutoff unless their conversation has an
unresolved attempt at or after the cutoff. Older unresolved attempts are retired
together with every snapshot of their conversation. Stored values that no
remaining snapshot references are deleted too. Ownership records are
unchanged. Ordinary `state prune` deletes ownership proof separately and does
not erase conversation text.
Pruning breaks affected recovery. Freed pages are reusable, not securely erased,
and backups can retain the deleted contents. Back up only a stopped store; an
older backup loses later completions and uncertainty markers.
