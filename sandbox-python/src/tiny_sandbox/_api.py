"""Typed endpoint operations shared by the synchronous client."""

from __future__ import annotations

import secrets
from abc import ABC, abstractmethod
from contextlib import contextmanager
from dataclasses import replace
from functools import wraps
from typing import Any, Callable, Iterator, NoReturn, ParamSpec, TypeVar
from urllib.parse import quote, urlencode

from ._cancel import CancellationToken, _Cancellation
from ._errors import Error
from ._models import CreateSandboxRequest, ExecuteRequest, Execution, Sandbox
from ._transport import Transport

_P = ParamSpec("_P")
_T = TypeVar("_T")


def _public_cancellation(method: Callable[_P, _T]) -> Callable[_P, _T]:
    """Keep the transport's cancellation signal out of the public API."""

    @wraps(method)
    def call(*args: _P.args, **kwargs: _P.kwargs) -> _T:
        try:
            return method(*args, **kwargs)
        except _Cancellation as exc:
            raise Error("canceled", cause=exc) from exc

    return call


def _resource_items(payload: dict[str, Any], field: str) -> list[dict[str, Any]]:
    if field not in payload:
        raise Error("decode", f"Response is missing {field!r}")
    items = payload[field]
    if items is None:
        return []
    if not isinstance(items, list) or any(not isinstance(item, dict) for item in items):
        raise Error("decode", f"Response field {field!r} must be an array of objects")
    return items


class SandboxAPI(Transport, ABC):
    """Resource methods using the client's transport and cleanup helpers."""

    @staticmethod
    def _sandbox_path(sandbox_id: str) -> str:
        return "/v1/sandboxes/" + quote(sandbox_id, safe="")

    def _execution_path(self, sandbox_id: str, execution_id: str) -> str:
        return self._sandbox_path(sandbox_id) + "/executions/" + quote(
            execution_id, safe=""
        )

    @_public_cancellation
    def create_sandbox(
        self,
        request: CreateSandboxRequest | None = None,
        *,
        cancel: CancellationToken | None = None,
    ) -> Sandbox:
        """Create a sandbox once; creation is never automatically retried."""
        payload = (request if request is not None else CreateSandboxRequest()).to_dict()
        return Sandbox.from_dict(
            self._json("POST", "/v1/sandboxes", json_body=payload, cancel=cancel, retry=False)
        )

    @_public_cancellation
    def get_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("GET", self._sandbox_path(sandbox_id), cancel=cancel)
        )

    @_public_cancellation
    def list_sandboxes(
        self, *, cancel: CancellationToken | None = None
    ) -> list[Sandbox]:
        payload = self._json("GET", "/v1/sandboxes", cancel=cancel)
        return [Sandbox.from_dict(item) for item in _resource_items(payload, "sandboxes")]

    @_public_cancellation
    def pause_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("POST", self._sandbox_path(sandbox_id) + "/pause", cancel=cancel)
        )

    @_public_cancellation
    def resume_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("POST", self._sandbox_path(sandbox_id) + "/resume", cancel=cancel)
        )

    @_public_cancellation
    def kill_sandbox(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> Sandbox:
        return Sandbox.from_dict(
            self._json("DELETE", self._sandbox_path(sandbox_id), cancel=cancel)
        )

    def start_execution(
        self,
        sandbox_id: str,
        request: ExecuteRequest,
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        """Start once across retries, recovering uncertain starts on cancellation."""
        keyed_request = replace(
            request, idempotency_key=request.idempotency_key or secrets.token_hex(16)
        )
        if cancel is not None:
            try:
                cancel._check()
            except _Cancellation as exc:
                raise Error("canceled", cause=exc) from exc
        try:
            payload = self._json(
                "POST",
                self._sandbox_path(sandbox_id) + "/executions",
                json_body=keyed_request.to_dict(),
                cancel=cancel,
            )
            return Execution.from_dict(payload)
        except _Cancellation as exc:
            if not exc.dispatched:
                raise Error("canceled", cause=exc) from exc
            return self._cancel_remote(
                sandbox_id, execution_id=None, request=keyed_request, cause=exc
            )
        except Error as exc:
            if not exc.idempotency_key:
                exc.idempotency_key = keyed_request.idempotency_key
            raise

    @_public_cancellation
    def get_execution(
        self,
        sandbox_id: str,
        execution_id: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        return Execution.from_dict(
            self._json("GET", self._execution_path(sandbox_id, execution_id), cancel=cancel)
        )

    @_public_cancellation
    def list_executions(
        self, sandbox_id: str, *, cancel: CancellationToken | None = None
    ) -> list[Execution]:
        payload = self._json(
            "GET", self._sandbox_path(sandbox_id) + "/executions", cancel=cancel
        )
        return [Execution.from_dict(item) for item in _resource_items(payload, "executions")]

    @_public_cancellation
    def cancel_execution(
        self,
        sandbox_id: str,
        execution_id: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        """Request cancellation and return the server's latest execution snapshot."""
        return Execution.from_dict(
            self._json(
                "POST",
                self._execution_path(sandbox_id, execution_id) + "/cancel",
                cancel=cancel,
            )
        )

    @_public_cancellation
    def read_file(
        self,
        sandbox_id: str,
        path: str,
        *,
        cancel: CancellationToken | None = None,
    ) -> bytes:
        return self._request(
            "GET",
            self._sandbox_path(sandbox_id) + "/files?" + urlencode({"path": path}),
            cancel=cancel,
        )

    @_public_cancellation
    def write_file(
        self,
        sandbox_id: str,
        path: str,
        data: bytes,
        *,
        cancel: CancellationToken | None = None,
    ) -> None:
        self._request(
            "PUT",
            self._sandbox_path(sandbox_id) + "/files?" + urlencode({"path": path}),
            body=data,
            cancel=cancel,
        )

    @contextmanager
    def sandbox(
        self,
        request: CreateSandboxRequest | None = None,
        *,
        cancel: CancellationToken | None = None,
    ) -> Iterator[Sandbox]:
        """Create a sandbox and kill it on exit with an independent cleanup budget."""
        sandbox = self.create_sandbox(request, cancel=cancel)
        try:
            yield sandbox
        except BaseException as body_error:
            try:
                self.kill_sandbox(sandbox.id, cancel=CancellationToken(timeout=5))
            except BaseException as cleanup_error:
                raise body_error from cleanup_error
            raise
        else:
            self.kill_sandbox(sandbox.id, cancel=CancellationToken(timeout=5))

    @abstractmethod
    def _cancel_remote(
        self, sandbox_id: str, execution_id: str | None, *,
        request: ExecuteRequest | None = None, cause: BaseException | None = None,
    ) -> NoReturn:
        """Recover and confirm cancellation, then raise its typed outcome."""
        raise NotImplementedError
