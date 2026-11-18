#!/usr/bin/env bash
# Live smoke for opt-in Responses-over-Chat against a real provider.
#
# Requires an explicit Chat-only model; there is no default so a run never
# silently selects (or bills) a different model:
#
#   RESPONSES_OVER_CHAT_SMOKE_MODEL=gemini-3.8-flash scripts/live-responses-over-chat-smoke.sh
#
# Starts a private proxy on a free loopback port with only that model opted
# in, runs bounded JSON, SSE, forced namespaced tool, tool-result continuation
# and parallel-call turns, and prints a redacted summary. Prompts are
# synthetic; responses are reduced to shapes and counts.

set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MODEL="${RESPONSES_OVER_CHAT_SMOKE_MODEL:-}"
PROXY_BIN="${PROXY_BIN:-${REPO_ROOT}/vekil}"
MAX_TIME="${SMOKE_CURL_MAX_TIME_SECONDS:-120}"
STARTUP_TIMEOUT="${SMOKE_STARTUP_TIMEOUT_SECONDS:-120}"

[[ -n "${MODEL}" ]] || die "set RESPONSES_OVER_CHAT_SMOKE_MODEL to an explicitly approved Chat-only model ID"
command -v curl >/dev/null || die "curl is required"
command -v python3 >/dev/null || die "python3 is required"
if [[ ! -x "${PROXY_BIN}" ]]; then
  log "building ${PROXY_BIN}"
  (cd "${REPO_ROOT}" && go build -o "${PROXY_BIN}" .)
fi

WORK_DIR="$(mktemp -d)"
chmod 700 "${WORK_DIR}"
PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
BASE="http://127.0.0.1:${PORT}"
PROXY_PID=""

cleanup() {
  if [[ -n "${PROXY_PID}" ]] && kill -0 "${PROXY_PID}" 2>/dev/null; then
    kill "${PROXY_PID}" 2>/dev/null || true
    wait "${PROXY_PID}" 2>/dev/null || true
  fi
  rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

log "starting isolated proxy on ${BASE} (opt-in: ${MODEL})"
# The smoke's loopback requests carry no inbound credential, so an inherited
# INBOUND_AUTH_TOKEN would make this private proxy answer 401 before any
# adapter turn runs.
env -u INBOUND_AUTH_TOKEN \
  "${PROXY_BIN}" --host 127.0.0.1 --port "${PORT}" --log-level warn \
  --state-bindings-mode memory \
  --responses-over-chat-models "${MODEL}" >"${WORK_DIR}/proxy.log" 2>&1 &
PROXY_PID=$!

deadline=$((SECONDS + STARTUP_TIMEOUT))
until curl -fsS -m 5 "${BASE}/readyz" >/dev/null 2>&1; do
  kill -0 "${PROXY_PID}" 2>/dev/null || { tail -20 "${WORK_DIR}/proxy.log" >&2; die "proxy exited during startup"; }
  (( SECONDS < deadline )) || die "proxy was not ready within ${STARTUP_TIMEOUT}s"
  sleep 1
done

post() { # name body
  curl -sS -m "${MAX_TIME}" -o "${WORK_DIR}/$1.out" -D "${WORK_DIR}/$1.headers" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary "$2" "${BASE}/v1/responses"
}

curl -fsS -m 30 "${BASE}/v1/models" -o "${WORK_DIR}/models.json"

TOOLS='[{"type":"namespace","name":"diag","description":"Diagnostics","tools":[{"type":"function","name":"echo_token","description":"Echo a token","parameters":{"type":"object","properties":{"token":{"type":"string"}},"required":["token"]}}]}]'
LOOKUP='[{"type":"function","name":"lookup","description":"Look up a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]'

log "JSON text turn"
json_status="$(post json '{"model":"'"${MODEL}"'","store":false,"input":"Reply with the single word: ready","max_output_tokens":256}')"
log "SSE text turn"
sse_status="$(post sse '{"model":"'"${MODEL}"'","store":false,"stream":true,"input":"Count from 1 to 5 separated by spaces.","max_output_tokens":256}')"
log "forced namespaced tool turn"
tool_status="$(post tool '{"model":"'"${MODEL}"'","store":false,"stream":true,"input":"Call the tool with token alpha.","tools":'"${TOOLS}"',"tool_choice":{"type":"function","namespace":"diag","name":"echo_token"}}')"
CALL_ID="$(python3 - "${WORK_DIR}/tool.out" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    if line.startswith("data: "):
        event = json.loads(line[6:])
        item = event.get("item") or {}
        if event.get("type") == "response.output_item.done" and item.get("type") == "function_call":
            print(item["call_id"]); break
PY
)"
[[ -n "${CALL_ID}" ]] || die "forced tool turn returned no function call"
log "tool-result continuation turn"
cont_status="$(post continuation '{"model":"'"${MODEL}"'","store":false,"stream":true,"tools":'"${TOOLS}"',"input":[
  {"type":"message","role":"user","content":"Call the tool with token alpha."},
  {"type":"function_call","call_id":"'"${CALL_ID}"'","namespace":"diag","name":"echo_token","arguments":"{\"token\":\"alpha\"}"},
  {"type":"function_call_output","call_id":"'"${CALL_ID}"'","output":"alpha-echoed"},
  {"type":"message","role":"user","content":"What did the tool return? Answer in one short sentence."}]}')"
