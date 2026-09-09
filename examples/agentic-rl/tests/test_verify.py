"""The verifier contract, fully offline."""

from __future__ import annotations

import json
import subprocess
import sys

from swe_mini.harness import verify


def test_buggy_baseline_is_partial(work_dir):
    result = verify.score(work_dir)
    assert 0.0 <= result["reward"] < 1.0
    assert result["passed"] is False
    assert result["detail"]["tests"] == 5


def test_fixed_scores_one(work_dir, task_id):
    from pathlib import Path

    tasks = Path(__file__).resolve().parent.parent / "swe_mini" / "tasks"
    sol = json.loads((tasks / task_id / "meta.json").read_text())["solution"]
    (work_dir / sol["path"]).write_text(sol["content"])
    result = verify.score(work_dir)
    assert result == {
        "reward": 1.0,
        "passed": True,
        "detail": {
            "tests": 5, "passed_count": 5, "failures": 0, "errors": 0,
            "skipped": 0, "pytest_returncode": 0,
        },
    }


def test_every_task_baseline_and_oracle(all_task_ids, tmp_path):
    """Each task's bug leaves reward < 1; each meta.json solution reaches 1.0."""
    import shutil
    from pathlib import Path

    tasks = Path(__file__).resolve().parent.parent / "swe_mini" / "tasks"
    for tid in all_task_ids:
        src = tasks / tid
        for tag, patch in (("baseline", False), ("oracle", True)):
            wd = tmp_path / f"{tid}-{tag}"
            wd.mkdir()
            for item in src.iterdir():
                if item.name == "meta.json":
                    continue
                (shutil.copytree if item.is_dir() else shutil.copy2)(item, wd / item.name)
            if patch:
                sol = json.loads((src / "meta.json").read_text())["solution"]
                (wd / sol["path"]).write_text(sol["content"])
            r = verify.score(wd)["reward"]
            if patch:
                assert r == 1.0, f"{tid} oracle solution did not pass"
            else:
                assert r < 1.0, f"{tid} baseline unexpectedly passes"


def test_sentinel_line_is_machine_readable(work_dir):
    proc = subprocess.run(
        [sys.executable, "-m", "swe_mini.harness.verify", str(work_dir)],
        capture_output=True, text=True, check=True,
    )
    lines = [ln for ln in proc.stdout.splitlines() if ln.startswith(verify.RESULT_SENTINEL)]
    assert len(lines) == 1
    payload = json.loads(lines[0][len(verify.RESULT_SENTINEL):].strip())
    assert set(payload) == {"reward", "passed", "detail"}
