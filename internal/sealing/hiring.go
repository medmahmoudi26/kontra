package sealing

import (
	"context"
	"log/slog"
	"net/http"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/unimpl"
)

// HiringConfig configures the hiring side.
type HiringConfig struct {
	Jobs *nodekey.JobKeys // ~/.kontra/jobs/<gate host>/
	Pins *pins.Store
	Opts pins.Options // --pin-recipients, --accept-change
}

// Hiring is the hiring node's sealing (spec/ext-sealed.md §5 "Hiring node"), for a service whose verified card,
// or for MCP whose initialize result, carries the extension:
//
//  1. checks the service's pin (§7) before sealing anything;
//  2. mints a per-job key and rk, and a contextId when the client gave none;
//  3. seals every part of the outgoing message (MCP: arguments, with no Mcp-Param-* header) to all recipients;
//  4. opens every sealed part of the job's answers, checking dir, ids, its own key id, auth, parts 0..parts-1
//     once, and per artifactId the chunks in order; a part that fails is dropped and the client gets a failed
//     status with kontra.failure sealed_rejected. An MCP input request (D175) must carry the job's session, the
//     task's tool and a messageId not opened before in the task (replayed otherwise); Job keeps which gate
//     request id (k.<taskId>.<seq>) carried it, and the local client's answer to that id is sealed as a request
//     with the same session, tool and messageId and the job's replyTo and rk; 4a. an unsealed part in a sealed
//     job is refused except the gate's own terminal status text, handed over with metadata["kontra.sealed"] =
//     false;
//  5. hands the local client plaintext parts with their original mediaType.
type Hiring struct {
	cfg HiringConfig
	log *slog.Logger
}

// NewHiring returns the hiring side.
func NewHiring(cfg HiringConfig, log *slog.Logger) (*Hiring, error) {
	return nil, unimpl.Error("sealing.NewHiring", "M3 WP-08")
}

// Job is one hired job's sealing state, from Request to Response: its per-job key and rk, and for an MCP task
// the messageId of each input request it opened, by the gate request id that carried it (D175).
type Job struct{}

// Request seals a local client's request to a sealed service (the card's or initialize's recipients), or
// returns it unchanged for a service that is not sealed. A pin refusal is a *pins.ChangeError, answered locally.
func (h *Hiring) Request(ctx context.Context, service string, req *http.Request) (*http.Request, *Job, error) {
	return nil, nil, unimpl.Error("sealing.Hiring.Request", "M3 WP-08")
}

// Response opens the gate's answer for job (unary or SSE), inlining kontra-blob: parts fetched on blob streams.
func (h *Hiring) Response(ctx context.Context, job *Job, resp *http.Response) (*http.Response, error) {
	return nil, unimpl.Error("sealing.Hiring.Response", "M3 WP-08")
}
