# swe-mini-rl — see README.md
PY ?= .venv/bin/python
PIP ?= .venv/bin/pip

RUN_ID     ?= demo
GROUP_SIZE ?= 4
TASK       ?= task_0001_sum_list

.PHONY: help venv test smoke-offline rollout-b rollout-a train image clean

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n", $$1, $$2}'

venv: ## create .venv and install the package + SDK (editable)
	python3 -m venv .venv
	$(PIP) install -q -e ../../sdks/python -e '.[dev]'

test: ## run the hermetic test suite (no GCP, no Docker, no LLM)
	$(PY) -m pytest -q

smoke-offline: ## full loop locally: harness (OraclePolicy) -> trajectories -> GRPO batch
	INFERENCE_URL=oracle:// $(PY) -m swe_mini.driver.offline --run-id offline --group-size $(GROUP_SIZE)
	$(PY) -m swe_mini.trainer.grpo_step --run offline

rollout-b: ## one topology-B rollout on a live Ignition (needs IGNITION_* + ENV_IMAGE + gateway port-forward)
	$(PY) -m swe_mini.driver.topology_b --task $(TASK) --run-id $(RUN_ID)

rollout-a: ## a batch of topology-A rollouts on a live Ignition (+ a reachable collector)
	$(PY) -m swe_mini.driver.controller --run-id $(RUN_ID) --group-size $(GROUP_SIZE)
	$(PY) -m swe_mini.trainer.grpo_step --run $(RUN_ID)

train: ## the outer loop, hermetic (offline rollouts). Use --topology a/b for a live cluster
	INFERENCE_URL=oracle:// $(PY) -m swe_mini.train --run-id $(RUN_ID) --steps 3 --topology offline

image: ## build + push + admit the sandbox env image
	env_image/build_and_admit.sh

clean:
	rm -rf trajectories training_batch state .pytest_cache
