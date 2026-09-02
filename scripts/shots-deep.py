#!/usr/bin/env python3
"""Capture the workflow thread: its tabs, the start-a-run form, and a run's transcript.

The five-surface pass (`shots.py`) photographs the frame. This one goes inside the surface that
matters — a workflow is a thread, so the interesting states are its tabs and a run rendered as a
conversation. Tabs are reached by CLICKING rather than by URL, because a tab is a view of the
selected run and not an address of its own.
"""
import sys, pathlib
from playwright.sync_api import sync_playwright

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:8088"
OUT = pathlib.Path(sys.argv[2] if len(sys.argv) > 2 else "/root/kontra-shots/deep")
WORKFLOW = sys.argv[3] if len(sys.argv) > 3 else "redditscrape"
OUT.mkdir(parents=True, exist_ok=True)


def shot(page, name):
    dest = OUT / f"{name}.png"
    page.wait_for_timeout(1500)
    page.screenshot(path=str(dest))
    txt = page.inner_text("body")[:100].replace("\n", " ")
    print(f"  {name:<26} {dest}")
    print(f"  {'':<26} {txt!r}")


def main() -> int:
    with sync_playwright() as p:
        b = p.chromium.launch()
        page = b.new_page(viewport={"width": 1700, "height": 1100})

        # The thread itself: run list beside the tabs, and whatever the default tab shows.
        page.goto(f"{BASE}/workflows/{WORKFLOW}", wait_until="networkidle", timeout=30000)
        shot(page, "01-thread")

        # Every tab, by its visible name. A tab that is not there is reported, not skipped
        # silently — an absent tab is a finding about the build, not a gap in the screenshots.
        for i, tab in enumerate(["Chat", "Code", "Scratch", "Monitor", "Data"], start=2):
            try:
                page.get_by_role("tab", name=tab).click(timeout=4000)
            except Exception:
                try:
                    page.get_by_text(tab, exact=True).first.click(timeout=4000)
                except Exception as e:
                    print(f"  {tab:<26} NOT FOUND ({str(e)[:60]})", file=sys.stderr)
                    continue
            shot(page, f"{i:02d}-tab-{tab.lower()}")

        b.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
