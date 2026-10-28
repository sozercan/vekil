import { afterAll, expect, test } from "bun:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { createHash } from "node:crypto"
import { EXPECTED, MODEL, UPSTREAM } from "../live-zen-control"
import { OPENCODE_VERSION } from "../opencode-zen-metadata.mjs"
import { readNativeReplay, runCLI, runNativeControls, validateNativeEvidence, validateNativeOutput } from "../live-zen-native"
import { setupObserver } from "../zen-native-observer/index.js"

const roots: string[] = []
function temporary() { const path = mkdtempSync(join(tmpdir(), "zen-native-test-")); roots.push(path); return path }
afterAll(() => { for (const root of roots) rmSync(root, { recursive: true, force: true }) })
const cliText = (text = EXPECTED) => JSON.stringify({ type: "text", part: { text } }) + "\n"
const usage = { prompt_tokens: 10, completion_tokens: 5, cost: 0, opencode: { billing_source: "free" } }
const completion = () => `data: ${JSON.stringify({ id: "native-test", object: "chat.completion.chunk", choices: [{ index: 0, delta: { role: "assistant", content: EXPECTED }, finish_reason: "stop" }], usage })}\n\ndata: [DONE]\n\n`

for (const [name, text] of [
  ["empty output", ""], ["wrong answer", cliText("wrong")], ["malformed JSON", "not JSON"],
  ["CLI error", cliText() + '{"type":"error","error":{"message":"failed"}}\n'],
  ["tool execution", cliText() + '{"type":"tool_use"}\n'],
  ["unknown event", cliText() + '{"type":"unknown"}\n'],
] as const) test(`native CLI rejects ${name}`, () => expect(() => validateNativeOutput(text)).toThrow())

test("native CLI accepts exact split text, not just process success", () => {
  expect(() => validateNativeOutput(cliText())).not.toThrow()
  expect(() => validateNativeOutput(cliText("ZEN_CONTROL_") + cliText("OK"))).not.toThrow()
})

function evidence(directory: string) {
  writeFileSync(join(directory, "observer.json"), JSON.stringify({ app: { name: "cli", version: OPENCODE_VERSION }, requests: 1, responses: 1, violations: [] }))
  writeFileSync(join(directory, "request.json"), JSON.stringify({ model: MODEL, body_sha256: "a".repeat(64), headers: { "x-opencode-client": "cli" } }))
  writeFileSync(join(directory, "response.json"), JSON.stringify({ status: 200, content_type: "text/event-stream" }))
  writeFileSync(join(directory, "response.txt"), completion())
}

test("native success requires matching HTTP, CLI, usage, and observer evidence", () => {
  const directory = temporary()
  evidence(directory)
  expect(validateNativeEvidence(directory, cliText(), 0)).toEqual(usage)
  expect(() => validateNativeEvidence(directory, cliText(), 1)).toThrow()
  writeFileSync(join(directory, "response.json"), JSON.stringify({ status: 429, content_type: "application/json" }))
  expect(() => validateNativeEvidence(directory, cliText(), 0)).toThrow("429")
  evidence(directory)
  writeFileSync(join(directory, "observer.json"), JSON.stringify({ app: { name: "cli", version: OPENCODE_VERSION }, requests: 2, responses: 1, violations: [] }))
  expect(() => validateNativeEvidence(directory, cliText(), 0)).toThrow()
  evidence(directory)
  writeFileSync(join(directory, "response.txt"), completion().replace('"cost":0', '"cost":1'))
  expect(() => validateNativeEvidence(directory, cliText(), 0)).toThrow()
})

async function installObserver(directory: string, upstream = "http://127.0.0.1:1234/inference/openai/v1/chat/completions", capture = true) {
  const hooks: Record<string, (event: any) => any> = {}
  const tool = { execute: async () => { throw new Error("original tool must never run") } }
  await setupObserver({ options: { directory, upstream, ...(capture ? { requestFile: join(directory, "native-request.txt") } : {}) }, app: { name: "cli", version: OPENCODE_VERSION },
    session: { hook: async (name: string, callback: any) => { hooks[name] = callback } },
    permission: { hook: async (_name: string, callback: any) => { hooks.permission = callback } },
    tool: { transform: async (callback: any) => callback({ list: () => [{ id: "blocked" }], update: (_id: string, update: any) => update(tool) }) },
  })
  return { hooks, tool }
}

