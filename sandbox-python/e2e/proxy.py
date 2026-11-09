"""HTTP faults in front of real sandboxd, retaining request and SSE evidence."""
from __future__ import annotations

import dataclasses
import http.client
import http.server
import json
import re
import socket
import threading
import time
from pathlib import Path


@dataclasses.dataclass
class Fault:
    method: str
    path: str
    action: str
    remaining: int = 1
    status: int = 503
    retry_after: str | None = None
    location: str | None = None
    delay: float = 0.0
    accepted: threading.Event = dataclasses.field(default_factory=threading.Event)


class FaultProxy:
    def __init__(self, upstream_port: int, trace: Path):
        self.upstream_port = upstream_port
        self.trace_path = trace
        self.trace_path.write_text("")
        self.lock = threading.Lock()
        self.faults: list[Fault] = []
        self.records: list[dict] = []
        self.scenario = "setup"
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):
                pass

            def do_GET(self):
                self.forward()

            do_POST = do_GET
            do_DELETE = do_GET
            do_PUT = do_GET

            def disconnect(self):
                self.close_connection = True
                try:
                    self.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

            def forward(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                headers = {k: v for k, v in self.headers.items() if k.lower() not in {"host", "connection"}}
                record = {"scenario": owner.scenario, "at": time.monotonic(),
                          "method": self.command, "path": self.path,
                          "last_event_id": self.headers.get("Last-Event-ID"),
                          "authenticated": bool(self.headers.get("Authorization")),
                          "body_bytes": len(body)}
                if body and "json" in self.headers.get("Content-Type", ""):
                    try:
                        record["body"] = json.loads(body)
                    except ValueError:
                        record["body"] = "<invalid JSON>"
                fault = owner.choose(self.command, self.path)
                record["fault"] = fault.action if fault else None
                upstream = None
                try:
                    if fault and fault.action in {"status", "redirect"}:
                        record["status"] = fault.status
                        payload = json.dumps({"code": "injected_failure", "message": "E2E proxy fault"}).encode()
                        self.send_response(fault.status)
                        self.send_header("Content-Type", "application/json")
                        self.send_header("Content-Length", str(len(payload)))
                        if fault.retry_after is not None:
                            self.send_header("Retry-After", fault.retry_after)
                        if fault.location is not None:
                            self.send_header("Location", fault.location)
                        self.end_headers()
                        self.wfile.write(payload)
                        return
                    if fault and fault.action == "replay":
                        headers.pop("Last-Event-ID", None)
                    upstream = http.client.HTTPConnection("127.0.0.1", owner.upstream_port, timeout=40)
                    upstream.request(self.command, self.path, body, headers)
                    response = upstream.getresponse()
                    record["status"] = response.status
                    content_type = response.getheader("Content-Type", "")
                    if "text/event-stream" in content_type:
                        self.send_response(response.status)
                        self.send_header("Content-Type", content_type)
                        self.send_header("Connection", "close")
                        self.end_headers()
                        self.close_connection = True
                        frame = bytearray()
                        delivered = 0
                        additive_inserted = False
                        record["frames"] = []
                        while True:
                            line = response.readline()
                            if not line:
                                break
                            frame.extend(line)
                            if line not in (b"\n", b"\r\n"):
                                continue
                            raw = bytes(frame)
                            frame.clear()
                            match = re.search(rb"^id: *(\d+)", raw, re.M)
                            event = re.search(rb"^event: *(\w+)", raw, re.M)
                            kind = event.group(1).decode() if event else "comment"
                            record["frames"].append({"id": int(match.group(1)) if match else None, "type": kind})
                            if fault and fault.action == "malformed" and kind in {"stdout", "stderr"}:
                                raw = re.sub(rb"^data:.*$", b"data: {broken", raw, flags=re.M)
                            if fault and fault.action == "false_exit_code" and kind == "complete":
                                raw = re.sub(rb'"exit_code":\s*\d+', b'"exit_code":false', raw)
                            if fault and fault.action == "additive" and match:
                                original_id = int(match.group(1))
                                if not additive_inserted:
                                    future = f': proxy comment\n\nid: {original_id}\nevent: future_event\ndata: {{"future":true}}\n\n'
                                    self.wfile.write(future.encode())
                                    additive_inserted = True
                                raw = re.sub(rb"^id: *\d+", f"id: {original_id + 1}".encode(), raw, flags=re.M)
                                if kind in {"stdout", "stderr"}:
                                    raw = raw.replace(b'data: {"data":', b'data: {"data":\ndata: ')
                            if fault and fault.action == "cut" and delivered >= 1:
                                raw = raw[:max(1, len(raw) // 2)]
                                record["partial_frame"] = True
                                self.wfile.write(raw)
                                self.wfile.flush()
                                self.disconnect()
                                return
                            self.wfile.write(raw)
                            self.wfile.flush()
                            if match:
                                delivered += 1
                        return
                    payload = response.read()
                    if "json" in content_type:
                        try:
                            decoded = json.loads(payload)
                            if isinstance(decoded, dict):
                                record["response_id"] = decoded.get("id")
                                record["response_state"] = decoded.get("state")
                        except ValueError:
                            pass
                    if fault:
                        fault.accepted.set()
                    if fault and fault.action == "hold":
                        time.sleep(fault.delay)
                    if fault and fault.action == "old_response":
                        decoded = json.loads(payload)
                        for key in ("pids_limit", "nano_cpus", "network"):
                            decoded.pop(key, None)
                        decoded["future_field"] = {"version": 999}
                        payload = json.dumps(decoded).encode()
                    if fault and fault.action == "future_state":
                        decoded = json.loads(payload)
                        decoded.update(state="future_state", network="future_network", future_field=42)
                        payload = json.dumps(decoded).encode()
                    if fault and fault.action == "malformed_start":
                        decoded = json.loads(payload)
                        decoded["command"] = "not an argv list"
                        payload = json.dumps(decoded).encode()
                    if fault and fault.action == "running_snapshot":
                        decoded = json.loads(payload)
                        decoded.update(state="running", exit_code=None, termination_reason=None, finished_at=None)
                        payload = json.dumps(decoded).encode()
                    if fault and fault.action == "plain":
                        payload = b"upstream proxy failed without JSON"
                        content_type = "text/plain"
                        record["status"] = 502
                    self.send_response(record["status"])
                    self.send_header("Content-Type", content_type)
                    self.send_header("Content-Length", str(len(payload)))
                    self.send_header("Connection", "close")
                    self.end_headers()
                    self.close_connection = True
                    if fault and fault.action == "truncate":
                        payload = payload[:max(1, len(payload) // 2)]
                        record["truncated_bytes"] = len(payload)
                    self.wfile.write(payload)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError, OSError, http.client.HTTPException) as exc:
                    record["transport_error"] = type(exc).__name__
                    self.close_connection = True
                finally:
                    if upstream:
                        upstream.close()
                    record["finished_at"] = time.monotonic()
                    owner.record(record)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.server.daemon_threads = True
        self.port = self.server.server_address[1]
        self.domain = f"http://127.0.0.1:{self.port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def choose(self, method: str, path: str):
        with self.lock:
            for fault in self.faults:
                if fault.remaining and fault.method == method and re.fullmatch(fault.path, path):
                    fault.remaining -= 1
                    return fault
        return None

    def arm(self, scenario: str, *faults: Fault):
        with self.lock:
            self.scenario = scenario
            self.faults = list(faults)

    def record(self, record: dict):
        with self.lock:
            self.records.append(record)
            with self.trace_path.open("a", encoding="utf-8") as file:
                file.write(json.dumps(record, ensure_ascii=False) + "\n")

    def requests(self, scenario: str, method: str | None = None, suffix: str | None = None):
        with self.lock:
            return sorted((dict(r) for r in self.records if r["scenario"] == scenario
                           and (method is None or r["method"] == method)
                           and (suffix is None or r["path"].endswith(suffix))), key=lambda r: r["at"])

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
