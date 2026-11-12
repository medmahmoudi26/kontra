"""The console in a browser, against a gate in the local development profile (conftest.py's `stack`).

tasks.md 5.4 (it loads with no sign-in), 5.5 (a thread drawn live from its events: the console's A2UI task submits,
another client's task is watch-only and inert), 5.6 (an AP2 checkout card), 5.7 (the list views over seeded data, the
retention horizon), 5.8 (agent Markdown carrying HTML is text and runs nothing) and 6.3 (one end-to-end scenario,
across a gate restart).
"""

from __future__ import annotations

import json
import re
import uuid

from playwright.sync_api import Page, expect

from conftest import Stack, eventually


def book(page: Page, name: str, guests: str, when: str) -> None:
    """Fill the A2UI booking form and press Book once what was filled in has held (a surface the thread draws again,
    on a reconnect, starts empty)."""
    for _ in range(5):
        page.get_by_label("Name for the booking").fill(name)
        page.get_by_label("Guests").select_option(label=guests)
        page.get_by_label("Date and time").fill(when)
        page.wait_for_timeout(300)
        if page.get_by_label("Name for the booking").input_value() == name and page.get_by_label("Date and time").input_value() == when:
            break
    page.get_by_role("button", name="Book").click()


def no_trouble(page: Page) -> None:
    assert page.errors == [], page.errors
    assert page.evaluate("window.__csp") == [], page.evaluate("window.__csp")


def test_loads_without_sign_in(stack: Stack, page: Page) -> None:
    page.goto(stack.base + "/")
    expect(page.locator(".brand, header").first).to_be_visible()
    for name in ["Contexts", "Tasks", "Traffic", "MCP calls", "Cards", "Playground"]:
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
    book(page, "Ada Lovelace", "2 guests", "2026-12-01T19:30")
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
    # The traffic record is written in batches, off the call: wait for the refused call's row before the page reads it.
    eventually("the refused call is recorded", lambda: any(
        "version_unsupported" in json.dumps(x) for x in stack.api("GET", "/v1/exchanges?failed=1&limit=50")["items"]))

    page.goto(stack.base + "/#/tasks")
    expect(page.locator(f'#taskrows tr[data-task="{first}"]')).to_be_visible(timeout=15000)

    for address in ["/#/", "/#/conversations"]:  # Contexts is the landing page; the old address opens it too
        page.goto(stack.base + address)
        expect(page.locator(f'[data-context="{ctx}"]')).to_be_visible(timeout=15000)
        expect(page.locator(f'[data-context="{ctx}"]')).to_contain_text("2")
    page.locator(f'[data-context="{ctx}"] a').first.click()
    expect(page.locator("#ctxtasks tbody tr")).to_have_count(2)

    page.goto(stack.base + "/#/traffic")
    expect(page.locator("#trafficretention")).to_have_text("Exchanges are kept forever, with their bodies.", timeout=15000)
    expect(page.locator("#trafficcard tbody tr").first).to_be_visible()
    page.locator("#failedonly").click()
    expect(page.locator("#trafficcard")).to_contain_text("version_unsupported", timeout=15000)
    assert page.locator('#trafficcard tbody tr[data-failed=""]').count() == 0
    page.locator("#failedonly").click()
    page.locator(f'#trafficcard a[href="#/t/{first}"]').first.click()
    expect(page).to_have_url(re.compile(f"#/t/{first}"))

    page.goto(stack.base + "/#/services")  # the old address opens the Cards page
    row = page.locator('article[data-service="notes"]')
    expect(row).to_contain_text("reachable", timeout=15000)
    expect(row).to_contain_text("kept forever")
    expect(page.locator('article[data-service="merchant"]')).to_contain_text("7d")

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
    book(page, "Grace Hopper", "3 guests", "2026-12-02T20:00")
    eventually("the booking completes", lambda: stack.state(booking) == "completed")

    def check() -> None:
        tasks = {t["task_id"]: t for t in stack.api("GET", "/v1/tasks?limit=100")["items"]}
        assert {client, console, booking} <= set(tasks)
        assert tasks[console]["via"] == "console" and tasks[client]["via"] == "anonymous"
        convs = {c["context_id"] for c in stack.api("GET", "/v1/contexts?limit=100")["items"]}
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