for (const variant of ["wrong-model", "wrong-destination", "wrong-key", "auxiliary", "second-request"] as const) {
  test(`observer fails closed for ${variant}`, async () => {
    const directory = temporary(), { hooks } = await installObserver(directory)
    const request = () => new Request(`http://127.0.0.1:1234/inference/openai/v1/${variant === "wrong-destination" ? "responses" : "chat/completions"}`, {
      method: "POST", headers: { authorization: variant === "wrong-key" ? "Bearer secret" : "Bearer public", "content-type": "application/json" }, body: JSON.stringify({ model: variant === "wrong-model" ? "paid-model" : MODEL }),
    })
    if (variant === "second-request") await hooks["http.request"]({ kind: "primary", request: request() })
    await expect(hooks["http.request"]({ kind: variant === "auxiliary" ? "title" : "primary", request: request() })).rejects.toThrow()
    expect(JSON.parse(readFileSync(join(directory, "observer.json"), "utf8")).violations.length).toBe(1)
  })
}

test("observer disables retries and tool execution without changing schemas", async () => {
  const { hooks, tool } = await installObserver(temporary())
  const retry = { decision: { retry: true } }, permission = { effect: "allow" }
  hooks.retry(retry)
  hooks.permission(permission)
  expect(retry.decision).toEqual({ retry: false })
  expect(permission.effect).toBe("deny")
  await expect(tool.execute()).rejects.toThrow("diagnostic tool execution denied")
})

for (const scenario of ["pass", "quota", "post-stop", "tool-call", "redirect-307", "redirect-308"] as const) {
  test(`actual OpenCode CLI and exact request replay through Vekil: ${scenario}`, async () => {
    const root = temporary(), requests: { body: any; headers: Headers }[] = []
    let redirectedRequests = 0
    const server = Bun.serve({ hostname: "127.0.0.1", port: 0, async fetch(request) {
      requests.push({ body: await request.json(), headers: new Headers(request.headers) })
      if (new URL(request.url).pathname === "/redirected") {
        redirectedRequests++
        return new Response(completion(), { headers: { "content-type": "text/event-stream" } })
      }
      if (scenario.startsWith("redirect-")) return new Response("redirect refused", { status: Number(scenario.slice(-3)), headers: { location: new URL("/redirected", request.url).href } })
      if (scenario === "quota") return Response.json({ error: { type: "FreeUsageLimitError", message: "Free usage limit reached" } }, { status: 429, headers: { "retry-after": "60" } })
      if (scenario === "tool-call") {
        expect(requests.at(-1)!.body.tools.some((tool: any) => tool.function?.name === "shell")).toBe(true)
        const toolCall = { index: 0, id: "call_diagnostic", type: "function", function: { name: "shell", arguments: JSON.stringify({ command: `touch ${join(root, "must-not-exist")}` }) } }
        return new Response(`data: ${JSON.stringify({ id: "native-tool-test", object: "chat.completion.chunk", choices: [{ index: 0, delta: { tool_calls: [toolCall] }, finish_reason: "tool_calls" }], usage })}\n\ndata: [DONE]\n\n`, { headers: { "content-type": "text/event-stream" } })
      }
      const text = scenario === "post-stop" ? completion().replace("data: [DONE]\n\n", 'data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n') : completion()
      return new Response(text, { headers: { "content-type": "text/event-stream" } })
    } })
    const modelsFile = join(root, "models.tsv")
    writeFileSync(modelsFile, `${MODEL}\tFree\t/chat/completions\n`)
    try {
      const outputDir = join(root, "results")
      const results = await runNativeControls({ opencodeBin: process.env.OPENCODE_BIN ?? "opencode", proxyBin: resolve(process.env.PROXY_BIN ?? "vekil"), modelsFile, outputDir, upstream: `http://127.0.0.1:${server.port}/inference/openai/v1/chat/completions` })
      expect(results.map((result) => result.mode)).toEqual(["opencode-direct", "vekil-direct"])
      // The baseline retains real OpenCode's redirect behavior. It cannot mask
      // the separate Vekil replay's rejection of redirects or malformed output.
      const redirected = scenario.startsWith("redirect-")
      expect(results.map((result) => result.passed)).toEqual([scenario === "pass" || redirected, scenario === "pass"])
      const status = scenario === "quota" ? 429 : redirected ? Number(scenario.slice(-3)) : 200
      expect(results.map((result) => result.http_status)).toEqual([redirected ? 200 : status, status])
      expect(redirectedRequests).toBe(redirected ? 1 : 0)
      expect(requests).toHaveLength(redirected ? 3 : 2)
      expect(existsSync(join(root, "must-not-exist"))).toBe(false)
      for (const request of requests) {
        expect(request.body.model).toBe(MODEL)
        expect(request.body.tools.length).toBeGreaterThan(0)
        expect(request.headers.get("authorization")).toBe("Bearer public")
        expect(request.headers.get("x-opencode-client")).toBe("cli")
        expect(request.headers.get("user-agent")).toBe(`opencode/latest/${OPENCODE_VERSION}/cli`)
      }
      const directRequest = JSON.parse(readFileSync(join(outputDir, "opencode-direct", "request.json"), "utf8"))
      const replayResult = JSON.parse(readFileSync(join(outputDir, "vekil-direct", "result.json"), "utf8"))
      expect(directRequest.body_sha256).toBe(replayResult.client_request_sha256)
      expect(requests[0].headers.get("x-opencode-session-id")).toBe(requests.at(-1)!.headers.get("x-opencode-session-id"))
      expect(requests[0].body.parallel_tool_calls).toBeUndefined()
      expect(requests.at(-1)!.body.parallel_tool_calls).toBe(true)
      expect(requests[0].body.messages).toEqual(requests.at(-1)!.body.messages)
      expect(requests[0].body.tools).toEqual(requests.at(-1)!.body.tools)
      expect(existsSync(join(outputDir, "opencode-direct", "proxy.log"))).toBe(false)
      expect(existsSync(join(outputDir, "vekil-direct", "proxy.log"))).toBe(true)
      const stderr = readFileSync(join(outputDir, "opencode-direct", "cli.stderr.txt"), "utf8")
      expect(stderr).not.toContain(root)
    } finally { server.stop(true) }
  }, 120_000)
}

