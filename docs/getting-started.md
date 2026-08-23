# Getting Started

Vekil commonly runs in one of two modes:

- **Zero-config mode**: no `--providers-config`; uses the built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` to expose any mix of `copilot`, `azure-openai`, `openai-codex`, `openai-compatible`, and `anthropic-compatible` providers behind the same local API surface.

Schema version 2 is the complete explicit-routing format: it supports public and internal routes, ordered failover, and optional semantic policy profiles. Existing route-only version-2 files remain valid. Policy routing follows each profile's YAML `mode` by default, while an explicit process mode can lower it. Profiles use a text/function-tool canonical Chat contract, support translated Anthropic and bounded stateless Responses ingress, and support one trusted user/tenant per deployment. See [Semantic Policy Routing](policy-routing.md).

With either routing mode, the native binary also supports **managed agent launches**: `vekil launch` starts a short-lived proxy and a supported coding agent together. Claude Code and Codex CLI may delegate model selection to their normal CLI default or be pinned to one public model; GitHub Copilot CLI requires a pinned model.

## Install Or Build

Download a binary from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest). Release binaries are published for Linux, macOS, and Windows on `amd64` and `arm64`.

On Apple Silicon Macs, you can also install the tray app:

```bash
brew install --cask sozercan/repo/vekil
xattr -cr /Applications/Vekil.app  # only if macOS quarantine blocks launch
```

The currently published cask/ZIP contains the Go tray shell, is `arm64`-only, and is ad-hoc signed rather than Developer ID signed. This source tree now contains and assembles the macOS 13 AppKit/SwiftUI shell, universal bundled helper, External Configuration and managed-config control plane, login migration, Keychain support, analytics, and build-once release tooling. The native candidate is not a production release until Developer ID signing/notarization, real N-1 update continuity, exact-artifact architecture/Homebrew checks, and forward-revert gates in [Development](development.md#native-macos-shell-and-release-gates) pass. Older installations must remain on a compatible release rather than receiving an incompatible update.

For current release behavior and the native source/release boundary, see [macOS App and Linux Tray](menubar.md).

Build from source:

```bash
go build -o vekil .
./vekil
```

## Launch A Coding Agent

Use a managed launcher when you want Vekil to configure and supervise the
client for a single session instead of maintaining client-specific environment
variables:

```bash
vekil launch claude
vekil launch codex
vekil launch copilot --model gpt-5.4-mini
```

For explicit routing, pass the same provider file used by the server:

```bash
vekil launch codex \
  --providers-config /path/to/providers.yaml \
  --model my-responses-model -- \
  exec --ephemeral "Reply with exactly OK"
```

The launcher binds an ephemeral loopback proxy, authenticates the child with a
random session token, removes upstream credentials from the child environment,
and shuts down with the agent. Supplying `--model` validates compatibility at
startup, restricts requests to that model, and pins the agent to it. Omitting
`--model` for Claude Code or Codex CLI leaves model selection to the CLI's
configured or built-in default and exposes the proxy's normal global public
model namespace. Dry-run reports that delegated choice as `CLI default`; for a
pinned model, it resolves statically known routing while catalog-discovered
metadata is clearly marked unresolved.

Agent executables are not included in the distroless container image, so this
workflow is intended for the native Vekil binary. See [Agent
Launchers](agent-launchers.md) for supported versions, required model endpoints,
argument forwarding, logs, and process-containment details.

## Docker

Base run:

```bash
mkdir -p ~/.config/vekil
docker run --user "$(id -u):$(id -g)" -e HOME=/home/nonroot -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

The image sets `HOST=0.0.0.0` so published Docker ports work. Native binary and tray-app runs default to `127.0.0.1`; bind to `0.0.0.0` only when you intentionally need network access.

With explicit provider routing:

```bash
mkdir -p ~/.config/vekil
docker run --user "$(id -u):$(id -g)" -e HOME=/home/nonroot -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/providers.yaml:/config/providers.yaml:ro \
  ghcr.io/sozercan/vekil:latest \
  --providers-config /config/providers.yaml
```

Validate a config before serving it. Ordinary validation is offline; `--live` additionally performs the fixed classifier protocol preflight required before policy observe/enforce:

