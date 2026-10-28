#!/usr/bin/env bun

// Run the actual pinned OpenCode CLI as a live availability baseline. Replaying
// its native identity through Vekil is restricted to loopback mocks; the live
// Vekil diagnostics must identify themselves as Vekil and pass independently.
import { spawn, type ChildProcess } from "node:child_process"
import { existsSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs"
import { createHash, randomBytes } from "node:crypto"
import { join, resolve } from "node:path"
import { tmpdir } from "node:os"
import { checkFreeModel, checkUpstream, EXPECTED, MODEL, UPSTREAM, runControlCase, stopControlChildren, validateCompletion } from "./live-zen-control"
import { OPENCODE_VERSION, sessionID } from "./opencode-zen-metadata.mjs"

const ROOT = resolve(import.meta.dir, "..")
const children = new Set<ChildProcess>()
const MAX_OUTPUT = 1024 * 1024
const TIMEOUT_MS = 45_000
const modes = ["opencode-direct", "vekil-direct"] as const
const metadataNames = ["user-agent", "x-opencode-client", "x-opencode-project", "x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"]
type Options = { opencodeBin: string; proxyBin: string; modelsFile: string; outputDir: string; upstream?: string }
type Result = { mode: typeof modes[number]; passed: boolean; cli_exit?: number | null; http_status?: number; requests?: number; error?: string; log_id?: string }

function stopGroup(child: ChildProcess, signal: NodeJS.Signals) {
  if (child.pid) try { process.kill(-child.pid, signal) } catch (error: any) { if (error.code !== "ESRCH") throw error }
}

export async function runCLI(cmd: string[], cwd: string, env: Record<string, string>, timeout = TIMEOUT_MS) {
  const child = spawn(cmd[0], cmd.slice(1), { cwd, env, detached: true, stdio: ["ignore", "pipe", "pipe"] })
  children.add(child)
  let stdout = "", stderr = "", error = "", outputBytes = 0
  let killer: ReturnType<typeof setTimeout> | undefined
  const abort = (reason: string) => {
    if (error) return
    error = reason
    stopGroup(child, "SIGTERM")
    killer = setTimeout(() => stopGroup(child, "SIGKILL"), 2000)
  }
  const timer = setTimeout(() => abort("CLI deadline exceeded"), timeout)
  child.stdout!.on("data", (value: Buffer) => { outputBytes += value.length; if (outputBytes > MAX_OUTPUT) abort("CLI output limit exceeded"); else stdout += value.toString() })
  child.stderr!.on("data", (value: Buffer) => { outputBytes += value.length; if (outputBytes > MAX_OUTPUT) abort("CLI output limit exceeded"); else stderr += value.toString() })
  child.once("exit", () => {
    stopGroup(child, "SIGTERM")
    killer ??= setTimeout(() => stopGroup(child, "SIGKILL"), 2000)
  })
  try {
    const code = await new Promise<number | null>((ok, fail) => { child.once("error", fail); child.once("close", ok) })
    if (error) throw new Error(error)
    return { code, stdout, stderr }
  } finally {
    clearTimeout(timer)
    if (killer) clearTimeout(killer)
    stopGroup(child, "SIGKILL")
    children.delete(child)
  }
}

export function validateNativeOutput(stdout: string) {
  let text = "", count = 0
  for (const line of stdout.split(/\r?\n/).filter((line) => line.trim())) {
    const event = JSON.parse(line)
    if (!["text", "step_start", "step_finish"].includes(event.type)) throw new Error(`unexpected CLI event: ${event.type}`)
    if (event.type === "text") {
      if (typeof event.part?.text !== "string") throw new Error("malformed CLI text event")
      text += event.part.text
      count++
    }
  }
  if (!count || text.trim() !== EXPECTED) throw new Error("missing or mismatched native CLI completion")
}

export function validateNativeEvidence(directory: string, stdout: string, code: number | null) {
  if (code !== 0) throw new Error(`OpenCode CLI exited ${code}`)
  const observer = JSON.parse(readFileSync(join(directory, "observer.json"), "utf8"))
  if (observer.app?.name !== "cli" || observer.app?.version !== OPENCODE_VERSION || observer.requests !== 1 || observer.responses !== 1 || !Array.isArray(observer.violations) || observer.violations.length) throw new Error("native observer did not prove one guarded CLI request/response")
  const request = JSON.parse(readFileSync(join(directory, "request.json"), "utf8"))
  if (request.model !== MODEL || request.headers?.["x-opencode-client"] !== "cli" || !/^[0-9a-f]{64}$/.test(request.body_sha256)) throw new Error("invalid native request evidence")
  const response = JSON.parse(readFileSync(join(directory, "response.json"), "utf8"))
  if (response.status !== 200 || !response.content_type?.includes("text/event-stream")) throw new Error(`native upstream HTTP ${response.status}; no completion`)
  const usage = validateCompletion(readFileSync(join(directory, "response.txt"), "utf8"))
  validateNativeOutput(stdout)
  return usage
}

async function runDirectCase(options: Options, binary: string, workspace: string, session: string): Promise<Result> {
  const directory = join(options.outputDir, "opencode-direct"), home = join(options.outputDir, "_private", "home")
  mkdirSync(directory)
  mkdirSync(home, { mode: 0o700 })
  const result: Result = { mode: "opencode-direct", passed: false }
  const upstream = options.upstream ?? UPSTREAM
  try {
    const baseURL = upstream.slice(0, -"/chat/completions".length)
    const config = {
      plugins: [{ package: join(ROOT, "scripts/zen-native-observer"), options: { directory, upstream, ...(upstream !== UPSTREAM ? { requestFile: join(options.outputDir, "_private", "request.txt") } : {}) } }],
      providers: { opencode: { settings: { apiKey: "public", baseURL }, models: { [MODEL]: { settings: { baseURL } } } } },
    }
    const env = { HOME: home, PATH: process.env.PATH ?? "/usr/bin:/bin", TMPDIR: home, XDG_CONFIG_HOME: join(home, "config"), XDG_DATA_HOME: join(home, "data"), XDG_CACHE_HOME: join(home, "cache"), XDG_STATE_HOME: join(home, "state"), OPENCODE_CONFIG_CONTENT: JSON.stringify(config), NO_COLOR: "1", TERM: "dumb" }
    const run = await runCLI([binary, "run", "--standalone", "--format", "json", "--title", "Zen CI", "--session", session, "--model", `opencode/${MODEL}`, `Reply with exactly ${EXPECTED}. Do not call tools.`], workspace, env)
    result.cli_exit = run.code
    writeFileSync(join(directory, "cli.stdout.jsonl"), run.stdout.replaceAll(home, "<home>").replaceAll(workspace, "<workspace>"))
    writeFileSync(join(directory, "cli.stderr.txt"), run.stderr.replaceAll(home, "<home>").replaceAll(workspace, "<workspace>"))
    const usage = validateNativeEvidence(directory, run.stdout, run.code)
    writeFileSync(join(directory, "usage.json"), JSON.stringify(usage, null, 2))
    result.passed = true
  } catch (error) { result.error = error instanceof Error ? error.message : String(error) }
  finally {
    for (const [file, field] of [["observer.json", "requests"], ["response.json", "http_status"]] as const) {
      try {
        const saved = JSON.parse(readFileSync(join(directory, file), "utf8"))
        result[field] = field === "requests" ? saved.requests : saved.status
        if (saved.log_id) result.log_id = saved.log_id
      } catch {}
    }
    writeFileSync(join(directory, "result.json"), JSON.stringify(result, null, 2))
  }
  return result
}

export function readNativeReplay(outputDir: string, upstream: string) {
  checkUpstream(upstream)
  if (upstream === UPSTREAM) throw new Error("native identity replay requires a loopback mock; live Zen is baseline-only")
  const body = readFileSync(join(outputDir, "_private", "request.txt"), "utf8")
  const receipt = JSON.parse(readFileSync(join(outputDir, "opencode-direct", "request.json"), "utf8"))
  if (Buffer.byteLength(body) > 128 * 1024 || JSON.parse(body).model !== MODEL || receipt.model !== MODEL || createHash("sha256").update(body).digest("hex") !== receipt.body_sha256) throw new Error("invalid native request capture")
  const headers = receipt.headers
  if (!headers || Object.keys(headers).some((key) => !metadataNames.includes(key) || typeof headers[key] !== "string") || headers["x-opencode-client"] !== "cli") throw new Error("invalid native request metadata")
  return { body, headers }
}

export async function runNativeControls(options: Options) {
  const upstream = options.upstream ?? UPSTREAM
  checkUpstream(upstream)
  checkFreeModel(readFileSync(options.modelsFile, "utf8"))
  const binary = Bun.which(options.opencodeBin)
  if (!binary) throw new Error("pinned OpenCode CLI is required")
  mkdirSync(options.outputDir, { recursive: true, mode: 0o700 })
  if (readdirSync(options.outputDir).length) throw new Error("native diagnostic output directory must be empty; refusing stale evidence")
  const workspace = join(options.outputDir, "_private", "workspace")
  mkdirSync(workspace, { recursive: true })
  const version = await runCLI([binary, "--version"], workspace, { HOME: workspace, PATH: process.env.PATH ?? "/usr/bin:/bin", TMPDIR: workspace }, 10_000)
  if (version.code !== 0 || version.stdout.trim() !== `opencode v${OPENCODE_VERSION}`) throw new Error(`expected OpenCode ${OPENCODE_VERSION}, got ${version.stdout.trim()}`)
  const results: Result[] = [await runDirectCase(options, binary, workspace, sessionID())]
  writeFileSync(join(options.outputDir, "results.json"), JSON.stringify(results, null, 2))
  // Free Zen does not permit proxy access. Never replay native identity live,
  // even when a native request succeeded or left capture files after a refusal.
  if (upstream === UPSTREAM) return results
  try {
    const { body, headers } = readNativeReplay(options.outputDir, upstream)
    results.push({ ...await runControlCase("vekil-direct", options, body, headers), mode: "vekil-direct" })
  } catch (error) {
    const result: Result = { mode: "vekil-direct", passed: false, error: error instanceof Error ? error.message : String(error) }
    results.push(result)
    mkdirSync(join(options.outputDir, "vekil-direct"), { recursive: true })
    writeFileSync(join(options.outputDir, "vekil-direct", "result.json"), JSON.stringify(result, null, 2))
  }
  writeFileSync(join(options.outputDir, "results.json"), JSON.stringify(results, null, 2))
  return results
}

if (import.meta.main) {
  for (const signal of ["SIGTERM", "SIGINT"] as const) process.once(signal, () => { for (const child of children) stopGroup(child, "SIGKILL"); stopControlChildren(); process.exit(signal === "SIGINT" ? 130 : 143) })
  try {
    if (!process.env.ZEN_FREE_MODELS_FILE) throw new Error("ZEN_FREE_MODELS_FILE is required")
    const outputDir = resolve(process.env.ZEN_NATIVE_DIR ?? join(tmpdir(), `vekil-zen-native-${randomBytes(8).toString("hex")}`))
    const results = await runNativeControls({ opencodeBin: process.env.OPENCODE_BIN ?? "opencode", proxyBin: process.env.PROXY_BIN ?? join(ROOT, "vekil"), modelsFile: process.env.ZEN_FREE_MODELS_FILE, outputDir })
    const summary = "## Actual OpenCode CLI availability baseline\n\n| Path | Exact completion and free billing | HTTP |\n|---|---|---|\n" + results.map((r) => `| ${r.mode} | ${r.passed ? "PASS" : "FAIL"} | ${r.http_status ?? "not reached"} |`).join("\n") + "\n\nOnly the actual OpenCode CLI contacts live Zen in this baseline. Its native identity is never replayed through Vekil live; exact native-request replay is restricted to loopback tests. A passing baseline cannot substitute for the separately required Vekil-identified controls and generic-client smoke.\n"
    writeFileSync(join(outputDir, "summary.md"), summary)
    if (process.env.GITHUB_STEP_SUMMARY) writeFileSync(process.env.GITHUB_STEP_SUMMARY, summary, { flag: "a" })
    console.log(summary)
    for (const result of results) if (!result.passed) console.error(`${result.mode}: ${result.error}`)
    process.exitCode = results.every((r) => r.passed) ? 0 : 1
  } catch (error) { console.error(String(error)); process.exitCode = 1 }
}
