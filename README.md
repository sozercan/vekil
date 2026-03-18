# copilot-proxy

High-performance Go proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs, forwarding all requests to GitHub Copilot's backend (`api.githubcopilot.com`). This lets you use tools that speak the Anthropic, Gemini, or OpenAI protocol — such as [Claude Code](https://docs.anthropic.com/en/docs/claude-code), Gemini-compatible SDKs, the OpenAI Python SDK, or `curl` — with your GitHub Copilot subscription.

## Features

- **Anthropic Messages API** (`POST /v1/messages`) — request/response translation for supported Anthropic text, image, and tool formats
- **Gemini Generate Content API** (`POST /v1beta/models/{model}:generateContent`, `POST /models/{model}:generateContent`) — Gemini request/response/SSE translation on top of Copilot chat completions
- **Gemini Count Tokens API** (`POST /v1beta/models/{model}:countTokens`, `POST /models/{model}:countTokens`) — translated token counting via a minimal upstream probe with short-lived caching
- **OpenAI Chat Completions API** (`POST /v1/chat/completions`) — near zero-copy passthrough (tools-aware, see below)
- **OpenAI Responses API** (`POST /v1/responses`) — near zero-copy passthrough
- **SSE streaming** support for all endpoints
- **Tool use** — Anthropic and Gemini function/tool definitions are translated to OpenAI function calling format
- **Parallel tool use** — reliable parallel tool calls on Anthropic, Gemini, and OpenAI paths via forced-streaming aggregation
- **Extended thinking** — Anthropic `thinking` parameter is mapped to `max_completion_tokens`
- **GitHub OAuth device code flow** with automatic token caching and refresh
- **Automatic retry** with exponential backoff on transient upstream errors (429, 502, 503, 504)
- **Compressed request bodies** — transparent decompression of `gzip` and `zstd` Content-Encoding (used by Codex CLI)
- **Structured JSON logging** with configurable log levels
- **Connection pooling** and HTTP/2 support
- **Single static binary**, zero runtime dependencies (distroless Docker image)

## Quick Start

### Build from source

```bash
go build -o copilot-proxy .
./copilot-proxy
```

### Docker

```bash
docker pull docker.io/sozercan/copilot-proxy:latest
docker run -p 1337:1337 -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy sozercan/copilot-proxy:latest
```

Or build from source:

```bash
docker build -t copilot-proxy .
docker run -p 1337:1337 -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy copilot-proxy
```

### Kubernetes

A sample deployment manifest is included in `k8s/copilot-proxy.yaml`:

```bash
kubectl apply -f k8s/copilot-proxy.yaml
```

The image is available at `docker.io/sozercan/copilot-proxy` with multi-arch support (linux/amd64, linux/arm64).

### First Run

On first run, you'll be prompted to authenticate via GitHub's device code flow:

1. Visit the URL shown in the terminal
2. Enter the one-time code displayed
3. Authorize the application

Tokens are cached to `~/.config/copilot-proxy/` and automatically refreshed before expiry.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `0.0.0.0` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/copilot-proxy` | Token storage directory |
| `--log-level` | `LOG_LEVEL` | `info` | Log level (`debug`, `info`, or `error`) |
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `User-Agent` header |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-04-01` | Upstream `x-github-api-version` header |

## Usage Examples

### Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:1337
# Claude Code will use the /v1/messages endpoint automatically
```

### OpenAI Codex CLI

```bash
export OPENAI_BASE_URL=http://localhost:1337/v1
codex --model gpt-5.4
```

### Gemini CLI

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

### Anthropic Messages API

```bash
curl http://localhost:1337/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

### Anthropic Messages API (streaming)

```bash
curl http://localhost:1337/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "stream": true,
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

### OpenAI Chat Completions API

```bash
curl http://localhost:1337/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

### Gemini Generate Content API

```bash
curl http://localhost:1337/v1beta/models/gemini-2.5-pro:generateContent \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Hello, world!"}]
      }
    ]
  }'
```

### Gemini Stream Generate Content API

```bash
curl http://localhost:1337/v1beta/models/gemini-2.5-pro:streamGenerateContent \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Stream a short answer"}]
      }
    ]
  }'
```

### Gemini Count Tokens API

```bash
curl http://localhost:1337/v1beta/models/gemini-2.5-pro:countTokens \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Count these tokens"}]
      }
    ]
  }'
```

### OpenAI Responses API

```bash
curl http://localhost:1337/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "input": "Hello, world!"
  }'
```

## API Endpoints

### `POST /v1/messages` (Anthropic)

