# Local Dashboard Configuration

Vekil's provider-configuration editor is a **local control plane** for a running, long-lived proxy. It can edit providers, schema-v2 model routes, and schema-v2 policy profiles while the currently published runtime continues serving traffic.

Open it from the traffic dashboard or directly at:

```text
http://localhost:1337/dashboard/config
```

The editor is not a remote administration surface. It does not provide RBAC, tenant isolation, audit history, configuration history, or arbitrary rollback. The only built-in rollback-like action is **Reset managed override**, which restores the current bootstrap source.

## Availability matrix

| Server topology | Config read | Config write | Notes |
|---|---:|---:|---|
| Long-lived CLI server on a loopback listener | Redacted full document | Yes, when the managed store is writable | `capability.mode` is `cli` |
| Menubar/tray server on a loopback listener | Redacted full document | Yes, when the managed store is writable | `capability.mode` is `menubar` |
| Supported loopback server with unavailable persistence | Redacted full document | No | The response explains why the editor is read-only |
| Long-lived server bound beyond loopback | Capability metadata only | No | Provider, route, policy, secret-state, source, and revision data are withheld |
| `vekil launch ...` managed-agent proxy | Capability metadata only | No | Ephemeral launch servers do not consume or write dashboard-managed overrides |
| Default Docker/Kubernetes deployment (`HOST=0.0.0.0`) | Capability metadata only | No | Edit the bootstrap file and redeploy instead |

`GET /dashboard/api/v1/config` is the capability discovery endpoint. When `capability.available` is false, the response contains only capability metadata and an unavailable reason.

## What the editor manages

The page has four views:

- **Providers** — provider type, destination, auth source, model discovery, endpoint paths, custom headers, static models, trust metadata, and provider-specific fields.
- **Model routes** — public or internal schema-v2 contracts, ordered targets, upstream model rewrites, endpoint metadata, request policy, and `primary_only` or bounded `priority_failover` routing.
- **Policy profiles** — profile identity, configured mode, terminal routes, classifier route and limits, fallback tiers, sampling, and required data-policy acknowledgements.
- **Structured JSON** — the complete non-secret editable document for lossless review/import when a field is easier to edit directly. Local JSON files are parsed in the browser; YAML files are strictly converted to canonical redacted JSON by the loopback server.

Only provider config schema versions 1 and 2 are accepted. The editor promotes a draft to schema v2 when a v2-only feature is used and does not auto-downgrade an existing v2 document. Schema version 3 is rejected.

The server reports `preserved_paths`. In the current release these are:

```text
/tool_optimizers
/insight_model
```

The browser shows them for context, but the server restores them from the exact base revision and rejects attempts to change them. The original JSON/YAML bootstrap file is never rewritten, reformatted, or used as the browser's hidden state.

## Bootstrap source and managed override

Every long-lived process starts from one immutable bootstrap source identity:

- `implicit-copilot` when no provider file is selected; or
- `file:<absolute-clean-path>` for `--providers-config`, `PROVIDERS_CONFIG`, or a menubar-selected file.

A managed override belongs to exactly one source identity. Vekil stores it below the operating system's user configuration directory:

```text
<UserConfigDir>/vekil/dashboard-config/implicit-copilot.json
<UserConfigDir>/vekil/dashboard-config/<sha256-of-file-source-id>.json
```

The managed file is strict, canonical JSON with this envelope shape:

```json
{
  "managed_schema_version": 1,
  "source": {
    "id": "file:/absolute/path/providers.yaml",
    "bootstrap_digest": "sha256:..."
  },
  "revision": "cfg_...",
  "config": {}
}
```

The bootstrap digest is calculated from canonical, complete, secret-bearing configuration semantics. Changing only JSON/YAML whitespace, key order, or comments does not change it. Changing a provider, route, policy, inline credential, configured credential source, or another behavior-significant value does.

### Startup precedence

Startup resolves configuration in this order:

1. Load and strictly validate the bootstrap source.
2. Calculate its source identity, canonical digest, and revision.
3. If no source-scoped managed file exists, use the bootstrap config.
4. If a managed file exists and both its source ID and bootstrap digest match, use the managed config.
5. If the source ID or digest conflicts, fail startup instead of silently choosing one side.