log "parallel tool turn"
par_status="$(post parallel '{"model":"'"${MODEL}"'","store":false,"stream":true,"parallel_tool_calls":true,"tool_choice":"required","tools":'"${LOOKUP}"',"input":"Look up Paris and Tokyo with two separate tool calls."}')"

python3 - "${WORK_DIR}" "${MODEL}" "${json_status}" "${sse_status}" "${tool_status}" "${cont_status}" "${par_status}" <<'PY'
import json, os, sys
work, model = sys.argv[1], sys.argv[2]
statuses = dict(zip(["json", "sse", "tool", "continuation", "parallel"], sys.argv[3:]))
failures = []

catalog = json.load(open(os.path.join(work, "models.json")))
entry = next((m for m in catalog.get("data", []) if m.get("id") == model), None)
codex = next((m for m in catalog.get("models", []) if m.get("slug") == model), None)
if not entry:
    failures.append("model missing from /v1/models")
else:
    if entry.get("vekil_responses_compatibility") != "responses_over_chat":
        failures.append("catalog entry is not marked responses_over_chat")
    if "/responses" in (entry.get("supported_endpoints") or []):
        failures.append("native supported_endpoints unexpectedly include /responses")
if not codex or codex.get("visibility") != "list":
    failures.append("Codex projection does not list the model")

def sse(name):
    events = []
    for line in open(os.path.join(work, name + ".out")):
        if line.startswith("data: "):
            events.append(json.loads(line[6:]))
    return events

def summarize(name):
    events = sse(name)
    types = [e.get("type") for e in events]
    terminal = events[-1] if events else {}
    response = terminal.get("response") or {}
    calls = [e["item"] for e in events if e.get("type") == "response.output_item.done" and e["item"].get("type") == "function_call"]
    return {
        "status": statuses[name], "events": len(events), "terminal": types[-1] if types else None,
        "text_deltas": types.count("response.output_text.delta"),
        "function_calls": [{"namespace": c.get("namespace"), "name": c.get("name")} for c in calls],
        "model": response.get("model"), "usage_total": (response.get("usage") or {}).get("total_tokens"),
    }

summary = {}
body = json.load(open(os.path.join(work, "json.out")))
summary["json"] = {"status": statuses["json"], "response_status": body.get("status"), "model": body.get("model"),
                   "output_items": len(body.get("output") or []), "usage_total": (body.get("usage") or {}).get("total_tokens")}
for name in ["sse", "tool", "continuation", "parallel"]:
    summary[name] = summarize(name)

if statuses["json"] != "200" or body.get("status") != "completed":
    failures.append("JSON turn did not complete")
for name in ["sse", "tool", "continuation", "parallel"]:
    if summary[name]["status"] != "200" or summary[name]["terminal"] != "response.completed":
        failures.append(f"{name} turn did not complete")
    if summary[name]["model"] != model:
        failures.append(f"{name} turn reported model {summary[name]['model']!r}")
if summary["sse"]["text_deltas"] < 1:
    failures.append("SSE turn produced no text deltas")
if summary["tool"]["function_calls"] != [{"namespace": "diag", "name": "echo_token"}]:
    failures.append("forced tool call did not restore diag/echo_token")
if summary["continuation"]["text_deltas"] < 1:
    failures.append("continuation produced no text")
if len(summary["parallel"]["function_calls"]) < 2:
    failures.append("parallel turn returned fewer than two calls (model choice; informational)")

print(json.dumps({"model": model, "summary": summary, "failures": failures}, indent=2))
hard = [f for f in failures if "informational" not in f]
sys.exit(1 if hard else 0)
PY
