package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/upstream"
	"kontra.sh/kontra/internal/version"
)

// WarmTools drains svc's tools/list into the cache in the background (relay.md §11.3, §12.2; DM4-4): the gate's own
// /mcp describe of an MCP service whose cache is cold answers without its tools and starts this, at most once per
// service per 10 minutes (internal/gatemcp), so no request waits on an upstream. The drain runs on a context of its
// own (bounded by DrainTimeout plus the session's opening and closing), for p, on an upstream session of its own: a
// legacy initialize to a target chosen as for any initialize (§6.1, the peer hop included), notifications/initialized,
// the tools/list pages, and a best-effort DELETE; or, for a service whose upstream is known to be of the 2026-07-28
// era, stateless pages. info, when not nil, gets the upstream's InitializeResult.serverInfo (describe and search show
// its title or name as the service's description, DM4-4). It writes no calls row and opens no mcp_sessions row (M4
// WP-04; a file of its own, so M3's files are unchanged).
func (h *Handler) WarmTools(svc *Service, p auth.Principal, info func(serverInfo json.RawMessage)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), DrainTimeout+2*ProbeTimeout)
		defer cancel()
		if err := h.warmTools(ctx, svc, p, info); err != nil {
			h.log.Info("mcp: the background drain of a tools/list failed", "service", svc.Ref.Name, "error", err)
		}
	}()
}

func (h *Handler) warmTools(ctx context.Context, svc *Service, p auth.Principal, info func(json.RawMessage)) error {
	who := &request{svc: svc, p: p}
	key := ToolsKey{TenantID: svc.Ref.TenantID, Service: svc.Ref.Name}
	if era, known, _ := h.deps.Tools.Era(ctx, key, nil); known && era == mcpwire.EraModern {
		who.msg = &mcpwire.Message{ID: json.RawMessage(`"kontra-warm"`), Params: json.RawMessage(`{"_meta":{}}`)}
		_, _, err := h.deps.Tools.ListModern(ctx, key, ToolView{}, h.modernPager(who))
		return err
	}
	t, ref := h.chooseIn(ctx, who, nil, nil)
	if ref != nil {
		return errors.New("no node serves the service")
	}
	_, hash, err := MintID()
	if err != nil {
		return err
	}
	x := upstream.MCPExchange{Session: hexHash(hash)}
	hdr := func(v string) http.Header {
		hd := http.Header{}
		hd.Set("Accept", "application/json, text/event-stream")
		hd.Set("Content-Type", "application/json")
		if v != "" {
			hd.Set(mcpwire.HeaderProtocolVersion, v)
		}
		return hd
	}
	initID := json.RawMessage(`"kontra-warm-init"`)
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": initID, "method": mcpwire.MethodInitialize,
		"params": map[string]any{"protocolVersion": mcpwire.V20251125, "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "kontra-gate", "version": version.Get()}}})
	resp, err := h.send(ctx, t, who, http.MethodPost, body, hdr(""), "", x)
	if err != nil {
		return err
	}
	sid := resp.Header.Get(mcpwire.HeaderSessionID)
	res, err := h.readAnswer(resp, initID, t.tunnelled())
	if err != nil {
		return err
	}
	if si, ok := mcpwire.Member(res, "serverInfo"); ok && info != nil && firstByte(si) == '{' {
		info(bytes.Clone(si))
	}
	v, _ := stringMember(res, "protocolVersion")
	if v != mcpwire.V20251125 && v != mcpwire.V20250618 {
		v = mcpwire.V20251125
	}
	defer func() {
		if sid == "" {
			return
		}
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if r, err := h.send(dctx, t, who, http.MethodDelete, nil, hdr(v), sid, x); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
			_ = r.Body.Close()
		}
	}()
	note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if r, err := h.send(ctx, t, who, http.MethodPost, note, hdr(v), sid, x); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
		_ = r.Body.Close()
	} else {
		return err
	}
	listID := json.RawMessage(`"kontra-warm-tools"`)
	pager := func(ctx context.Context, cursor string) (json.RawMessage, error) {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": listID, "method": mcpwire.MethodToolsList, "params": params})
		if err != nil {
			return nil, err
		}
		r, err := h.send(ctx, t, who, http.MethodPost, b, hdr(v), sid, x)
		if err != nil {
			return nil, err
		}
		return h.readAnswer(r, bytes.Clone(listID), t.tunnelled())
	}
	_, err = h.deps.Tools.List(ctx, key, mcpwire.EraLegacy, ToolView{}, pager)
	return err
}
