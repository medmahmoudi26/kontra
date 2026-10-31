package gatemcp

import (
	"context"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/store"
)

// Served reads a service as this replica's registry serves it (*gate.Services, LookupByID): ok false when the
// registry serves no enabled service of that name (unknown to it, disabled, a row it refused to build, such as one
// whose allow: or card_extensions it cannot read, or one it has not loaded yet); otherwise its kind and its allow:
// list as the A2A route and /mcp/{svc} read them on every request.
//
// /mcp lists, describes, hires and follows exactly what those routes serve (relay.md §12.2, D303): search, describe
// and hire see only served services, and admission is the registry's allow:; a hire task is answered only while its
// service is served and its allow: admits the reader, as the A2A route answers GetTask, CancelTask and a follow-up
// (404 "unknown service"): reply "unknown task", tasks/* -32602, left out of tasks/list. A nil Served reads the
// services table alone (tests).
type Served func(ctx context.Context, tenantID uuid.UUID, name string) (ServedService, bool)

// ServedService is what Served reads of a service.
type ServedService struct {
	Kind  string
	Allow auth.AllowList
}

// admitsTask reports whether the hire task row's service is an A2A service served here whose allow: admits p (Served;
// true when served is nil).
func admitsTask(ctx context.Context, served Served, p auth.Principal, row *store.TaskRow) bool {
	if served == nil {
		return true
	}
	s, ok := served(ctx, row.TenantID, row.Service)
	return ok && s.Kind == config.KindA2A && s.Allow.Admits(p)
}