def test_cards_show_the_agents_cards(stack: Stack, page: Page) -> None:
    """console-agent-cards: each service is a card; an A2A service's shows its agent card (name, version, skills); a
    long description is clamped under a down arrow that opens it and the skills' details and closes them again; what
    the card says is text."""
    page.goto(stack.base + "/#/cards")
    notes = page.locator('article[data-service="notes"]')
    expect(notes.locator(".nm b")).to_have_text("Echo", timeout=15000)
    expect(notes.locator(".nm .v")).to_have_text("v1.0.0")
    expect(notes.locator(".skills .tag")).to_have_text(["Echo"])
    expect(notes).to_contain_text("http://127.0.0.1")
    expect(notes.get_by_role("link", name="Start a task")).to_have_attribute("href", "#/run/notes")
    desc = notes.locator(".desc")
    closed = desc.evaluate("e => e.clientHeight")
    assert desc.evaluate("e => e.scrollHeight > e.clientHeight + 2"), "the long description is not clamped"

    arrow = notes.locator("button.expand")
    expect(arrow).to_be_visible()
    expect(arrow).to_have_attribute("aria-expanded", "false")
    expect(notes.locator(".details")).to_be_hidden()
    arrow.click()
    expect(arrow).to_have_attribute("aria-expanded", "true")
    expect(notes.locator('.details [data-skill="Echo"]')).to_contain_text("Echoes Markdown.")
    expect(notes.locator('.details [data-skill="Echo"]')).to_contain_text("in: text/plain · out: text/markdown")
    expect(notes.locator('.details [data-skill="Echo"] code')).to_have_text(["**hello**"])
    assert desc.evaluate("e => e.clientHeight") > closed, "the description did not open"
    expect(desc).to_contain_text('<img src=x onerror="window.__pwned = 4"> stays text.')
    assert page.evaluate("window.__pwned") is None
    assert page.locator("#cards img").count() == 0
    arrow.click()
    expect(arrow).to_have_attribute("aria-expanded", "false")
    expect(notes.locator(".details")).to_be_hidden()
    assert desc.evaluate("e => e.clientHeight") == closed

    if stack.mcp:
        files = page.locator('article[data-service="files"]')
        expect(files).to_contain_text("mcp server")
        expect(files.get_by_role("link", name="Start a task")).to_have_count(0)
    no_trouble(page)


# ---------------------------------------------------------------------- console-a2a-protocol-view (D355-D361)

def timeline(page: Page) -> list[str]:
    """The thread's A2A timeline, opened: each row's label."""
    box = page.locator("#eventsbox")
    if not box.evaluate("e => e.open"):
        box.locator("summary").click()
    return page.locator("#eventlog .tl-row").evaluate_all("rows => rows.map(r => r.dataset.wire)")


