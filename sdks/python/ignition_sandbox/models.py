"""Resource views. Each wraps the server's JSON so a new field never breaks the
SDK; the common fields are exposed as attributes."""

from __future__ import annotations

from typing import Any

_SANDBOX_TERMINAL = {"FINISHED", "FAILED"}
_PROCESS_TERMINAL = {"EXITED", "FAILED"}
_OPERATION_TERMINAL = {"SUCCEEDED", "FAILED", "CANCELLED"}


class _Resource:
    __slots__ = ("raw",)

    def __init__(self, raw: dict[str, Any]) -> None:
        self.raw = raw or {}

    def __getitem__(self, key: str) -> Any:
        return self.raw[key]

    def get(self, key: str, default: Any = None) -> Any:
        return self.raw.get(key, default)

    @property
    def id(self) -> str:
        return self.raw.get("id", "")

    def __repr__(self) -> str:
        return f"{type(self).__name__}(id={self.id!r}, state={self.raw.get('state')!r})"


class SandboxModel(_Resource):
    @property
    def state(self) -> str:
        return self.raw.get("state", "")

    @property
    def state_reason(self) -> str:
        return self.raw.get("stateReason", "")

    @property
    def operation_id(self) -> str:
        return self.raw.get("operationId", "")

    @property
    def is_ready(self) -> bool:
        return self.state == "READY"

    @property
    def is_terminal(self) -> bool:
        return self.state in _SANDBOX_TERMINAL


class ProcessModel(_Resource):
    @property
    def state(self) -> str:
        return self.raw.get("state", "")

    @property
    def exit_code(self) -> int | None:
        return self.raw.get("exitCode")

    @property
    def signal(self) -> str:
        return self.raw.get("signal", "")

    @property
    def is_terminal(self) -> bool:
        return self.state in _PROCESS_TERMINAL


class OperationModel(_Resource):
    @property
    def state(self) -> str:
        return self.raw.get("state", "")

    @property
    def kind(self) -> str:
        return self.raw.get("kind", "")

    @property
    def resource_id(self) -> str:
        return self.raw.get("resourceId", "")

    @property
    def error(self) -> dict | None:
        return self.raw.get("error")

    @property
    def is_terminal(self) -> bool:
        return self.state in _OPERATION_TERMINAL

    @property
    def succeeded(self) -> bool:
        return self.state == "SUCCEEDED"
