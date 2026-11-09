"""One exception family with stable codes and retained execution evidence."""

from __future__ import annotations

from ._models import Execution

ERR_NONZERO_EXIT = "nonzero_exit"
ERR_TIMEOUT = "timeout"
ERR_CANCELED = "canceled"
ERR_OOM = "oom"
ERR_EVICTED = "evicted"
ERR_CANCEL_UNCONFIRMED = "cancel_unconfirmed"
ERR_NOT_FOUND = "not_found"
ERR_UNAUTHORIZED = "unauthorized"
ERR_BUSY = "busy"
ERR_INVALID_STATE = "invalid_state"

RETRYABLE_STATUSES = frozenset({429, 500, 502, 503, 504})


class Error(Exception):
    def __init__(
        self, code: str, message: str = "", *, status_code: int = 0,
        execution: Execution | None = None, idempotency_key: str = "",
        cause: BaseException | None = None,
    ):
        self.code = code
        self.message = message or "sandbox: " + code
        self.status_code = status_code
        self.execution = execution
        self.idempotency_key = idempotency_key
        self.cause = cause
        super().__init__(self.message)
        if cause is not None:
            self.__cause__ = cause

    @property
    def retryable(self) -> bool:
        return self.status_code in RETRYABLE_STATUSES or (
            self.status_code == 0 and self.code == "transport"
        )

    def matches(self, code: str) -> bool:
        if code == self.code:
            return True
        if code == ERR_NOT_FOUND:
            return self.status_code == 404
        if code == ERR_UNAUTHORIZED:
            return self.status_code == 401
        return code == ERR_NONZERO_EXIT and self.execution is not None and (
            self.execution.exit_code is not None and self.execution.exit_code != 0
        )


def outcome_error(execution: Execution) -> Error | None:
    reason = execution.termination_reason
    if execution.state != "completed" or reason is None:
        return Error("incomplete", execution=execution)
    if reason == "exited":
        if execution.exit_code == 0:
            return None
        return Error(ERR_NONZERO_EXIT, execution=execution)
    # Preserve future reason strings; an unknown reason must not mean success.
    return Error(reason, execution=execution)
