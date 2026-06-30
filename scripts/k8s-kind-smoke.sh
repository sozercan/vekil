#!/usr/bin/env bash

set -euo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

CLUSTER_NAME="${K8S_SMOKE_CLUSTER_NAME:-vekil-smoke-${GITHUB_RUN_ID:-local}}"
NAMESPACE="${K8S_SMOKE_NAMESPACE:-vekil-smoke}"
IMAGE="${K8S_SMOKE_IMAGE:-vekil:k8s-smoke}"
LOCAL_PORT="${K8S_SMOKE_LOCAL_PORT:-18080}"
LIVENESS_WINDOW_SECONDS="${K8S_SMOKE_LIVENESS_WINDOW_SECONDS:-15}"
DELETE_CLUSTER="${K8S_SMOKE_DELETE_CLUSTER:-1}"
PORT_FORWARD_LOG="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/vekil-kind-port-forward.log"

port_forward_pid=""

redact_device_codes() {
  sed -E 's/(enter code: )[A-Z0-9-]+/\1[REDACTED_DEVICE_CODE]/g'
}

cleanup() {
  local rc=$?

  if [[ -n "${port_forward_pid}" ]] && kill -0 "${port_forward_pid}" 2>/dev/null; then
    kill "${port_forward_pid}" 2>/dev/null || true
    wait "${port_forward_pid}" 2>/dev/null || true
  fi

  if [[ ${rc} -ne 0 ]]; then
    log "Kubernetes smoke debug output"
    kubectl -n "${NAMESPACE}" get all -o wide >&2 || true
    kubectl -n "${NAMESPACE}" describe pod -l app=vekil >&2 || true
    kubectl -n "${NAMESPACE}" logs -l app=vekil --all-containers --prefix 2>/dev/null | redact_device_codes >&2 || true
    if [[ -f "${PORT_FORWARD_LOG}" ]]; then
      log "kubectl port-forward log"
      cat "${PORT_FORWARD_LOG}" >&2 || true
    fi
  fi

  if [[ "${DELETE_CLUSTER}" != "0" ]]; then
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  fi

  exit "${rc}"
}
trap cleanup EXIT

require_cmd docker
require_cmd kind
require_cmd kubectl
require_cmd curl

cd "${REPO_ROOT}"

log "Building ${IMAGE}"
docker build -t "${IMAGE}" .

log "Creating kind cluster ${CLUSTER_NAME}"
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
kind create cluster --name "${CLUSTER_NAME}" --wait 120s

log "Loading ${IMAGE} into ${CLUSTER_NAME}"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

log "Deploying Vekil without credentials"
kubectl create namespace "${NAMESPACE}"
kubectl -n "${NAMESPACE}" apply -f - <<EOF_MANIFEST
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vekil
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vekil
  template:
    metadata:
      labels:
        app: vekil
    spec:
      containers:
        - name: vekil
          image: ${IMAGE}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 1337
          env:
            - name: HOST
              value: "0.0.0.0"
            - name: PORT
              value: "1337"
            - name: TOKEN_DIR
              value: /home/nonroot/.config/vekil
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
            initialDelaySeconds: 15
            periodSeconds: 2
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /readyz
              port: http
            initialDelaySeconds: 1
            periodSeconds: 2
            failureThreshold: 1
          volumeMounts:
            - name: token-cache
              mountPath: /home/nonroot/.config/vekil
      volumes:
        - name: token-cache
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: vekil
spec:
  selector:
    app: vekil
  ports:
    - name: http
      port: 1337
      targetPort: http
EOF_MANIFEST

pod=""
for _ in $(seq 1 60); do
  pod="$(kubectl -n "${NAMESPACE}" get pod -l app=vekil -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${pod}" ]]; then
    phase="$(kubectl -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    running_since="$(kubectl -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="vekil")].state.running.startedAt}' 2>/dev/null || true)"
    if [[ "${phase}" == "Running" && -n "${running_since}" ]]; then
      break
    fi
  fi
  sleep 2
done

[[ -n "${pod}" ]] || die "Vekil pod was not created"
running_since="$(kubectl -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="vekil")].state.running.startedAt}' 2>/dev/null || true)"
[[ -n "${running_since}" ]] || die "Vekil container did not start"

log "Port-forwarding ${pod} to localhost:${LOCAL_PORT}"
rm -f "${PORT_FORWARD_LOG}"
kubectl -n "${NAMESPACE}" port-forward "pod/${pod}" "${LOCAL_PORT}:1337" >"${PORT_FORWARD_LOG}" 2>&1 &
port_forward_pid="$!"

health_body=""
for _ in $(seq 1 30); do
  if health_body="$(curl -fsS "http://127.0.0.1:${LOCAL_PORT}/healthz" 2>/dev/null)"; then
    break
  fi
  sleep 1
done

[[ "${health_body}" == '{"status":"ok"}' ]] || die "unexpected /healthz response: ${health_body:-<none>}"

ready_body_file="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/vekil-kind-readyz.json"
ready_code="$(curl -sS -o "${ready_body_file}" -w '%{http_code}' "http://127.0.0.1:${LOCAL_PORT}/readyz")"
if [[ "${ready_code}" != "503" ]]; then
  die "expected /readyz HTTP 503 while startup auth is pending, got ${ready_code}: $(cat "${ready_body_file}" 2>/dev/null || true)"
fi
if ! grep -q 'not_ready' "${ready_body_file}"; then
  die "expected /readyz not_ready body, got: $(cat "${ready_body_file}" 2>/dev/null || true)"
fi

log "Waiting ${LIVENESS_WINDOW_SECONDS}s to verify liveness probe does not restart the pod"
sleep "${LIVENESS_WINDOW_SECONDS}"

restarts="$(kubectl -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.containerStatuses[?(@.name=="vekil")].restartCount}')"
if [[ "${restarts}" != "0" ]]; then
  die "expected zero restarts while device-code login is pending, got ${restarts}"
fi

pod_ready="$(kubectl -n "${NAMESPACE}" get pod "${pod}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')"
if [[ "${pod_ready}" == "True" ]]; then
  die "expected pod to remain unready until Copilot auth completes"
fi

log "Kubernetes smoke passed: /healthz is live, /readyz is gated, liveness caused 0 restarts"
