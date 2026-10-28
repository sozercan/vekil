#!/usr/bin/env node

import { randomBytes } from "node:crypto"
import { pathToFileURL } from "node:url"

export const OPENCODE_VERSION = "2.1.2"

export function sessionID(now = Date.now()) {
  // OpenCode's descending 48-bit timestamp/counter, then 14 base-62 characters.
  const timestamp = (~(BigInt(now) * 4096n + 1n) & ((1n << 48n) - 1n)).toString(16).padStart(12, "0")
  const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
  return "ses_" + timestamp + Array.from(randomBytes(14), (b) => alphabet[b % alphabet.length]).join("")
}

export function compatibilityHeaders(session = sessionID()) {
  return {
    "user-agent": `opencode/compat/${OPENCODE_VERSION}/vekil`,
    "x-opencode-client": "vekil",
    "x-opencode-project": "vekil",
    "x-opencode-session": session,
    "x-opencode-session-id": session,
    "x-session-affinity": session,
    "x-session-id": session,
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const args = process.argv.slice(2)
  if (args.length === 1 && args[0] === "--version") console.log(OPENCODE_VERSION)
  else if (args.length === 1 && args[0] === "--session-id") console.log(sessionID())
  else {
    console.error("Usage: opencode-zen-metadata.mjs <--version|--session-id>")
    process.exitCode = 1
  }
}
