"""The console in a browser, against a gate in the local development profile (conftest.py's `stack`).

tasks.md 5.4 (it loads with no sign-in), 5.5 (a thread drawn live from its events: the console's A2UI task submits,
another client's task is watch-only and inert), 5.6 (an AP2 checkout card), 5.7 (the list views over seeded data, the
retention horizon), 5.8 (agent Markdown carrying HTML is text and runs nothing) and 6.3 (one end-to-end scenario,
across a gate restart).
"""

from __future__ import annotations

import re
import uuid

from playwright.sync_api import Page, expect

from conftest import Stack, eventually


def no_trouble(page: Page) -> None:
    assert page.errors == [], page.errors
    assert page.evaluate("window.__csp") == [], page.evaluate("window.__csp")


def test_loads_without_sign_in(stack: Stack, page: Page) -> None:
    page.goto(stack.base + "/")
    expect(page.locator(".brand, header").first).to_be_visible()
    for name in ["Tasks", "Conversations", "Traffic", "MCP calls", "Services"]:
        expect(page.get_by_role("link", name=name).first).to_be_visible()
    assert not re.search(r"sign in|log in|password", page.locator("body").inner_text(), re.I)
    info = stack.api("GET", "/v1/info")
    assert info["profile"] == "local" and "prompt" in info["features"]
    no_trouble(page)


def test_console_task_a2ui_form_submits(stack: Stack, page: Page) -> None:
    page.goto(stack.base + "/#/run/booking-form")
    page.locator("#startmessage").fill("Book a table for two")
    page.locator("#startsend").click()
    expect(page.get_by_label("Name for the booking")).to_be_visible(timeout=20000)
    task = re.search(r"#/t/([0-9a-f-]{36})", page.url).group(1)
    page.get_by_label("Name for the booking").fill("Ada Lovelace")
    page.get_by_label("Guests").select_option(label="2 guests")
    page.get_by_label("Date and time").fill("2026-12-01T19:30")
    page.get_by_role("button", name="Book").click()
    eventually("the console's task completes", lambda: stack.state(task) == "completed")
    expect(page.locator("#thread")).to_contain_text("Ada Lovelace", timeout=15000)
    view = stack.api("GET", f"/v1/tasks/{task}")
    assert view["via"] == "console" and view["actions"] == ["continue"]
    no_trouble(page)


def test_other_clients_task_is_watch_only(stack: Stack, page: Page) -> None:
    task = stack.send("booking-form", "a table, please", a2ui=True)
    eventually("it waits for input", lambda: stack.state(task) == "input-required")
    assert stack.api("GET", f"/v1/tasks/{task}")["actions"] == []
    page.goto(stack.base + f"/#/t/{task}")
    expect(page.locator("#thread")).to_contain_text("Watch-only", timeout=15000)
    name = page.get_by_label("Name for the booking")
    expect(name).to_be_attached()
    assert name.evaluate("n => Boolean(n.closest('[inert]'))"), "the A2UI surface is not inert"
    # No way to answer or cancel: whatever the thread drew for it is inert or hidden.
    for n in page.get_by_label("Your answer").all() + page.get_by_role("button", name="Cancel the task").all():
        assert n.evaluate("n => Boolean(n.closest('[inert]')) || !n.checkVisibility()"), "an answer control is usable"
    # The console refuses to act on it.
    code, _, _ = stack.request("POST", f"/v1/tasks/{task}/messages", {"text": "hi"})
    assert code == 403
    no_trouble(page)


def test_ap2_checkout_card(stack: Stack, page: Page) -> None:
    started = stack.api("POST", "/v1/tasks", {"service": "merchant", "text": "I want to buy the example item"})
    page.goto(stack.base + f"/#/t/{started['task_id']}")
    card = page.locator('[data-ap2="checkout"]').first
    expect(card).to_be_visible(timeout=20000)
    expect(card).to_contain_text("Example item")
    expect(card).to_contain_text("10.00 USD")
    card.get_by_role("button", name=re.compile("ap2.checkout")).click()
    expect(card.locator("pre.json")).to_contain_text("checkout_jwt")
    no_trouble(page)


