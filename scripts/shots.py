#!/usr/bin/env python3
"""Capture the kontra console's surfaces, one PNG each.

WHY A SCRIPT AND NOT `firefox --screenshot`: the one-shot flag fires on the load event, which
for this SPA is before the first API answer — every surface photographs as "Loading...". This
waits for the network to settle AND for a surface-specific element, so a shot is either the real
thing or an explicit failure, never a spinner presented as a screenshot.
"""
import sys, pathlib
from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8088"
OUT = pathlib.Path(sys.argv[2] if len(sys.argv) > 2 else "/root/kontra-shots")
OUT.mkdir(parents=True, exist_ok=True)

# (slug, path, a selector that only exists once the surface has real content)
SURFACES = [
    ("workflows", "/workflows", "text=/workflow/i"),
    ("actors", "/actors", "text=/actor/i"),
    ("datasets", "/datasets", "text=/dataset/i"),
    ("monitor", "/monitor", "text=/monitor|terminal|machine/i"),
    ("settings", "/settings", "text=/secret|setting/i"),
]


def main() -> int:
    failures = []
    with sync_playwright() as p:
        browser = p.chromium.launch()
        page = browser.new_page(viewport={"width": 1600, "height": 1000})
        for slug, path, ready in SURFACES:
            url = f"{BASE}{path}"
            try:
                page.goto(url, wait_until="networkidle", timeout=30000)
                try:
                    page.wait_for_selector(ready, timeout=8000)
                except Exception:
                    pass  # surface may legitimately be empty; the shot still shows the chrome
                page.wait_for_timeout(1200)
                dest = OUT / f"{slug}.png"
                page.screenshot(path=str(dest), full_page=False)
                body = page.inner_text("body")[:80].replace("\n", " ")
                print(f"  {slug:<10} {dest}  first-text: {body!r}")
            except Exception as e:  # noqa: BLE001 - report, do not mask
                failures.append((slug, str(e)[:120]))
                print(f"  {slug:<10} FAILED: {e}", file=sys.stderr)
        browser.close()
    if failures:
        print(f"\n{len(failures)} surface(s) failed", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
