# copilot-proxy

High-performance Go proxy that exposes Anthropic and OpenAI-compatible APIs, forwarding all requests to GitHub Copilot's backend (`api.githubcopilot.com`). This lets you use any tool that speaks the Anthropic or OpenAI protocol — such as [Claude Code](https://docs.anthropic.com/en/docs/claude-code), the OpenAI Python SDK, or `curl` — with your GitHub Copilot subscription.

## Features

- **Anthropic Messages API** (`POST /v1/messages`) — full request/response translation between Anthropic and OpenAI formats
- **OpenAI Chat Completions API** (`POST /v1/chat/completions`) — near zero-copy passthrough
- **OpenAI Responses API** (`POST /v1/responses`) — near zero-copy passthrough
- **SSE streaming** support for all endpoints
- **Tool use** — Anthropic tool definitions are translated to OpenAI function calling format
- **Extended thinking** — Anthropic `thinking` parameter is mapped to `max_completion_tokens`
- **GitHub OAuth device code flow** with automatic token caching and refresh
- **Automatic retry** with exponential backoff on transient upstream errors (429, 502, 503, 504)
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
docker build -t copilot-proxy .
docker run -p 8080:8080 -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy copilot-proxy
```

### First Run

On first run, you'll be prompted to authenticate via GitHub's device code flow:

1. Visit the URL shown in the terminal
2. Enter the one-time code displayed
3. Authorize the application

Tokens are cached to `~/.config/copilot-proxy/` and automatically refreshed before expiry.

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `8080` | Listen port |
| `--host` | `HOST` | `0.0.0.0` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/copilot-proxy` | Token storage directory |
| `--log-level` | `LOG_LEVEL` | `info` | Log level (`debug`, `info`, or `error`) |

## Usage Examples

### Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
# Claude Code will use the /v1/messages endpoint automatically
```

### Anthropic Messages API

```bash
curl http://localhost:8080/v1/messages \
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
curl http://localhost:8080/v1/messages \
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
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

### OpenAI Responses API

```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "input": "Hello, world!"
  }'
```

## API Endpoints

### `POST /v1/messages` (Anthropic)

Full Anthropic Messages API compatibility. Incoming requests are translated to OpenAI Chat Completions format, forwarded to GitHub Copilot, and responses are translated back to Anthropic format.

**Supported features:**
- Text and tool-use content blocks
- System messages (string or content block array)
- Tool definitions and tool choice (`auto`, `any`, `tool`)
- Stop sequences
- Extended thinking (`thinking.type: "enabled"`)
- Streaming with proper Anthropic SSE event translation (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`)

**Model name normalization:**
Dated model suffixes (e.g., `claude-sonnet-4-20250514`) are stripped automatically. Hyphenated version numbers (e.g., `claude-sonnet-4-5`) are mapped to dotted form (`claude-sonnet-4.5`).

**Available model names (`GET /v1/models`):**

| Provider  | Model name             |
| --------- | ---------------------- |
| OpenAI    | `gpt-4o`               |
| OpenAI    | `gpt-4o-mini`          |
| OpenAI    | `gpt-4.1`              |
| OpenAI    | `gpt-4.1-mini`         |
| OpenAI    | `gpt-4.1-nano`         |
| OpenAI    | `gpt-5.3-codex`        |
| OpenAI    | `o1`                   |
| OpenAI    | `o1-mini`              |
| OpenAI    | `o1-preview`           |
| OpenAI    | `o3`                   |
| OpenAI    | `o3-mini`              |
| OpenAI    | `o4-mini`              |
| Anthropic | `claude-3.5-sonnet`    |
| Anthropic | `claude-sonnet-4`      |
| Anthropic | `claude-sonnet-4.5`    |
| Anthropic | `claude-haiku-4.5`     |
| Anthropic | `claude-opus-4`        |
| Anthropic | `claude-opus-4.5`      |
| Anthropic | `claude-sonnet-4.6`    |
| Anthropic | `claude-opus-4.6`      |
| Anthropic | `claude-opus-4.6-fast` |

### `POST /v1/chat/completions` (OpenAI)

Near zero-copy passthrough. Only authentication headers are injected; request and response bodies are streamed through untouched.

### `POST /v1/responses` (OpenAI)

Near zero-copy passthrough for the OpenAI Responses API. Only authentication headers are injected.

### `GET /v1/models`

Returns a list of available models in OpenAI-compatible format. Useful for clients that enumerate models before making requests.

### `GET /healthz`

Health check endpoint. Returns `{"status":"ok"}`.

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
│  /v1/chat/completions ─────────► /chat/completions   │
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
| `proxy/` | HTTP handlers, Anthropic↔OpenAI translation, SSE streaming, retry with backoff |
| `models/` | Request/response type definitions for both APIs (data-only, no logic) |
| `logger/` | Structured JSON logging with level filtering |
| `server/` | Reusable HTTP server lifecycle (Start/Stop/IsRunning) |
| `cmd/menubar/` | macOS menubar app using systray |

## macOS Menubar App

A native macOS menubar app that lets you start/stop the proxy without a terminal.

### Build & Run

```bash
make build-menubar
./copilot-proxy-menubar
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
make build-menubar

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
