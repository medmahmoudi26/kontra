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
// credentials as one caller and shows every conversation on its console, at publicURL/.
func LocalBanner(log *slog.Logger, publicURL string) {
	log.Warn("local development profile: this gate serves every client without credentials, as one shared caller, and "+
		"shows every conversation, task and A2A call on its console; it listens on loopback only and must never be "+
		"exposed", "console", publicURL+"/")
}
