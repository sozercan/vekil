// Test-only observation of the actual OpenCode CLI. It records the native
// request for a separate, redirect-rejecting Vekil replay, not an upstream relay.
import { createHash } from "node:crypto"
import { writeFileSync } from "node:fs"
import { join } from "node:path"

const MODEL = "glm-5.3-flash-free"
const metadataNames = ["user-agent", "x-opencode-client", "x-opencode-project", "x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"]

async function readLimited(body, limit) {
  if (!body) throw new Error("missing diagnostic body")
  const reader = body.getReader(), chunks = []
  let length = 0
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      length += value.byteLength
      if (length > limit) throw new Error("diagnostic body limit exceeded")
      chunks.push(value)
    }
  } catch (error) {
    // A canceled clone may wait for its original branch. Do not deadlock the
    // observer while the native client has not received that branch yet.
    void reader.cancel().catch(() => {})
    throw error
  } finally { reader.releaseLock() }
  return Buffer.concat(chunks).toString("utf8")
}

export async function setupObserver(ctx) {
  const { directory, upstream, requestFile } = ctx.options
  const state = { app: ctx.app, requests: 0, responses: 0, violations: [] }
  const save = () => writeFileSync(join(directory, "observer.json"), JSON.stringify(state, null, 2))
  const reject = (message) => { state.violations.push(message); save(); throw new Error(message) }
  save()
  await ctx.session.hook("retry", (event) => { event.decision = { retry: false } })
  await ctx.permission.hook("evaluate", (event) => { event.effect = "deny" })
  // Preserve native definitions, but never execute any model-selected tool.
  await ctx.tool.transform((editor) => {
    for (const tool of editor.list()) editor.update(tool.id, (value) => {
      value.execute = async () => reject("diagnostic tool execution denied")
    })
  })
  await ctx.session.hook("http.request", async (event) => {
    state.requests++
    save()
    if (event.kind !== "primary" || state.requests !== 1) reject("diagnostic permits one primary request only")
    if (event.request.url !== upstream || event.request.method !== "POST" || event.request.headers.get("authorization") !== "Bearer public") reject("unexpected diagnostic inference destination or credential")
    const raw = await readLimited(event.request.clone().body, 128 * 1024)
    if (JSON.parse(raw).model !== MODEL) reject("unexpected diagnostic model")
    const headers = Object.fromEntries(metadataNames.flatMap((name) => {
      const value = event.request.headers.get(name)
      return value === null ? [] : [[name, value]]
    }))
    writeFileSync(requestFile, raw, { mode: 0o600 })
    writeFileSync(join(directory, "request.json"), JSON.stringify({ model: MODEL, body_sha256: createHash("sha256").update(raw).digest("hex"), headers }, null, 2))
  })
  await ctx.session.hook("http.response", async (event) => {
    state.responses++
    save()
    if (state.responses !== 1) reject("unexpected extra diagnostic response")
    const response = event.response
    writeFileSync(join(directory, "response.json"), JSON.stringify({ status: response.status, content_type: response.headers.get("content-type"), log_id: response.headers.get("x-opencode-log-id") }, null, 2))
    writeFileSync(join(directory, "response.txt"), await readLimited(response.clone().body, 1024 * 1024))
  })
}

export default { id: "vekil.zen-ci-observer", setup: setupObserver }
