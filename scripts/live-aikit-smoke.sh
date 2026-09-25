#!/usr/bin/env bash

# Live AIKit smoke: runs a small real model through Vekil end to end.
#
# 1. The `vekil` server with a `type: aikit` provider on a pre-made image checks
#    Chat, Anthropic Messages, and Responses (including LocalAI dialect
#    normalization and context-overflow mapping), then verifies the container
#    is removed on shutdown.
# 2. `vekil launch claude --model aikit:<runner reference>` with a stub agent
#    checks the launcher lifecycle, the agent's routing environment, and a real
#    request through the launched proxy, without installing Claude Code.
#
# Needs no credentials. Requires Docker or podman, curl, and python3.
#
# Usage:
#   make build
#   scripts/live-aikit-smoke.sh
#
# Overrides (env): VEKIL_BIN, AIKIT_RUNTIME (auto|docker|podman),
# AIKIT_IMAGE_MODEL, AIKIT_RUNNER_MODEL, LIVE_AIKIT_SMOKE_DIR,
# AIKIT_LOAD_TIMEOUT.

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VEKIL_BIN="${VEKIL_BIN:-${REPO_ROOT}/vekil}"
AIKIT_RUNTIME="${AIKIT_RUNTIME:-auto}"
AIKIT_IMAGE_MODEL="${AIKIT_IMAGE_MODEL:-llama3.2:1b}"
AIKIT_RUNNER_MODEL="${AIKIT_RUNNER_MODEL:-hf.co/MaziyarPanahi/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct.Q4_K_M.gguf}"
AIKIT_LOAD_TIMEOUT="${AIKIT_LOAD_TIMEOUT:-20m}"
SMOKE_DIR="${LIVE_AIKIT_SMOKE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/live-aikit-smoke.XXXXXX")}"
READY_TIMEOUT_SECONDS="${READY_TIMEOUT_SECONDS:-1500}"

for cmd in curl python3; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
done
[ -x "$VEKIL_BIN" ] || die "vekil binary not found at $VEKIL_BIN; run make build"
mkdir -p "$SMOKE_DIR"

SERVE_PID=""
LAUNCH_PID=""

# owned_containers lists AIKit containers created by the given vekil PID on
# this host, across whichever engines are installed.
owned_containers() {
  local pid="$1" engine
  for engine in docker podman; do
    command -v "$engine" >/dev/null 2>&1 || continue
    "$engine" ps -a -q --filter "label=dev.vekil.aikit.owner=$(hostname)/${pid}" 2>/dev/null | sed "s|^|${engine} |" || true
  done
}

remove_owned_containers() {
  local pid="$1" engine id
  [ -n "$pid" ] || return 0
  owned_containers "$pid" | while read -r engine id; do
    [ -n "$id" ] && "$engine" rm -f "$id" >/dev/null 2>&1 || true
  done
}

cleanup() {
  if [ -n "$SERVE_PID" ] && kill -0 "$SERVE_PID" 2>/dev/null; then
    kill -INT "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
  fi
  if [ -n "$LAUNCH_PID" ] && kill -0 "$LAUNCH_PID" 2>/dev/null; then
    kill -INT "$LAUNCH_PID" 2>/dev/null || true
    wait "$LAUNCH_PID" 2>/dev/null || true
  fi
  remove_owned_containers "$SERVE_PID"
  remove_owned_containers "$LAUNCH_PID"
}
trap cleanup EXIT

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

# ---------------------------------------------------------------------------
# Scenario 1: the vekil server with a type: aikit provider.
# ---------------------------------------------------------------------------

CONFIG="$SMOKE_DIR/providers.yaml"
cat >"$CONFIG" <<EOF
providers:
  - id: local
    type: aikit
    default: true
    aikit:
      model: ${AIKIT_IMAGE_MODEL}
      runtime: ${AIKIT_RUNTIME}
      load_timeout: ${AIKIT_LOAD_TIMEOUT}
    models:
      - public_id: smoke-model
