# Per-actor image for the `crawl4ai` actor.
#
# FROM the canonical kontra-host, then add ONLY this actor's deps. The entrypoint
# (`python3 /actor/crawl4ai/actor.py`) and identity stamp are added by the build step —
# do NOT set ENTRYPOINT/CMD here; the author writes deps only.
#
# crawl4ai drives a real Chromium via Playwright — both live in THIS actor's image, not
# the framework base (the dependency moves to the actor that needs it, like playwright).
FROM kontra-host:1

COPY . /actor/crawl4ai/

# >=0.8: the actor drives crawl4ai's NATIVE BFS resume_state/on_state_change (the RESUMABLE
# tier) — 0.7.x has neither kwarg and every crawl would TypeError.
RUN pip install --no-cache-dir 'crawl4ai>=0.8,<1' 'simhash>=2.1,<3' \
    && python -m playwright install --with-deps chromium
