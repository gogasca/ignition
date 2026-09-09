"""The verifier. Runs the task's pytest suite in a work directory and prints a
single machine-readable line to stdout::

    ---IGN-RESULT--- {"reward": 0.57, "passed": false, "detail": {...}}

- ``reward`` is dense: (tests - failures - errors - skipped) / tests, in [0, 1].
- ``passed`` is True only when every test passed.

Usage (inside the sandbox, or locally against a temp dir)::

    python -m swe_mini.harness.verify /scratch/work

The reward contract is intentionally simple and rule-based — the whole point of
RLVR is that the environment, not a learned model, produces the signal.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path

RESULT_SENTINEL = "---IGN-RESULT---"


def run_pytest(work_dir: str | Path) -> dict:
    """Run ``pytest`` in ``work_dir`` and return counts parsed from a JUnit XML
    report (robust — no summary-line scraping)."""
    work = Path(work_dir)
    with tempfile.NamedTemporaryFile(suffix=".xml", delete=False) as tf:
        report = tf.name
    proc = subprocess.run(
        [
            sys.executable, "-m", "pytest",
            "-q", "-p", "no:cacheprovider", "--tb=no",
            f"--junitxml={report}", str(work),
        ],
        cwd=str(work),
        capture_output=True,
        text=True,
        timeout=300,
    )
    counts = _parse_junit(report)
    counts["pytest_returncode"] = proc.returncode
    counts["pytest_tail"] = (proc.stdout or proc.stderr or "")[-2000:]
    return counts


def _parse_junit(report_path: str) -> dict:
    try:
        root = ET.parse(report_path).getroot()
    except (ET.ParseError, FileNotFoundError):
        return {"tests": 0, "failures": 0, "errors": 1, "skipped": 0}
    suite = root if root.tag == "testsuite" else root.find("testsuite")
    if suite is None:
        return {"tests": 0, "failures": 0, "errors": 1, "skipped": 0}
    g = lambda k: int(suite.get(k, "0"))  # noqa: E731
    return {
        "tests": g("tests"),
        "failures": g("failures"),
        "errors": g("errors"),
        "skipped": g("skipped"),
    }


def score(work_dir: str | Path) -> dict:
    c = run_pytest(work_dir)
    total = c["tests"]
    good = total - c["failures"] - c["errors"] - c["skipped"]
    reward = 0.0 if total == 0 else max(0.0, min(1.0, good / total))
    passed = total > 0 and good == total
    return {
        "reward": round(reward, 6),
        "passed": passed,
        "detail": {
            "tests": total,
            "passed_count": good,
            "failures": c["failures"],
            "errors": c["errors"],
            "skipped": c["skipped"],
            "pytest_returncode": c.get("pytest_returncode"),
        },
    }


def main(argv: list[str]) -> int:
    work_dir = argv[1] if len(argv) > 1 else "/scratch/work"
    result = score(work_dir)
    print(RESULT_SENTINEL + " " + json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