A managed override for one bootstrap path is never consumed by another path, even when both files currently contain the same config.

### Conflict and malformed-file recovery

The CLI supports two mutually exclusive startup recovery modes:

| Flag | Environment variable | Behavior |
|---|---|---|
| `--ignore-managed` | `PROVIDERS_CONFIG_IGNORE_MANAGED=true` | Start from the current bootstrap without reading or deleting the matching override. Dashboard writes are disabled for that process, so a later normal restart can still use the override. |
| `--reset-managed` | `PROVIDERS_CONFIG_RESET_MANAGED=true` | Delete the matching override before startup and use the current bootstrap. Normal managed writes are available afterward when the store is writable. |

Examples:

```bash
vekil --providers-config ./providers.yaml --ignore-managed
vekil --providers-config ./providers.yaml --reset-managed
```

Use `--ignore-managed` to inspect or recover a bootstrap without destroying the old override. Use `--reset-managed` after deciding the current bootstrap is authoritative. Both modes can recover a source conflict or a malformed managed envelope; only reset removes the file.

The menubar/tray app provides **Reset Dashboard Override** for the currently selected bootstrap source. It works before server construction, stops/restarts the proxy if necessary, and does not change the selected JSON/YAML path. **Use Default Copilot Routing** changes the bootstrap source to `implicit-copilot`; it is not the same action as resetting the override attached to the current source.

The running editor's **Reset managed override** action submits an asynchronous reset against the active revision. It deletes the matching managed file, privately rebuilds the current bootstrap config, and publishes that bootstrap as the next runtime generation.

## Secret handling

### Reads are redacted

`GET /dashboard/api/v1/config` never returns raw provider credentials:

- inline `api_key` fields are omitted from `config`;
- every `extra_headers` value is returned as an empty string;
- environment-variable **names** such as `api_key_env` remain visible, but Vekil never reads the environment variable's value back into the response; and
- `secret_states` reports only path, configured/clear state, and source such as `inline`, `env`, or `none`.

The response and config page use `Cache-Control: no-store`. Revision and target-revision tokens are opaque and do not contain raw credentials.

### Writes use `keep`, `set`, or `clear`

Raw secret values do not belong in the submitted `config` object. Each managed inline API key or extra-header value is changed through an explicit `secret_operations` entry:

```json
{
  "path": "/providers/azure/api_key",
  "operation": "keep"
}
```

Supported operations are:

- `keep` — reuse the secret from the exact base revision;
- `set` — replace it with the non-empty `value` supplied in the operation; or
- `clear` — remove it.

Secret paths use provider IDs, not provider array indexes. JSON Pointer escaping applies to provider IDs and header names.

`keep` is rejected when the provider/auth identity is no longer compatible. Renaming or retyping a provider, changing its destination, auth mode/source/header/prefix, Azure API version, or token scope requires a new `set` or an explicit `clear`. A renamed extra header must clear the old path and set the new path. The literal string `***` is only a visual placeholder: it is never treated as a stored secret and is rejected as a `set` value.

All `extra_headers` values are treated as secrets, even when a particular header seems non-sensitive. Provider base URLs containing URL userinfo are rejected.

### Managed files contain plaintext secrets

Inline secrets set through the editor are persisted in plaintext inside the source-scoped managed JSON file. Prefer `api_key_env` when practical, and protect the user configuration directory like any other credential store.

On Unix, Vekil creates the managed directory with mode `0700` and managed temporary, lock, and final files with mode `0600`. On Windows, the file inherits the current user's configuration-directory ACL and replacement uses a write-through operation. Vekil rejects symbolic-link destinations and refuses a managed file that aliases the bootstrap through the same path or a hard link.

## Browser and HTTP security

Full configuration access is enabled only for an explicitly supported long-lived loopback listener. When enabled, every config page, asset, and API request must use:

- a literal `Host` of `localhost`, a loopback IPv4 address in `127.0.0.0/8`, or `::1`; and
- the **actual bound listener port** in the `Host` header.

A DNS name that merely resolves to loopback is rejected, as is a missing or different port. This also applies when Vekil was configured with port `0`: use the actual port selected by the listener.