EOF
"$VEKIL_BIN" config validate --providers-config "$CONFIG"

PORT="$(free_port)"
BASE="http://127.0.0.1:${PORT}"
log "starting vekil server on ${BASE} with aikit:${AIKIT_IMAGE_MODEL}"
PROVIDERS_CONFIG="$CONFIG" "$VEKIL_BIN" --host 127.0.0.1 --port "$PORT" >"$SMOKE_DIR/serve.log" 2>&1 &
SERVE_PID=$!

deadline=$((SECONDS + READY_TIMEOUT_SECONDS))
until curl -fsS --max-time 5 "$BASE/readyz" >/dev/null 2>&1; do
  kill -0 "$SERVE_PID" 2>/dev/null || die "vekil server exited before becoming ready (see $SMOKE_DIR/serve.log)"
  [ "$SECONDS" -lt "$deadline" ] || die "vekil server was not ready within ${READY_TIMEOUT_SECONDS}s"
  sleep 3
done
log "vekil server is ready"

python3 - "$BASE" <<'PY'
import json
import sys
import urllib.error
import urllib.request

BASE = sys.argv[1]
MODEL = "smoke-model"


def call(path, body=None, timeout=600):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as err:
        return err.code, err.read().decode()


def check(name, condition, detail=""):
    if not condition:
        sys.exit(f"FAIL {name}: {detail[:600]}")
    print(f"ok   {name}", file=sys.stderr)


status, body = call("/v1/models")
models = {m["id"]: m for m in json.loads(body)["data"]}
check("model listed", MODEL in models, body)
context = models[MODEL].get("capabilities", {}).get("limits", {}).get("max_context_window_tokens", 0)
check("served context advertised", context > 0, json.dumps(models[MODEL]))

hello = [{"role": "user", "content": "Say hello in one word."}]
status, body = call("/v1/chat/completions", {"model": MODEL, "max_tokens": 16, "messages": hello})
check("chat", status == 200 and json.loads(body)["choices"][0]["message"].get("content"), body)

status, body = call("/v1/chat/completions", {"model": MODEL, "max_tokens": 16, "stream": True, "messages": hello})
check("chat stream", status == 200 and "data: [DONE]" in body and '"content"' in body, body)

systems = [{"role": "system", "content": "Be brief."}, {"role": "system", "content": "Answer in English."}] + hello
status, body = call("/v1/chat/completions", {"model": MODEL, "max_tokens": 16, "messages": systems})
check("chat with two system messages", status == 200, body)

status, body = call("/v1/messages", {"model": MODEL, "max_tokens": 16, "messages": hello})
check("anthropic messages", status == 200 and json.loads(body)["content"], body)

status, body = call("/v1/messages", {"model": MODEL, "max_tokens": 16, "stream": True, "messages": hello})
check("anthropic stream", status == 200 and "message_stop" in body, body)

