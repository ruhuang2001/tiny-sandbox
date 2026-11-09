"""Bounded retries and interruptible HTTP I/O, without a global stream timeout."""

from __future__ import annotations

import http.client
import json
import os
import random
import socket
import threading
import time
from contextlib import contextmanager
from urllib.parse import urlsplit

from ._cancel import CancellationToken, _Cancellation
from ._errors import Error


def _backoff(attempt: int, retry_after: str = "") -> float:
    if retry_after.isascii() and retry_after.isdecimal():
        seconds = int(retry_after)
        if 0 < seconds <= 2**31 - 1:
            return float(seconds)
    cap = min(2.0, 0.2 * 2 ** min(attempt, 4))
    return random.uniform(cap / 2, cap)


def _sleep(seconds: float, cancel: CancellationToken | None) -> None:
    if cancel is None:
        time.sleep(seconds)
    else:
        cancel._wait(seconds)


def _connect(conn, cancel):
    if cancel is None:
        conn.connect()
        return
    # The worker only connects; it cannot send requests after cancellation.
    finished, abandoned = threading.Event(), threading.Event()
    errors = []

    def connect():
        try:
            conn.connect()
        except Exception as exc:
            errors.append(exc)
        finally:
            finished.set()
            if abandoned.is_set():
                conn.close()

    threading.Thread(target=connect, daemon=True).start()
    try:
        while not finished.wait(0.02):
            cancel._check()
        cancel._check()
    except BaseException:
        abandoned.set()
        if finished.is_set():
            conn.close()
        raise
    if errors:
        raise errors[0]


def _status_error(status: int, body: bytes) -> Error:
    code, message = "http_error", "HTTP " + str(status)
    try:
        payload = json.loads(body)
        if isinstance(payload, dict):
            code = payload.get("code") or code
            message = payload.get("message") or message
    except (ValueError, UnicodeError):
        message = body.decode("utf-8", errors="replace") or message
    return Error(str(code), str(message), status_code=status)


class Transport:
    def __init__(
        self, *, domain: str = "", api_key: str = "",
        max_retries: int | None = None,
    ):
        domain = domain or os.environ.get("SANDBOX_DOMAIN") or "http://localhost:8080"
        if "://" not in domain:
            domain = "http://" + domain
        try:
            url = urlsplit(domain)
            port = url.port
        except ValueError as exc:
            raise Error("config", "invalid domain", cause=exc) from exc
        if url.scheme not in {"http", "https"} or not url.hostname or (
            url.username is not None or url.password is not None or url.query or url.fragment
        ):
            raise Error("config", "domain must be an HTTP(S) origin or path prefix")
        if max_retries is None:
            max_retries = 3
        if isinstance(max_retries, bool) or not isinstance(max_retries, int) or max_retries < 0:
            raise Error("config", "max_retries must be a nonnegative integer or None")
        self.domain = domain.rstrip("/")
        self._url = url
        self._port = port
        self._api_key = api_key or os.environ.get("SANDBOX_API_KEY", "")
        self._max_retries = max_retries
        self._closed = False

    def close(self) -> None:
        """Reject future calls; each request already owns and closes its socket."""
        self._closed = True

    def __enter__(self):
        if self._closed:
            raise Error("closed", "client is closed")
        return self

    def __exit__(self, *_):
        self.close()

    @contextmanager
    def _exchange(self, method, path, *, body=None, headers=None, cancel=None):
        if self._closed:
            raise Error("closed", "client is closed")
        if cancel is not None:
            cancel._check()
        timeout = None if cancel is None else cancel._remaining()
        if timeout is not None:
            timeout = max(0.001, timeout)
        connection_type = (
            http.client.HTTPSConnection if self._url.scheme == "https"
            else http.client.HTTPConnection
        )
        conn = connection_type(self._url.hostname, self._port, timeout=timeout)
        done = threading.Event()
        sockets = []
        watcher = None
        dispatched = False
        response = None

        def interrupt():
            # shutdown(), not close(), wakes another thread's buffered read.
            while not done.wait(0.02):
                if cancel.cancelled:
                    for sock in [conn.sock, *sockets]:
                        if sock is not None:
                            try:
                                sock.shutdown(socket.SHUT_RDWR)
                            except OSError:
                                pass

        if cancel is not None:
            watcher = threading.Thread(target=interrupt, daemon=True)
            watcher.start()
        try:
            _connect(conn, cancel)
            sockets.append(conn.sock)
            if cancel is not None:
                cancel._check()
            request_headers = {"Accept": "application/json", **(headers or {})}
            if self._api_key:
                request_headers["Authorization"] = "Bearer " + self._api_key
            dispatched = True
            conn.request(method, self._url.path.rstrip("/") + path, body, request_headers)
            response = conn.getresponse()
            if cancel is not None:
                cancel._check()
            yield response
            if cancel is not None:
                cancel._check()
        except _Cancellation as exc:
            exc.dispatched = exc.dispatched or dispatched
            raise
        except (OSError, http.client.HTTPException) as exc:
            if cancel is not None and cancel.cancelled:
                raise _Cancellation(dispatched=dispatched) from exc
            error = Error("transport", "HTTP connection failed", cause=exc)
            error._dispatched = dispatched
            raise error from exc
        finally:
            done.set()
            if response is not None:
                response.close()
            conn.close()
            if watcher is not None:
                watcher.join()

    def _request(self, method, path, *, body=None, json_body=None, cancel=None, retry=True):
        headers = {}
        if json_body is not None:
            try:
                body = json.dumps(json_body, ensure_ascii=False, allow_nan=False).encode("utf-8")
            except (TypeError, ValueError, UnicodeError) as exc:
                raise Error("encode", "invalid request body", cause=exc) from exc
            headers["Content-Type"] = "application/json"
        elif body is not None:
            headers["Content-Type"] = "application/octet-stream"
        dispatched = False
        try:
            for attempt in range(self._max_retries + 1):
                after = ""
                try:
                    with self._exchange(method, path, body=body, headers=headers, cancel=cancel) as response:
                        dispatched = True
                        data = response.read()
                        if 200 <= response.status < 300:
                            return data
                        after = response.getheader("Retry-After", "")
                        error = _status_error(response.status, data)
                except Error as exc:
                    error = exc
                    dispatched = dispatched or getattr(exc, "_dispatched", False)
                if not retry or not error.retryable or attempt == self._max_retries:
                    raise error
                _sleep(_backoff(attempt, after), cancel)
        except _Cancellation as exc:
            exc.dispatched = exc.dispatched or dispatched
            raise

    def _json(self, method, path, **kwargs):
        data = self._request(method, path, **kwargs)
        try:
            result = json.loads(data)
            if not isinstance(result, dict):
                raise ValueError("expected an object")
            return result
        except (ValueError, UnicodeError) as exc:
            raise Error("decode", "invalid JSON response", cause=exc) from exc
