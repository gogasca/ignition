"""The four tools the agent can call, each scoped to the work directory.

Actions are a small text protocol rather than provider-specific function-calling,
so any chat model (or an offline scripted policy) can drive the loop. The model
emits::

    ACTION: write_file
    ARGS: {"path": "bug.py", "content": "def f(): ..."}

and the harness returns the tool's observation as the next user message.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

TOOLS = ("list_files", "read_file", "write_file", "run_tests", "submit")

MAX_OBS_CHARS = 6000


def _safe_path(work: Path, rel: str) -> Path:
    p = (work / rel).resolve()
    if work.resolve() not in p.parents and p != work.resolve():
        raise ValueError(f"path escapes work dir: {rel}")
    return p


def _clip(s: str) -> str:
    return s if len(s) <= MAX_OBS_CHARS else s[:MAX_OBS_CHARS] + "\n...[truncated]"


def list_files(work: Path, args: dict) -> str:
    files = sorted(
        str(p.relative_to(work))
        for p in work.rglob("*")
        if p.is_file() and "__pycache__" not in p.parts
    )
    return "files:\n" + "\n".join(files)


def read_file(work: Path, args: dict) -> str:
    p = _safe_path(work, args["path"])
    if not p.is_file():
        return f"error: no such file: {args['path']}"
    return _clip(p.read_text(encoding="utf-8"))


def write_file(work: Path, args: dict) -> str:
    p = _safe_path(work, args["path"])
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(args["content"], encoding="utf-8")
    return f"wrote {len(args['content'])} bytes to {args['path']}"


def run_tests(work: Path, args: dict) -> str:
    proc = subprocess.run(
        [sys.executable, "-m", "pytest", "-q", "-p", "no:cacheprovider",
         "--tb=short", str(work)],
        cwd=str(work), capture_output=True, text=True, timeout=300,
    )
    return _clip(proc.stdout + proc.stderr)


def submit(work: Path, args: dict) -> str:
    return "submitted"


_DISPATCH = {
    "list_files": list_files,
    "read_file": read_file,
    "write_file": write_file,
    "run_tests": run_tests,
    "submit": submit,
}


def dispatch(work_dir: str | Path, name: str, args: dict) -> str:
    if name not in _DISPATCH:
        return f"error: unknown tool {name!r}; tools are {', '.join(TOOLS)}"
    try:
        return _DISPATCH[name](Path(work_dir), args or {})
    except KeyError as e:
        return f"error: missing arg {e} for tool {name!r}"
    except Exception as e:  # noqa: BLE001 - tool errors are observations, not crashes
        return f"error: {type(e).__name__}: {e}"


def parse_action(text: str) -> tuple[str | None, dict]:
    """Pull ``ACTION:`` / ``ARGS:`` out of a model completion. ``ARGS`` may be
    inline JSON or a fenced block; missing ARGS means ``{}``."""
    name: str | None = None
    args_raw = ""
    in_args = False
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.upper().startswith("ACTION:"):
            name = stripped.split(":", 1)[1].strip().strip("`").split()[0] if ":" in stripped else None
            in_args = False
        elif stripped.upper().startswith("ARGS:"):
            args_raw = stripped.split(":", 1)[1].strip()
            in_args = True
        elif in_args:
            args_raw += "\n" + line
    args_raw = args_raw.strip().strip("`")
    if args_raw.startswith("json"):
        args_raw = args_raw[4:].strip()
    try:
        args = json.loads(args_raw) if args_raw else {}
    except json.JSONDecodeError:
        args = {"_raw": args_raw}
    return name, args
