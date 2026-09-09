"""The "swe-mini" task family: small, self-contained Python bug-fix tasks.

Each task directory holds:
  bug.py         the module under repair (contains one deliberate bug)
  test_task.py   the pytest suite the verifier runs (the ground truth)
  PROMPT.md      the instruction shown to the agent
  meta.json      {"task_id", "solution": {"path", "content"}} — the known fix,
                 used only by OraclePolicy for hermetic tests, never by the agent

The whole directory is baked into the sandbox image; the harness copies the task
(minus meta.json) into the work dir at rollout start.
"""
