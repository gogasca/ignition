#!/usr/bin/env bash
# Build the swe-mini env image, push it to the Artifact Registry sandbox prefix,
# and admit it with the Ignition image API (which pins it to a digest and returns
# the imageId the controller schedules).
#
# Env:
#   IMAGE_REGISTRY   e.g. us-central1-docker.pkg.dev/anyscale-demo/ignition   (required)
#   IMAGE_TAG        default: dev
#   IGNITION_SERVER  Ignition API base URL       (required for the admit step)
#   IGNITION_TOKEN   bearer                      (required for the admit step)
#   IGNITION_PROJECT project id                  (required for the admit step)
set -euo pipefail

cd "$(dirname "$0")/.."   # examples/agentic-rl

: "${IMAGE_REGISTRY:?set IMAGE_REGISTRY}"
IMAGE_TAG="${IMAGE_TAG:-dev}"
REF="${IMAGE_REGISTRY}/img_swe_mini:${IMAGE_TAG}"

echo "building ${REF}"
docker build -f env_image/Dockerfile -t "${REF}" .
docker push "${REF}"

if [[ -n "${IGNITION_SERVER:-}" && -n "${IGNITION_TOKEN:-}" && -n "${IGNITION_PROJECT:-}" ]]; then
  echo "admitting ${REF} with Ignition"
  curl -fsS -X POST \
    -H "Authorization: Bearer ${IGNITION_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"sourceRef\": \"${REF}\"}" \
    "${IGNITION_SERVER}/v1/projects/${IGNITION_PROJECT}/images" | tee /dev/stderr
  echo
  echo "use the returned imageId as ENV_IMAGE (or the bare AR path under the sandbox prefix)"
else
  echo "IGNITION_SERVER/TOKEN/PROJECT not set — skipped admission; pushed ${REF}"
fi
