#!/usr/bin/env bun

// A frozen OpenCode-shaped diagnostic, not a coding-client smoke or an access
// workaround. All three paths must complete; a Bun-assisted pass cannot mask a
// failure of Vekil's normal Go transport. The relay exists only in this process.
import { closeSync, mkdirSync, mkdtempSync, openSync, readFileSync, writeFileSync } from "node:fs"
import { createHash, randomBytes } from "node:crypto"
import { createServer } from "node:net"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"

const ROOT = resolve(import.meta.dir, "..")
export const FIXTURE = join(ROOT, "scripts/tests/fixtures/opencode-zen-control.json")
export const UPSTREAM = "https://opencode.ai/inference/openai/v1/chat/completions"
export const EXPECTED = "ZEN_CONTROL_OK"
export const MODEL = "glm-5.3-flash-free"
const ROUTE = "/inference/openai/v1/chat/completions"
const MODES = ["bun-reference", "vekil-bun-relay", "vekil-direct"] as const
const MAX_BYTES = 1024 * 1024
const TIMEOUT_MS = 25_000
const HOP_HEADERS = ["host", "connection", "content-length", "content-encoding", "transfer-encoding"]
const children = new Set<ReturnType<typeof Bun.spawn>>()

type Mode = (typeof MODES)[number]
type Result = { mode: Mode; passed: boolean; client_request_sha256: string; http_status?: number; error?: string; relay_requests?: number; log_id?: string }
type Options = { proxyBin: string; modelsFile: string; outputDir: string; upstream?: string }

export function sessionID(now = Date.now()) {
  // OpenCode's descending 48-bit timestamp/counter, then 14 base-62 characters.
  const timestamp = (~(BigInt(now) * 4096n + 1n) & ((1n << 48n) - 1n)).toString(16).padStart(12, "0")
  const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
  return "ses_" + timestamp + Array.from(randomBytes(14), (b) => alphabet[b % alphabet.length]).join("")
}

export function checkFreeModel(tsv: string) {
  if (!tsv.split(/\r?\n/).some((line) => {
    const fields = line.split("\t")
    return fields.length === 3 && fields[0] === MODEL && fields[2] === "/chat/completions"
  })) throw new Error(`${MODEL} is not in the supplied current free Chat catalog; refusing inference`)
}

function checkUpstream(value: string) {
  if (value === UPSTREAM) return
  const url = new URL(value)
  // The only override is a loopback mock, never another live/paid provider.
  if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || !url.port || url.pathname !== ROUTE || url.search || url.hash || url.username || url.password) {
    throw new Error("upstream override must be a loopback mock on the inference route")
  }
}

export function validateCompletion(text: string) {
  let content = "", done = false, stopped = false, usage: any
  const normalized = text.replaceAll("\r\n", "\n")
  if (!normalized.endsWith("\n\n")) throw new Error("unterminated SSE frame")
  for (const frame of normalized.split("\n\n")) {
    const data = frame.split("\n").filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join("\n")
    if (!data) continue
    if (done) throw new Error("SSE data after [DONE]")
    if (data === "[DONE]") { done = true; continue }
    const chunk = JSON.parse(data)
    if (chunk.error || !Array.isArray(chunk.choices) || chunk.choices.length > 1) throw new Error("invalid Chat SSE chunk")
    for (const choice of chunk.choices) {
      if (choice.index !== 0) throw new Error("unexpected completion choice index")
      const calls = choice.delta?.tool_calls
      if ((calls != null && (!Array.isArray(calls) || calls.length > 0)) || choice.delta?.function_call || (choice.finish_reason && choice.finish_reason !== "stop")) {
        throw new Error("unexpected tool call or incomplete completion")
      }
      if (choice.delta?.content != null) {
        if (typeof choice.delta.content !== "string") throw new Error("non-text completion")
        content += choice.delta.content
      }
      if (choice.finish_reason === "stop") stopped = true
    }
    if (chunk.usage) usage = chunk.usage
  }
  if (!done || !stopped || content.trim() !== EXPECTED) throw new Error("missing, incomplete, or mismatched completion")
  if (usage?.cost !== 0 || usage?.opencode?.billing_source !== "free" || !Number.isFinite(usage.prompt_tokens) || usage.prompt_tokens <= 0 || !Number.isFinite(usage.completion_tokens) || usage.completion_tokens <= 0) {
    throw new Error("missing positive token usage or zero-cost free billing proof")
  }
  return usage
}