def test_thread_labels_every_item(stack: Stack, page: Page) -> None:
    """7.2: every item of a task's thread is labelled as A2A names it, from the gate's record of its origin: the
    caller's SendMessage, the callee's Task (polled: logged as status and artifact events, converted), the caller's
    GetTask calls between them; the cards carry their event's label; taskId and contextId are shown in full, copyable."""
    page.context.grant_permissions(["clipboard-read", "clipboard-write"])
    task = stack.send("notes", "hello wire")
    eventually("it completes", lambda: stack.state(task) == "completed")
    for _ in range(2):
        stack.a2a("notes", "GetTask", {"id": task})
    eventually("the GetTask calls are recorded", lambda: sum(
        1 for x in stack.api("GET", f"/v1/exchanges?task={task}&method=GetTask&leg=caller")["items"]) >= 2)
    page.goto(stack.base + f"/#/t/{task}")
    expect(page.locator("#thread")).to_contain_text("hello wire", timeout=15000)
    eventually("the timeline has the GetTask calls", lambda: timeline(page).count("anonymous · GetTask") >= 2, within=15)
    labels = timeline(page)
    assert labels[0] == "anonymous · SendMessage", labels
    assert "notes · Task" in labels, labels
    expect(page.locator('#eventlog .tl-row[data-wire="notes · Task"] .conv').first).to_contain_text(
        re.compile(r"anonymous received a Task(Status|Artifact)UpdateEvent \(converted by the gate\)"))
    expect(page.locator('#eventlog .tl-row[data-wire="anonymous · SendMessage"] .conv')).to_contain_text("anonymous received a Task")
    expect(page.locator("#eventlog .vmark.ok").first).to_be_visible()
    assert page.locator("#thread [data-wire]").count() > 0, "no card carries an A2A label"
    row = page.locator('#ctxfoot [data-id="taskId"]')
    expect(row.locator(".mono")).to_have_text(task)
    row.get_by_role("button", name="Copy").click()
    assert page.evaluate("navigator.clipboard.readText()") == task
    ctx = stack.api("GET", f"/v1/tasks/{task}")["context_id"]
    expect(page.locator('#ctxfoot [data-id="contextId"] .mono')).to_have_text(ctx)
    no_trouble(page)


def test_message_answer_passes_through(stack: Stack, page: Page) -> None:
    """D362: an agent that answers with a bare Message. A stock client's blocking SendMessage receives that Message,
    with the gate's contextId and no taskId; the interaction is in no Tasks list, and its context's timeline shows
    anonymous · SendMessage and notes · Message, anonymous having received the Message as it was sent."""
    code, got = stack.a2a("notes", "SendMessage", {"message": {"messageId": str(uuid.uuid4()), "role": "ROLE_USER",
                                                               "parts": [{"text": "message only"}]}})
    assert code == 200 and "message" in got["result"], got
    msg = got["result"]["message"]
    assert "taskId" not in msg and msg["contextId"] and msg["parts"][0]["text"] == "A plain answer.", msg
    ctx = msg["contextId"]
    conv = next(c for c in stack.api("GET", "/v1/conversations?service=notes&limit=100")["items"] if c["context_id"] == ctx)
    assert conv["tasks"] == 0, conv
    assert all(t["context_id"] != ctx for t in stack.api("GET", "/v1/tasks?service=notes&limit=100")["items"])
    page.goto(stack.base + f"/#/c/notes/{conv['caller']}/{ctx}")
    rows = page.locator("#ctxtimeline .tl-row")
    expect(rows.nth(1)).to_be_visible(timeout=15000)
    assert rows.evaluate_all("rs => rs.map(r => r.dataset.wire)") == ["anonymous · SendMessage", "notes · Message"]
    answer = page.locator('#ctxtimeline .tl-row[data-wire="notes · Message"] .conv')
    expect(answer).to_have_text("anonymous received a Message")
    expect(page.locator("#ctxtasks tbody tr")).to_have_count(0)
    expect(page.locator('#ctxtimeline a[href^="#/t/"]')).to_have_count(0)
    no_trouble(page)


def test_console_prompt_message_answer(stack: Stack, page: Page) -> None:
    """D362: the console prompts with message:stream, as the A2A Inspector does, so a Message answer reaches it as it
    reaches any client: the console's interaction shows notes · Message, anonymous (the console's shared caller) having received
    that Message, and no task."""
    started = stack.api("POST", "/v1/tasks", {"service": "notes", "text": "message only"})
    eventually("the answer arrives", lambda: stack.state(started["task_id"]) == "completed")
    view = stack.api("GET", f"/v1/tasks/{started['task_id']}")
    assert view["exposed"] is False and view["actions"] == ["continue"], view
    page.goto(stack.base + f"/#/t/{started['task_id']}")
    expect(page.locator("#thread")).to_contain_text("A plain answer.", timeout=15000)
    eventually("the Message is labelled", lambda: "notes · Message" in timeline(page), within=15)
    row = page.locator('#eventlog .tl-row[data-wire="notes · Message"] .conv')
    expect(row).to_have_text("anonymous received a Message")
    no_trouble(page)


