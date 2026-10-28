import { afterAll, expect, test } from "bun:test"
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { checkFreeModel, EXPECTED, FIXTURE, MODEL, runControls, sessionID, validateCompletion } from "../live-zen-control"

const directories: string[] = []
const temporary = () => { const path = mkdtempSync(join(tmpdir(), "zen-control-test-")); directories.push(path); return path }
afterAll(() => { for (const path of directories) rmSync(path, { recursive: true, force: true }) })
const usage = { prompt_tokens: 10, completion_tokens: 4, cost: 0, opencode: { billing_source: "free" } }
const completion = (content = EXPECTED, extra: Record<string, unknown> = {}) =>
  `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content }, finish_reason: "stop" }], ...extra })}\n\n` +
  `data: ${JSON.stringify({ choices: [], usage })}\n\ndata: [DONE]\n\n`

// These are offline checks. The integration cases launch the real built Vekil
// and the actual Bun relay, with a loopback mock as the only possible upstream.
test("requires an exact completed answer and explicit free usage", () => {
  expect(validateCompletion(completion())).toEqual(usage)
  expect(validateCompletion(completion().replaceAll("\n", "\r\n"))).toEqual(usage)
})

test("accepts text deltas before a separate stop and usage-only trailer", () => {
  const text = `data: {"choices":[{"index":0,"delta":{"content":"ZEN_CONTROL_"},"finish_reason":null}]}\n\n` +
    `data: {"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}\n\n` +
    completion("")
  expect(validateCompletion(text)).toEqual(usage)
})

for (const [name, text] of [
  ["wrong answer", completion("wrong")],
  ["nonzero choice", completion().replace('"index":0', '"index":1')],
  ["missing choice index", completion().replace('"index":0,', '')],
  ["split choices", `data: {"choices":[{"index":0,"delta":{"content":"ZEN_CONTROL_OK"}}]}\n\ndata: {"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}\n\ndata: {"choices":[],"usage":${JSON.stringify(usage)}}\n\ndata: [DONE]\n\n`],
  ["multiple choices", completion().replace('"choices":[{', '"choices":[{"index":1,"delta":{},"finish_reason":"stop"},{')],
  ["missing DONE", completion().replace("data: [DONE]\n\n", "")],
  ["unterminated frame", completion().trimEnd()],
  ["missing stop", completion().replace('"finish_reason":"stop"', '"finish_reason":null')],
  ["text after stop", `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\n` + completion().replace('"finish_reason":"stop"', '"finish_reason":null')],
  ["answer completed after stop", completion("ZEN_CONTROL_").replace("data: [DONE]\n\n", `data: {"choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":null}]}\n\ndata: [DONE]\n\n`)],
  ["duplicate stop", completion().replace("data: [DONE]\n\n", `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n`)],
  ["choice after stop", completion().replace("data: [DONE]\n\n", `data: {"choices":[{"index":0,"delta":{},"finish_reason":null}]}\n\ndata: [DONE]\n\n`)],
  ["truncation", completion().replace('"finish_reason":"stop"', '"finish_reason":"length"')],
  ["tool call", completion().replace('"content":"ZEN_CONTROL_OK"', '"content":"ZEN_CONTROL_OK","tool_calls":[{"id":"call_fake"}]')],
  ["malformed tool calls", completion().replace('"content":"ZEN_CONTROL_OK"', '"content":"ZEN_CONTROL_OK","tool_calls":{}')],
  ["missing usage", completion().replace(/data: \{"choices":\[\],"usage":.*?\}\n\n/, "")],
  ["missing cost", completion().replace(',"cost":0', "")],
  ["nonzero cost", completion().replace('"cost":0', '"cost":0.01')],
  ["paid billing", completion().replace('"billing_source":"free"', '"billing_source":"balance"')],
  ["zero usage", completion().replace('"prompt_tokens":10', '"prompt_tokens":0')],
  ["malformed JSON", "data: {bad}\n\ndata: [DONE]\n\n"],
  ["provider error", "data: {\"error\":{\"message\":\"refused\"}}\n\n"],
  ["data after DONE", completion() + "data: {}\n\n"],
] as const) test(`rejects ${name}`, () => expect(() => validateCompletion(text)).toThrow())