test("native harness rejects non-loopback overrides and missing free labels before starting the CLI", async () => {
  const options = { opencodeBin: "must-not-run", proxyBin: "must-not-run", modelsFile: "missing", outputDir: "unused", upstream: "https://example.com/paid" }
  await expect(runNativeControls(options)).rejects.toThrow("loopback mock")
  const root = temporary(), modelsFile = join(root, "models.tsv")
  writeFileSync(modelsFile, "paid-model\tPaid\t/chat/completions\n")
  await expect(runNativeControls({ ...options, upstream: undefined, modelsFile })).rejects.toThrow("current free Chat catalog")
})


test("native CLI deadline kills its process group", async () => {
  const root = temporary(), pidFile = join(root, "pid")
  const code = `require("node:fs").writeFileSync(${JSON.stringify(pidFile)}, String(process.pid)); process.on("SIGTERM", () => {}); setInterval(() => {}, 1000)`
  const promise = runCLI([process.execPath, "-e", code], root, { PATH: process.env.PATH ?? "/usr/bin:/bin" }, 500)
  await expect(promise).rejects.toThrow("deadline")
  const pid = Number(readFileSync(pidFile, "utf8"))
  expect(() => process.kill(pid, 0)).toThrow()
}, 10_000)

test("native CLI oversized output is not buffered without bound", async () => {
  await expect(runCLI([process.execPath, "-e", 'process.stdout.write("x".repeat(2 * 1024 * 1024))'], temporary(), { PATH: process.env.PATH ?? "/usr/bin:/bin" })).rejects.toThrow("output limit")
})


test("native harness refuses stale evidence before executing the CLI", async () => {
  const root = temporary(), outputDir = join(root, "results"), modelsFile = join(root, "models.tsv")
  mkdirSync(outputDir)
  writeFileSync(join(outputDir, "results.json"), "stale")
  writeFileSync(modelsFile, `${MODEL}\tFree\t/chat/completions\n`)
  await expect(runNativeControls({ opencodeBin: process.execPath, proxyBin: "must-not-run", modelsFile, outputDir })).rejects.toThrow("stale evidence")
  expect(readFileSync(join(outputDir, "results.json"), "utf8")).toBe("stale")
})


test("native request replay rejects altered captures and unexpected metadata", () => {
  const root = temporary()
  mkdirSync(join(root, "_private"))
  mkdirSync(join(root, "opencode-direct"))
  const body = JSON.stringify({ model: MODEL })
  writeFileSync(join(root, "_private", "request.txt"), body)
  writeFileSync(join(root, "opencode-direct", "request.json"), JSON.stringify({ model: MODEL, body_sha256: "wrong", headers: { "x-opencode-client": "cli" } }))
  expect(() => readNativeReplay(root, "http://127.0.0.1:1234/inference/openai/v1/chat/completions")).toThrow("invalid native request capture")
  writeFileSync(join(root, "opencode-direct", "request.json"), JSON.stringify({ model: MODEL, body_sha256: createHash("sha256").update(body).digest("hex"), headers: { "x-opencode-client": "cli", authorization: "Bearer other" } }))
  expect(() => readNativeReplay(root, "http://127.0.0.1:1234/inference/openai/v1/chat/completions")).toThrow("invalid native request metadata")
})