def test_contexts_page(stack: Stack, page: Page) -> None:
    """7.3: Contexts is the landing page; a context's page says how it was opened (the caller's contextId, or the
    gate's), names the callee's own context, and draws one timeline across its tasks, each caller message showing its
    contextId, its taskId or none, and its referenceTaskIds as links."""
    ctx = "ctx-" + uuid.uuid4().hex[:8]
    first = stack.send("notes", "first", context=ctx)
    eventually("it completes", lambda: stack.state(first) == "completed")
    code, got = stack.a2a("notes", "SendMessage", {"message": {"messageId": str(uuid.uuid4()), "role": "ROLE_USER",
        "contextId": ctx, "referenceTaskIds": [first], "parts": [{"text": "refine it"}]}})
    second = got["result"]["task"]["id"]
    eventually("the refinement completes", lambda: stack.state(second) == "completed")
    caller = stack.api("GET", f"/v1/tasks/{first}")["caller_id"]
    page.goto(stack.base + "/")
    expect(page.locator(f'[data-context="{ctx}"]')).to_be_visible(timeout=15000)
    page.goto(stack.base + f"/#/c/notes/{caller}/{ctx}")
    expect(page.locator('#ctxopening [data-opening="caller"]')).to_contain_text("contextId sent by anonymous", timeout=15000)
    expect(page.locator("#ctxopening [data-callee]")).to_have_count(1)
    expect(page.locator("#ctxopening [data-callee]")).to_have_attribute("data-leg", "gate → notes")
    expect(page.locator('#ctxopening [data-opening="caller"] [data-id="contextId"]')).to_have_attribute("data-leg", "anonymous → gate")
    expect(page.locator("#ctxtasks tbody tr")).to_have_count(2)
    rows = page.locator("#ctxtimeline .tl-row")
    expect(rows.first).to_be_visible()
    assert rows.evaluate_all("rs => rs.map(r => r.dataset.wire)").count("anonymous · SendMessage") == 2
    refine = page.locator('#ctxtimeline .tl-row[data-wire="anonymous · SendMessage"]').nth(1)
    expect(refine.locator('[data-id="taskId"]')).to_contain_text("none")
    expect(refine.locator(f'a[href="#/t/{first}"]')).to_have_text(first)
    # A message with no contextId: the gate assigned one.
    alone = stack.send("notes", "alone")
    eventually("it completes", lambda: stack.state(alone) == "completed")
    actx = stack.api("GET", f"/v1/tasks/{alone}")["context_id"]
    page.goto(stack.base + f"/#/c/notes/{caller}/{actx}")
    expect(page.locator('#ctxopening [data-opening="gate"]')).to_contain_text("contextId assigned by the gate", timeout=15000)
    no_trouble(page)


def test_traffic_and_exchange(stack: Stack, page: Page) -> None:
    """7.4: the Traffic view shows both legs, filtered by leg; an exchange's page shows its request and answer with
    their validation; a refused call (extension_required) is an exchange with no task; a stream's frames."""
    task = stack.send("notes", "traffic")
    eventually("it completes", lambda: stack.state(task) == "completed")
    code, got = stack.a2a("merchant", "SendMessage", {"message": {"messageId": str(uuid.uuid4()), "role": "ROLE_USER",
        "parts": [{"text": "no AP2"}]}})
    assert got["error"]["code"] == -32008
    eventually("both legs recorded", lambda: {x["leg"] for x in stack.api("GET", f"/v1/exchanges?task={task}")["items"]} == {"caller", "callee"})
    page.goto(stack.base + "/#/traffic")
    page.locator("#legfilter").select_option("callee")
    expect(page.locator('#trafficcard tbody tr[data-leg="callee"]').first).to_be_visible(timeout=15000)
    assert page.locator('#trafficcard tbody tr[data-leg="caller"]').count() == 0
    page.locator("#legfilter").select_option("")
    page.locator("#failedonly").click()
    expect(page.locator("#trafficcard")).to_contain_text("extension_required", timeout=15000)
    page.locator("#failedonly").click()
    send = next(x for x in stack.api("GET", f"/v1/exchanges?task={task}&leg=caller&method=SendMessage")["items"])
    page.goto(stack.base + f"/#/x/{send['id']}")
    expect(page.locator("#exchangetitle")).to_have_text("anonymous · SendMessage", timeout=15000)
    expect(page.locator("#exchangerequest")).to_contain_text('"text": "traffic"')
    expect(page.locator("#exchangerequest .vmark.ok")).to_be_visible()
    expect(page.locator("#exchangeresponse .vmark.ok")).to_be_visible()
    no_trouble(page)