tool = {"type": "function", "name": "get_weather", "description": "Get the weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}
status, body = call("/v1/responses", {"model": MODEL, "store": False, "max_output_tokens": 64,
                                      "instructions": "You are terse.",
                                      "input": [{"type": "message", "role": "developer", "content": "Use tools when useful."},
                                                {"role": "user", "content": "What is the weather in Paris?"}],
                                      "tools": [tool]})
check("responses with a function tool", status == 200 and json.loads(body).get("output"), body)

status, body = call("/v1/responses", {"model": MODEL, "store": False, "input": "hi",
                                      "tools": [{"type": "custom", "name": "apply_patch"}]})
check("responses rejects custom tools", status == 400 and "unsupported_tool_type" in body, body)

# Roughly one token per repeated word, well past the served context.
big = "word " * int(context * 1.3 + 256)
overflow = [{"role": "user", "content": big}]
status, body = call("/v1/chat/completions", {"model": MODEL, "max_tokens": 8, "messages": overflow})
check("chat overflow is context_length_exceeded", status == 400 and "context_length_exceeded" in body, body)
status, body = call("/v1/chat/completions", {"model": MODEL, "max_tokens": 8, "stream": True, "messages": overflow})
check("streamed chat overflow is context_length_exceeded", status == 400 and "context_length_exceeded" in body, body)
status, body = call("/v1/messages", {"model": MODEL, "max_tokens": 8, "messages": overflow})
check("anthropic overflow is prompt is too long", status == 400 and "prompt is too long" in body, body)
status, body = call("/v1/responses", {"model": MODEL, "store": False, "input": big})
check("responses overflow is context_length_exceeded", status == 400 and "context_length_exceeded" in body, body)
PY

log "stopping vekil server"
kill -INT "$SERVE_PID"
wait "$SERVE_PID" || true
[ -z "$(owned_containers "$SERVE_PID")" ] || die "vekil server left AIKit containers running"
log "ok   serve removed its container"
SERVE_PID=""

# ---------------------------------------------------------------------------
# Scenario 2: vekil launch with a runner reference and a stub agent.
# ---------------------------------------------------------------------------

AGENT_DIR="$SMOKE_DIR/agent"
mkdir -p "$AGENT_DIR/bin"
cat >"$AGENT_DIR/bin/claude" <<'EOF'
#!/usr/bin/env bash
# Stub Claude Code: reports a supported version, records the routing
# environment, and sends one Messages request through the launched proxy.
set -euo pipefail
if [ "${1:-}" = "--version" ]; then
  echo "2.1.300 (Claude Code)"
  exit 0
fi
: "${ANTHROPIC_BASE_URL:?}" "${ANTHROPIC_AUTH_TOKEN:?}" "${ANTHROPIC_MODEL:?}"
printf '%s\n' "${CLAUDE_CODE_MAX_CONTEXT_TOKENS:-}" >"$FAKE_AGENT_OUT/context"
code=$(curl -sS --max-time 600 -o "$FAKE_AGENT_OUT/response.json" -w '%{http_code}' \
  "$ANTHROPIC_BASE_URL/v1/messages" \
  -H "Authorization: Bearer $ANTHROPIC_AUTH_TOKEN" -H "Content-Type: application/json" \
  -d "{\"model\":\"$ANTHROPIC_MODEL\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in one word.\"}]}")
[ "$code" = "200" ]
EOF
chmod +x "$AGENT_DIR/bin/claude"

log "launching the stub agent with aikit:${AIKIT_RUNNER_MODEL}"
FAKE_AGENT_OUT="$AGENT_DIR" "$VEKIL_BIN" launch claude \
  --model "aikit:${AIKIT_RUNNER_MODEL}" --runtime "$AIKIT_RUNTIME" --load-timeout "$AIKIT_LOAD_TIMEOUT" \
  --binary "$AGENT_DIR/bin/claude" --no-summary --proxy-log "$SMOKE_DIR/launch-proxy-$$.log" \
  </dev/null >"$SMOKE_DIR/launch.log" 2>&1 &
LAUNCH_PID=$!
if ! wait "$LAUNCH_PID"; then
  die "vekil launch failed (see $SMOKE_DIR/launch.log)"
fi

context="$(cat "$AGENT_DIR/context" 2>/dev/null || true)"
[ -n "$context" ] && [ "$context" -ge 49152 ] || die "agent was not told the served context (got '${context}')"
log "ok   agent received CLAUDE_CODE_MAX_CONTEXT_TOKENS=${context}"
python3 - "$AGENT_DIR/response.json" <<'PY'
import json
import sys
with open(sys.argv[1]) as handle:
    body = json.load(handle)
if not body.get("content"):
    sys.exit(f"FAIL launched agent request: {json.dumps(body)[:600]}")
print("ok   launched agent request", file=sys.stderr)
PY
[ -z "$(owned_containers "$LAUNCH_PID")" ] || die "vekil launch left AIKit containers running"
log "ok   launch removed its container"
LAUNCH_PID=""

log "live AIKit smoke passed"