Any inbound authentication configured on the server still protects these routes; dashboard config is not exempt. Mutations additionally require:

- same-origin Fetch Metadata (`Sec-Fetch-Site` may be absent, `none`, or `same-origin`, but not cross-site);
- any supplied `Origin` and `Referer` host to match the request `Host`; and
- the process-local nonce returned as `csrf_token` by the config read, sent in `X-Vekil-CSRF`.

The config page is served with a same-origin Content Security Policy, no referrer, and no-store assets. These checks limit a local browser control plane; they do not turn Vekil into a supported remote administration service.

## Versioned API

All config API responses are JSON with `Cache-Control: no-store`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/dashboard/api/v1/config` | Read capability and, when available, the redacted active document, source, revision, generation, policy modes, secret states, preserved paths, provider capabilities, and CSRF token |
| `POST` | `/dashboard/api/v1/config/import` | Strictly decode one YAML provider document and return canonical redacted JSON for the browser draft; does not validate, persist, or publish |
| `POST` | `/dashboard/api/v1/config/validate` | Strictly decode, merge secret operations, preserve protected fields, and perform offline semantic validation without changing the runtime |
| `POST` | `/dashboard/api/v1/config/applies` | Submit one validated candidate for asynchronous discovery, policy preflight, persistence, and publication |
| `GET` | `/dashboard/api/v1/config/applies/{id}` | Poll a retained apply/reset status |
| `DELETE` | `/dashboard/api/v1/config/managed` | Asynchronously remove the managed override and publish the bootstrap source |

The page assets are:

```text
GET /dashboard/config
GET /dashboard/config.js
GET /dashboard/config.css
```

### YAML import

The Structured JSON view accepts `.yaml` and `.yml` files. The browser sends the selected text only to the same-origin loopback import endpoint with the process-local CSRF token. Vekil applies the same strict YAML decoder used at startup, including duplicate-key, unknown-field, merge-key, and single-document checks, then returns canonical JSON for review. Import alone does not change the active runtime, write a managed override, or modify the bootstrap file.

Inline `api_key` values and non-empty `extra_headers` values are removed from the response. The response lists only their JSON Pointer paths in `stripped_secret_paths`; use the Providers view to explicitly keep, set, or clear those secrets before validation or apply. YAML comments, anchors, and formatting are not preserved after conversion to structured JSON.

### Read response

An available response includes an HTTP `ETag` containing the quoted active revision:

```json
{
  "capability": {"available": true, "writable": true, "mode": "cli"},
  "revision": "cfg_...",
  "generation": 3,
  "schema_version": 2,
  "source": {
    "kind": "file",
    "id": "file:/absolute/path/providers.yaml",
    "bootstrap_path": "/absolute/path/providers.yaml",
    "bootstrap_digest": "sha256:...",
    "managed_path": "/user/config/vekil/dashboard-config/...json",
    "managed_active": true
  },
  "policy": {
    "process_ceiling": "enforce",
    "profiles": [
      {
        "id": "coding-economy",
        "public_id": "coding-economy",
        "configured_mode": "observe",
        "effective_mode": "observe"
      }
    ]
  },
  "config": {},
  "secret_states": [],
  "preserved_paths": ["/tool_optimizers", "/insight_model"],
  "provider_capabilities": [],
  "policy_eligibility": {
    "terminal_routes": [],
    "classifier_routes": []
  },
  "csrf_token": "..."
}
```

When full reads are unavailable, the response is intentionally limited:

```json
{
  "capability": {
    "available": false,
    "writable": false,
    "reason": "configuration control is available only on a loopback listener",
    "mode": "cli"
  }
}
```

### Validation and apply request

Validation and apply use the same mutation envelope. The body revision and `If-Match` value must both equal the active revision:

```http
POST /dashboard/api/v1/config/validate
Content-Type: application/json
If-Match: "cfg_..."
X-Vekil-CSRF: <csrf_token>
```

```json
{
  "base_revision": "cfg_...",
  "config": {
    "schema_version": 2,
    "providers": []
  },
  "secret_operations": [
    {"path": "/providers/azure/api_key", "operation": "keep"},
    {"path": "/providers/azure/extra_headers/X-Tenant", "operation": "set", "value": "tenant-secret"}
  ]
}
```

`policy_eligibility` is derived from the compiled active runtime and is used by the policy wizard to filter terminal and classifier route choices. The server still validates the complete submitted candidate authoritatively, including newly edited routes.

The JSON body limit is 1 MiB. `Content-Type` must be `application/json`. Duplicate keys, unknown fields, trailing JSON values, unsupported schema versions, invalid references, and typed field errors are rejected. Error bodies contain one stable code, an optional RFC 6901 JSON Pointer `path`, and a sanitized message; the API does not promise multi-error aggregation.

A successful validation returns `200` with `valid: true` and does not perform network discovery or policy preflight. A successful apply submission returns `202 Accepted`, a `Location` header, and:

```json
{
  "id": "apply_...",
  "state": "accepted",
  "location": "/dashboard/api/v1/config/applies/apply_..."
}
```

Only one apply or reset may be active. Another submission returns `409 apply_in_progress`. A stale or mismatched body/header revision returns `412 revision_mismatch` instead of overwriting a newer config.

### Reset request

Reset has no JSON body but still requires the active ETag and CSRF token:

```http
DELETE /dashboard/api/v1/config/managed
If-Match: "cfg_..."
X-Vekil-CSRF: <csrf_token>
```

It returns the same `202` receipt shape as apply.

### Apply status and retention

Accepted work normally progresses through:

```text
accepted -> building -> discovering -> preflighting -> encoding
         -> persisting -> publishing -> succeeded