def test_cards_compared(stack: Stack, page: Page) -> None:
    """7.5: a service's callee card and served card side by side, the members the gate set marked, each validated."""
    page.goto(stack.base + "/#/cards")
    page.locator('article[data-service="notes"] [data-act="compare"]').click()
    expect(page.locator("#calleecard")).to_contain_text('"name": "Echo"', timeout=15000)
    expect(page.locator("#servedcard .gateset")).to_contain_text("$.capabilities.streaming")
    expect(page.locator("#servedcard .gateset")).to_contain_text("$.supportedInterfaces")
    expect(page.locator("#calleecard .vmark")).to_be_visible()
    expect(page.locator("#servedcard .vmark.ok")).to_be_visible()
    no_trouble(page)


def test_playground(stack: Stack, page: Page) -> None:
    """7.6: ListTasks over JSON-RPC from the playground answers the shared caller's tasks (the local profile lifts the
    anonymous refusal, D361) and is recorded as a caller-leg exchange; SubscribeToTask with Last-Event-ID over HTTP+JSON
    shows the frames after that seq as they come."""
    page.goto(stack.base + "/#/play/notes")
    page.locator("#playmethod").select_option("ListTasks")
    page.locator("#playsend").click()
    expect(page.locator("#playstatus")).to_contain_text("HTTP 200", timeout=15000)
    expect(page.locator("#playout pre.json")).to_contain_text('"tasks"')
    expect(page.locator("#playrecorded")).to_be_visible(timeout=15000)
    assert any(x["method"] == "ListTasks" and x["leg"] == "caller" for x in stack.api("GET", "/v1/exchanges?service=notes&method=ListTasks")["items"])

    waiting = stack.send("booking-form", "a table, please", a2ui=True)
    eventually("it waits for input", lambda: stack.state(waiting) == "input-required")
    page.goto(stack.base + "/#/play/booking-form")
    page.locator("#playbinding").select_option("rest")
    page.locator("#playmethod").select_option("SubscribeToTask")
    page.locator("#playbody").fill(json.dumps({"id": waiting}))
    page.locator("#playlastid").fill("1")
    page.locator("#playsend").click()
    expect(page.locator("#playframes .frame").first).to_be_visible(timeout=15000)
    expect(page.locator("#playframes")).to_contain_text("INPUT_REQUIRED")
    no_trouble(page)


def test_bodies_render_as_text(stack: Stack, page: Page) -> None:
    """7.7: a body carrying markup is drawn as text on the exchange page, and none of it runs."""
    task = stack.send("notes", '<script>window.__pwned = 9</script><img src=x onerror="window.__pwned = 10">')
    eventually("it completes", lambda: stack.state(task) == "completed")
    eventually("its exchange is recorded", lambda: stack.api("GET", f"/v1/exchanges?task={task}&leg=caller&method=SendMessage")["items"])
    x = stack.api("GET", f"/v1/exchanges?task={task}&leg=caller&method=SendMessage")["items"][0]
    page.goto(stack.base + f"/#/x/{x['id']}")
    expect(page.locator("#exchangerequest")).to_contain_text("<script>window.__pwned = 9</script>", timeout=15000)
    page.wait_for_timeout(500)
    assert page.evaluate("window.__pwned") is None
    assert page.locator("#exchange script, #exchange img").count() == 0
    no_trouble(page)
