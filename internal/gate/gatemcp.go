package gate

import (
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
		DB: r.pool, Multi: multi, PublicURL: cfg.PublicURL, MCP: r.services, Tools: r.tools, Warm: r.mcp, Logger: r.log,
	})
	tasks, err := gatemcp.NewHireTasks(gatemcp.HireTasksConfig{
		Log: r.storeLog, Waker: r.listener, Lifecycle: r.lifecycle, Hirer: hirer, Render: r.render, Logger: r.log,
	})
	if err != nil {
		return err
	}
	h, err := gatemcp.NewHandler(gatemcp.Deps{
		PublicURL: cfg.PublicURL, Multi: multi, Auth: authn, Sessions: mcprelay.NewSessions(r.pool), Catalog: catalog,
		Hirer: hirer, Tasks: tasks, Elicitations: gatemcp.NewElicitations(r.storeLog, r.listener, r.log), Log: r.storeLog,
		Waker: r.listener, Render: r.render, AllowedOrigins: cfg.MCP.AllowedOrigins, Logger: r.log,
		PlainWait: r.opts.plainWait,
	})
	if err != nil {
		return err
	}
	h.Register(routes)
	return nil
}