async function readBounded(response: Response) {
  if (!response.body) throw new Error("empty HTTP body")
  const reader = response.body.getReader(), chunks: Uint8Array[] = []
  let size = 0
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      size += value.byteLength
      if (size > MAX_BYTES) throw new Error("response exceeds diagnostic byte limit")
      chunks.push(value)
    }
  } catch (error) {
    await reader.cancel().catch(() => {})
    throw error
  } finally { reader.releaseLock() }
  return Buffer.concat(chunks).toString("utf8")
}

async function unusedPort(): Promise<number> {
  const socket = createServer()
  await new Promise<void>((ok, fail) => { socket.once("error", fail); socket.listen(0, "127.0.0.1", ok) })
  const port = (socket.address() as { port: number }).port
  await new Promise<void>((ok) => socket.close(() => ok()))
  return port
}

const delay = (ms: number) => new Promise((ok) => setTimeout(ok, ms))

async function runCase(mode: Mode, options: Options, body: string, metadata: Record<string, string>): Promise<Result> {
  const directory = join(options.outputDir, mode)
  mkdirSync(directory)
  const result: Result = { mode, passed: false, client_request_sha256: createHash("sha256").update(body).digest("hex") }
  let relay: ReturnType<typeof Bun.serve> | undefined, child: ReturnType<typeof Bun.spawn> | undefined, log: number | undefined
  let relayRequests = 0
  try {
    let upstreamBase = "https://opencode.ai/inference/openai/v1"
    if (mode === "vekil-bun-relay") {
      relay = Bun.serve({ hostname: "127.0.0.1", port: 0, maxRequestBodySize: 128 * 1024, async fetch(request) {
        if (request.method !== "POST" || new URL(request.url).pathname !== ROUTE) return new Response("unexpected route", { status: 404 })
        if (++relayRequests !== 1) return new Response("diagnostic dispatch limit", { status: 403 })
        const raw = await request.text(), parsed = JSON.parse(raw)
        if (parsed.model !== MODEL || request.headers.get("authorization") !== "Bearer public") return new Response("anonymous free-model guard", { status: 403 })
        const headers = new Headers(request.headers)
        for (const name of HOP_HEADERS) headers.delete(name)
        try {
          const response = await fetch(options.upstream ?? UPSTREAM, { method: "POST", headers, body: raw, redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS) })
          const text = await readBounded(response)
          writeFileSync(join(directory, "relay-receipt.json"), JSON.stringify({ application_dispatches: relayRequests, request_sha256: createHash("sha256").update(raw).digest("hex"), status: response.status, log_id: response.headers.get("x-opencode-log-id") }, null, 2))
          const responseHeaders = new Headers(response.headers)
          for (const name of HOP_HEADERS) responseHeaders.delete(name)
          return new Response(text, { status: response.status, headers: responseHeaders })
        } catch { return new Response("diagnostic upstream transport failed", { status: 502 }) }
      } })
      upstreamBase = `http://127.0.0.1:${relay.port}/inference/openai/v1`
    } else if (options.upstream) {
      upstreamBase = options.upstream.slice(0, -"/chat/completions".length)
    }
    let url = upstreamBase + "/chat/completions"
    let headers: Record<string, string> = { ...metadata, authorization: "Bearer public", "content-type": "application/json" }
    if (mode !== "bun-reference") {
      const home = join(directory, "home")
      mkdirSync(home)
      const config = join(directory, "providers.json")
      writeFileSync(config, JSON.stringify({ providers: [{ id: "zen-control", type: "openai-compatible", base_url: upstreamBase, auth_type: "bearer", api_key: "public", model_discovery: "static", models: [{ public_id: MODEL, endpoints: ["/chat/completions"] }], extra_headers: metadata }] }, null, 2))
      const port = await unusedPort()
      log = openSync(join(directory, "proxy.log"), "w")
      child = Bun.spawn({ cmd: [resolve(options.proxyBin), "--providers-config", config, "--port", String(port), "--token-dir", join(home, "tokens"), "--streaming-upstream-timeout", "30s"], cwd: directory, env: { HOME: home, PATH: process.env.PATH ?? "/usr/bin:/bin", TMPDIR: home }, stdout: log, stderr: log })
      children.add(child)
      const base = `http://127.0.0.1:${port}`
      let ready = false
      for (const end = Date.now() + 10_000; Date.now() < end;) {
        if (child.exitCode !== null) throw new Error("Vekil exited before readiness")
        try { const response = await fetch(base + "/readyz", { signal: AbortSignal.timeout(250) }); await response.arrayBuffer(); ready = response.status === 200 } catch {}
        if (ready) break
        await delay(50)
      }
      if (!ready) throw new Error("Vekil readiness deadline exceeded")
      url = base + "/v1/chat/completions"
      headers = { "content-type": "application/json" }
    }
    const response = await fetch(url, { method: "POST", headers, body, redirect: "manual", signal: AbortSignal.timeout(TIMEOUT_MS + 5_000) })
    result.http_status = response.status
    result.log_id = response.headers.get("x-opencode-log-id") ?? undefined
    const text = await readBounded(response)
    writeFileSync(join(directory, "response.txt"), text)
    if (response.status !== 200) throw new Error(`HTTP ${response.status}; no completion (see response.txt)`)
    if (!response.headers.get("content-type")?.includes("text/event-stream")) throw new Error("expected Chat SSE response")
    const usage = validateCompletion(text)
    writeFileSync(join(directory, "usage.json"), JSON.stringify(usage, null, 2))
    if (relay && relayRequests !== 1) throw new Error("expected exactly one relay application request")
    result.passed = true
  } catch (error) { result.error = error instanceof Error ? error.message : String(error) }
  finally {
    if (child && child.exitCode === null) {
      child.kill("SIGTERM")
      let timer: ReturnType<typeof setTimeout> | undefined
      await Promise.race([child.exited, new Promise((ok) => { timer = setTimeout(ok, 2000) })])
      if (timer) clearTimeout(timer)
      if (child.exitCode === null) child.kill("SIGKILL")
      await child.exited
    }
    if (child) children.delete(child)
    if (log !== undefined) closeSync(log)
    relay?.stop(true)
    if (relay) result.relay_requests = relayRequests
    writeFileSync(join(directory, "result.json"), JSON.stringify(result, null, 2))
  }
  return result
}