def test_agent_markdown_runs_nothing(stack: Stack, page: Page) -> None:
    evil = ("# Report\n\n<script>window.__pwned = 1</script>\n\n<img src=x onerror=\"window.__pwned = 2\">\n\n"
            "[link](javascript:window.__pwned=3) **bold**")
    started = stack.api("POST", "/v1/tasks", {"service": "notes", "text": evil})
    eventually("the echo completes", lambda: stack.state(started["task_id"]) == "completed")
    page.goto(stack.base + f"/#/t/{started['task_id']}")
    expect(page.locator("#thread")).to_contain_text("Report", timeout=15000)
    expect(page.locator("#thread")).to_contain_text("<script>window.__pwned = 1</script>")
    page.wait_for_timeout(500)
    for link in page.locator("#thread a").all():
        assert not (link.get_attribute("href") or "").lower().startswith("javascript:")
    assert page.evaluate("window.__pwned") is None
    assert page.locator("#thread script, #thread img[onerror]").count() == 0
    no_trouble(page)


def test_list_views(stack: Stack, page: Page) -> None:
    ctx = "ctx-" + uuid.uuid4().hex[:8]
    first = stack.send("notes", "one", context=ctx)
    second = stack.send("notes", "two", context=ctx)
    eventually("both complete", lambda: stack.state(first) == stack.state(second) == "completed")
    code, _ = stack.a2a("notes", "SendMessage", {"message": {"messageId": "m", "role": "ROLE_USER", "parts": [{"text": "x"}]}},
                        headers={"A2A-Version": ""})
    assert code == 200  # a JSON-RPC error: -32009

    page.goto(stack.base + "/#/")
    expect(page.locator(f'#taskrows tr[data-task="{first}"]')).to_be_visible(timeout=15000)

    page.goto(stack.base + "/#/conversations")
    expect(page.locator(f'[data-context="{ctx}"]')).to_be_visible(timeout=15000)
    expect(page.locator(f'[data-context="{ctx}"]')).to_contain_text("2")
    page.locator(f'[data-context="{ctx}"] a').click()
    expect(page.locator("#convtasks tbody tr")).to_have_count(2)

    page.goto(stack.base + "/#/traffic")
    expect(page.locator("#trafficretention")).to_have_text("Exchanges are kept forever.", timeout=15000)
    expect(page.locator("#trafficcard tbody tr").first).to_be_visible()
    page.locator("#failedonly").click()
    expect(page.locator("#trafficcard")).to_contain_text("version_unsupported", timeout=15000)
    assert page.locator('#trafficcard tbody tr[data-failed=""]').count() == 0
    page.locator("#failedonly").click()
    page.locator(f'#trafficcard a[href="#/t/{first}"]').first.click()
    expect(page).to_have_url(re.compile(f"#/t/{first}"))

    page.goto(stack.base + "/#/services")
    row = page.locator('tr[data-service="notes"]')
    expect(row).to_contain_text("reachable", timeout=15000)
    expect(row).to_contain_text("kept forever")
    expect(page.locator('tr[data-service="merchant"]')).to_contain_text("7d")

    page.goto(stack.base + "/#/calls")
    expect(page.locator("#calls")).to_be_visible()
    no_trouble(page)


