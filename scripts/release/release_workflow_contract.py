#!/usr/bin/env python3
"""Static release workflow and Docker input contract checks."""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable


@dataclass(frozen=True)
class Line:
    number: int
    indent: int
    text: str
    raw: str


@dataclass
class Step:
    lines: list[Line]
    name: str = ""
    uses: str = ""
    with_values: dict[str, str] | None = None


@dataclass
class Job:
    job_id: str
    lines: list[Line]
    permissions: dict[str, str] | None
    environment: str
    needs: set[str]
    steps: list[Step]

    @property
    def text(self) -> str:
        return "\n".join(line.text for line in self.lines)

    @property
    def raw_text(self) -> str:
        return "\n".join(line.raw for line in self.lines)


class Contract:
    def __init__(self) -> None:
        self.errors: list[str] = []

    def error(self, message: str) -> None:
        self.errors.append(message)


def strip_comment(raw: str) -> str:
    single = False
    double = False
    index = 0
    while index < len(raw):
        char = raw[index]
        if char == "'" and not double:
            if single and index + 1 < len(raw) and raw[index + 1] == "'":
                index += 2
                continue
            single = not single
        elif char == '"' and not single:
            escaped = index > 0 and raw[index - 1] == "\\"
            if not escaped:
                double = not double
        elif char == "#" and not single and not double and (index == 0 or raw[index - 1].isspace()):
            return raw[:index].rstrip()
        index += 1
    return raw.rstrip()


