"""The console in a browser (make test-browser): one gate in the local development profile and what it serves.

The session fixture `stack` builds kontra-gate, kontra, the example agents a2ui and ap2-merchant and test/browser's
echoagent; starts a Temporal development server from $KONTRA_TEMPORAL_CLI, a fresh database on $KONTRA_TEST_PG, the gate
(profile: local, traffic kept forever, a configured service `notes` whose tasks are kept forever), the agents, and one
`kontra connect` with no credential serving booking-form (a2ui), merchant (ap2-merchant), notes (echoagent) and the MCP
service files (@modelcontextprotocol/server-filesystem 1.0.0 over stdio, installed from examples/mcp/fs-stdio's lock).
Everything runs on loopback ports of its own and is stopped when the session ends.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import psycopg
import pytest

ROOT = Path(__file__).resolve().parents[2]
A2UI_EXT = "https://a2ui.org/a2a-extension/a2ui/v0.9.1"
CATALOG = "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_port(port: int, within: float = 60) -> None:
    deadline = time.time() + within
    while time.time() < deadline:
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.2)
    raise RuntimeError(f"nothing listens on 127.0.0.1:{port} after {within}s")


def eventually(what: str, ok, within: float = 30, every: float = 0.25):
    deadline = time.time() + within
    last = None
    while time.time() < deadline:
        try:
            last = ok()
            if last:
                return last
        except Exception as e:  # noqa: BLE001 - retried until the deadline
            last = e
        time.sleep(every)
    raise AssertionError(f"{what}: not within {within}s (last: {last!r})")


@dataclass
class Stack:
    base: str
    dir: Path
    bin: Path
    config: Path
    dsn: str
    procs: dict[str, subprocess.Popen] = field(default_factory=dict)
    gate_port: int = 0

    # ------------------------------------------------------------ processes

    def start(self, name: str, args: list[str], env: dict[str, str] | None = None, first_line: bool = False) -> str:
        log = open(self.dir / f"{name}.log", "ab")
        p = subprocess.Popen(args, stdout=subprocess.PIPE if first_line else log, stderr=log,
                             env={**os.environ, **(env or {})}, cwd=self.dir, start_new_session=True)
        self.procs[name] = p
        if first_line:
            assert p.stdout is not None
            line = p.stdout.readline().decode().strip()
            if not line:
                raise RuntimeError(f"{name} printed nothing: {(self.dir / f'{name}.log').read_text()[-2000:]}")
            return line
        return ""

    def stop(self, name: str) -> None:
        p = self.procs.pop(name, None)
        if p is None or p.poll() is not None:
            return
        try:
            os.killpg(p.pid, 15)
            p.wait(15)
        except Exception:  # noqa: BLE001
            os.killpg(p.pid, 9)
            p.wait(5)

    def start_gate(self) -> None:
        self.start("gate", [str(self.bin / "kontra-gate"), "serve", "-f", str(self.config)])
        eventually("the gate answers /healthz", lambda: self.status("GET", "/healthz") == 200, within=90)

    def restart_gate(self) -> None:
        self.stop("gate")
        self.start_gate()

    def gate_log(self) -> str:
        return (self.dir / "gate.log").read_text(errors="replace")

    # ------------------------------------------------------------ HTTP

    def request(self, method: str, path: str, body: Any = None, headers: dict[str, str] | None = None,
                raw: bytes | None = None) -> tuple[int, dict[str, str], bytes]:
        data = raw if raw is not None else (None if body is None else json.dumps(body).encode())
        h = {"Content-Type": "application/json"} if data is not None else {}
        h.update(headers or {})
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(req, timeout=60) as r:
                return r.status, dict(r.headers), r.read()
        except urllib.error.HTTPError as e:
            return e.code, dict(e.headers), e.read()

    def status(self, method: str, path: str) -> int:
        try:
            return self.request(method, path)[0]
        except OSError:
            return 0

    def api(self, method: str, path: str, body: Any = None, want: int = 200) -> Any:
        code, _, b = self.request(method, path, body)
        assert code == want, f"{method} {path} = {code}: {b[:500]!r}"
        return json.loads(b)

    def a2a(self, service: str, method: str, params: dict, headers: dict[str, str] | None = None) -> tuple[int, Any]:
        """A JSON-RPC call to the gate's A2A route as a stock client: no credential, A2A-Version 1.0 unless told."""
        h = {"A2A-Version": "1.0"}
        h.update(headers or {})
        code, _, b = self.request("POST", f"/agents/{service}",
                                  {"jsonrpc": "2.0", "id": str(uuid.uuid4()), "method": method, "params": params}, h)
        return code, json.loads(b) if b else None

    def send(self, service: str, text: str, context: str | None = None, a2ui: bool = False) -> str:
        msg: dict[str, Any] = {"messageId": str(uuid.uuid4()), "role": "ROLE_USER", "parts": [{"text": text}]}
        if context:
            msg["contextId"] = context
        headers = {}
        if a2ui:
            msg["metadata"] = {"a2uiClientCapabilities": {"v0.9.1": {"supportedCatalogIds": [CATALOG]},
                                                          "v0.9": {"supportedCatalogIds": [CATALOG]}}}
            headers["A2A-Extensions"] = A2UI_EXT
        code, got = self.a2a(service, "SendMessage", {"message": msg, "configuration": {"returnImmediately": True}}, headers)
        assert code == 200 and "result" in got, got
        return got["result"]["task"]["id"]

    def state(self, task: str) -> str:
        return self.api("GET", f"/v1/tasks/{task}")["state"]

    def mcp_call(self, service: str, tool: str, arguments: dict) -> Any:
        """One MCP tool call through /mcp/<service> (Streamable HTTP): initialize, initialized, tools/call."""
        def post(body: dict, sid: str | None) -> tuple[int, dict[str, str], bytes]:
            h = {"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25"}
            if sid:
                h["Mcp-Session-Id"] = sid
            return self.request("POST", f"/mcp/{service}", body, h)

        code, hdr, b = post({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-11-25", "capabilities": {}, "clientInfo": {"name": "browser-test", "version": "1"}}}, None)
        assert code == 200, (code, b[:500])
        sid = next((v for k, v in hdr.items() if k.lower() == "mcp-session-id"), None)
        post({"jsonrpc": "2.0", "method": "notifications/initialized"}, sid)
        code, _, b = post({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": tool, "arguments": arguments}}, sid)
        assert code == 200, (code, b[:500])
        return b

    def sql(self, query: str, *args: Any) -> list[tuple]:
        with psycopg.connect(self.dsn) as c:
            return c.execute(query, args).fetchall()


@pytest.fixture(scope="session")
def stack(tmp_path_factory: pytest.TempPathFactory):
    temporal = os.environ.get("KONTRA_TEMPORAL_CLI")
    server = os.environ.get("KONTRA_TEST_PG")
    if not temporal or not server:
        pytest.fail("KONTRA_TEMPORAL_CLI and KONTRA_TEST_PG must be set (scripts/test_browser.sh)")
    d = Path(os.environ["KONTRA_BROWSER_DIR"]) if os.environ.get("KONTRA_BROWSER_DIR") else tmp_path_factory.mktemp("console")
    d.mkdir(parents=True, exist_ok=True)
    bindir = d / "bin"
    go = os.environ.get("GO", "go")
    subprocess.run([go, "build", "-o", str(bindir) + "/", "./cmd/kontra-gate", "./cmd/kontra", "./examples/sellers/a2ui",
                    "./examples/sellers/ap2-merchant", "./test/browser/echoagent"], cwd=ROOT, check=True)

    db = "kontra_browser_" + uuid.uuid4().hex[:10]
    with psycopg.connect(server, autocommit=True) as c:
        c.execute(f'create database "{db}"')
    dsn = psycopg.conninfo.make_conninfo(server, dbname=db)
    pg_url = server.rsplit("/", 1)[0] + "/" + db + ("?" + server.split("?", 1)[1] if "?" in server else "")

    tport = free_port()
    s = Stack(base="", dir=d, bin=bindir, config=d / "kontra.yaml", dsn=dsn)
    try:
        s.start("temporal", [temporal, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", str(tport),
                             "--http-port", str(free_port()), "--metrics-port", str(free_port()), "--namespace", "kontra",
                             "--log-level", "warn"])
        wait_port(tport)
        subprocess.run([str(bindir / "kontra-gate"), "migrate", "--postgres", pg_url, "up"], check=True, capture_output=True)
        s.gate_port = free_port()
        s.base = f"http://127.0.0.1:{s.gate_port}"
        s.config.write_text(
            "profile: local\n"
            f"listen: 127.0.0.1:{s.gate_port}\n"
            f"metrics: {{ listen: 127.0.0.1:{free_port()} }}\n"
            f"postgres: {json.dumps(pg_url)}\n"
            f"temporal: {{ address: 127.0.0.1:{tport} }}\n"
            "traffic: { retention: 0 }\n"
            "services:\n"
            "  notes:\n"
            "    kind: a2a\n"
            "    backend: { tunnel: {} }\n"
            "    task: { retention: 0 }\n")
        s.start_gate()

        a2ui = s.start("a2ui", [str(bindir / "a2ui"), "--port", "0"], first_line=True)
        merchant = s.start("merchant", [str(bindir / "ap2-merchant"), "--port", "0"], first_line=True)
        echo = s.start("echo", [str(bindir / "echoagent")], first_line=True)
        # live: the same agent declaring streaming, so the gate logs each of its events as it comes.
        live = s.start("echolive", [str(bindir / "echoagent"), "--streaming"], first_line=True)
        serves = ["--serve", f"a2a:booking-form={a2ui}", "--serve", f"a2a:merchant={merchant}", "--serve", f"a2a:notes={echo}",
                  "--serve", f"a2a:live={live}"]
        npm = shutil.which("npm")
        node = shutil.which("node")
        s.mcp = False
        if not (npm and node) and os.environ.get("KONTRA_E2E_OFFLINE") != "1":
            pytest.fail("node and npm are needed for the MCP service (or set KONTRA_E2E_OFFLINE=1 to leave it out)")
        if npm and node and os.environ.get("KONTRA_E2E_OFFLINE") != "1":
            fs = d / "fs-stdio"
            shutil.copytree(ROOT / "examples/mcp/fs-stdio", fs)
            subprocess.run([npm, "ci", "--ignore-scripts", "--no-audit", "--no-fund"], cwd=fs, check=True, capture_output=True)
            files = d / "files"
            files.mkdir()
            (files / "hello.txt").write_text("hello\n")
            server_js = fs / "node_modules/@modelcontextprotocol/server-filesystem/dist/index.js"
            serves += ["--serve", f"mcp:files=stdio:{node} {server_js} {files}"]
            s.mcp = True
        home = d / "home"
        home.mkdir()
        s.start("node", [str(bindir / "kontra"), "connect", "--gate", s.base, *serves, "--listen", "off", "--name", "dev"],
                env={"HOME": str(home), "KONTRA_KEY": str(home / "identity.key")})

        def reachable():
            names = {x["name"]: x["reachable"] for x in s.api("GET", "/v1/services")["items"]}
            want = ["booking-form", "merchant", "notes", "live"] + (["files"] if s.mcp else [])
            # A session serves each, and the gate holds each A2A agent's card (its keeper fetches it after the session).
            return all(names.get(n) for n in want) and all(
                s.api("GET", f"/v1/services/{n}").get("card") for n in ["booking-form", "merchant", "notes", "live"])
        eventually("every service is reachable, with its card", reachable, within=90)
        yield s
    finally:
        for name in list(s.procs)[::-1]:
            s.stop(name)
        try:
            with psycopg.connect(server, autocommit=True) as c:
                c.execute(f'drop database if exists "{db}" with (force)')
        except Exception:  # noqa: BLE001
            pass


@pytest.fixture
def page(stack, browser_page):
    return browser_page


@pytest.fixture(scope="session")
def browser():
    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        b = p.chromium.launch()
        yield b
        b.close()


@pytest.hookimpl(wrapper=True)
def pytest_runtest_makereport(item, call):
    rep = yield
    setattr(item, "rep_" + rep.when, rep)
    return rep


@pytest.fixture
def browser_page(browser, request, stack):
    """A page that records console errors, page errors and CSP violations (window.__csp). On a failed test it prints
    what the browser saw (console, failed requests, 4xx/5xx answers) and keeps a screenshot and the gate's log in the
    session directory (CI uploads it)."""
    ctx = browser.new_context()
    pg = ctx.new_page()
    pg.errors = []
    seen: list[str] = []
    pg.on("console", lambda m: (seen.append(f"console.{m.type}: {m.text}"),
                                pg.errors.append(f"console.{m.type}: {m.text}") if m.type == "error" else None))
    pg.on("pageerror", lambda e: (seen.append(f"pageerror: {e}"), pg.errors.append(f"pageerror: {e}")))
    pg.on("requestfailed", lambda r: seen.append(f"requestfailed: {r.method} {r.url} {r.failure}"))
    pg.on("response", lambda r: seen.append(f"response: {r.status} {r.request.method} {r.url}") if r.status >= 400 else None)
    pg.add_init_script("""window.__csp = []; document.addEventListener('securitypolicyviolation',
        e => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));""")
    yield pg
    rep = getattr(request.node, "rep_call", None)
    if rep is not None and rep.failed:
        name = request.node.name
        try:
            pg.screenshot(path=str(stack.dir / f"{name}.png"), full_page=True)
            print(f"--- {name}: page {pg.url}\n" + pg.locator("body").inner_text()[:3000])
        except Exception as e:  # noqa: BLE001
            print(f"--- {name}: no screenshot: {e}")
        print("--- what the browser saw:\n" + "\n".join(seen[-80:]))
        print("--- gate log tail:\n" + stack.gate_log()[-6000:])
    ctx.close()
