"""Synchronous SDK orchestration over the language-neutral HTTP contract."""

from __future__ import annotations

from collections.abc import Callable
from typing import NoReturn

from ._api import SandboxAPI
from ._cancel import CancellationToken, _Cancellation
from ._errors import Error, outcome_error
from ._models import Event, ExecuteRequest, Execution
from ._stream import read_event
from ._transport import _backoff, _sleep, _status_error


class Client(SandboxAPI):
    """Client for sandboxd. See docs/sdk-python.md for the public contract."""

    def exec(
        self, sandbox_id: str, command: list[str], *,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        return self.exec_stream(sandbox_id, command, cancel=cancel)

    def exec_stream(
        self, sandbox_id: str, command: list[str], *,
        on_event: Callable[[Event], None] | None = None,
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        started = self.start_execution(sandbox_id, ExecuteRequest(command=command), cancel=cancel)
        execution = self.stream_execution(
            sandbox_id, started.id, on_event=on_event, on_stdout=on_stdout,
            on_stderr=on_stderr, cancel=cancel,
        )
        error = outcome_error(execution)
        if error is not None:
            raise error
        return execution

    def stream_execution(
        self, sandbox_id: str, execution_id: str, *,
        on_event: Callable[[Event], None] | None = None,
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
        cancel: CancellationToken | None = None,
    ) -> Execution:
        cursor = 0
        try:
            for attempt in range(self._max_retries + 1):
                headers = {"Accept": "text/event-stream"}
                if cursor:
                    headers["Last-Event-ID"] = str(cursor)
                try:
                    with self._exchange(
                        "GET", self._execution_path(sandbox_id, execution_id) + "/events",
                        headers=headers, cancel=cancel,
                    ) as response:
                        if response.status != 200:
                            raise _status_error(response.status, response.read())
                        while True:
                            if cancel is not None:
                                cancel._check()
                            event = read_event(response)
                            if event.id <= cursor:
                                continue
                            if event.type not in {"stdout", "stderr", "truncated", "complete"}:
                                cursor = event.id
                                continue
                            if event.execution is not None and (
                                event.execution.id != execution_id or event.execution.sandbox_id != sandbox_id
                            ):
                                raise Error("stream", "complete event identifies a different execution")
                            try:
                                if on_event is not None:
                                    on_event(event)
                                if event.type == "stdout" and on_stdout is not None:
                                    on_stdout(event.data)
                                if event.type == "stderr" and on_stderr is not None:
                                    on_stderr(event.data)
                            except Exception as exc:
                                raise Error("callback", "event callback failed", cause=exc) from exc
                            cursor = event.id
                            if event.execution is not None:
                                return event.execution
                except Error as exc:
                    if exc.code == "callback":
                        raise
                    if cancel is not None:
                        cancel._check()
                    if not exc.retryable:
                        raise
                    if attempt == self._max_retries:
                        raise Error(
                            "stream", "stream reconnect budget exhausted",
                            status_code=exc.status_code, cause=exc,
                        ) from exc
                _sleep(_backoff(attempt), cancel)
        except _Cancellation as exc:
            return self._cancel_remote(sandbox_id, execution_id, cause=exc)
        raise Error("stream", "stream ended without completion")

    def _cancel_remote(
        self, sandbox_id: str, execution_id: str | None, *,
        request: ExecuteRequest | None = None, cause: BaseException | None = None,
    ) -> NoReturn:
        """Recover and confirm under one fresh five-second total deadline."""
        cleanup = CancellationToken(timeout=5)
        key = request.idempotency_key if request is not None else ""
        execution = Execution(id=execution_id or "", sandbox_id=sandbox_id)
        try:
            if request is not None:
                execution = Execution.from_dict(self._json(
                    "POST", self._sandbox_path(sandbox_id) + "/executions",
                    json_body=request.to_dict(), cancel=cleanup,
                ))
                execution_id = execution.id
            if not execution_id:
                raise Error("decode", "cancellation requires an execution id")
            execution = self.cancel_execution(sandbox_id, execution_id, cancel=cleanup)
            while execution.state != "completed":
                _sleep(0.05, cleanup)
                execution = self.get_execution(sandbox_id, execution_id, cancel=cleanup)
            if execution.id != execution_id or execution.sandbox_id != sandbox_id:
                raise Error("decode", "cancellation snapshot identifies another execution")
        except (Error, _Cancellation) as exc:
            raise Error(
                "cancel_unconfirmed", "could not confirm that remote execution stopped",
                execution=execution, idempotency_key=key, cause=exc,
            ) from exc
        raise Error(
            "canceled", "caller canceled; remote execution is terminal",
            execution=execution, idempotency_key=key, cause=cause,
        )
