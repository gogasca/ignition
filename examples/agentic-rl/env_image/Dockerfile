# The swe-mini environment image: what an Ignition rollout sandbox runs.
#
# Topology A: the entrypoint IS the agent (python -m swe_mini.harness.rollout).
# Topology B: the driver passes command=["sleep", ...] to override this and
#             drives the harness modules over the exec stream instead.
#
# Kept deliberately small (python:slim + pytest) — cold start is image-pull-bound
# until an operator turns on a warm pool. No secrets, no tokens: those arrive as
# env at Pod create via Ignition secretRefs.
#
#   docker build -f env_image/Dockerfile -t <registry>/img_swe_mini:<tag> .
# (build context is examples/agentic-rl/)

FROM python:3.12-slim

# pytest is the verifier's only dependency; the harness itself is stdlib-only.
RUN pip install --no-cache-dir "pytest>=8" && \
    useradd -u 65532 -m nonroot

WORKDIR /env
COPY swe_mini/ /env/swe_mini/

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 WORK_DIR=/scratch/work
USER 65532:65532

ENTRYPOINT ["python", "-m", "swe_mini.harness.rollout"]
