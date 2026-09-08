"""Ignition Python SDK — synchronous client for the shipped control plane.

    from ignition_sandbox import Client

    with Client() as ignition:               # server/token/project from env
        sb = ignition.sandboxes.create("img_seed", accelerator="NONE", wait=True)
        result = sb.run(["echo", "hello"])
        print(result.exit_code)
        sb.terminate(wait=True)

Configuration is read from ``IGNITION_SERVER`` / ``IGNITION_TOKEN`` /
``IGNITION_PROJECT`` unless passed to ``Client(...)``.

Scope: sandbox lifecycle, the process control plane, operations, ``:watch``
streams, and exec streaming through ``ignition-gateway`` (with a polling
fallback). A native ``async`` client is not built yet.
"""

from __future__ import annotations

from .client import Client, ExecResult, Operation, Process, Sandbox
from .errors import (
    APIError,
    ConflictError,
    IgnitionError,
    NotFoundError,
    PermissionDeniedError,
    StreamError,
    TimeoutError,
    UnauthenticatedError,
)
from .models import OperationModel, ProcessModel, SandboxModel

__version__ = "0.1.0"

__all__ = [
    "Client",
    "Sandbox",
    "Process",
    "Operation",
    "ExecResult",
    "SandboxModel",
    "ProcessModel",
    "OperationModel",
    "IgnitionError",
    "APIError",
    "NotFoundError",
    "PermissionDeniedError",
    "UnauthenticatedError",
    "ConflictError",
    "StreamError",
    "TimeoutError",
]
