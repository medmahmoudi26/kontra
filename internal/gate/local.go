package gate

import (
	"log/slog"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/console"
	"kontra.sh/kontra/internal/edge"
)

// mountConsole registers the console (the page at / and the console API under /v1/) in the local development profile
// only, and reports whether it did: on any other gate those paths stay the closed router's empty 404.
func mountConsole(cfg *config.Config, routes *edge.Router, d console.Deps) bool {
	if !cfg.Local() {
		return false
	}
	console.New(d).Register(routes)
	return true
}

// LocalBanner is the local development profile's start warning (relay.md §18): the gate serves every client without
// credentials as one caller and shows every conversation on its console, at publicURL/; with bodies on (D358) it also
// says that every A2A call's request and response bodies are kept.
func LocalBanner(log *slog.Logger, publicURL string, bodies bool) {
	msg := "local development profile: this gate serves every client without credentials, as one shared caller, and " +
		"shows every conversation, task and A2A call on its console; it listens on loopback only and must never be exposed"
	if bodies {
		msg += "; it keeps the request and response bodies of every A2A call, on both legs, credentials redacted " +
			"(traffic.bodies: false keeps metadata only)"
	}
	log.Warn(msg, "console", publicURL+"/")
}