def test_end_to_end_across_a_restart(stack: Stack, page: Page) -> None:
    """6.3: an A2A client's task and a console-started task both show in tasks, conversations and traffic; a refused
    call shows in traffic only; an MCP tool call shows in MCP calls; an A2UI input-required task the console started is
    completed from the console; after a gate restart all of it is still there."""
    client = stack.send("notes", "from a client")
    console = stack.api("POST", "/v1/tasks", {"service": "notes", "text": "from the console"})["task_id"]
    eventually("both complete", lambda: stack.state(client) == stack.state(console) == "completed")
    refused_before = len(stack.api("GET", "/v1/tasks?limit=100")["items"])
    code, got = stack.a2a("notes", "SendMessage", {"message": {"messageId": "r", "role": "ROLE_USER", "parts": [{"text": "x"}]}},
                          headers={"A2A-Version": ""})
    assert got["error"]["code"] == -32009
    assert len(stack.api("GET", "/v1/tasks?limit=100")["items"]) == refused_before, "a refused call made a task"
    if stack.mcp:
        stack.mcp_call("files", "list_allowed_directories", {})

    page.goto(stack.base + "/#/run/booking-form")
    page.locator("#startmessage").fill("book a table for two")
    page.locator("#startsend").click()
    expect(page.get_by_label("Name for the booking")).to_be_visible(timeout=20000)
    booking = re.search(r"#/t/([0-9a-f-]{36})", page.url).group(1)
    page.get_by_label("Name for the booking").fill("Grace Hopper")
    page.get_by_label("Guests").select_option(label="3 guests")
    page.get_by_label("Date and time").fill("2026-12-02T20:00")
    page.get_by_role("button", name="Book").click()
    eventually("the booking completes", lambda: stack.state(booking) == "completed")

    def check() -> None:
        tasks = {t["task_id"]: t for t in stack.api("GET", "/v1/tasks?limit=100")["items"]}
        assert {client, console, booking} <= set(tasks)
        assert tasks[console]["via"] == "console" and tasks[client]["via"] == "anonymous"
        convs = {c["context_id"] for c in stack.api("GET", "/v1/conversations?limit=100")["items"]}
        assert {tasks[client]["context_id"], tasks[console]["context_id"], tasks[booking]["context_id"]} <= convs
        ex = stack.api("GET", "/v1/exchanges?limit=100")["items"]
        by_task = {x["task_id"] for x in ex if x["method"] == "SendMessage"}
        assert {client, console, booking} <= by_task
        assert any(x["rpc_code"] == -32009 and x["task_id"] is None for x in ex), "the refused call is not in traffic"
        if stack.mcp:
            calls = stack.api("GET", "/v1/calls?limit=100")["items"]
            assert any(c["service"] == "files" and c["tool"] == "list_allowed_directories" for c in calls), calls

    eventually("everything shows", lambda: check() or True, within=15)
    stack.restart_gate()
    eventually("everything shows after a restart", lambda: check() or True, within=30)
    page.goto(stack.base + f"/#/t/{booking}")
    expect(page.locator("#thread")).to_contain_text("Grace Hopper", timeout=20000)
    page.goto(stack.base + "/#/calls")
    if stack.mcp:
        expect(page.locator("#callscard")).to_contain_text("list_allowed_directories", timeout=15000)
    no_trouble(page)


def test_another_site_cannot_prompt_agents(stack: Stack, page: Page) -> None:
    """A page of another site in the developer's browser cannot start a task on a local agent: its simple request (a
    text/plain POST with A2A-Version in the query, no preflight) is refused 403 by the profile's edge, and shows in
    traffic as cross_site."""
    import http.server
    import threading

    class Blank(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"<!doctype html><title>another site</title>")

        def log_message(self, *a):  # noqa: ANN002
            pass

    other = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Blank)
    threading.Thread(target=other.serve_forever, daemon=True).start()
    before = {t["task_id"] for t in stack.api("GET", "/v1/tasks?limit=100")["items"]}
    page.goto(f"http://127.0.0.1:{other.server_address[1]}/")
    body = '{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":{"messageId":"csrf","role":"ROLE_USER","parts":[{"text":"pwn"}]}}}'
    page.evaluate("""([url, body]) => fetch(url, {method: 'POST', mode: 'no-cors', headers: {'Content-Type': 'text/plain'}, body})
        .catch(() => null)""", [stack.base + "/agents/notes?A2A-Version=1.0", body])
    page.wait_for_timeout(1500)
    after = {t["task_id"] for t in stack.api("GET", "/v1/tasks?limit=100")["items"]}
    assert after == before, "a cross-site page started a task"
    other.shutdown()
    eventually("the refusal is in traffic", lambda: any(
        x["reason"] == "cross_site" and x["http_status"] == 403 and x["service"] == "notes"
        for x in stack.api("GET", "/v1/exchanges?failed=1&limit=100")["items"]), within=10)