test("live observer records request evidence without saving raw replay data", async () => {
  const directory = temporary(), { hooks } = await installObserver(directory, UPSTREAM, false)
  const body = JSON.stringify({ model: MODEL, messages: [{ role: "user", content: EXPECTED }] })
  await hooks["http.request"]({ kind: "primary", request: new Request(UPSTREAM, {
    method: "POST", headers: { authorization: "Bearer public", "x-opencode-client": "cli" }, body,
  }) })
  expect(existsSync(join(directory, "native-request.txt"))).toBe(false)
  const receipt = JSON.parse(readFileSync(join(directory, "request.json"), "utf8"))
  expect(receipt.body_sha256).toBe(createHash("sha256").update(body).digest("hex"))
  expect(receipt.headers["x-opencode-client"]).toBe("cli")
})

for (const upstream of [UPSTREAM, "https://example.com/inference/openai/v1/chat/completions", "http://localhost:1234/inference/openai/v1/chat/completions", "http://127.0.0.1:1234/inference/openai/v1/chat/completions?upstream=live", "http://127.0.0.1:1234/inference/openai/v1/chat/completions#live", "http://public@127.0.0.1:1234/inference/openai/v1/chat/completions", "https://127.0.0.1:1234/inference/openai/v1/chat/completions"]) {
  test(`native replay rejects unsafe destination before reading any capture: ${upstream}`, () => {
    expect(() => readNativeReplay("must-not-be-read", upstream)).toThrow("loopback mock")
  })
}

for (const upstream of [undefined, UPSTREAM]) for (const status of [200, 429]) {
  test(`live baseline never dispatches native identity through Vekil: ${status}, ${upstream ? "explicit" : "default"} URL`, async () => {
    const root = temporary(), outputDir = join(root, "results"), modelsFile = join(root, "models.tsv"), binary = join(root, "fake-opencode")
    const proxyMarker = join(root, "proxy-started"), configFile = join(root, "observed-options.json")
    writeFileSync(modelsFile, `${MODEL}\tFree\t/chat/completions\n`)
    // Fake only the native CLI: no external request is made. Leave a valid
    // capture behind to prove neither success nor refusal can trigger replay.
    const body = JSON.stringify({ model: MODEL }), headers = { "x-opencode-client": "cli", "user-agent": `opencode/latest/${OPENCODE_VERSION}/cli` }
    const receipt = { model: MODEL, body_sha256: createHash("sha256").update(body).digest("hex"), headers }
    writeFileSync(binary, `#!${process.execPath}
const { writeFileSync } = require("node:fs"), { join } = require("node:path");
if (process.argv.includes("--version")) { console.log("opencode v${OPENCODE_VERSION}"); process.exit(0) }
if (!process.env.OPENCODE_CONFIG_CONTENT) { writeFileSync(${JSON.stringify(proxyMarker)}, "started"); process.exit(1) }
const options = JSON.parse(process.env.OPENCODE_CONFIG_CONTENT).plugins[0].options;
writeFileSync(${JSON.stringify(configFile)}, JSON.stringify(options));
writeFileSync(join(options.directory, "observer.json"), ${JSON.stringify(JSON.stringify({ app: { name: "cli", version: OPENCODE_VERSION }, requests: 1, responses: 1, violations: [] }))});
writeFileSync(join(options.directory, "request.json"), ${JSON.stringify(JSON.stringify(receipt))});
writeFileSync(join(options.directory, "response.json"), ${JSON.stringify(JSON.stringify({ status, content_type: status === 200 ? "text/event-stream" : "application/json" }))});
writeFileSync(join(options.directory, "response.txt"), ${JSON.stringify(status === 200 ? completion() : '{"error":{"type":"FreeUsageLimitError"}}')});
writeFileSync(${JSON.stringify(join(outputDir, "_private", "request.txt"))}, ${JSON.stringify(body)});
process.stdout.write(${JSON.stringify(cliText())});
`, { mode: 0o700 })
    const results = await runNativeControls({ opencodeBin: binary, proxyBin: binary, modelsFile, outputDir, upstream })
    expect(results).toHaveLength(1)
    expect(results[0].mode).toBe("opencode-direct")
    expect(results[0].passed).toBe(status === 200)
    expect(results[0].http_status).toBe(status)
    expect(JSON.parse(readFileSync(configFile, "utf8")).requestFile).toBeUndefined()
    expect(existsSync(proxyMarker)).toBe(false)
    expect(existsSync(join(outputDir, "vekil-direct"))).toBe(false)
    expect(JSON.parse(readFileSync(join(outputDir, "results.json"), "utf8"))).toEqual(results)
  })
}
