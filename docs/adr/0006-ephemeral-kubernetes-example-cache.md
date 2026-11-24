# 0006: Keep the example Kubernetes token cache ephemeral

Status: Accepted

## Context

A sample deployment should work without assuming a particular storage class or teaching users to put credentials in the manifest. A writable cache is still useful across container restarts in the same Pod.

## Decision

Mount an `emptyDir` for the cache in the checked-in Kubernetes example. Don't implicitly provision persistent storage. For persistent authentication, configure a Secret-backed `COPILOT_GITHUB_TOKEN` or replace the sample volume with a PVC.

## Consequences

The cache is lost when the Pod is replaced, rescheduled, or rolled out. The sample isn't a durable credential deployment by itself. See [Getting Started](../getting-started.md#kubernetes) for deployment options.
