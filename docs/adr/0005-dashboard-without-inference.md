# 0005: Keep the dashboard usable without model inference

Status: Accepted

## Context

The dashboard polls local statistics continuously. An optional model-generated insight has a different latency and failure profile, and the most recent rate bucket may still be filling.

## Decision

Render the live summary from local statistics and display generated insight in a separate, on-demand panel. Send the already displayed summary with the insight request so the model can add information instead of repeating it. A generation failure hides only the insight. Reuse the latest polled snapshot on filter and sort changes, showing only positive values in error or latency breakdowns. Average over complete seconds, excluding the active bucket so headline rates don't dip on every poll.

## Consequences

The dashboard stays useful with insight disabled or its provider unavailable. Snapshot controls don't wait for the next poll, and displayed rates lag the active bucket by one second.
