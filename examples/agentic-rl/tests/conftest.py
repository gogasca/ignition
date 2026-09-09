from __future__ import annotations

import shutil
from pathlib import Path

import pytest

TASKS = Path(__file__).resolve().parent.parent / "swe_mini" / "tasks"


@pytest.fixture
def task_id() -> str:
    return "task_0001_sum_list"


@pytest.fixture
def work_dir(tmp_path, task_id) -> Path:
    """A mutable copy of a task (minus meta.json), like the harness makes."""
    src = TASKS / task_id
    dst = tmp_path / "work"
    dst.mkdir()
    for item in src.iterdir():
        if item.name == "meta.json":
            continue
        (shutil.copytree if item.is_dir() else shutil.copy2)(item, dst / item.name)
    return dst


@pytest.fixture
def all_task_ids() -> list[str]:
    return sorted(p.name for p in TASKS.iterdir() if p.is_dir())
