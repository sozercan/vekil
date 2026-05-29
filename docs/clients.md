# Client Usage Examples

These examples all target the same local proxy. Replace model IDs with public IDs from `/v1/models` in your deployment; client setup does not need to change when a model is backed by GitHub Copilot, Azure OpenAI, OpenAI Codex, or a generic compatible provider.

## Claude Code

```bash
env ANTHROPIC_BASE_URL=http://localhost:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-4 --print --output-format text "Reply with exactly PROXY_OK"
```

## OpenAI Codex CLI

```bash
env OPENAI_API_KEY=dummy \
  OPENAI_BASE_URL=http://localhost:1337/v1 \
  codex exec --skip-git-repo-check -m gpt-5.5 "Reply with exactly PROXY_OK"
```

## GitHub Copilot CLI

```bash
env COPILOT_PROVIDER_BASE_URL=http://localhost:1337/v1 \
  COPILOT_PROVIDER_TYPE=openai \
  COPILOT_PROVIDER_WIRE_API=responses \
  COPILOT_MODEL=gpt-5.5 \
  COPILOT_OFFLINE=true \
  copilot -p "Reply with exactly PROXY_OK" -s
```

## Gemini CLI

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

## Anthropic Messages API

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

## Anthropic Messages API (Streaming)

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

## OpenAI Chat Completions API

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

## OpenAI Responses API

```bash
curl http://localhost:1337/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "input": "Hello, world!"
  }'
```

## Gemini Generate Content API

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

## Gemini Stream Generate Content API

```bash
curl -N http://localhost:1337/v1/models/gemini-2.5-pro:streamGenerateContent \
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

`-N` disables curl buffering so SSE chunks print as they arrive. The proxy accepts Gemini routes under `/v1beta/models`, `/v1/models`, and `/models`.

## Gemini Count Tokens API

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
