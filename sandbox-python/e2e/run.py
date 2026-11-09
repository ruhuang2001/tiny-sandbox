#!/usr/bin/env python3
"""Installed-wheel E2E with real sandboxd + Docker, and HTTP transport faults."""
from __future__ import annotations

import argparse
import dataclasses
import hashlib
import http.client
import importlib.metadata
import json
import os
import platform
import socket
import subprocess
import sys
import threading
import time
import traceback
from pathlib import Path

from tiny_sandbox import CancellationToken, Client, CreateSandboxRequest, Error, ExecuteRequest
from proxy import Fault, FaultProxy


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def error_from(call, code=None):
    try:
        call()
    except Error as exc:
        if code is not None:
            require(exc.code == code, f"expected {code}, got {exc.code}: {exc}")
            require(exc.matches(code), f"matches({code!r}) returned false")
        return exc
    raise AssertionError(f"expected SDK Error ({code or 'any code'})")


def snapshot(value):
    return dataclasses.asdict(value)


class Suite:
    def __init__(self, server: Path, artifacts: Path):
        self.server, self.artifacts = server, artifacts
        self.results = []
        self.process = self.proxy = self.client = self.direct = self.owner = self.log = None
        self.key = "python-e2e-local-only"
        self.report = {"schema_version": 1, "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                       "python": sys.version, "platform": platform.platform(),
                       "distribution": importlib.metadata.version("tiny-sandbox"),
                       "command": "PYTHON=python3 bash scripts/e2e-python.sh artifacts/python-e2e", "checks": self.results}

    def start(self):
        with socket.socket() as probe:
            probe.bind(("127.0.0.1", 0))
            port = probe.getsockname()[1]
        self.owner = f"127.0.0.1:{port}"
        self.domain = f"http://{self.owner}"
        self.log = (self.artifacts / "sandboxd.log").open("w")
        self.process = subprocess.Popen([str(self.server), "-addr", self.owner], stdout=self.log, stderr=subprocess.STDOUT,
                                        env=dict(os.environ, SANDBOX_API_KEY=self.key))
        for _ in range(100):
            connection = http.client.HTTPConnection("127.0.0.1", port, timeout=.2)
            try:
                connection.request("GET", "/healthz")
                response = connection.getresponse()
                if response.status == 200:
                    self.report["health"] = json.loads(response.read())
                    break
            except OSError:
                time.sleep(.1)
            finally:
                connection.close()
        else:
            raise RuntimeError("sandboxd failed to start; inspect sandboxd.log")
        self.proxy = FaultProxy(port, self.artifacts / "proxy.ndjson")
        self.client = Client(domain=self.proxy.domain, api_key=self.key, max_retries=2)
        self.direct = Client(domain=self.domain, api_key=self.key, max_retries=2)
        self.report.update(owner=self.owner, domain=self.domain, proxy=self.proxy.domain)
        root = Path(__file__).resolve().parents[2]
        files = list((root / "sandbox-python").rglob("*.py"))
        files += [root / "sandbox-python/pyproject.toml", root / "scripts/e2e-python.sh", self.server]
        self.report["sha256"] = {str(p.relative_to(root)) if p.is_relative_to(root) else str(p): hashlib.sha256(p.read_bytes()).hexdigest()
                                 for p in files if p.is_file() and "__pycache__" not in str(p)}
        self.report["git_head"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=root, text=True).strip()
        self.report["git_status"] = subprocess.check_output(["git", "status", "--short"], cwd=root, text=True).splitlines()
        distribution = importlib.metadata.distribution("tiny-sandbox")
        self.report["installed_sha256"] = {
            str(file): hashlib.sha256(Path(distribution.locate_file(file)).read_bytes()).hexdigest()
            for file in distribution.files or [] if str(file).startswith("tiny_sandbox/") and file.suffix != ".pyc"
        }
        self.report["wheel_sha256"] = {str(path): hashlib.sha256(path.read_bytes()).hexdigest()
                                         for path in (self.artifacts / "wheels").glob("*.whl")}
        self.persist()

    def persist(self):
        (self.artifacts / "report.json").write_text(json.dumps(self.report, indent=2, ensure_ascii=False) + "\n")

    def check(self, name, action):
        started = time.monotonic()
        self.proxy.arm(name)
        try:
            evidence = action(name)
            row = {"name": name, "status": "passed", "seconds": round(time.monotonic() - started, 3), "evidence": evidence}
            print(f"PASS {name}", flush=True)
        except Exception as exc:
            row = {"name": name, "status": "failed", "seconds": round(time.monotonic() - started, 3),
                   "error": str(exc), "traceback": traceback.format_exc()}
            print(f"FAIL {name}: {exc}", flush=True)
        self.results.append(row)
        self.proxy.arm("between-scenarios")
        self.persist()

    def sandbox(self, **kwargs):
        return self.direct.sandbox(CreateSandboxRequest(timeout_seconds=0, network="none", **kwargs))

    def requests(self, name, method=None, suffix=None):
        # Flush the handler's finally record after the client receives a response.
        time.sleep(.06)
        return self.proxy.requests(name, method, suffix)

    def entry(self, name):
        with self.sandbox() as sb:
            command = ["sh", "-c", "i=0; while [ $i -lt 100 ]; do printf 'chunk-%s-世界\n' $i; i=$((i+1)); sleep 0.15; done; sleep 30"]
            execution = self.direct.start_execution(sb.id, ExecuteRequest(command=command))
            token = CancellationToken(timeout=15)
            events, chunks = [], []
            self.proxy.arm(name, Fault("GET", r".*/events", "cut"), Fault("GET", r".*/events", "replay"))
            def event(value):
                events.append({"id": value.id, "type": value.type})
            def stdout(value):
                chunks.append(value)
                if len(chunks) >= 5:
                    token.cancel()
            exc = error_from(lambda: self.client.stream_execution(sb.id, execution.id, on_event=event, on_stdout=stdout, cancel=token), "canceled")
            require(exc.execution.state == "completed", "cancellation was not terminal")
            require(exc.execution.termination_reason == "canceled", "server cancellation reason was lost")
            require(exc.execution.stdout.startswith("".join(chunks)), "replayed output duplicated or changed UTF-8")
            ids = [e["id"] for e in events]
            require(ids == sorted(set(ids)), f"event IDs duplicated or out of order: {ids}")
            requests = self.requests(name, "GET", "/events")
            require(len(requests) >= 2, "stream did not reconnect")
            require(requests[1]["last_event_id"] == "1", f"partial frame advanced replay cursor: {requests}")
            require(any(r.get("partial_frame") for r in requests), "proxy did not cut an SSE frame")
            return {"execution": snapshot(exc.execution), "events": events, "stream_requests": len(requests), "resume_cursor": requests[1]["last_event_id"]}

    def lifecycle(self, name):
        request = CreateSandboxRequest(timeout_seconds=0, idle_timeout_seconds=0, memory_mb=64, pids_limit=128, nano_cpus=100000000, network="none")
        sb = self.client.create_sandbox(request)
        try:
            require(sb.state == "running" and sb.memory_mb == 64 and sb.network == "none", "typed sandbox fields lost")
            require(sb.pids_limit == 128 and sb.nano_cpus == 100000000, "resource controls not returned")
            require(sb.id in [value.id for value in self.client.list_sandboxes()], "list omitted created sandbox")
            require(self.client.pause_sandbox(sb.id).state == "paused", "pause failed")
            require(self.client.pause_sandbox(sb.id).state == "paused", "pause not idempotent")
            require(self.client.resume_sandbox(sb.id).state == "running", "resume failed")
            path, data = "/tmp/directory with spaces/世界 ?#%.bin", bytes(range(256)) * 8 + b"\x00end"
            self.client.write_file(sb.id, path, data)
            require(self.client.read_file(sb.id, path) == data, "file bytes or escaped path changed")
            error_from(lambda: self.client.read_file(sb.id, "/absent-file"), "not_found")
            execution = self.client.exec(sb.id, ["sh", "-c", "printf out; printf err >&2"])
            require((execution.stdout, execution.stderr, execution.exit_code) == ("out", "err", 0), "successful execution lost output")
            require(self.client.get_execution(sb.id, execution.id).id == execution.id, "get execution changed ID")
            require(execution.id in [value.id for value in self.client.list_executions(sb.id)], "list executions lost result")
            require(self.client.kill_sandbox(sb.id).state == "killed", "kill failed")
            require(self.client.kill_sandbox(sb.id).state == "killed", "kill not idempotent")
            error_from(lambda: self.client.pause_sandbox(sb.id), "invalid_state")
            return {"sandbox": sb.id, "execution": execution.id, "file_bytes": len(data)}
        finally:
            self.direct.kill_sandbox(sb.id)

    def retry_statuses(self, name):
        evidence = []
        for status in (429, 500, 502, 503, 504, 400, 401, 404, 409, 413, 501):
            label = f"{name}-{status}"
            self.proxy.arm(label, Fault("GET", "/v1/sandboxes", "status", remaining=20, status=status))
            exc = error_from(self.client.list_sandboxes)
            requests = self.requests(label)
            expected = 3 if status in (429, 500, 502, 503, 504) else 1
            require(len(requests) == expected, f"status {status}: {len(requests)} attempts != {expected}")
            require(exc.status_code == status, f"HTTP {status} lost")
            require(exc.retryable == (expected == 3), f"retryable differs for {status}")
            gaps = [b["at"] - a["at"] for a, b in zip(requests, requests[1:])]
            for i, gap in enumerate(gaps):
                cap = min(2, .2 * 2 ** i)
                require(cap / 2 - .02 <= gap <= cap + .35, f"backoff outside jitter window with scheduling allowance: {gaps}")
            evidence.append({"status": status, "attempts": len(requests), "gaps": gaps})
        return evidence

    def distribution(self, name):
        distribution = importlib.metadata.distribution("tiny-sandbox")
        files = [str(file) for file in distribution.files or []]
        require("tiny_sandbox/py.typed" in files, "installed wheel omitted py.typed")
        require(distribution.requires is None or not distribution.requires, "standard-library SDK has runtime dependencies")
        installed = Path(distribution.locate_file("tiny_sandbox/__init__.py")).resolve()
        require("site-packages" in installed.parts, f"SDK imported from source instead of installed wheel: {installed}")
        return {"version": distribution.version, "installed_module": str(installed), "py_typed": True}

    def retry_after(self, name):
        evidence = []
        for value in ("1", "0", "invalid", "Wed, 21 Oct 2015 07:28:00 GMT"):
            label = name + "-" + value
            self.proxy.arm(label, Fault("GET", "/v1/sandboxes", "status", status=503, retry_after=value))
            self.client.list_sandboxes()
            requests = self.requests(label)
            require(len(requests) == 2, "retry-after scenario must retry once")
            gap = requests[1]["at"] - requests[0]["at"]
            require(.95 <= gap <= 1.5 if value == "1" else .08 <= gap <= .55, f"Retry-After {value!r}: delay {gap}")
            evidence.append({"retry_after": value, "gap_seconds": gap})
        return evidence

    def retry_configuration(self, name):
        evidence = []
        for retries in (0, 3):
            label = f"{name}-{retries}"
            self.proxy.arm(label, Fault("GET", "/v1/sandboxes", "status", remaining=30))
            with Client(domain=self.proxy.domain, api_key=self.key, max_retries=retries) as client:
                error_from(client.list_sandboxes)
            requests = self.requests(label)
            require(len(requests) == retries + 1, f"max_retries={retries} sent {len(requests)} requests")
            evidence.append({"max_retries": retries, "requests": len(requests)})
        self.proxy.arm(name, Fault("GET", "/v1/sandboxes", "status", remaining=30, retry_after="20"))
        started = time.monotonic()
        token = CancellationToken(timeout=.2)
        error_from(lambda: self.client.list_sandboxes(cancel=token), "canceled")
        require(time.monotonic() - started < 1, "cancellation waited for Retry-After")
        require(len(self.requests(name)) == 1, "canceled retry wait sent another request")
        return evidence

    def credential_redirect(self, name):
        # A different localhost port is a different origin. The sink forwards to
        # the same real daemon only if the SDK incorrectly follows the redirect.
        sink = FaultProxy(self.proxy.upstream_port, self.artifacts / "redirect-sink.ndjson")
        try:
            self.proxy.arm(name, Fault("GET", "/v1/sandboxes", "redirect", status=307,
                                       location=sink.domain + "/v1/sandboxes"))
            exc = error_from(self.client.list_sandboxes)
            require(exc.status_code == 307, "redirect did not surface original status")
            require(not sink.records, "redirect forwarded request or bearer credentials to another origin")
            return {"status": exc.status_code, "cross_origin_requests": len(sink.records)}
        finally:
            sink.close()

    def tls_predispatch_cancel(self, name):
        accepted, stopping = threading.Event(), threading.Event()
        records, peers = [], []
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        listener.settimeout(.1)
        def serve():
            while not stopping.is_set():
                try:
                    peer, _ = listener.accept()
                except TimeoutError:
                    continue
                except OSError:
                    break
                peers.append(peer)
                peer.settimeout(.2)
                try:
                    records.append({"at": time.monotonic(), "client_hello_bytes": len(peer.recv(65536))})
                except OSError:
                    records.append({"at": time.monotonic(), "client_hello_bytes": 0})
                accepted.set()
                # Keep each accepted TCP socket open without sending TLS bytes.
        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        token = CancellationToken()
        def interrupt():
            if accepted.wait(5):
                token.cancel()
        interrupter = threading.Thread(target=interrupt, daemon=True)
        interrupter.start()
        started = time.monotonic()
        try:
            with Client(domain=f"https://127.0.0.1:{listener.getsockname()[1]}", api_key=self.key) as client:
                exc = error_from(lambda: client.start_execution("sbx_never_dispatched", ExecuteRequest(command=["true"]), cancel=token), "canceled")
            elapsed = time.monotonic() - started
            require(accepted.is_set(), "TLS peer did not receive ClientHello")
            require(elapsed < 1.5, f"pre-dispatch TLS cancel blocked: {elapsed}")
            time.sleep(.3)
            require(len(records) == 1, f"pre-dispatch cancel attempted start recovery: {records}")
            require(exc.execution is None, "pre-dispatch cancellation invented an execution")
            evidence = {"code": exc.code, "elapsed_seconds": elapsed, "connections": records, "http_requests": 0}
            (self.artifacts / "tls-blackhole.json").write_text(json.dumps(evidence, indent=2) + "\n")
            return evidence
        finally:
            stopping.set()
            listener.close()
            for peer in peers:
                peer.close()
            thread.join(timeout=1)
            interrupter.join(timeout=1)

    def creation_once(self, name):
        evidence = []
        for action in ("status", "truncate"):
            label = name + "-" + action
            before = {s.id for s in self.direct.list_sandboxes()}
            self.proxy.arm(label, Fault("POST", "/v1/sandboxes", action))
            error_from(lambda: self.client.create_sandbox(CreateSandboxRequest(timeout_seconds=0, network="none")))
            requests = self.requests(label)
            require(len(requests) == 1, "non-idempotent create retried")
            created = [s for s in self.direct.list_sandboxes() if s.id not in before]
            try:
                require(len(created) == (1 if action == "truncate" else 0), "create effect count changed")
                evidence.append({"fault": action, "requests": len(requests), "created": [s.id for s in created]})
            finally:
                for sb in created:
                    self.direct.kill_sandbox(sb.id)
        return evidence

    def truncated_responses(self, name):
        with self.sandbox() as sb:
            self.proxy.arm(name, Fault("GET", rf"/v1/sandboxes/{sb.id}", "truncate"), Fault("POST", r".*/executions", "truncate"))
            require(self.client.get_sandbox(sb.id).id == sb.id, "truncated GET did not recover")
            execution = self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo once >> /tmp/count; cat /tmp/count"]))
            result = self.direct.stream_execution(sb.id, execution.id)
            require(result.stdout == "once\n", "replayed start ran the command twice")
            starts = self.requests(name, "POST", "/executions")
            keys = [r["body"]["idempotency_key"] for r in starts]
            require(len(starts) == 2 and len(set(keys)) == 1 and len(keys[0]) == 32, f"start recovery changed key: {keys}")
            return {"execution": execution.id, "keys": keys, "stdout": result.stdout}

    def idempotency(self, name):
        with self.sandbox() as sb:
            command = ["sh", "-c", "echo first; sleep 30"]
            first = self.client.start_execution(sb.id, ExecuteRequest(command=command, idempotency_key="e2e-fixed-key"))
            second = self.client.start_execution(sb.id, ExecuteRequest(command=command, idempotency_key="e2e-fixed-key", timeout_seconds=1))
            require(first.id == second.id, "same key started another execution")
            error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["true"], idempotency_key="e2e-fixed-key")), "idempotency_conflict")
            error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["true"])), "busy")
            final = self.client.cancel_execution(sb.id, first.id)
            require(final.state == "completed", "explicit cancel did not complete")
            return {"execution": first.id, "cancel_reason": final.termination_reason}

    def precanceled(self, name):
        token = CancellationToken()
        token.cancel()
        with self.sandbox() as sb:
            error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["true"]), cancel=token), "canceled")
            error_from(lambda: self.client.exec(sb.id, ["true"], cancel=token), "canceled")
            require(not self.requests(name), "pre-canceled operation sent HTTP requests")
            require(not self.direct.list_executions(sb.id), "pre-canceled command executed")
            return {"requests": 0, "cancelled": token.cancelled}

    def start_recovery(self, name, fail=False):
        with self.sandbox() as sb:
            token = CancellationToken()
            hold = Fault("POST", r".*/executions", "hold", delay=1.2)
            faults = [hold]
            if fail:
                faults.append(Fault("POST", r".*/executions", "status", remaining=20))
            self.proxy.arm(name, *faults)
            def interrupt():
                if hold.accepted.wait(10):
                    token.cancel()
            thread = threading.Thread(target=interrupt, daemon=True)
            thread.start()
            started = time.monotonic()
            exc = error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo marker >> /tmp/recovered; sleep 30"]), cancel=token), "cancel_unconfirmed" if fail else "canceled")
            elapsed = time.monotonic() - started
            thread.join(timeout=1)
            require(hold.accepted.is_set(), "original start was never accepted")
            require(elapsed <= 6.5, f"cleanup exceeded shared five-second budget: {elapsed}")
            time.sleep(max(0, 1.25 - elapsed))
            requests = self.requests(name, "POST", "/executions")
            keys = [r["body"]["idempotency_key"] for r in requests]
            require(len(requests) >= 2 and len(set(keys)) == 1, f"recovery key changed: {keys}")
            executions = self.direct.list_executions(sb.id)
            require(len(executions) == 1, "recovery ran twice")
            if fail:
                require(exc.idempotency_key == keys[0], "unconfirmed cancellation lost recovery key")
                self.direct.cancel_execution(sb.id, executions[0].id)
            else:
                require(exc.execution.state == "completed", "cancel falsely confirmed a running snapshot")
                require(exc.execution.termination_reason == "canceled", "recovery did not cancel remote work")
            require(self.direct.read_file(sb.id, "/tmp/recovered") == b"marker\n", "start ran more than once")
            return {"code": exc.code, "execution": executions[0].id, "keys": keys, "elapsed_seconds": elapsed}

    def unconfirmed_cancel(self, name):
        with self.sandbox() as sb:
            execution = self.direct.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo ready; sleep 30"]))
            token = CancellationToken()
            self.proxy.arm(name, Fault("POST", r".*/cancel", "running_snapshot", remaining=30),
                           Fault("GET", rf".*/executions/{execution.id}", "running_snapshot", remaining=100))
            started = time.monotonic()
            exc = error_from(lambda: self.client.stream_execution(sb.id, execution.id, on_stdout=lambda _: token.cancel(), cancel=token), "cancel_unconfirmed")
            require(exc.execution.id == execution.id and exc.execution.sandbox_id == sb.id, "unconfirmed cancellation lost IDs")
            require(time.monotonic() - started <= 6.5, "confirmation exceeded shared deadline")
            return {"execution": execution.id, "code": exc.code, "reported_state": exc.execution.state}

    def shared_cleanup_budget(self, name):
        with self.sandbox() as sb:
            token = CancellationToken()
            first = Fault("POST", r".*/executions", "hold", delay=.8)
            recovery = Fault("POST", r".*/executions", "hold", delay=3.5)
            cleanup = Fault("POST", r".*/cancel", "hold", delay=3.0)
            self.proxy.arm(name, first, recovery, cleanup)
            def interrupt():
                if first.accepted.wait(10):
                    token.cancel()
            thread = threading.Thread(target=interrupt, daemon=True)
            thread.start()
            started = time.monotonic()
            exc = error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo budget; sleep 30"]), cancel=token), "cancel_unconfirmed")
            elapsed = time.monotonic() - started
            require(4.7 <= elapsed <= 5.8, f"recovery and cancel did not share one 5s budget: {elapsed}")
            require(recovery.accepted.is_set() and cleanup.accepted.is_set(), "budget scenario did not reach recovery and cancel")
            require(exc.idempotency_key and exc.execution and exc.execution.id, "budget failure lost recovery key or execution IDs")
            executions = self.direct.list_executions(sb.id)
            require(len(executions) == 1, "budget recovery started duplicate work")
            # The server finished canceling even though its held response missed
            # the client deadline. Only direct follow-up can establish that fact.
            final = self.direct.get_execution(sb.id, executions[0].id)
            require(final.state == "completed", "server-side cancel was not observed")
            time.sleep(max(0, 7.0 - elapsed))
            return {"code": exc.code, "execution": final.id, "elapsed_seconds": elapsed, "idempotency_key": exc.idempotency_key}

    def callback_failure(self, name):
        evidence = []
        for retryable in (False, True):
            with self.sandbox() as sb:
                if retryable:
                    self.proxy.arm(name, Fault("GET", "/v1/sandboxes", "status", remaining=20))
                    cause = error_from(self.client.list_sandboxes)
                    require(cause.retryable, "setup error is not retryable")
                else:
                    cause = ValueError("E2E renderer failed")
                label = name + ("-sdk-error" if retryable else "-ordinary")
                self.proxy.arm(label)
                execution = self.direct.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo ready; sleep 30"]))
                def broken(_):
                    raise cause
                exc = error_from(lambda: self.client.stream_execution(sb.id, execution.id, on_stdout=broken))
                require(self.direct.get_execution(sb.id, execution.id).state == "running", "callback failure canceled execution")
                requests = self.requests(label)
                require(not any(r["path"].endswith("/cancel") for r in requests), "callback error sent a cancel")
                self.direct.cancel_execution(sb.id, execution.id)
                requests = self.requests(label, "GET", "/events")
                require(len(requests) == 1, "retryable callback error reconnected")
                require(exc.cause is cause, "callback cause identity lost")
                evidence.append({"retryable_callback": retryable, "code": exc.code, "stream_requests": len(requests)})
        return evidence

    def reconnect_budget(self, name):
        with self.sandbox() as sb:
            execution = self.direct.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "i=0; while [ $i -lt 100 ]; do echo $i; i=$((i+1)); sleep 0.1; done; sleep 30"]))
            self.proxy.arm(name, Fault("GET", r".*/events", "cut", remaining=20))
            events = []
            error_from(lambda: self.client.stream_execution(sb.id, execution.id, on_event=lambda e: events.append(e.id)))
            requests = self.requests(name, "GET", "/events")
            require(len(requests) == 3, f"progress reset reconnect budget: {len(requests)}")
            require(len(events) == 3 and events == sorted(set(events)), f"progress was lost or duplicated: {events}")
            require(self.direct.get_execution(sb.id, execution.id).state == "running", "reconnect exhaustion canceled execution")
            self.direct.cancel_execution(sb.id, execution.id)
            return {"attempts": len(requests), "event_ids": events, "cursors": [r["last_event_id"] for r in requests]}

    def additive_sse(self, name):
        with self.sandbox() as sb:
            self.proxy.arm(name, Fault("GET", r".*/events", "additive"))
            value = self.client.exec(sb.id, ["sh", "-c", "printf '世界'; printf warning >&2"])
            require(value.stdout == "世界" and value.stderr == "warning", "multiline or additive SSE damaged output")
            return {"stdout": value.stdout, "stderr": value.stderr}

    def malformed_sse(self, name):
        with self.sandbox() as sb:
            execution = self.direct.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo ready; sleep 30"]))
            self.proxy.arm(name, Fault("GET", r".*/events", "malformed"))
            error_from(lambda: self.client.stream_execution(sb.id, execution.id))
            require(self.direct.get_execution(sb.id, execution.id).state == "running", "malformed SSE canceled work")
            self.direct.cancel_execution(sb.id, execution.id)
            require(len(self.requests(name, "GET", "/events")) == 1, "malformed SSE retried")
            return {"execution": execution.id, "remote_continued": True}

    def outcomes(self, name):
        with self.sandbox() as sb:
            exc = error_from(lambda: self.client.exec(sb.id, ["sh", "-c", "printf kept; printf error >&2; exit 7"]), "nonzero_exit")
            require((exc.execution.stdout, exc.execution.stderr, exc.execution.exit_code) == ("kept", "error", 7), "error lost output")
            code137 = error_from(lambda: self.client.exec(sb.id, ["sh", "-c", "exit 137"]), "nonzero_exit")
            require(code137.execution.termination_reason == "exited", "exit 137 falsely classified OOM")
            execution = self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo deadline; sleep 30"], timeout_seconds=1))
            result = self.client.stream_execution(sb.id, execution.id)
            require(result.termination_reason == "timeout" and result.stdout == "deadline\n", "execution timeout was not server-owned")
            require(self.client.get_sandbox(sb.id).state == "running", "execution timeout killed healthy sandbox")
            direct_nonzero = self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "exit 9"]))
            require(self.client.stream_execution(sb.id, direct_nonzero.id).exit_code == 9, "stream converted outcome into exception")
            return {"nonzero": snapshot(exc.execution), "exit137": snapshot(code137.execution), "timeout": snapshot(result)}

    def local_deadline(self, name):
        with self.sandbox() as sb:
            exc = error_from(lambda: self.client.exec(sb.id, ["sh", "-c", "echo local; sleep 30"], cancel=CancellationToken(timeout=.5)), "canceled")
            require(exc.execution.state == "completed" and exc.execution.termination_reason == "canceled", "local deadline invented timeout reason")
            return {"execution": snapshot(exc.execution)}

    def truncation(self, name):
        with self.sandbox() as sb:
            events, chunks = [], []
            result = self.client.exec_stream(sb.id, ["sh", "-c", "head -c 1100000 /dev/zero | tr '\\000' x"], on_event=lambda e: events.append(e.type), on_stdout=chunks.append)
            require(result.output_truncated, "retention overflow did not set flag")
            require(events.count("truncated") == 1, "truncated event missing or repeated")
            require("".join(chunks) == result.stdout and len(result.stdout.encode()) <= 1048576, "retained and streamed output differ")
            return {"retained_bytes": len(result.stdout.encode()), "truncated_events": events.count("truncated"), "execution": result.id}

    def context_cleanup(self, name):
        sandbox_id = None
        try:
            with self.client.sandbox(CreateSandboxRequest(timeout_seconds=0, network="none")) as sb:
                sandbox_id = sb.id
                raise ValueError("body failed")
        except ValueError:
            pass
        require(self.direct.get_sandbox(sandbox_id).state == "killed", "exception context leaked sandbox")
        with Client(domain=self.proxy.domain, api_key=self.key) as client:
            sb = client.create_sandbox(CreateSandboxRequest(timeout_seconds=0, network="none"))
        try:
            require(self.direct.get_sandbox(sb.id).state == "running", "client context killed resource")
        finally:
            self.direct.kill_sandbox(sb.id)
        return {"sandbox_context": sandbox_id, "client_context": sb.id}

    def compatibility_errors(self, name):
        with self.sandbox() as sb:
            self.proxy.arm(name, Fault("GET", rf"/v1/sandboxes/{sb.id}", "old_response"))
            require(self.client.get_sandbox(sb.id).id == sb.id, "missing additive fields broke decode")
            self.proxy.arm(name, Fault("GET", rf"/v1/sandboxes/{sb.id}", "future_state"))
            future = self.client.get_sandbox(sb.id)
            require(future.state == "future_state" and future.network == "future_network", "future strings were rejected or rewritten")
            require(future.extra["future_field"] == 42, "future fields were discarded")
            missing = error_from(lambda: self.client.get_sandbox("sbx_absent"), "not_found")
            require(missing.status_code == 404, "not found HTTP status lost")
            with Client(domain=self.proxy.domain, api_key="wrong") as client:
                unauthorized = error_from(client.list_sandboxes, "unauthorized")
                require(unauthorized.status_code == 401, "unauthorized status lost")
            self.proxy.arm(name, Fault("GET", rf"/v1/sandboxes/{sb.id}", "plain", remaining=20))
            plain = error_from(lambda: self.client.get_sandbox(sb.id))
            require(plain.status_code == 502, "non-JSON HTTP error lost status")
            return {"missing": missing.code, "unauthorized": unauthorized.code, "plain_status": plain.status_code}

    def malformed_typed_fields(self, name):
        with self.sandbox() as sb:
            self.proxy.arm(name, Fault("POST", r".*/executions", "malformed_start"))
            exc = error_from(lambda: self.client.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo valid; sleep 30"])), "decode")
            requests = self.requests(name, "POST", "/executions")
            require(len(requests) == 1, "malformed typed start automatically retried")
            key = requests[0]["body"]["idempotency_key"]
            require(exc.idempotency_key == key, "malformed accepted start lost idempotency key")
            executions = self.direct.list_executions(sb.id)
            require(len(executions) == 1 and executions[0].state == "running", "malformed response affected running work")
            self.direct.cancel_execution(sb.id, executions[0].id)
            self.proxy.arm(name, Fault("GET", r".*/events", "false_exit_code"))
            malformed = error_from(lambda: self.client.exec(sb.id, ["true"]))
            require(malformed.code in {"stream", "decode"}, f"boolean exit_code treated as another outcome: {malformed.code}")
            return {"start_code": exc.code, "idempotency_key": key, "complete_code": malformed.code}

    def oom(self, name):
        with self.sandbox(image="python:3.12-alpine", memory_mb=32) as sb:
            command = ["python3", "-c", "print('before-oom',flush=True); a=[]\nwhile True: a.append(bytearray(1024*1024))"]
            exc = error_from(lambda: self.client.exec(sb.id, command), "oom")
            require("before-oom" in exc.execution.stdout, "OOM lost retained output")
            return {"execution": snapshot(exc.execution)}

    def eviction(self, name):
        with self.sandbox() as sb:
            execution = self.direct.start_execution(sb.id, ExecuteRequest(command=["sh", "-c", "echo before-eviction; sleep 30"]))
            time.sleep(.2)
            subprocess.run(["docker", "kill", f"tiny-sandbox-{sb.id}"], check=True, capture_output=True)
            result = self.client.stream_execution(sb.id, execution.id)
            require(result.termination_reason == "evicted", f"external kill reason {result.termination_reason}")
            require("before-eviction" in result.stdout, "eviction lost retained output")
            return {"execution": snapshot(result)}

    def cleanup(self):
        errors = []
        if self.proxy:
            self.proxy.arm("cleanup")
        if self.direct:
            try:
                for sb in self.direct.list_sandboxes(cancel=CancellationToken(timeout=5)):
                    if sb.state != "killed":
                        try:
                            self.direct.kill_sandbox(sb.id, cancel=CancellationToken(timeout=5))
                        except Exception as exc:
                            errors.append(f"kill {sb.id}: {exc}")
            except Exception as exc:
                errors.append(f"list sandboxes: {exc}")
        if self.client:
            self.client.close()
        if self.direct:
            self.direct.close()
        if self.proxy:
            self.proxy.close()
        if self.process:
            self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        if self.log:
            self.log.close()
        if self.owner:
            before = subprocess.check_output(["docker", "ps", "-aq", "--filter", f"label=tiny-sandbox.owner={self.owner}"], text=True).split()
            if before:
                subprocess.run(["docker", "rm", "-f", *before], check=True, capture_output=True)
            after = subprocess.check_output(["docker", "ps", "-aq", "--filter", f"label=tiny-sandbox.owner={self.owner}"], text=True).split()
            self.report["cleanup"] = {"owner": self.owner, "leaked_before_fallback": before, "remaining_after_cleanup": after, "errors": errors}
            if before or after or errors:
                self.results.append({"name": "cleanup-no-leaks", "status": "failed", "evidence": self.report["cleanup"]})

    def run(self):
        try:
            self.start()
            scenarios = [("installed-distribution", self.distribution), ("entry-replay-confirmed-cancel", self.entry), ("lifecycle-files-resources", self.lifecycle),
                         ("retry-status-matrix", self.retry_statuses), ("retry-after", self.retry_after),
                         ("retry-configuration-and-cancel", self.retry_configuration), ("redirect-credential-boundary", self.credential_redirect),
                         ("tls-predispatch-cancellation", self.tls_predispatch_cancel),
                         ("create-never-retries", self.creation_once), ("truncated-read-and-start", self.truncated_responses),
                         ("idempotency-busy", self.idempotency), ("precanceled-no-request", self.precanceled),
                         ("start-recovery-confirmed", self.start_recovery),
                         ("start-recovery-unconfirmed", lambda n: self.start_recovery(n, fail=True)),
                         ("shared-five-second-cleanup-budget", self.shared_cleanup_budget),
                         ("running-snapshot-unconfirmed", self.unconfirmed_cancel), ("callback-does-not-cancel", self.callback_failure),
                         ("reconnect-budget-keeps-count", self.reconnect_budget), ("additive-multiline-sse", self.additive_sse),
                         ("malformed-sse-does-not-cancel", self.malformed_sse), ("execution-outcomes", self.outcomes),
                         ("local-deadline-cancels", self.local_deadline), ("retention-truncation", self.truncation),
                         ("context-cleanup", self.context_cleanup), ("compatibility-and-errors", self.compatibility_errors),
                         ("malformed-typed-fields", self.malformed_typed_fields),
                         ("real-oom", self.oom), ("real-eviction", self.eviction)]
            for name, action in scenarios:
                self.check(name, action)
        except Exception as exc:
            self.results.append({"name": "runner", "status": "failed", "error": str(exc), "traceback": traceback.format_exc()})
            print(traceback.format_exc(), flush=True)
        finally:
            try:
                self.cleanup()
            except Exception as exc:
                self.results.append({"name": "cleanup", "status": "failed", "error": str(exc), "traceback": traceback.format_exc()})
            self.report["status"] = "passed" if self.results and all(r["status"] == "passed" for r in self.results) else "failed"
            self.report["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            self.persist()
        print(f"{self.report['status'].upper()}: {len(self.results)} scenarios; {self.artifacts / 'report.json'}", flush=True)
        return 0 if self.report["status"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    return Suite(args.server.resolve(), args.artifacts.resolve()).run()


if __name__ == "__main__":
    raise SystemExit(main())
