---
name: delete-comment
description: Audit and remove prose comments from Vekil. Use for a comment-cleanup batch, including categorizing comments, moving design decisions into ADRs, and tracking implementation shortcuts as issues.
---

# Delete Comment

Use the setup in [Development](../../../docs/development.md#lint). Save the output of `scripts/comment-lint.py --inventory` with that Python environment, then review the requested paths or one coherent cluster. Read each comment together with its code, nearby tests, and relevant docs. The inventory is a locator, not a verdict; check lexical boundaries before deleting.

| Category | Action |
| --- | --- |
| Tautology: repeats the name, code, or assertion | Delete it. |
| Stale: contradicts current code or describes a removed implementation | Verify against current code, then delete it. |
| Excuse: explains away an avoidable implementation shortcut | Verify the concrete limitation, look for an existing issue, and record its link or draft a focused issue with acceptance criteria. File a draft only if this run authorizes filing; otherwise leave it ready for review. Delete the comment once the debt is recorded. Do not silently fix the implementation in the cleanup. |
| Decision Record: preserves a non-obvious invariant, tradeoff, or rejected option | Capture the rationale in the matching [ADR](../../../docs/adr/README.md) or add a numbered one. Update the index, then delete the source comment. |

Leave executable directives, cgo preambles, vendored code, and fixture text intact. For mixed comments keep the decision rationale or issue context outside source before removing the whole block. If a classification is uncertain, mark it as provisional and preserve its exact source location and revision; don't treat an uncertain note as an established design decision.

Record the audited paths, classifications, and ADR or issue references by original location in [the audit log](../../../docs/comment-audit.md). Run `make lint-comments` with the same Python; it requires a zero-prose tree. Use `COMMENT_BASE` only for incremental work on a branch predating the full cleanup. Check that only comments and associated whitespace changed in existing source, then run the affected tests and repeat the inventory. Report removed comment-only lines separately from shortened inline lines.
