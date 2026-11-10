package gate

import "log/slog"

// LocalBanner is the local development profile's start warning (relay.md §18): the gate serves every client without
// credentials as one caller and shows every conversation on its console, at publicURL/.
func LocalBanner(log *slog.Logger, publicURL string) {
	log.Warn("local development profile: this gate serves every client without credentials, as one shared caller, and "+
		"shows every conversation, task and A2A call on its console; it listens on loopback only and must never be "+
		"exposed", "console", publicURL+"/")
}
