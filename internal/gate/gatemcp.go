package gate

import (
	"context"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/gatemcp"
	"kontra.sh/kontra/internal/mcprelay"
	"kontra.sh/kontra/internal/relay"
)

// assembleGateMCP builds the gate's own /mcp (spec/relay.md §12; M4 WP-04) and mounts it: the catalog over the
// services table and the MCP relay's tools cache (describe's background drain through the relay's own sessions), the
// hirer over the A2A services' handlers (a hire task is an ordinary A2A task, created by the service's interceptors
// and work queue; assemble builds the hirer), the MCP Tasks of hire tasks (the lifecycle client's MCP-mode cancel),
// the URL elicitations, and /mcp's legacy sessions (mcp_sessions rows with service NULL). It runs after
// assembleMCP, whose handler and tools cache it uses.
func (r *Replica) assembleGateMCP(routes *edge.Router, authn *auth.Authenticator, hirer *relay.Hirer) error {
	cfg := r.cfg
	multi := cfg.Tenancy == config.TenancyMulti
	catalog := gatemcp.NewCatalog(gatemcp.CatalogConfig{
		DB: r.pool, Multi: multi, PublicURL: cfg.PublicURL, MCP: r.services, Tools: r.tools, Warm: r.mcp,
		Served: r.servedService, Logger: r.log,
	})
	tasks, err := gatemcp.NewHireTasks(gatemcp.HireTasksConfig{
		Log: r.storeLog, Waker: r.listener, Lifecycle: r.lifecycle, Hirer: hirer, Render: r.render,
		Served: r.servedService, Logger: r.log,
	})
	if err != nil {
		return err
	}
	r.hireTasks = tasks
	h, err := gatemcp.NewHandler(gatemcp.Deps{
		PublicURL: cfg.PublicURL, Multi: multi, Auth: authn, Sessions: mcprelay.NewSessions(r.pool), Catalog: catalog,
		Hirer: hirer, Tasks: tasks, Elicitations: gatemcp.NewElicitations(r.storeLog, r.listener, r.log), Log: r.storeLog,
		Waker: r.listener, Render: r.render, Served: r.servedService, AllowedOrigins: cfg.MCP.AllowedOrigins, Logger: r.log,
		PlainWait: r.opts.plainWait,
	})
	if err != nil {
		return err
	}
	h.Register(routes)
	return nil
}

// servedService is gatemcp.Served over the replica's registry (D303): an enabled service it serves, with its kind and
// allow: as the A2A route and /mcp/{svc} read them on every request.
func (r *Replica) servedService(_ context.Context, tenantID uuid.UUID, name string) (gatemcp.ServedService, bool) {
	s := r.services.LookupByID(tenantID, name)
	if s == nil || s.Disabled {
		return gatemcp.ServedService{}, false
	}
	return gatemcp.ServedService{Kind: s.Ref.Kind, Allow: s.Allow}, true
}

// awaitHireCancels waits, at most gatemcp.DetachedWait, for /mcp's detached kontra.cancel Updates and for the modern
// hires that may still send one (relay.md §12.5, D178): the public listener has stopped, which closed the streams of
// modern hires still waiting, and each such hire's task is being cancelled. A cancel whose Update had not reached
// Temporal when Run closes the client would be lost, and its task would run unseen; once this returns, each was accepted
// (by another replica's kontra-lifecycle worker: this one's have stopped) and is in its task's history. A gate of one
// replica has no worker left to accept it: the drain waits out the bound, and the Update, admitted by Temporal, waits
// for the next worker.
func (r *Replica) awaitHireCancels(ctx context.Context) {
	if r.hireTasks == nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, gatemcp.DetachedWait)
	defer cancel()
	if err := r.hireTasks.Wait(wctx); err != nil {
		r.log.Warn("/mcp's cancels of hire tasks were still unaccepted when the drain closed Temporal", "error", err)
	}
}