test("free label and native Chat eligibility are mandatory", () => {
  expect(() => checkFreeModel(`${MODEL}\tFree label\t/chat/completions\n`)).not.toThrow()
  for (const tsv of ["", `paid-model\tPaid\t/chat/completions\n`, `${MODEL}\tFree\t/responses\n`]) expect(() => checkFreeModel(tsv)).toThrow()
})

test("session matches the pinned native identifier shape", () => {
  expect(sessionID(0)).toMatch(/^ses_fffffffffffe[0-9A-Za-z]{14}$/)
  expect(sessionID()).toHaveLength(30)
})

test("fixture is fixed free-model data with no captured local identity", () => {
  const raw = readFileSync(FIXTURE, "utf8"), body = JSON.parse(raw)
  expect(body.model).toBe(MODEL)
  expect(body.tools.length).toBe(12)
  expect(body.messages[1].content).toContain(EXPECTED)
  for (const value of ["/private/", "/Users/", "sozercan", "ses_", "Bearer "]) expect(raw).not.toContain(value)
})

for (const scenario of ["pass", "quota", "wrong-answer", "post-stop"] as const) {
  test(`real Vekil and relay: ${scenario}; no skips or hidden retries`, async () => {
    const directory = temporary(), requests: { body: any; headers: Headers }[] = []
    const server = Bun.serve({ hostname: "127.0.0.1", port: 0, async fetch(request) {
      requests.push({ body: await request.json(), headers: new Headers(request.headers) })
      if (scenario === "quota") return Response.json({ error: { type: "FreeUsageLimitError", message: "Free usage limit reached" } }, { status: 429, headers: { "retry-after": "60" } })
      const text = scenario === "post-stop"
        ? completion("").replace("data: [DONE]\n\n", `data: {"choices":[{"index":0,"delta":{"content":"${EXPECTED}"},"finish_reason":null}]}\n\ndata: [DONE]\n\n`)
        : completion(scenario === "pass" ? EXPECTED : "wrong")
      return new Response(text, { headers: { "content-type": "text/event-stream" } })
    } })
    const modelsFile = join(directory, "models.tsv")
    writeFileSync(modelsFile, `${MODEL}\tFree\t/chat/completions\n`)
    try {
      const results = await runControls({ proxyBin: resolve(process.env.PROXY_BIN ?? "vekil"), modelsFile, outputDir: join(directory, "results"), upstream: `http://127.0.0.1:${server.port}/inference/openai/v1/chat/completions` })
      expect(results.map((r) => r.mode)).toEqual(["bun-reference", "vekil-bun-relay", "vekil-direct"])
      expect(results.map((r) => r.passed)).toEqual([scenario === "pass", scenario === "pass", scenario === "pass"])
      expect(results.map((r) => r.http_status)).toEqual([scenario === "quota" ? 429 : 200, scenario === "quota" ? 429 : 200, scenario === "quota" ? 429 : 200])
      expect(requests).toHaveLength(3)
      const fixture = JSON.parse(readFileSync(FIXTURE, "utf8"))
      const sessions = new Set<string>()
      for (const { body, headers } of requests) {
        expect(body.messages).toEqual(fixture.messages)
        expect(body.tools).toEqual(fixture.tools)
        expect(body.model).toBe(MODEL)
        expect(headers.get("authorization")).toBe("Bearer public")
        expect(headers.get("x-opencode-client")).toBe("vekil")
        expect(headers.get("user-agent")).toBe("opencode/compat/2.1.2/vekil")
        sessions.add(headers.get("x-opencode-session")!)
      }
      expect(sessions.size).toBe(1)
      expect(requests[0].body.parallel_tool_calls).toBeUndefined()
      expect(requests[1].body.parallel_tool_calls).toBe(true)
      expect(requests[2].body.parallel_tool_calls).toBe(true)
      expect(results[0].relay_requests).toBeUndefined()
      expect(results[1].relay_requests).toBe(1)
    } finally { server.stop(true) }
  }, 15_000)
}

test("rejects remote overrides before starting any control", async () => {
  await expect(runControls({ proxyBin: "unused", modelsFile: "unused", outputDir: "unused", upstream: "https://example.com/paid" })).rejects.toThrow("loopback mock")
})