def unquote(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    return value


def read_lines(path: Path) -> list[Line]:
    result: list[Line] = []
    for number, raw in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if "\t" in raw[: len(raw) - len(raw.lstrip())]:
            raise ValueError(f"{path}:{number}: tabs are not allowed for YAML indentation")
        text = strip_comment(raw)
        indent = len(text) - len(text.lstrip(" "))
        result.append(Line(number, indent, text.strip(), raw))
    return result


def parse_inline_map(value: str) -> dict[str, str] | None:
    value = value.strip()
    if value == "{}":
        return {}
    if not (value.startswith("{") and value.endswith("}")):
        return None
    result: dict[str, str] = {}
    inner = value[1:-1].strip()
    if not inner:
        return result
    for part in inner.split(","):
        if ":" not in part:
            return None
        key, item_value = part.split(":", 1)
        result[unquote(key.strip())] = unquote(item_value.strip())
    return result


def direct_property(lines: list[Line], indent: int, key: str) -> tuple[int, str] | None:
    pattern = re.compile(rf"^{re.escape(key)}\s*:\s*(.*)$")
    for index, line in enumerate(lines):
        if line.indent != indent:
            continue
        match = pattern.match(line.text)
        if match:
            return index, unquote(match.group(1).strip())
    return None


def mapping_property(lines: list[Line], indent: int, key: str) -> dict[str, str] | None:
    found = direct_property(lines, indent, key)
    if found is None:
        return None
    index, scalar = found
    if scalar:
        inline = parse_inline_map(scalar)
        if inline is not None:
            return inline
        return {"*": scalar}
    result: dict[str, str] = {}
    for line in lines[index + 1 :]:
        if line.text and line.indent <= indent:
            break
        if line.indent == indent + 2:
            match = re.match(r"^([^:]+):\s*(.*)$", line.text)
            if match:
                result[unquote(match.group(1).strip())] = unquote(match.group(2).strip())
    return result


def scalar_property(lines: list[Line], indent: int, key: str) -> str:
    found = direct_property(lines, indent, key)
    if found is None:
        return ""
    index, scalar = found
    if scalar:
        return scalar
    for line in lines[index + 1 :]:
        if line.text and line.indent <= indent:
            break
        if line.indent == indent + 2:
            match = re.match(r"^name:\s*(.*)$", line.text)
            if match:
                return unquote(match.group(1).strip())
    return ""


def list_property(lines: list[Line], indent: int, key: str) -> set[str]:
    found = direct_property(lines, indent, key)
    if found is None:
        return set()
    index, scalar = found
    if scalar:
        if scalar.startswith("[") and scalar.endswith("]"):
            return {unquote(item.strip()) for item in scalar[1:-1].split(",") if item.strip()}
        return {scalar}
    values: set[str] = set()
    for line in lines[index + 1 :]:
        if line.text and line.indent <= indent:
            break
        if line.indent == indent + 2 and line.text.startswith("-"):
            values.add(unquote(line.text[1:].strip()))
    return values


def parse_steps(lines: list[Line]) -> list[Step]:
    found = direct_property(lines, 4, "steps")
    if found is None:
        return []
    start, _ = found
    blocks: list[list[Line]] = []
    current: list[Line] = []
    for line in lines[start + 1 :]:
        if line.text and line.indent <= 4:
            break
        if line.indent == 6 and line.text.startswith("-"):
            if current:
                blocks.append(current)
            current = [line]
        elif current:
            current.append(line)
    if current:
        blocks.append(current)

    steps: list[Step] = []
    for block in blocks:
        first = block[0]
        synthetic = Line(first.number, 8, first.text[1:].strip(), first.raw)
        properties = [synthetic, *block[1:]]
        name = scalar_property(properties, 8, "name")
        uses = scalar_property(properties, 8, "uses")
        with_values = mapping_property(properties, 8, "with")
        steps.append(Step(block, name=name, uses=uses, with_values=with_values))
    return steps


def parse_jobs(lines: list[Line]) -> dict[str, Job]:
    jobs_index = next((index for index, line in enumerate(lines) if line.indent == 0 and line.text == "jobs:"), None)
    if jobs_index is None:
        raise ValueError("release workflow has no top-level jobs mapping")
    blocks: list[tuple[str, list[Line]]] = []
    current_id = ""
    current: list[Line] = []
    for line in lines[jobs_index + 1 :]:
        if line.text and line.indent == 0:
            break
        match = re.match(r"^([A-Za-z0-9_-]+):\s*$", line.text) if line.indent == 2 else None
        if match:
            if current_id:
                blocks.append((current_id, current))
            current_id = match.group(1)
            current = [line]
        elif current_id:
            current.append(line)
    if current_id:
        blocks.append((current_id, current))
    result: dict[str, Job] = {}
    for job_id, block in blocks:
        result[job_id] = Job(
            job_id=job_id,
            lines=block,
            permissions=mapping_property(block, 4, "permissions"),
            environment=scalar_property(block, 4, "environment"),
            needs=list_property(block, 4, "needs"),
            steps=parse_steps(block),
        )
    return result


def workflow_files(directory: Path) -> list[Path]:
    return sorted({*directory.glob("*.yml"), *directory.glob("*.yaml")})


def load_version(path: Path, key: str) -> str:
    pattern = re.compile(rf"^{re.escape(key)}=(.*)$")
    for line in path.read_text(encoding="utf-8").splitlines():
        match = pattern.match(line.strip())
        if match:
            return unquote(match.group(1).strip())
    raise ValueError(f"{key} is missing from {path}")


def check_action_pins(contract: Contract, paths: Iterable[Path]) -> None:
    for path in paths:
        try:
            lines = read_lines(path)
        except (OSError, ValueError) as exc:
            contract.error(str(exc))
            continue
        for line in lines:
            match = re.match(r"^(?:-\s*)?uses:\s*(.+)$", line.text)
            if not match:
                continue
            value = unquote(match.group(1).strip())
            if value.startswith("./"):
                continue
            if value.startswith("docker://"):
                if not re.search(r"@sha256:[0-9a-f]{64}$", value):
                    contract.error(f"{path}:{line.number}: container action is not digest-pinned: {value}")
                continue
            if "@" not in value or not re.fullmatch(r"[0-9a-fA-F]{40}", value.rsplit("@", 1)[1]):
                contract.error(f"{path}:{line.number}: action/reusable workflow is not pinned to a full commit SHA: {value}")


def permission_is_write(value: str) -> bool:
    return value.strip().lower() == "write"


def permission_map_is_read_only(value: dict[str, str] | None) -> bool:
    if value is None:
        return False
    if "*" in value:
        return value["*"].lower() in {"read-all", "{}"}
    return all(item.lower() in {"read", "none", ""} for item in value.values())


def depends_on(jobs: dict[str, Job], job_id: str, ancestor: str, seen: set[str] | None = None) -> bool:
    if job_id == ancestor:
        return True
    if seen is None:
        seen = set()
    if job_id in seen or job_id not in jobs:
        return False
    seen.add(job_id)
    return any(depends_on(jobs, dependency, ancestor, seen) for dependency in jobs[job_id].needs)


def find_jobs_containing(jobs: dict[str, Job], *needles: str) -> list[str]:
    return [job_id for job_id, job in jobs.items() if all(needle in job.raw_text for needle in needles)]


def check_release_workflow(contract: Contract, path: Path, goreleaser_version: str) -> None:
    try:
        lines = read_lines(path)
        jobs = parse_jobs(lines)
    except (OSError, ValueError) as exc:
        contract.error(f"{path}: {exc}")
        return
    if not jobs:
        contract.error(f"{path}: release workflow contains no jobs")
        return

    top_permissions = mapping_property(lines, 0, "permissions")
    if not permission_map_is_read_only(top_permissions):
        contract.error(f"{path}: top-level permissions must be explicit and read-only")

    concurrency = mapping_property(lines, 0, "concurrency")
    if concurrency is None:
        contract.error(f"{path}: top-level release concurrency is required")
    else:
        if concurrency.get("cancel-in-progress", "").lower() != "false":
            contract.error(f"{path}: release concurrency must set cancel-in-progress: false")
        if not concurrency.get("group", ""):
            contract.error(f"{path}: release concurrency must define a group")

    release_text = "\n".join(line.text for line in lines)
    if "--clobber" in release_text:
        contract.error(f"{path}: release publication must not use --clobber")
    direct_main = re.compile(r"""\bgit(?:\s+-C\s+\S+)?\s+push\b[^\n]*(?:\borigin\s+["']?main["']?(?:\s|$)|refs/heads/main\b|["']?main["']?\s*$)""", re.MULTILINE)
    if direct_main.search(release_text):
        contract.error(f"{path}: Homebrew/tap publication must not push directly to main")

    protected = {"release", "homebrew"}
    for job_id, job in jobs.items():
        if job.permissions is None:
            contract.error(f"{path}: job {job_id!r} must declare explicit job-level permissions")
            writes = False
        else:
            scalar_permission = job.permissions.get("*")
            if scalar_permission is not None and scalar_permission.lower() not in {"read-all", "{}"}:
                contract.error(f"{path}: job {job_id!r} must not use broad scalar permission {scalar_permission!r}")
            if scalar_permission is None:
                invalid_values = sorted(
                    {value for value in job.permissions.values() if value.lower() not in {"read", "write", "none"}}
                )
                if invalid_values:
                    contract.error(f"{path}: job {job_id!r} has invalid permission values: {invalid_values}")
            writes = any(permission_is_write(value) or value.lower() == "write-all" for value in job.permissions.values())
        privileged_marker = any(
            marker in job.raw_text
            for marker in (
                "gh release create",
                "gh release upload",
                "push: true",
                "HOMEBREW_REPO_TOKEN",
                "SPARKLE_PRIVATE_ED_KEY",
            )
        )
        if re.match(r"^(?:preflight|build-|scan-|assemble|post-publish)", job_id) and writes:
            contract.error(f"{path}: read-only build/scan/verification job {job_id!r} must not request write permissions")
        if (writes or privileged_marker) and job.environment not in protected:
            contract.error(f"{path}: privileged job {job_id!r} must use a protected release/homebrew environment")
        if ("homebrew" in job_id.lower() or "tap" in job_id.lower()) and (writes or privileged_marker) and job.environment != "homebrew":
            contract.error(f"{path}: Homebrew/tap job {job_id!r} must use the homebrew environment")

        if ("homebrew" in job_id.lower() or "tap" in job_id.lower()) and (writes or privileged_marker):
            for marker in ("isCrossRepository", "headRepositoryOwner", "headRefOid"):
                if marker not in job.raw_text:
                    contract.error(f"{path}: Homebrew/tap job {job_id!r} is missing canonical PR-head guard {marker!r}")

        for step in job.steps:
            if not step.uses.startswith("actions/checkout@"):
                continue
            with_values = step.with_values or {}
            persist = with_values.get("persist-credentials", "").lower()
            repository = with_values.get("repository", "")
            tap_checkout = (
                bool(repository)
                and repository not in {"${{ github.repository }}", "${{github.repository}}"}
                and ("homebrew" in repository.lower() or "tap" in step.name.lower() or "homebrew" in job_id.lower())
            )
            if tap_checkout:
                if persist not in {"true", "false"}:
                    contract.error(f"{path}: tap checkout in job {job_id!r} must set persist-credentials explicitly")
                if persist == "true" and job.environment != "homebrew":
                    contract.error(
                        f"{path}: tap push checkout in job {job_id!r} is the only checkout allowed to set persist-credentials: true"
                    )
            elif persist != "false":
                contract.error(f"{path}: checkout in job {job_id!r} must set persist-credentials: false")

    goreleaser_steps = [
        (job_id, step)
        for job_id, job in jobs.items()
        for step in job.steps
        if step.uses.startswith("goreleaser/goreleaser-action@")
    ]
    if len(goreleaser_steps) != 1:
        contract.error(f"{path}: release workflow must contain exactly one GoReleaser action step")
    else:
        job_id, step = goreleaser_steps[0]
        actual = (step.with_values or {}).get("version", "")
        if actual != goreleaser_version:
            contract.error(f"{path}: GoReleaser in job {job_id!r} must use exact version {goreleaser_version}, got {actual!r}")

    tag_jobs = find_jobs_containing(
        jobs,
        "scripts/release/verify-release-tag.sh",
        "scripts/release/verify-required-workflows.sh",
        "scripts/release/check-image-tag-absent.sh",
        "scripts/release/query-github-release.sh",
    )
    evidence_jobs = find_jobs_containing(
        jobs,
        "scripts/release/generate-release-manifest.py",
        "scripts/release/verify-release-manifest.py",
    )
    post_jobs = [
        job_id
        for job_id, job in jobs.items()
        if "scripts/release/verify-published-release.py" in job.raw_text
        and "post" in job_id.lower()
        and "publish" in job_id.lower()
    ]
    if len(tag_jobs) != 1:
        contract.error(f"{path}: exactly one read-only tag/preflight stage must invoke all release identity helpers")
    if len(evidence_jobs) != 1:
        contract.error(f"{path}: exactly one evidence stage must generate and offline-verify the release manifest")
    if len(post_jobs) != 1:
        contract.error(f"{path}: exactly one post-publish stage must invoke verify-published-release.py")

    finalizer_jobs = [
        job_id
        for job_id, job in jobs.items()
        if "gh release create" in job.raw_text or "gh release upload" in job.raw_text
    ]
    if not finalizer_jobs:
        contract.error(f"{path}: release workflow has no GitHub release publication barrier")
    for finalizer in finalizer_jobs:
        forbidden = (
            "actions/setup-go@",
            "goreleaser/goreleaser-action@",
            "go build",
            "make build",
            "docker build",
        )
        marker = next((value for value in forbidden if value in jobs[finalizer].raw_text), None)
        if marker:
            contract.error(f"{path}: publication finalizer {finalizer!r} must not rebuild artifacts ({marker!r})")

    if len(tag_jobs) == 1 and len(evidence_jobs) == 1:
        if not depends_on(jobs, evidence_jobs[0], tag_jobs[0]):
            contract.error(f"{path}: evidence stage must depend on the verified tag/preflight stage")
    if len(evidence_jobs) == 1:
        for finalizer in finalizer_jobs:
            if not depends_on(jobs, finalizer, evidence_jobs[0]):
                contract.error(f"{path}: publication job {finalizer!r} must depend on the evidence stage")
    if len(post_jobs) == 1:
        post_job = jobs[post_jobs[0]]
        if post_job.permissions is not None and any(permission_is_write(value) for value in post_job.permissions.values()):
            contract.error(f"{path}: post-publish verification job must be read-only")
        for finalizer in finalizer_jobs:
            if not depends_on(jobs, post_jobs[0], finalizer):
                contract.error(f"{path}: post-publish stage must depend on publication job {finalizer!r}")

    latest_re = re.compile(r"(?:ghcr\.io/[^\s'\"]+:latest(?:-rtk)?\b|\btag\s+[^\n]*:latest(?:-rtk)?\b)")
    for job_id, job in jobs.items():
        if not latest_re.search(job.raw_text):
            continue
        if len(post_jobs) != 1 or not depends_on(jobs, job_id, post_jobs[0]) or job_id == post_jobs[0]:
            contract.error(f"{path}: mutable latest tag in job {job_id!r} must be promoted only after post-publish verification")
        promotion_concurrency = mapping_property(job.lines, 4, "concurrency") or {}
        if (
            promotion_concurrency.get("group") != "release-alias-promotion"
            or promotion_concurrency.get("cancel-in-progress", "").lower() != "false"
            or promotion_concurrency.get("queue", "").lower() != "max"
        ):
            contract.error(f"{path}: mutable alias promotion job {job_id!r} must use non-canceling queue:max global release-alias-promotion concurrency")
        for marker in ("releases/latest", "previous_standard", "previous_rtk", "rollback_aliases"):
            if marker not in job.raw_text:
                contract.error(f"{path}: mutable alias promotion job {job_id!r} is missing stale-release/rollback guard {marker!r}")


def check_dry_run_workflow(contract: Contract, path: Path) -> None:
    if not path.exists():
        contract.error(f"dry-run workflow is missing: {path}")
        return
    lines = read_lines(path)
    jobs = parse_jobs(lines)
    permissions = mapping_property(lines, 0, "permissions")
    if permissions != {"contents": "read"}:
        contract.error(f"{path}: dry-run top-level permissions must be contents: read only")
    concurrency = mapping_property(lines, 0, "concurrency")
    if concurrency.get("cancel-in-progress", "").lower() != "false" or not concurrency.get("group", ""):
        contract.error(f"{path}: dry-run concurrency must be explicit with cancel-in-progress: false")
    for job_id, job in jobs.items():
        if job.permissions is None:
            contract.error(f"{path}: dry-run job {job_id!r} must declare explicit permissions")
        elif any(permission_is_write(value) for value in job.permissions.values()):
            contract.error(f"{path}: dry-run job {job_id!r} must be read-only")
        if job.environment:
            contract.error(f"{path}: dry-run job {job_id!r} must not enter a protected production environment")
    raw = "\n".join(line.text for line in lines)
    forbidden = (
        "${{ secrets.",
        "HOMEBREW_REPO_TOKEN",
        "SPARKLE_PRIVATE_ED_KEY",
        "gh release create",
        "gh release upload",
        "docker/login-action@",
        "push: true",
    )
    for marker in forbidden:
        if marker in raw:
            contract.error(f"{path}: dry-run workflow contains publication credential/action marker {marker!r}")
    required = (
        "workflow_dispatch",
        "--snapshot --clean --skip=publish",
        "type=oci",
        "docker/setup-docker-action@",
        "containerd-snapshotter",
        "load: true",
        "docker run --rm --platform",
        "Generate an ephemeral dry-run Sparkle keypair",
        "scripts/release/generate-release-manifest.py",
        "actions/upload-artifact@",
    )
    for marker in required:
        if marker not in raw:
            contract.error(f"{path}: dry-run workflow is missing required evidence/build marker {marker!r}")


def check_dockerfile(contract: Contract, path: Path) -> None:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        contract.error(str(exc))
        return
    aliases: set[str] = set()
    found = False
    for number, raw in enumerate(lines, 1):
        match = re.match(r"^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?\s*$", strip_comment(raw), re.IGNORECASE)
        if not match:
            continue
        found = True
        image, alias = match.groups()
        if image.lower() not in aliases and not re.search(r"@sha256:[0-9a-f]{64}$", image):
            contract.error(f"{path}:{number}: Docker base image is not pinned by sha256 digest: {image}")
        if alias:
            aliases.add(alias.lower())
    if not found:
        contract.error(f"{path}: no Docker FROM instruction found")


def run_actionlint(contract: Contract, actionlint: str, expected_version: str, paths: list[Path]) -> None:
    try:
        version_output = subprocess.run([actionlint, "-version"], check=True, capture_output=True, text=True).stdout.strip()
        version = version_output.splitlines()[0] if version_output else ""
    except (OSError, subprocess.CalledProcessError) as exc:
        contract.error(f"unable to run pinned actionlint: {exc}")
        return
    if version != expected_version:
        contract.error(f"actionlint version must be {expected_version}, got {version!r}")
        return
    result = subprocess.run(
        [actionlint, "-ignore", 'unexpected key "queue" for "concurrency" section', *map(str, paths)],
        check=False,
        text=True,
        capture_output=True,
    )
    if result.returncode != 0:
        details = (result.stdout + result.stderr).strip()
        contract.error(f"actionlint failed for workflow set:\n{details}")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workflow", type=Path, default=Path(".github/workflows/release.yaml"))
    parser.add_argument("--workflows-dir", type=Path, default=Path(".github/workflows"))
    parser.add_argument("--dockerfile", type=Path, action="append", default=[])
    parser.add_argument("--actionlint", default="actionlint")
    parser.add_argument("--skip-actionlint", action="store_true", help="self-test only; production contract runs must not use this")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    contract = Contract()
    script_dir = Path(__file__).resolve().parent
    versions = script_dir / "tool-versions.env"
    try:
        actionlint_version = load_version(versions, "ACTIONLINT_VERSION")
        goreleaser_version = load_version(versions, "GORELEASER_VERSION")
    except (OSError, ValueError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    paths = workflow_files(args.workflows_dir)
    if not paths:
        contract.error(f"no workflow YAML files found in {args.workflows_dir}")
    if args.workflow not in paths and args.workflow.exists():
        paths.append(args.workflow)
        paths.sort()
    check_action_pins(contract, paths)
    check_release_workflow(contract, args.workflow, goreleaser_version)
    check_dry_run_workflow(contract, Path(".github/workflows/release-dry-run.yaml"))

    dockerfiles = args.dockerfile or [path for path in (Path("Dockerfile"), Path("Dockerfile.rtk")) if path.exists()]
    if not dockerfiles:
        contract.error("no Dockerfiles supplied or discovered")
    for dockerfile in dockerfiles:
        check_dockerfile(contract, dockerfile)

    if not args.skip_actionlint:
        run_actionlint(contract, args.actionlint, actionlint_version, paths)

    if contract.errors:
        for error in contract.errors:
            print(f"contract violation: {error}", file=sys.stderr)
        print(f"release workflow contract failed with {len(contract.errors)} violation(s)", file=sys.stderr)
        return 1
    print(
        f"release workflow contract passed ({len(paths)} workflows, {len(jobs := parse_jobs(read_lines(args.workflow)))} release jobs, {len(dockerfiles)} Dockerfiles)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
