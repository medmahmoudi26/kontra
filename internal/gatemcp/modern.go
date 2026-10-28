package gatemcp

import (
	"net/http"

	"kontra.sh/kontra/internal/mcpwire"
)

// modern serves a 2026-07-28 request on /mcp (§12.5, §11.12 rules 2–4): server/discover, the modern tools/list,
// hire as §11.12 step 4, tasks/get, tasks/update and tasks/cancel over hire tasks. M4 WP-05 builds it.
//
// Until then /mcp answers a modern request as a server of the legacy era only does (§11.12 rule 3's 400 with
// -32000): a dual-era client falls back to initialize and gets the legacy surface, which WP-04 serves in full.
func (h *Handler) modern(rq *request) {
	writeRefusal(rq.w, &mcpwire.Refusal{Status: http.StatusBadRequest, Body: mcpwire.LegacyNoSessionBody})
}
