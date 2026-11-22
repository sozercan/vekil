# 0004: Verify macOS durable state through open descriptors

Status: Accepted

## Context

bbolt uses `os.File.Sync` for both data and metadata commits. Go on Darwin can fall back to ordinary `fsync` when `F_FULLFSYNC` is unsupported. Darwin ACLs can also grant access independently of mode bits, so a private-looking path can still be shared.

## Decision

Require writable local APFS and verify full-sync support on the actual opened state descriptors before relying on bbolt. Reject an ACL on either the state directory or database, in addition to checking mode bits. Use the descriptor-based `fgetattrlist` Darwin syscall: the version of `x/sys` used here has no wrapper for it, path-based checks would not establish properties of the opened object, and CGO-disabled builds must remain supported. `ATTR_CMN_EXTENDED_SECURITY` with `FSOPT_REPORT_FULLSIZE` makes an absent ACL report only the attribute-reference header; a larger or unexpected result is rejected without reading ACL entries.

## Consequences

Durable state fails closed if the OS or filesystem can't prove the necessary conditions. The staticcheck suppression on the Darwin syscall is scoped to that call while no suitable CGO-free wrapper exists. See [State Recovery](../state-recovery.md) for supported storage and recovery guarantees.
