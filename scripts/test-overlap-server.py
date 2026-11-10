#!/usr/bin/env python3
"""Linux integration test for host-built server/gateway overlap upgrades.

Run only on the selected build host. WB2API_TEST_POSTGRES_DSN must point to a
disposable local PostgreSQL database. The DSN is injected into child processes,
never written to config or printed. All accounts, credentials and upstream HTTP
responses are synthetic; every configurable business origin points to localhost.
"""
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse


API_KEY = "overlap-integration-only-api-key"
PASSWORD = "overlap-integration-only-password"
MODEL = "glm-5.3"


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=5)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def control(path, action="status", expected_status=200, **params):
    conn = UnixHTTP(path)
    try:
        query = urllib.parse.urlencode(params)
        conn.request("GET" if action == "status" else "POST",
                     "/" + action + ("?" + query if query else ""))
        response = conn.getresponse()
        body = response.read()
        assert response.status == expected_status, (action, response.status, expected_status)
        return json.loads(body) if action == "status" else None
    finally:
        conn.close()


def request(port, path, method="GET", body=None, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=20)
    try:
        if body is not None:
            body = json.dumps(body).encode()
        # Cookie checks must not accidentally succeed through the global API key.
        values = {} if headers and "Cookie" in headers else {"Authorization": "Bearer " + API_KEY}
        if body is not None:
            values["Content-Type"] = "application/json"
        values.update(headers or {})
        conn.request(method, path, body, values)
        response = conn.getresponse()
        return response.status, {k.lower(): v for k, v in response.getheaders()}, response.read()
    finally:
        conn.close()


def wait_for(predicate, label, timeout=60):
    deadline = time.monotonic() + timeout
    while True:
        if predicate():
            return
        assert time.monotonic() < deadline, label + " deadline"
        time.sleep(0.05)


def ready(control_path, release, child):
    assert child.poll() is None, release + " exited before readiness"
    try:
        status = control(control_path)
        return status["release"] == release and status["ready"]
    except (OSError, http.client.HTTPException):
        return False


def local_test_dsn():
    dsn = os.environ.get("WB2API_TEST_POSTGRES_DSN", "")
    assert dsn, "WB2API_TEST_POSTGRES_DSN must name a disposable local database"
    if "://" in dsn:
        host = urllib.parse.urlsplit(dsn).hostname
    else:
        fields = dict(item.split("=", 1) for item in shlex.split(dsn) if "=" in item)
        host = fields.get("host", "")
    assert host in ("localhost", "127.0.0.1", "::1") or (host and host.startswith("/")), \
        "only loopback/Unix-socket PostgreSQL is allowed"
    return dsn


class FakeUpstream(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), FakeHandler)
        self.first_started = threading.Event()
        self.finish_first = threading.Event()
        self.chat_count = 0
        self.chat_lock = threading.Lock()


class FakeHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.respond()

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.respond()

    def respond(self):
        if urllib.parse.urlsplit(self.path).path == "/v2/chat/completions":
            with self.server.chat_lock:
                self.server.chat_count += 1
                first = self.server.chat_count == 1
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b'data: {"id":"overlap-fake","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":"before-switch"}}]}\n\n')
            self.wfile.flush()
            if first:
                self.server.first_started.set()
                if not self.server.finish_first.wait(45):
                    self.close_connection = True
                    return
            self.wfile.write(b'data: {"id":"overlap-fake","model":"glm-5.3","choices":[{"index":0,"delta":{"content":"after-switch"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}\n\ndata: [DONE]\n\n')
            self.wfile.flush()
            self.close_connection = True
            return
        # Startup observation is allowed to consult this fake endpoint too.
        # No imported account, real token or public provider origin is used.
        payload = {"code": 0, "data": {"Response": {"Data": {
            "TotalCount": 1, "Accounts": [{"PackageName": "integration",
                "CapacityRemain": 100000, "CycleCapacityRemain": 100000}]
        }}}}
        raw = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def run(server, gateway):
    assert sys.platform.startswith("linux"), "run this integration test on Linux"
    dsn = local_test_dsn()
    children, logs = [], []
    upstream = FakeUpstream()
    upstream_thread = threading.Thread(target=upstream.serve_forever, daemon=True)
    upstream_thread.start()
    stream_conn = None
    with tempfile.TemporaryDirectory(prefix="wb-overlap-") as tmp:
        root = Path(tmp)
        auths = root / "auths"
        auths.mkdir()
        # Overlap validates the physical shared directories before opening any
        # state store, so both must exist before A registers its fingerprint.
        (root / "data").mkdir(mode=0o700)
        (auths / "workbuddy-overlap-fake.json").write_text(json.dumps({"uid": "overlap-fake-account",
            "accessToken": "integration-only-access", "refreshToken": "integration-only-refresh",
            "expiresAt": 9999999999, "domain": "www.codebuddy.cn"}))
        fake_origin = "http://127.0.0.1:" + str(upstream.server_port)
        config = root / "config.json"
        config.write_text(json.dumps({
            "api_key": API_KEY, "frontend_password": PASSWORD, "auth_dir": str(auths),
            "state_file": str(root / "data/state.json"), "region": "cn",
            "schedule": {name: False for name in ["checkin_enabled", "travel_enabled",
                "activity_enabled", "keepalive_enabled", "blackcat_enabled", "autoenroll_growth_tasks"]},
            "session_sticky": {"enabled": True, "salt": "22" * 32},
            "cache_routing": {"enabled": False}, "pool": {"max_in_flight": 2},
            "upstream": {"chat_base_cn": fake_origin, "billing_base_cn": fake_origin,
                "chat_base_global": fake_origin, "billing_base_global": fake_origin,
                "web_base_cn": fake_origin, "web_auth_mode": "bearer", "tls_fingerprint": False,
                "shared_conversation_trace": False, "refresh_identity_check": "off",
                "stream_idle_seconds": 45, "timeout_seconds": 5},
        }))
        lock, coordination = root / "writer.lock", root / "coordination"
        base_env = {k: v for k, v in os.environ.items()
                    if not k.startswith("WB2A_") and k.lower() not in ("http_proxy", "https_proxy", "all_proxy")}
        base_env.update(WB2A_POSTGRES_DSN=dsn, NO_PROXY="*", no_proxy="*")

        def diagnostic_text(value):
            text = str(value).replace(dsn, "<test-postgres-dsn>")
            # The database is disposable, but its connection credential still
            # does not belong in the integration output or a traceback.
            if "://" in dsn:
                password = urllib.parse.urlsplit(dsn).password
            else:
                fields = dict(item.split("=", 1) for item in shlex.split(dsn) if "=" in item)
                password = fields.get("password")
            if password:
                text = text.replace(password, "<test-postgres-password>")
            return text

        def launch(binary, args, name, env=None):
            logfile = (root / (name + ".log")).open("wb")
            logs.append(logfile)
            child = subprocess.Popen([binary, *args], cwd=root, env=env or base_env,
                                     stdout=logfile, stderr=logfile)
            children.append(child)
            return child

        def start(release, port, overlap=True):
            path = root / (release + ".sock")
            args = ["-config", str(config), "-release", release,
                    "-control", str(path), "-state-lock", str(lock)]
            if overlap:
                args += ["-upgrade-mode", "overlap", "-coordination-dir", str(coordination)]
            child = launch(server, args, release, dict(base_env, WB2A_LISTEN=f"127.0.0.1:{port}"))
            return child, path

        try:
            a_port, b_port, gateway_port = free_port(), free_port(), free_port()
            a, a_control = start("a", a_port)
            wait_for(lambda: ready(a_control, "a", a), "A readiness")
            assert control(a_control)["background_owner"], "first process did not own background"
            legacy, _ = start("legacy", free_port(), overlap=False)
            assert legacy.wait(timeout=10) != 0, "legacy exclusive writer entered an overlap domain"
            gateway_control = root / "gateway.sock"
            launch(gateway, ["-listen", f"127.0.0.1:{gateway_port}", "-target", f"http://127.0.0.1:{a_port}",
                "-release", "a", "-control", str(gateway_control), "-state", str(root / "gateway.state"),
                "-overlap", "-wait", "20s"], "gateway")
            wait_for(lambda: gateway_control.exists(), "gateway control")
            code, headers, _ = request(gateway_port, "/admin/unlock", "POST", {"password": PASSWORD})
            assert code == 200 and "set-cookie" in headers, "A unlock failed"
            cookie = headers["set-cookie"].split(";", 1)[0]
            b, b_control = start("b", b_port)
            wait_for(lambda: ready(b_control, "b", b), "B readiness")
            standby = control(b_control)
            assert standby["mode"] == "overlap" and not standby["background_owner"], "B started background early"
            assert standby["coordination_domain"] == control(a_control)["coordination_domain"], "coordination domains differ"
            assert request(b_port, "/admin/config", headers={"Cookie": cookie})[0] == 503, "candidate accepted management"
            assert request(b_port, "/admin/unlock", "POST", {"password": PASSWORD})[0] == 503, "candidate accepted unlock"
            assert request(a_port, "/admin/config", "POST", {}, headers={"Cookie": cookie})[0] == 503, "overlap accepted an owner management mutation"
            control(b_control, "activate-background", expected_status=409, release="b")

            stream_conn = http.client.HTTPConnection("127.0.0.1", gateway_port, timeout=30)
            stream_conn.request("POST", "/v1/chat/completions", json.dumps({"model": MODEL,
                "messages": [{"role": "user", "content": "isolated overlap test"}], "stream": True}),
                {"Authorization": "Bearer " + API_KEY, "Content-Type": "application/json"})
            stream = stream_conn.getresponse()
            stream_release = stream.getheader("X-WorkBuddy-Release")
            if stream.status != 200 or stream_release != "a":
                detail = {"status": stream.status, "release": stream_release,
                          "content_type": stream.getheader("Content-Type")}
                if stream.status != 200:
                    detail["error_body"] = diagnostic_text(stream.read(16384).decode("utf-8", "replace"))
                raise AssertionError("old SSE did not enter A: " + json.dumps(detail))
            first_frame = b""
            while b"before-switch" not in first_frame:
                line = stream.readline()
                assert line, "old SSE ended before its first frame"
                first_frame += line
            assert upstream.first_started.is_set(), "fake upstream was not reached"
            control(gateway_control, "switch-live", expected_status=204, expected="a", next="b", target=f"http://127.0.0.1:{b_port}")
            switched = control(gateway_control)
            assert switched["release"] == "b" and switched["management_release"] == "a", "relay/management switch was not split"
            assert switched["releases"]["a"]["relay_active"] >= 1, "old SSE lost its release binding"
            code, headers, _ = request(gateway_port, "/v1/models")
            assert code == 200 and headers.get("x-workbuddy-release") == "b", "new relay did not enter B"
            code, headers, _ = request(gateway_port, "/admin/config", headers={"Cookie": cookie})
            assert code == 200 and headers.get("x-workbuddy-release") == "a", "management left A before handoff"
            code, headers, body = request(gateway_port, "/v1/chat/completions", "POST", {
                "model": MODEL, "messages": [{"role": "user", "content": "new request"}], "stream": True})
            assert code == 200 and headers.get("x-workbuddy-release") == "b", "new SSE did not enter B"
            assert b"after-switch" in body and b"[DONE]" in body, "new B stream did not complete"
            assert not upstream.finish_first.is_set() and control(a_control)["active"] >= 1, "A old stream was cut short"
            upstream.finish_first.set()
            old_tail = stream.read()
            assert b"after-switch" in old_tail and b"[DONE]" in old_tail, "A old stream was interrupted by switch"
            stream_conn.close()
            stream_conn = None
            wait_for(lambda: control(gateway_control)["releases"]["a"]["relay_active"] == 0, "A ingress drain")
            wait_for(lambda: control(a_control)["active"] == 0, "A business drain")
            control(gateway_control, "hold-management", expected_status=204, expected="a")
            wait_for(lambda: control(gateway_control)["releases"]["a"]["management_active"] == 0, "A management drain")
            control(a_control, "pause", release="a")
            control(a_control, "quiesce", release="a")
            wait_for(lambda: control(a_control)["background"] == 0, "A background drain")
            control(a_control, "stop", release="a")
            assert a.wait(timeout=20) == 0, "A did not exit cleanly"
            control(b_control, "activate-management", release="b")
            control(b_control, "activate-background", release="b")
            assert control(b_control)["background_owner"], "B did not acquire background ownership"
            control(gateway_control, "switch-management", expected_status=204, expected="a", next="b")
            control(gateway_control, "resume-management", expected_status=204, expected="b")
            control(gateway_control, "retire", expected_status=204, expected="b", release="a")
            control(gateway_control, "complete", expected_status=204, expected="b")
            code, headers, _ = request(gateway_port, "/admin/config", headers={"Cookie": cookie})
            assert code == 200 and headers.get("x-workbuddy-release") == "b", "shared management cookie was lost"
            assert request(gateway_port, "/admin/config", headers={"Cookie": "wb2api_frontend=not-a-session"})[0] == 401, "invalid cookie accepted without API key"
            assert set(control(gateway_control)["releases"]) == {"b"}, "A was not retired"
            control(b_control, "pause", release="b")
            control(b_control, "quiesce", release="b")
            wait_for(lambda: control(b_control)["background"] == 0, "B background drain")
            control(b_control, "stop", release="b")
            assert b.wait(timeout=20) == 0, "B did not exit cleanly"
            print("PASS: overlap server owner/standby, legacy lock exclusion, live relay switch, "
                  "old real SSE completion, split management, ownership promotion and shared cookie")
        except BaseException:
            print("FAILURE DIAGNOSTICS: isolated synthetic test only", file=sys.stderr)
            print("fake_upstream_chat_count=" + str(upstream.chat_count), file=sys.stderr)
            for logfile in logs:
                logfile.flush()
                path = Path(logfile.name)
                tail = path.read_bytes()[-6000:].decode("utf-8", "replace")
                print("--- " + path.name + " tail ---", file=sys.stderr)
                print(diagnostic_text(tail), file=sys.stderr)
            raise
        finally:
            upstream.finish_first.set()
            if stream_conn is not None:
                stream_conn.close()
            for child in reversed(children):
                if child.poll() is None:
                    child.terminate()
                    try:
                        child.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        child.kill()
                        child.wait()
            for logfile in logs:
                logfile.close()
            upstream.shutdown()
            upstream.server_close()
            upstream_thread.join(timeout=5)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: test-overlap-server.py /absolute/wb2api /absolute/handoff-gateway")
    run(str(Path(sys.argv[1]).resolve()), str(Path(sys.argv[2]).resolve()))