```

Terminal failure states are:

```text
failed_decode
failed_revision
failed_validation
failed_discovery
failed_preflight
failed_encoding
failed_persistence
timed_out
canceled
canceled_shutdown
```

Decode, revision, secret-merge, and offline validation failures are normally returned synchronously before an apply ID is created. `failed_revision` can still occur if the active revision changes before the commit fence.

Each job has a two-minute timeout. Terminal statuses are retained for 15 minutes and the in-memory status store keeps at most 64 entries; an expired or pruned ID returns `404 apply_not_found`. Status objects include timestamps and may include the published generation/revision, a sanitized error, or a durability warning.

Common HTTP outcomes are:

| Status | Meaning |
|---:|---|
| `200` | Read, validation, or status lookup succeeded |
| `202` | Apply/reset accepted |
| `400` | Strict decode, secret operation, or semantic validation failure |
| `401` | Configured inbound authentication failed |
| `403` | Loopback Host, same-origin, or CSRF check failed |
| `404` | Apply status expired or was not found |
| `409` | Another apply/reset is in progress |
| `412` | `ETag`, `If-Match`, body revision, or commit revision is stale |
| `503` | Control plane is unavailable/read-only or the server is shutting down |

## Apply transaction and runtime generations

A successful hot apply is a transaction, not an in-place mutation:

1. The request handler enforces the body limit, strict JSON contract, active base revision, secret operations, preserved fields, URL rules, and offline semantic validation.
2. A lifecycle-owned worker privately builds the candidate provider setup.
3. Dynamic model discovery runs against the candidate.
4. The candidate policy controller is compiled and any required preflight runs against candidate routes.
5. The candidate config is canonically encoded.
6. Under the commit fence, Vekil rechecks shutdown, active-job ownership, the active revision, and the bootstrap source fingerprint.
7. The managed file is written, synced, strictly read back, and atomically replaced.
8. Only after persistence succeeds does Vekil publish one complete runtime snapshot.

The old runtime remains active through steps 1-7. A discovery, preflight, encoding, persistence, timeout, cancellation, or stale-revision failure publishes nothing and leaves both the active runtime and previous managed file unchanged. There is no publish-then-rollback path.

A directory-sync failure after an otherwise successful atomic replacement is reported as a warning, not used to roll the runtime back after persistence has already committed.

### Active readiness isolation

Candidate discovery and preflight do not toggle the active generation's startup/readiness gates. While a candidate is pending or fails before publication:

- current inference traffic continues on the old generation;
- `/dashboard`, the config status endpoint, and `/readyz` continue describing the old generation; and
- candidate caches, policy observations, and discovery results cannot populate the active generation.

After publication, readiness and diagnostics describe the new active snapshot.

### Request and WebSocket pinning

Every admitted HTTP request loads one runtime snapshot at entry and uses it for the entire operation. A request already running on generation G1 may finish on G1 while G2 is built and published; a later request uses G2. Provider setup, route registry, policy binding, and model/Chat discovery caches are generation-owned so a late G1 refresh cannot mutate G2.

A `GET /v1/responses` WebSocket pins one generation for the complete session. It does not switch provider setup or route targets when a later config generation publishes. New WebSocket sessions use the new generation.

`GET /stats.json` exposes the current numeric `config_generation`, and the config read returns both the active generation and optimistic revision.

## Stateful continuation after a config change

Logical route and target IDs are not enough to prove provider state is portable. Vekil derives an internal, opaque **target revision** from the physical and request-semantic identity of a target, including destination, auth identity, endpoint paths, API version/scope, secret-bearing headers through non-reversible fingerprints, trust metadata, upstream model, native endpoint contract, and relevant request policy.

The target revision is process-local, is not an API credential, and is not returned to clients. Provider state bindings, Responses-backed Chat replay records, and WebSocket target pins include it.

Continuation behavior is fail-closed:

- An in-flight G1 operation may complete using its pinned G1 target.
- A later request may reuse process-local state only when the required logical identity and target revision still match.
- Changing a target's destination, credentials/auth source, endpoint semantics, upstream model, secret headers, or relevant request policy invalidates old state for the new generation.
- Unchanged physical targets can continue to use compatible state/replay even when unrelated configuration changes publish a new generation.
- Incompatible explicit Responses state fails locally instead of switching targets.
- Incompatible `call_vekil_...` replay is reported as missing replay state; restart the assistant tool-call turn.
- An existing Responses WebSocket stays on its pinned old generation; Vekil does not migrate it to G2.
- A process restart still loses process-local bindings, replay groups, and WebSocket sessions.

This is exact continuation fencing, not transparent state migration.

## Policy modes during hot apply

The editor and read API display three separate values:

- **Configured mode** — `off`, `observe`, or `enforce` in each policy profile.
- **Process ceiling** — the startup `--policy-routing` / `POLICY_ROUTING_MODE` value. A hot apply cannot raise it.
- **Effective mode** — the lower of the configured mode and process ceiling, after candidate preflight behavior.

The candidate controller is compiled against the staged providers and routes, not the active generation. Effective `off` profiles perform no preflight. An effective `enforce` preflight failure fails the apply with `failed_preflight` and leaves the old generation active. For an observe-only classifier-route failure, the affected candidate profile is kept effectively off with a readiness/configuration diagnostic; the candidate may publish with that effective mode visible in the next config read.

Because writes are supported only on loopback, the editor cannot activate policy routing on a non-loopback server. The separate `--policy-routing-allow-remote-single-tenant` acknowledgement applies to startup/file-based non-loopback deployments and does not create a remote config control plane.

## Containers and read-only filesystems

The original provider file may remain mounted read-only because the editor never modifies it. Managed writes require a separate writable user configuration directory. If Vekil cannot resolve or write that directory, a supported loopback server continues from the bootstrap/effective startup config but reports the editor as read-only.

The official container and Kubernetes examples bind to `0.0.0.0`, so the config API is capability-only even when a writable config volume is mounted. Current container operations should edit the bootstrap configuration through the deployment system and restart/roll out the workload.

If a persisted managed override conflicts with a newly mounted bootstrap:

- start once with `--ignore-managed` to inspect the new bootstrap without deleting the override;
- use `--reset-managed` only when the managed directory is writable; or
- remove the source-scoped managed file through the volume/storage administrator.

Do not make the bootstrap mount writable merely to enable dashboard editing, and do not expose `/dashboard/api/v1/config` through an ingress as an assumed remote admin API.

## Related documentation

- [Traffic Dashboard](dashboard.md)
- [Configuration](configuration.md)
- [Provider Routing and Authentication](provider-routing.md)
- [Semantic Policy Routing](policy-routing.md)
- [Provider API Keys](provider-api-keys.md)
- [API Reference](api.md)
- [Architecture](architecture.md)
