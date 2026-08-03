#!/usr/bin/env python3
"""Execute each helper slice, validate hello/get_state, then close stdin."""

from __future__ import annotations

import argparse
import json
import os
import selectors
import signal
import subprocess
import sys
import time
import tempfile
import urllib.request
from pathlib import Path
from typing import Any


class SmokeError(RuntimeError):
    pass


def load_manifest(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SmokeError(f"cannot read release manifest: {exc}") from exc
    if not isinstance(value, dict):
        raise SmokeError("release manifest root must be an object")
    return value


def terminate(process: subprocess.Popen[bytes]) -> None:
    if process.poll() is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait(timeout=2)


def read_frame(
    process: subprocess.Popen[bytes],
    timeout: float,
    stdout: bytearray,
    stderr: bytearray,
) -> dict[str, Any]:
    assert process.stdout is not None
    assert process.stderr is not None
    selector = selectors.DefaultSelector()
    selector.register(process.stdout, selectors.EVENT_READ, "stdout")
    selector.register(process.stderr, selectors.EVENT_READ, "stderr")
    deadline = time.monotonic() + timeout

    while time.monotonic() < deadline:
        if b"\n" in stdout:
            line, _, remainder = stdout.partition(b"\n")
            stdout[:] = remainder
            try:
                frame = json.loads(line)
            except json.JSONDecodeError as exc:
                raise SmokeError(f"helper frame is not JSON: {line[:4096]!r}") from exc
            if not isinstance(frame, dict):
                raise SmokeError("helper protocol frame must be an object")
            return frame
        if process.poll() is not None and not stdout:
            raise SmokeError(
                f"helper exited before the next frame with status {process.returncode}; "
                f"stderr={stderr[-4096:].decode(errors='replace')!r}"
            )
        events = selector.select(max(0.0, deadline - time.monotonic()))
        for key, _ in events:
            chunk = os.read(key.fileobj.fileno(), 4096)
            if not chunk:
                selector.unregister(key.fileobj)
                continue
            if key.data == "stdout":
                stdout.extend(chunk)
            elif len(stderr) < 65536:
                stderr.extend(chunk[: 65536 - len(stderr)])
    raise SmokeError(
        f"timed out waiting for helper frame; "
        f"stderr={stderr[-4096:].decode(errors='replace')!r}"
    )


def read_hello(
    process: subprocess.Popen[bytes], timeout: float
) -> tuple[dict[str, Any], bytearray, bytearray]:
    stdout = bytearray()
    stderr = bytearray()
    return read_frame(process, timeout, stdout, stderr), stdout, stderr


def send_request(
    process: subprocess.Popen[bytes], request_id: str, command: str, payload: dict[str, Any] | None = None
) -> None:
    assert process.stdin is not None
    body = {"v": 1, "id": request_id, "command": command, "payload": payload or {}}
    process.stdin.write(json.dumps(body, separators=(",", ":")).encode() + b"\n")
    process.stdin.flush()


def wait_for_response(
    process: subprocess.Popen[bytes],
    request_id: str,
    timeout: float,
    stdout: bytearray,
    stderr: bytearray,
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        frame = read_frame(process, max(0.01, deadline - time.monotonic()), stdout, stderr)
        if frame.get("id") == request_id:
            if frame.get("ok") is not True:
                raise SmokeError(f"helper command {request_id} failed: {frame.get('error')!r}")
            return frame
    raise SmokeError(f"timed out waiting for helper response {request_id}")


def wait_for_operation(
    process: subprocess.Popen[bytes],
    operation_id: str,
    timeout: float,
    stdout: bytearray,
    stderr: bytearray,
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        frame = read_frame(process, max(0.01, deadline - time.monotonic()), stdout, stderr)
        if frame.get("event") != "operation":
            continue
        payload = frame.get("payload")
        if not isinstance(payload, dict) or payload.get("operation_id") != operation_id:
            continue
        status = payload.get("status")
        if status == "succeeded":
            return
        if status in {"failed", "canceled"}:
            raise SmokeError(f"helper operation {operation_id} ended as {status}: {payload.get('error')!r}")
    raise SmokeError(f"timed out waiting for helper operation {operation_id}")


def submit_operation(
    process: subprocess.Popen[bytes],
    request_id: str,
    command: str,
    timeout: float,
    stdout: bytearray,
    stderr: bytearray,
    payload: dict[str, Any] | None = None,
) -> None:
    send_request(process, request_id, command, payload)
    response = wait_for_response(process, request_id, timeout, stdout, stderr)
    result = response.get("result")
    operation_id = result.get("operation_id") if isinstance(result, dict) else None
    if not isinstance(operation_id, str) or not operation_id:
        raise SmokeError(f"helper command {command} did not return an operation ID")
    wait_for_operation(process, operation_id, timeout, stdout, stderr)


def get_state(
    process: subprocess.Popen[bytes],
    request_id: str,
    timeout: float,
    stdout: bytearray,
    stderr: bytearray,
) -> dict[str, Any]:
    send_request(process, request_id, "get_state")
    response = wait_for_response(process, request_id, timeout, stdout, stderr)
    result = response.get("result")
    if not isinstance(result, dict) or not isinstance(result.get("state_revision"), int):
        raise SmokeError("helper get_state result lacks state_revision")
    payload = result.get("payload")
    if not isinstance(payload, dict):
        raise SmokeError("helper get_state result lacks payload")
    return payload


def smoke_arch(helper: Path, arch: str, expected_build_id: str, timeout: float) -> None:
    with tempfile.TemporaryDirectory(prefix=f"vekil-helper-{arch}-") as temporary:
        root = Path(temporary)
        state_dir = root / "state"
        token_dir = root / "tokens"
        home_dir = root / "home"
        state_dir.mkdir(mode=0o700)
        token_dir.mkdir(mode=0o700)
        home_dir.mkdir(mode=0o700)
        command = [
            "/usr/bin/arch",
            f"-{arch}",
            str(helper),
            "--host",
            "127.0.0.1",
            "--port",
            "0",
            "--state-dir",
            str(state_dir),
            "--token-dir",
            str(token_dir),
        ]
        environment = dict(os.environ)
        environment["HOME"] = str(home_dir)
        environment["XDG_CONFIG_HOME"] = str(home_dir / ".config")
        process = subprocess.Popen(
            command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
            env=environment,
        )
        try:
            frame, stdout, stderr = read_hello(process, timeout)
            if frame.get("event") != "hello":
                raise SmokeError(f"{arch} helper first event is {frame.get('event')!r}, expected 'hello'")
            payload = frame.get("payload")
            if not isinstance(payload, dict):
                payload = {}
            actual_build_id = frame.get("bundle_build_id") or payload.get("bundle_build_id")
            if actual_build_id != expected_build_id:
                raise SmokeError(
                    f"{arch} helper build ID {actual_build_id!r} != {expected_build_id!r}"
                )
            protocol_version = frame.get("v")
            if protocol_version != 1:
                raise SmokeError(f"{arch} helper protocol frame version is {protocol_version!r}, expected 1")
            state_payload = get_state(process, "req_smoke_state", timeout, stdout, stderr)
            if state_payload.get("service") != "stopped":
                raise SmokeError(f"{arch} helper initial state is invalid: {state_payload!r}")

            config_path = root / "providers.yaml"
            config_path.write_text(
                "schema_version: 2\n"
                "providers:\n"
                "  - id: local\n"
                "    type: openai-compatible\n"
                "    default: true\n"
                "    base_url: https://example.test/v1\n"
                "    auth_type: none\n"
                "    model_discovery: static\n"
                "    models:\n"
                "      - public_id: smoke-model\n"
                "        endpoints: [/chat/completions]\n",
                encoding="utf-8",
            )
            os.chmod(config_path, 0o600)
            submit_operation(
                process, "req_select", "select_external_config", timeout, stdout, stderr,
                {"path": str(config_path)},
            )
            submit_operation(process, "req_start", "start", timeout, stdout, stderr)
            running = get_state(process, "req_running", timeout, stdout, stderr)
            if running.get("service") != "running" or not isinstance(running.get("addr"), str):
                raise SmokeError(f"{arch} helper did not reach running state: {running!r}")
            with urllib.request.urlopen(f"http://{running['addr']}/healthz", timeout=timeout) as health:
                if health.status != 200:
                    raise SmokeError(f"{arch} helper health returned {health.status}")
            submit_operation(process, "req_stop", "stop", timeout, stdout, stderr)
            stopped = get_state(process, "req_stopped", timeout, stdout, stderr)
            if stopped.get("service") != "stopped":
                raise SmokeError(f"{arch} helper did not stop cleanly: {stopped!r}")
            process.stdin.close()
            try:
                return_code = process.wait(timeout=7)
            except subprocess.TimeoutExpired as exc:
                raise SmokeError(f"{arch} helper did not exit within seven seconds after stdin EOF") from exc
            if return_code != 0:
                raise SmokeError(
                    f"{arch} helper exited with status {return_code}; "
                    f"stderr={stderr[-4096:].decode(errors='replace')!r}"
                )
        finally:
            terminate(process)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--helper", required=True)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--arch", action="append", choices=("arm64", "x86_64"))
    parser.add_argument("--timeout", type=float, default=10.0)
    args = parser.parse_args()

    helper = Path(args.helper).resolve()
    if not helper.is_file() or not os.access(helper, os.X_OK):
        raise SmokeError(f"helper is not executable: {helper}")
    manifest = load_manifest(Path(args.manifest))
    expected_build_id = str(manifest.get("bundle_build_id") or "")
    if not expected_build_id:
        raise SmokeError("manifest bundle_build_id is missing")
    arches = args.arch or (["arm64", "x86_64"] if os.environ.get("MACOS_RELEASE") == "1" else [os.uname().machine])
    for arch in arches:
        smoke_arch(helper, arch, expected_build_id, args.timeout)
        print(f"helper {arch} hello/config/start/health/stop/EOF smoke passed")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except SmokeError as exc:
        print(f"error: {exc}", file=sys.stderr)
        raise SystemExit(1)
