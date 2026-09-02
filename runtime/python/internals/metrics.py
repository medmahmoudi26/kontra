"""Prometheus exposition for the Python actor host — parity with runtime/go's metrics.go.

This exists because the crawler is a PYTHON actor, and the crawler is what lost the work:
~1,400 seeds vanished inside nodes that reported `completed` with healthy-looking blob counts.
Instrumenting only the Go host would have left the actor that actually caused the incident
uninstrumented.

Metric names, labels and semantics are deliberately identical to the Go host so one dashboard
query covers both runtimes.

Labels deliberately exclude run and node id: droplets are destroyed every run, so per-run
labels would mint a fresh series set each time and grow the TSDB index without bound.
"""

import os
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

_lock = threading.Lock()
_isolated: dict[str, float] = {}
_reloads = 0.0
_batches = 0.0
_loads = 0.0
_load_failures = 0.0


def count_load(ok: bool) -> None:
    """One @actor.load attempt, and whether it returned.

    THE DENOMINATOR AND THE NUMERATOR MOVE TOGETHER, WHICH IS WHY THIS IS ONE FUNCTION.
    `count_batch()` has sat here since the incident with no caller in either host, so the ratio
    the fleet dashboard plots divides by a permanent zero; two separate functions is how that
    happens, because a caller can wire one and forget the other. A single call that always
    advances the denominator cannot be half-wired.

    This is the pair the round-3 signature is actually stated in: kf-m9 failed **81 of 82 resource
    loads** in five minutes while its process stayed alive and its poller stayed live. Nothing
    counted the 82 until now, so that sentence was not expressible as a query — it was found by
    reading logs by hand, hours later.

    Read by the **Warden**, which scrapes this listener on its own five-second turn and therefore
    owns the WINDOW (ADR 0037). A counter plus the Warden's own clock is a rate; the log line this
    replaces had neither a timestamp nor a bound, so no window could be derived from it at all.
    """
    global _loads, _load_failures
    with _lock:
        _loads += 1.0
        if not ok:
            _load_failures += 1.0


def count_isolated(category: str = "exhausted") -> None:
    """One unit permanently dropped. Non-zero means the run lost work."""
    global _isolated
    cat = category or "unknown"
    with _lock:
        _isolated[cat] = _isolated.get(cat, 0.0) + 1.0


def count_reload() -> None:
    """A resource was declared dead and rebuilt. A worker whose reloads climb while its peers
    sit at zero is the silently-sick-worker signature (kf-m9 ran at 81 of 82)."""
    global _reloads
    with _lock:
        _reloads += 1.0


def count_batch() -> None:
    global _batches
    with _lock:
        _batches += 1.0


def _esc(s: str) -> str:
    return str(s).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def render(actor: str, version: str) -> str:
    with _lock:
        iso = dict(_isolated)
        rel, bat = _reloads, _batches
        loads, load_fails = _loads, _load_failures
    base = f'actor="{_esc(actor)}",version="{_esc(version)}"'
    out = [
        "# HELP kontra_isolated_units_total Units permanently dropped after exhausting reloads. NON-ZERO MEANS THE RUN LOST WORK.",
        "# TYPE kontra_isolated_units_total counter",
    ]
    if not iso:
        # Emit an explicit zero so the series always exists. Without it a healthy worker has no
        # series at all, and `rate()` over nothing renders identically to "no data" — the exact
        # ambiguity this metric exists to remove.
        out.append(f'kontra_isolated_units_total{{{base},category="exhausted"}} 0')
    else:
        for cat in sorted(iso):
            out.append(f'kontra_isolated_units_total{{{base},category="{_esc(cat)}"}} {iso[cat]:g}')
    out += [
        "# HELP kontra_resource_reloads_total Times the actor resource was declared dead and rebuilt.",
        "# TYPE kontra_resource_reloads_total counter",
        f"kontra_resource_reloads_total{{{base}}} {rel:g}",
        "# HELP kontra_batches_total Unit batches executed by this worker.",
        "# TYPE kontra_batches_total counter",
        f"kontra_batches_total{{{base}}} {bat:g}",
        # The sick-worker ratio's two halves. Names pinned by shared/conformance/workerhealth.json,
        # because the Go host spells them independently and a `_failures_total` against a
        # `_failure_total` would leave the Warden dividing by an absent series on half a Fleet —
        # visible as a permanently `unknown` chip and as nothing else.
        "# HELP kontra_resource_loads_total @actor.load attempts. The DENOMINATOR of the sick-worker ratio.",
        "# TYPE kontra_resource_loads_total counter",
        f"kontra_resource_loads_total{{{base}}} {loads:g}",
        "# HELP kontra_resource_load_failures_total @actor.load attempts that raised. 81 of 82 in five minutes is the round-3 signature.",
        "# TYPE kontra_resource_load_failures_total counter",
        f"kontra_resource_load_failures_total{{{base}}} {load_fails:g}",
    ]
    return "\n".join(out) + "\n"


def serve(actor: str, version: str) -> None:
    """Start the metrics listener on its own thread and port.

    On its own port so enabling it cannot collide with anything else the process binds.
    Defaults to 0.0.0.0:9110 INSIDE the container — the deploy publishes it to the host's
    loopback only (127.0.0.1:9110:9110), so nothing is exposed on the VPC or the public IP.
    KONTRA_METRICS_ADDR=off disables it.
    """
    addr = os.getenv("KONTRA_METRICS_ADDR", "0.0.0.0:9110")
    if addr.lower() == "off":
        return
    host, _, port = addr.rpartition(":")
    host = host or "0.0.0.0"

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            if self.path.split("?")[0] != "/metrics":
                self.send_response(404)
                self.end_headers()
                return
            body = render(actor, version).encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            pass  # a scrape every 15s must not fill the actor's log

    def _run():
        try:
            HTTPServer((host, int(port)), Handler).serve_forever()
        except Exception as exc:  # a metrics listener must never take the actor down
            print(f"[actorkit] metrics listener on {addr} stopped: {exc}", flush=True)

    threading.Thread(target=_run, daemon=True).start()
