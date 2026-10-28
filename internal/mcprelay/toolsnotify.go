package mcprelay

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/config"
)

// An upstream's notifications/tools/list_changed refreshes the service's drained list (§11.3) on every replica, not
// only on the one that relayed it: the list drives tools/list and the "Unknown tool" policy decision on each, and a
// replica that kept the old one would refuse a tool the upstream just added, or list a removed one, until
// RefreshTools. The replica that sees it (a relayed stream, a durable dispatch's own session) drops its lists at once
// and sends NOTIFY kontra_tunnel 'tools:<tenant_id>/<service>' (gate-internal, between replicas); every replica drops
// the service's lists, both eras, on it.

// toolsPrefix starts NOTIFY kontra_tunnel's payload for a service whose tools changed.
const toolsPrefix = "tools:"

// ToolsChangedPayload is NOTIFY kontra_tunnel's payload for key's tools/list_changed.
func ToolsChangedPayload(key ToolsKey) string {
	return toolsPrefix + key.TenantID.String() + "/" + key.Service
}

// ParseToolsChanged reads a ToolsChangedPayload; ok is false for anything else.
func ParseToolsChanged(payload string) (ToolsKey, bool) {
	rest, ok := strings.CutPrefix(payload, toolsPrefix)
	if !ok {
		return ToolsKey{}, false
	}
	tid, service, ok := strings.Cut(rest, "/")
	if !ok || !config.NameRE.MatchString(service) {
		return ToolsKey{}, false
	}
	id, err := uuid.Parse(tid)
	if err != nil {
		return ToolsKey{}, false
	}
	return ToolsKey{TenantID: id, Service: service}, true
}

// toolsNotifyAfter coalesces a service's list_changed notices: at most one NOTIFY per service per window, sent at
// its end, so that the last change is always told and an upstream that repeats the notification costs one NOTIFY.
const toolsNotifyAfter = 250 * time.Millisecond

// ToolsNotifier sends ToolsChangedPayload through Notify, coalesced per service (toolsNotifyAfter).
type ToolsNotifier struct {
	Notify func(ctx context.Context, payload string) error
	Log    *slog.Logger

	mu      sync.Mutex
	pending map[ToolsKey]bool
	after   time.Duration // toolsNotifyAfter; tests shorten it
}

// Changed schedules the NOTIFY for key, unless one is already scheduled.
func (n *ToolsNotifier) Changed(key ToolsKey) {
	if n == nil || n.Notify == nil {
		return
	}
	n.mu.Lock()
	if n.pending == nil {
		n.pending = map[ToolsKey]bool{}
	}
	if n.pending[key] {
		n.mu.Unlock()
		return
	}
	n.pending[key] = true
	after := n.after
	if after == 0 {
		after = toolsNotifyAfter
	}
	n.mu.Unlock()
	time.AfterFunc(after, func() {
		n.mu.Lock()
		delete(n.pending, key)
		n.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := n.Notify(ctx, ToolsChangedPayload(key)); err != nil && n.Log != nil {
			n.Log.Warn("mcp: the other replicas were not told of a tools/list_changed", "service", key.Service, "error", err)
		}
	})
}
