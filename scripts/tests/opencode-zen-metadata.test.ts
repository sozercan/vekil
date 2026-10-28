import { expect, test } from "bun:test"
import { spawnSync } from "node:child_process"
import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { pathToFileURL } from "node:url"
import { compatibilityHeaders, OPENCODE_VERSION, sessionID } from "../opencode-zen-metadata.mjs"

const SCRIPT = resolve(import.meta.dir, "../opencode-zen-metadata.mjs")
const SESSION_SHAPE = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/
const node = (...args: string[]) => {
  const result = spawnSync("node", args, { encoding: "utf8" })
  expect(result.error).toBeUndefined()
  return result
}

for (const [now, prefix] of [[0, "fffffffffffe"], [1, "ffffffffeffe"], [1_700_000_000_000, "4301a97ffffe"]] as const) {
  test(`session has the native shape and deterministic timestamp prefix at ${now}`, () => {
    const session = sessionID(now)
    expect(session).toMatch(SESSION_SHAPE)
    expect(session).toHaveLength(30)
    expect(session.slice(4, 16)).toBe(prefix)
  })
}

test("metadata pins the compatibility version and honestly identifies Vekil", () => {
  expect(OPENCODE_VERSION).toBe("2.1.2")
  const session = sessionID(0)
  expect(compatibilityHeaders(session)).toEqual({
    "user-agent": "opencode/compat/2.1.2/vekil",
    "x-opencode-client": "vekil",
    "x-opencode-project": "vekil",
    "x-opencode-session": session,
    "x-opencode-session-id": session,
    "x-session-affinity": session,
    "x-session-id": session,
  })
})

test("default metadata generates one native-shaped session for all affinity headers", () => {
  const headers = compatibilityHeaders()
  const session = headers["x-opencode-session"]
  expect(session).toMatch(SESSION_SHAPE)
  expect(session).toHaveLength(30)
  expect(headers).toEqual(compatibilityHeaders(session))
})

test("Node imports the module without running its CLI", () => {
  const result = node("--input-type=module", "--eval", `
    const { OPENCODE_VERSION, sessionID, compatibilityHeaders } = await import(${JSON.stringify(pathToFileURL(SCRIPT).href)})
    const session = sessionID(0)
    console.log(JSON.stringify({ version: OPENCODE_VERSION, session, headers: compatibilityHeaders(session) }))
  `)
  expect(result.status).toBe(0)
  expect(result.stderr).toBe("")
  const value = JSON.parse(result.stdout)
  expect(value.version).toBe(OPENCODE_VERSION)
  expect(value.session).toMatch(/^ses_fffffffffffe[0-9A-Za-z]{14}$/)
  expect(value.headers).toEqual(compatibilityHeaders(value.session))
})

test("Node CLI prints the pinned version", () => {
  const result = node(SCRIPT, "--version")
  expect(result.status).toBe(0)
  expect(result.stderr).toBe("")
  expect(result.stdout).toBe(`${OPENCODE_VERSION}\n`)
})

test("Node CLI prints one native-shaped session ID", () => {
  const result = node(SCRIPT, "--session-id")
  expect(result.status).toBe(0)
  expect(result.stderr).toBe("")
  expect(result.stdout).toMatch(/^ses_[0-9a-f]{12}[0-9A-Za-z]{14}\n$/)
})

for (const args of [[], ["--unknown"], ["--version", "--unknown"], ["--session-id", "--unknown"], ["--version", "--session-id"]]) {
  test(`Node CLI rejects invalid arguments: ${args.join(" ") || "(none)"}`, () => {
    const result = node(SCRIPT, ...args)
    expect(result.status).toBe(1)
    expect(result.stdout).toBe("")
    expect(result.stderr).toContain("Usage:")
  })
}

test("example User-Agent uses the pinned compatibility version", () => {
  const example = readFileSync(resolve(import.meta.dir, "../../examples/opencode-zen-free.yaml"), "utf8")
  const userAgent = example.match(/^\s+User-Agent: (.+)$/m)?.[1]
  expect(userAgent).toBe(`opencode/compat/${OPENCODE_VERSION}/vekil`)
})