export async function runControls(options: Options) {
  checkUpstream(options.upstream ?? UPSTREAM)
  checkFreeModel(readFileSync(options.modelsFile, "utf8"))
  const body = readFileSync(FIXTURE, "utf8")
  if (JSON.parse(body).model !== MODEL) throw new Error("unexpected fixture model")
  mkdirSync(options.outputDir, { recursive: true })
  const session = sessionID()
  const metadata = { "user-agent": "opencode/compat/2.1.2/vekil", "x-opencode-client": "vekil", "x-opencode-project": "vekil", "x-opencode-session": session, "x-opencode-session-id": session, "x-session-affinity": session, "x-session-id": session }
  const results: Result[] = []
  for (const mode of MODES) {
    results.push(await runCase(mode, options, body, metadata))
    writeFileSync(join(options.outputDir, "results.json"), JSON.stringify(results, null, 2))
  }
  return results
}

if (import.meta.main) {
  // Vekil is a single binary here, with no tool/CLI descendants. Do not leave
  // its listener behind when a local run or CI job is interrupted.
  for (const [signal, code] of [["SIGINT", 130], ["SIGTERM", 143]] as const) {
    process.once(signal, () => {
      for (const child of children) child.kill("SIGKILL")
      process.exit(code)
    })
  }
  try {
    const modelsFile = process.env.ZEN_FREE_MODELS_FILE
    if (!modelsFile) throw new Error("ZEN_FREE_MODELS_FILE is required; fetch and parse current Free labels first")
    const outputDir = process.env.ZEN_CONTROL_DIR ? resolve(process.env.ZEN_CONTROL_DIR) : mkdtempSync(join(tmpdir(), "vekil-zen-control-"))
    const results = await runControls({ proxyBin: process.env.PROXY_BIN ?? join(ROOT, "vekil"), modelsFile, outputDir })
    const summary = "## OpenCode-shaped diagnostic (not a coding-client pass)\n\n| Path | Exact completion and free billing | HTTP |\n|---|---|---|\n" + results.map((r) => `| ${r.mode} | ${r.passed ? "PASS" : "FAIL"} | ${r.http_status ?? "not reached"} |`).join("\n") + "\n\nAll paths are required. The Bun relay is test-only; it does not change Vekil's native transport.\n"
    writeFileSync(join(outputDir, "summary.md"), summary)
    if (process.env.GITHUB_STEP_SUMMARY) writeFileSync(process.env.GITHUB_STEP_SUMMARY, summary, { flag: "a" })
    console.log(summary)
    for (const result of results) if (!result.passed) console.error(`${result.mode}: ${result.error}`)
    console.log(`Diagnostic artifacts: ${outputDir}`)
    process.exitCode = results.every((r) => r.passed) ? 0 : 1
  } catch (error) { console.error(String(error)); process.exitCode = 1 }
}
