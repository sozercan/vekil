# Client Usage Examples

## Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:1337
# Claude Code will use the /v1/messages endpoint automatically
```

## OpenAI Codex CLI

```bash
export OPENAI_BASE_URL=http://localhost:1337/v1
codex --model gpt-5.4
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