```bash
vekil config validate --providers-config /path/to/providers.yaml
vekil config validate --live --providers-config /path/to/providers.yaml
```

The published container binds to `0.0.0.0`. A schema-v2 policy profile in effective `observe` or `enforce` therefore also requires `--policy-routing-allow-remote-single-tenant` (or `POLICY_ROUTING_ALLOW_REMOTE_SINGLE_TENANT=true`). This is only an acknowledgement: it does not add authentication, authorize multiple tenants, or make a published port safe. Put the deployment behind a trusted external access layer.

If the config includes `type: "openai-codex"`, also mount the Codex home read-write so Vekil can journal and persist rotated refresh tokens back to the Codex-owned `auth.json`. The `--user` setting above is required for normal host files/directories owned by your UID/GID; alternatively, arrange equivalent ownership and permissions explicitly:

```bash
-v ~/.codex:/home/nonroot/.codex
```

If you intentionally mount it read-only, fresh access tokens work until they become stale, but Vekil cannot refresh them; run `codex login` on the host and restart the container. If you customize `CODEX_HOME`, set the container-side path and mount your host directory there. The published image supports `linux/amd64` and `linux/arm64`.

### RTK image variant

Use the `-rtk` image variant when your providers config enables the optional [`rtk_cli` tool optimizer](tool-optimizers.md#rtk_cli-provider). The default image stays minimal; the RTK variant only adds the `rtk` binary and does not enable tool optimizers by itself.

```bash
mkdir -p ~/.config/vekil
docker run --user "$(id -u):$(id -g)" -e HOME=/home/nonroot -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/providers.yaml:/config/providers.yaml:ro \
  ghcr.io/sozercan/vekil:latest-rtk \
  --providers-config /config/providers.yaml
```

Inside the variant, set the optimizer path to `/usr/local/bin/rtk` for explicitness.

Build a local image:

```bash
docker build -t vekil .
# Optional RTK variant:
# docker build -f Dockerfile.rtk -t vekil:rtk .
mkdir -p ~/.config/vekil
docker run --user "$(id -u):$(id -g)" -e HOME=/home/nonroot -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  vekil
```

## Kubernetes

A sample manifest is included at [`k8s/vekil.yaml`](../k8s/vekil.yaml).

```bash
kubectl apply -f k8s/vekil.yaml
```

The manifest first uses `/healthz` as a startup probe with a 90-second failure budget. This covers synchronous dynamic-provider model initialization, which can take up to 30 seconds before the HTTP listener exists; Kubernetes suppresses liveness and readiness checks until startup succeeds. Afterward, `/healthz` provides liveness and `/readyz` provides readiness. The readiness probe allows 12 seconds, leaving margin over Vekil's 10-second readiness checks, and runs every 15 seconds with a single failure threshold. With the default zero-config Copilot startup, the process can therefore stay live while device-code authentication is pending, but the Pod remains not Ready and the Service has no ready endpoint until authentication succeeds. Static Azure and generic compatible providers are not actively probed by `/readyz`; for routes composed only of those providers, readiness confirms compiled configuration, locally available startup credentials, admission state, and shutdown state—not target reachability or credential acceptance.

The example token cache is an `emptyDir`. It survives a container restart inside the same Pod, but **does not survive Pod replacement, rescheduling, or a Deployment rollout**. For a durable deployment, either:

- inject `COPILOT_GITHUB_TOKEN` from a Kubernetes Secret so every replacement can authenticate non-interactively, or
- replace the `emptyDir` with persistent storage if you rely on Vekil-managed cached credentials.

Explicit provider routing can mount a JSON/YAML config from a Secret or ConfigMap and pass `--providers-config /path/in/container/providers.yaml`, or load an HTTP(S) URL directly. A remote source makes startup depend on that endpoint and is fetched only at process start, so use HTTPS and restart the workload to apply changes. Any `api_key_env` or other credential environment variables referenced by the config must be supplied separately, normally from Secrets. Do not assume the example `emptyDir` persists either provider credentials or configuration. Provider-state bindings and Responses-backed Chat replay state are memory-only: use one replica or session affinity to the same Pod for stateful explicit Responses traffic, and drain those continuations before rolling or replacing Pods.

Policy v1 has a separate deployment restriction: one trusted user/tenant per Vekil process/deployment, no downstream identity or tenant quotas, and no policy affinity/shared state. A non-loopback Pod requires the explicit remote-single-tenant acknowledgement plus external authentication/network isolation. Use stateless Chat traffic canaries for policy rollout; keep direct stateful Responses/websocket traffic on its existing sticky topology.

## First Run And Authentication

Startup behavior depends on active providers. For the full auth matrix, see [Provider Authentication](provider-routing.md#provider-authentication). For provider console links and key setup patterns, see [Provider API Keys](provider-api-keys.md).

### Native macOS source build

The implemented native candidate opens a setup assistant on a true first run. Its provider gallery covers GitHub Copilot, OpenAI Codex, Azure OpenAI, OpenAI-compatible, Anthropic-compatible, and multi-provider imports. Copilot has a direct zero-config sign-in path; the other choices select an existing JSON/YAML provider configuration, which can contain several providers and remains user-owned. The runtime validates the selected file, discovers the combined model catalog, rejects global public-model-ID collisions, verifies the local endpoint, and then offers temporary client commands. Managed provider-key entry stays unavailable until the native release's cross-version Keychain gate is satisfied.

The app distinguishes a fresh installation from an upgrade by inspecting recovered authentication, configuration, service, window/navigation, and startup-preference state. Existing users are not forced through setup merely because the native onboarding preference is absent. **Skip Setup** suppresses the current assistant version, and **Settings > Run Setup Assistant…** opens it again on demand without first changing the active proxy.

This flow describes the native source build, not the currently published Go tray bundle. See [macOS App and Linux Tray](menubar.md#implementation-and-release-status) for the release boundary and [Client Usage Examples](clients.md#native-macos-client-setup) for the native copyable-command surface.

### GitHub Copilot

Zero-config startup and explicit `type: "copilot"` providers need GitHub Copilot auth. The proxy checks, in order:

1. explicit `COPILOT_GITHUB_TOKEN`
2. Vekil-managed cached credentials in `~/.config/vekil/`
3. GitHub CLI auth, but only after `vekil login --github-cli` or `vekil login --gh`

If none are available, first startup starts GitHub's device-code flow after binding the HTTP listener. During that one-time login window, `/healthz` is available for liveness probes and `/readyz` remains `not_ready` until authentication and upstream probing succeed. You can also run `vekil login` ahead of time, `vekil login --force` for a fresh device-code sign-in, or `vekil logout` to clear Vekil-managed auth and disable silent GitHub CLI reuse.

### Azure OpenAI and Microsoft Foundry

Configure Azure credentials in the provider entry. Use `api_key`/`api_key_env` for key auth, or set `auth_mode: azure_identity` to use Microsoft Entra auth through the Azure SDK `DefaultAzureCredential` chain. Vekil does not run `az login` itself; for local SDK auth, sign in with Azure CLI or another supported Azure credential before starting the proxy.

### OpenAI Codex

Run `codex login` first so `~/.codex/auth.json` exists. Set `CODEX_HOME` if your Codex home lives elsewhere. API-key auth and OS keychain-backed credentials are not read by the proxy.

### Generic Compatible Providers

Generic `openai-compatible` and `anthropic-compatible` providers use the auth fields in the providers config. Use `auth_type: none` for local services, or set `api_key_env` with `auth_type: bearer` or `auth_type: api-key-header` for hosted services. There is no interactive login flow.

If your provider config omits Copilot, startup skips GitHub authentication entirely.

For an effective schema-v2 policy `observe` or `enforce` profile, startup also performs one live fixed-fixture preflight per distinct classifier route. Enforce preflight failure prevents startup/readiness completion. Observe preflight failure keeps that profile off and reports a readiness/configuration diagnostic. Effective off mode sends no classifier preflight.

## Verify The Proxy Is Up

```bash
curl http://localhost:1337/healthz
curl http://localhost:1337/readyz
curl http://localhost:1337/v1/models
```

- `/healthz` confirms the process is serving HTTP.
- `/readyz` verifies compiled configuration, required startup auth/catalog initialization where supported, admission state, and shutdown state. It is not a health probe for static Azure or generic route targets.
- `/v1/models` shows the merged public model catalog clients will see.
