"""SSE framing and replay. Only a fully terminated frame advances the cursor."""

from __future__ import annotations

import json

from ._errors import Error
from ._models import Event, Execution

MAX_EVENT_BYTES = 16 << 20


def read_event(response):
    event_type, event_id, data, size = "", None, [], 0
    while True:
        raw = response.readline(MAX_EVENT_BYTES + 1)
        if not raw:
            raise Error("transport", "stream closed before a complete event")
        size += len(raw)
        if len(raw) > MAX_EVENT_BYTES or size > MAX_EVENT_BYTES:
            raise Error("stream", "event exceeds 16 MiB")
        if not raw.endswith(b"\n"):
            raise Error("transport", "stream closed during an event")
        try:
            line = raw.decode("utf-8").rstrip("\r\n")
        except UnicodeError as exc:
            raise Error("stream", "invalid UTF-8 event", cause=exc) from exc
        if not line:
            if not event_type and not data:
                event_id, size = None, 0
                continue
            if event_id is None:
                raise Error("stream", "event has no positive integer id")
            return _decode_event(event_id, event_type, "\n".join(data))
        if line.startswith(":"):
            continue
        name, _, value = line.partition(":")
        if value.startswith(" "):
            value = value[1:]
        if name == "id":
            if not value.isascii() or not value.isdecimal() or len(value) > 20:
                raise Error("stream", "event id is not a positive integer")
            event_id = int(value)
            if not 0 < event_id < 2**64:
                raise Error("stream", "event id is out of range")
        elif name == "event":
            event_type = value
        elif name == "data":
            data.append(value)


def _decode_event(event_id, event_type, data):
    if event_type not in {"stdout", "stderr", "truncated", "complete"}:
        return Event(id=event_id, type=event_type)
    try:
        payload = json.loads(data)
        if not isinstance(payload, dict):
            raise ValueError("expected a JSON object")
        if event_type in {"stdout", "stderr"}:
            if not isinstance(payload.get("data"), str):
                raise ValueError("output data must be a string")
            return Event(id=event_id, type=event_type, data=payload["data"])
        if event_type == "truncated":
            limit = payload.get("limit_bytes")
            if type(limit) is not int or limit < 0:
                raise ValueError("invalid output limit")
            return Event(id=event_id, type=event_type, limit_bytes=limit)
        execution = Execution.from_dict(payload)
        if execution.state != "completed":
            raise ValueError("complete event is not terminal")
        return Event(id=event_id, type=event_type, execution=execution)
    except (ValueError, UnicodeError, Error) as exc:
        raise Error("stream", "invalid event payload", cause=exc) from exc
