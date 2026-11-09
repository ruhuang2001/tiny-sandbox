"""Additive wire models: open strings, keyword-only fields, unknown-field retention."""

from __future__ import annotations

from dataclasses import dataclass, field, fields
from typing import Any, TypeVar

_M = TypeVar("_M", bound="_Model")


@dataclass(kw_only=True)
class _Model:
    extra: dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_dict(cls: type[_M], data: dict[str, Any]) -> _M:
        from ._errors import Error

        if not isinstance(data, dict):
            raise Error("decode", "expected a JSON object")
        definitions = {f.name: f for f in fields(cls) if f.name != "extra"}
        known = definitions.keys()
        for key, value in data.items():
            if key not in known:
                continue
            annotation = definitions[key].type
            if value is None and "None" in annotation:
                continue
            expected = annotation.split(" | ")[0]
            valid = {
                "str": lambda: isinstance(value, str),
                "int": lambda: type(value) is int,
                "bool": lambda: type(value) is bool,
                "list[str]": lambda: isinstance(value, list) and all(isinstance(v, str) for v in value),
            }.get(expected)
            if valid is not None and not valid():
                raise Error("decode", "invalid response field: " + key)
        for key in ("id", "sandbox_id"):
            if key in known and not data.get(key):
                raise Error("decode", "response is missing " + key)
        if "state" in known and not data.get("state"):
            raise Error("decode", "response is missing state")
        try:
            return cls(
                **{key: value for key, value in data.items() if key in known},
                extra={key: value for key, value in data.items() if key not in known},
            )
        except TypeError as exc:
            raise Error("decode", "response is missing required fields", cause=exc) from exc


@dataclass(kw_only=True)
class Sandbox(_Model):
    id: str
    state: str
    image: str = ""
    memory_mb: int | None = None
    pids_limit: int | None = None
    nano_cpus: int | None = None
    network: str | None = None
    timeout_seconds: int | None = None
    timeout_action: str | None = None
    idle_timeout_seconds: int | None = None
    idle_timeout_action: str | None = None
    created_at: str = ""
    expires_at: str | None = None
    kill_reason: str | None = None


@dataclass(kw_only=True)
class Execution(_Model):
    id: str
    sandbox_id: str
    state: str = ""
    command: list[str] = field(default_factory=list)
    exit_code: int | None = None
    termination_reason: str | None = None
    stdout: str = ""
    stderr: str = ""
    output_truncated: bool = False
    started_at: str = ""
    finished_at: str | None = None


@dataclass(kw_only=True)
class Event:
    id: int
    type: str
    data: str = ""
    limit_bytes: int = 0
    execution: Execution | None = None


@dataclass(kw_only=True)
class _Request:
    def to_dict(self) -> dict[str, Any]:
        return {
            f.name: getattr(self, f.name)
            for f in fields(self)
            if getattr(self, f.name) is not None
            and not (f.name == "idempotency_key" and not getattr(self, f.name))
        }


@dataclass(kw_only=True)
class CreateSandboxRequest(_Request):
    image: str | None = None
    memory_mb: int | None = None
    pids_limit: int | None = None
    nano_cpus: int | None = None
    network: str | None = None
    timeout_seconds: int | None = None
    timeout_action: str | None = None
    idle_timeout_seconds: int | None = None
    idle_timeout_action: str | None = None


@dataclass(kw_only=True)
class ExecuteRequest(_Request):
    command: list[str]
    idempotency_key: str = ""
    timeout_seconds: int | None = None