Anthropic Messages API compatibility for the supported content and tool subset. Incoming requests are translated to OpenAI Chat Completions format, forwarded to GitHub Copilot, and responses are translated back to Anthropic format.

**Supported features:**
- Text, image-input, and tool-use content blocks
- System messages (string or content block array)
- Tool definitions and tool choice (`auto`, `any`, `tool`)
- Stop sequences
- Extended thinking (`thinking.type: "enabled"`)
- Streaming with proper Anthropic SSE event translation (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`)

**Model name normalization:**
Dated model suffixes (e.g., `claude-sonnet-4-20250514`) are stripped automatically. Hyphenated version numbers (e.g., `claude-sonnet-4-5`) are mapped to dotted form (`claude-sonnet-4.5`).

**Available models (`GET /v1/models`):**

The list of available models is fetched dynamically from the upstream GitHub Copilot API. Query the endpoint to see all currently available models:

```bash
curl http://localhost:1337/v1/models
```

<details>
<summary>Known models</summary>

| Provider  | Model name             |
| --------- | ---------------------- |
| OpenAI    | `gpt-4o`               |
| OpenAI    | `gpt-4.1`             |
| OpenAI    | `gpt-5-mini`           |
| OpenAI    | `gpt-5.1`             |
| OpenAI    | `gpt-5.1-codex`       |
| OpenAI    | `gpt-5.1-codex-mini`  |
| OpenAI    | `gpt-5.1-codex-max`   |
| OpenAI    | `gpt-5.2`             |
| OpenAI    | `gpt-5.2-codex`       |
| OpenAI    | `gpt-5.3-codex`       |
| OpenAI    | `gpt-5.4`             |
| Anthropic | `claude-haiku-4.5`     |
| Anthropic | `claude-sonnet-4`      |
| Anthropic | `claude-sonnet-4.5`    |
| Anthropic | `claude-sonnet-4.6`    |
| Anthropic | `claude-opus-4.5`      |
| Anthropic | `claude-opus-4.6`      |
| Anthropic | `claude-opus-4.6-1m`   |
| Google    | `gemini-2.5-pro`       |
| Google    | `gemini-3-pro-preview` |
| Google    | `gemini-3-flash-preview` |
| Google    | `gemini-3.1-pro-preview` |

</details>

### `POST /v1beta/models/{model}:generateContent` and `POST /models/{model}:generateContent` (Gemini)

Gemini is implemented as a translation path like Anthropic, not a passthrough path like OpenAI. Gemini requests are translated into OpenAI Chat Completions, forwarded to GitHub Copilot, and translated back into Gemini responses. When function declarations are present, non-streaming `generateContent` requests force upstream streaming and aggregate the result back into a normal Gemini JSON response so parallel function calls remain reliable.

The request decoder accepts both the standard Gemini camelCase envelope fields and LiteLLM-style snake_case aliases such as `system_instruction`, `function_declarations`, `inline_data`, `max_output_tokens`, and `response_json_schema`.

**Supported subset:**
- `systemInstruction.parts[].text`
- `contents[].parts[].text`
- `contents[].parts[].inlineData` for `image/*` payloads
- `contents[].parts[].functionCall`
- `contents[].parts[].functionResponse`
- `tools[].functionDeclarations` with either `parameters` or `parametersJsonSchema`
- `toolConfig.functionCallingConfig`
- `generationConfig.temperature`
- `generationConfig.topP`
- `generationConfig.maxOutputTokens`
- `generationConfig.stopSequences`
- `generationConfig.responseMimeType`
- `generationConfig.responseSchema`
- `generationConfig.responseJsonSchema`
- `generationConfig.topK` is accepted but ignored because Copilot/OpenAI has no equivalent control
- `generationConfig.thinkingConfig` with `includeThoughts`, `thinkingBudget`, and `thinkingLevel` is accepted but ignored because Copilot/OpenAI has no equivalent thinking control
- `generationConfig.candidateCount` only when it is `1`
- `generationConfig.presencePenalty`
- `generationConfig.frequencyPenalty`
- `generationConfig.seed`

**Streaming behavior:**
- `streamGenerateContent` always returns Gemini-style data-only SSE (`data: {...}\n\n`)
- Partial OpenAI tool-call chunks are buffered until the arguments become valid JSON, then emitted as Gemini `functionCall` parts
- Final streaming chunks include Gemini `finishReason` and `usageMetadata`

**Explicit `501 UNIMPLEMENTED` cases:**
- `generationConfig.candidateCount != 1`
- `generationConfig.responseModalities`
- `generationConfig.speechConfig`
- `generationConfig.imageConfig`
- `generationConfig.mediaResolution`
- `generationConfig.responseLogprobs`
- `generationConfig.logprobs`
- `cachedContent`
- `safetySettings`
- `functionResponse.parts` and other multimodal tool-response payloads
- Gemini built-in tools such as `googleSearch`, `googleSearchRetrieval`, `urlContext`, `codeExecution`, `googleMaps`, `computerUse`, and `enterpriseWebSearch`
- Non-image `inlineData`, `fileData`, and other non-text/media parts

**Validation failures (`400 INVALID_ARGUMENT`):**
- Path/body model mismatches
- Malformed content parts
- Invalid function-call history
- Unmatched `functionResponse` parts

### `POST /v1beta/models/{model}:countTokens` and `POST /models/{model}:countTokens` (Gemini)

`countTokens` translates the Gemini request into the same normalized OpenAI-style prompt/tool payload used by `generateContent`, then performs a minimal upstream `/chat/completions` probe with `stream=false`, `temperature=0`, and `max_completion_tokens=1` (falling back to `max_tokens=1` when needed). The proxy returns `usage.prompt_tokens` as Gemini `totalTokens` and caches normalized requests for 60 seconds.

### `POST /v1/chat/completions` (OpenAI)

Near zero-copy passthrough for requests without tools. When tools are present, the proxy injects `parallel_tool_calls: true` and forces streaming to the upstream for reliable parallel tool call support, then aggregates the response back to non-streaming JSON for the client. Streaming requests with tools are passed through as-is (parallel tool calls work natively in streaming mode).

### `POST /v1/responses` (OpenAI)

Near zero-copy passthrough for the OpenAI Responses API. Only authentication headers are injected.

### `GET /v1/models`

Proxies the upstream GitHub Copilot models endpoint, returning all available models dynamically. No hardcoded model list — new models are available as soon as Copilot adds them.

### `GET /healthz`

Health check endpoint. Returns `{"status":"ok"}`.

### `GET /readyz`

Readiness endpoint. Validates that the proxy can obtain a Copilot token and successfully probe the upstream Copilot `/models` API. Returns `{"status":"ready"}` on success or `503` with `{"status":"not_ready","error":"..."}` on failure.

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   copilot-proxy                      │
│                                                      │
│  /v1/messages ──► Translate ──► /chat/completions    │
│                   Anthropic→OAI   api.githubcopilot  │
│                   Translate ◄──   .com               │
│                   OAI→Anthropic                      │
│                                                      │
│  /v1beta/models/... ─► Translate ─► /chat/completions│
│  /models/...          Gemini→OAI     api.githubcopilot│
│                       Translate ◄──   .com            │
│                       OAI→Gemini                      │
│                                                      │
│  /v1/chat/completions ─────────► /chat/completions   │
│                   (passthrough; forced-stream         │
│                    + aggregate when tools present)    │
│  /v1/responses ────────────────► /responses          │
│                   (passthrough)                       │
│                                                      │
│  auth/ ── Device code flow ── Token cache & refresh  │
└─────────────────────────────────────────────────────┘
```

**Packages:**

| Package | Responsibility |
|---------|---------------|
| `main` | CLI flags, HTTP server setup, graceful shutdown |
| `auth/` | GitHub OAuth device code flow, Copilot token exchange, disk caching, auto-refresh with `sync.RWMutex` |
| `proxy/` | HTTP handlers, Anthropic↔OpenAI and Gemini↔OpenAI translation, SSE streaming, retry with backoff |
| `models/` | Request/response type definitions for all supported APIs (data-only, no logic) |
| `logger/` | Structured JSON logging with level filtering |
| `server/` | Reusable HTTP server lifecycle (Start/Stop/IsRunning) |
| `cmd/menubar/` | macOS menubar app using systray |

## macOS Menubar App

A native macOS menubar app that lets you start/stop the proxy without a terminal.

### Build & Run

```bash
make build-app
open "Copilot Proxy.app"
```

### Features

- **Start/Stop toggle** — click the menu item to start or stop the proxy
- **Status icon** — white robot when running, gray when stopped
- **Launch at Login** — optional macOS LaunchAgent integration
- Hover the icon for status tooltip (running/stopped, port)

## Development

```bash
# Build
go build -o copilot-proxy .

# Build menubar app
make build-app

# Run all tests
go test ./... -count=1

# Run specific tests
go test ./proxy/ -run TestHandle -v
go test ./proxy/ -run TestMapStopReason/stop -v

# Lint
go vet ./...
```

## License

MIT
