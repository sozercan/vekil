# 0003: Bound menubar and serve logs per file

Status: Accepted

## Context

The menubar and `vekil serve --log-file` can generate more output than should be retained locally. They reopen log files left by earlier runs, whose permissions aren't changed by a new open call's create mode.

## Decision

Use a private append-only current file and retain at most one `.1` backup. Rotate before a write would exceed the current file's cap. Explicitly enforce private permissions on an existing current file as well as newly created files. If one input entry exceeds the cap, keep the beginning, discard the excess, and report the entire entry as consumed on success; retrying the intentionally discarded suffix would defeat the retention bound.

## Consequences

Only the current file and its immediate predecessor are kept. An oversized entry may be truncated even though the caller receives no error. The file size limit controls disk usage, not the size of accepted messages.

Agent launches use a separate, exclusively created log file per launch. They reject existing paths and have neither rotation nor the per-entry truncation performed by the rotating writer.
