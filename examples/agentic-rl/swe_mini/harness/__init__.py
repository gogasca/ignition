"""In-sandbox harness: the agent tool-loop and the pytest verifier.

Standard-library only — this runs inside the Ignition sandbox, whose root
filesystem is read-only and which has no packages beyond what the image bakes in
(python + pytest). All mutable work happens under ``$WORK_DIR`` (default
``/scratch/work``), a copy of the task made at rollout start.
"""
