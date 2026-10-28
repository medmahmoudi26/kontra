package mcprelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"kontra.sh/kontra/internal/mcpwire"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/upstream"
	"kontra.sh/kontra/internal/version"
)

// The service's era (spec/relay.md §11.12, D177; M3 WP-07). The gate keeps, per service and per replica, whether its
// upstream speaks 2026-07-28, and learns that a service is legacy-only from its own probe only:
//
//   - a server/discover result from the upstream makes the service modern: the one a client's forwarded
//     server/discover returns (LearnModern), or the answer to the gate's own server/discover, sent with no session
//     headers, one per service at a time, when there is none (Era);
//   - only an explicit legacy answer to the gate's own probe makes it legacy-only (mcpwire.ClassifyDiscover): a 400
//     with rule 3's -32000 body, a 404 or 405, or a 200 or 400 whose body is not a 2026-07-28 response;
//   - any other outcome (a modern error, another status, a node-written answer, a timeout) leaves it unknown, and the
//     request waiting for the probe is forwarded; another probe is sent no sooner than probeBackoff later;
//   - the answer to a client's request never sets it: a 400, 404 or 405 that an upstream returns to a client's modern
//     request is relayed to that client and changes nothing for other principals.
//
// What it learned is kept RefreshTools, and dropped when a node of the service connects or disconnects on any replica
// (DropEra: the tunnel registry's hook, which the replica holding the node runs and tells every other one by NOTIFY
// kontra_tunnel, since through the peer hop each replica uses the nodes another one holds), when the replica's LISTEN
// connection comes back (DropEras: a notice may have been lost) or when the service's row changes (Forget).

// ProbeTimeout bounds the gate's own server/discover (an answer that does not come within it leaves the era
// unknown).
const ProbeTimeout = 10 * time.Second

// probeBackoff is how long after an inconclusive probe the era stays unknown without another probe: the requests in
// that window are forwarded, as the one that waited for the probe was.
//
// SPEC-CONFLICT (D204; relay §11.12, D177: "A modern request that finds the era unknown waits for that probe"): read
// literally, every modern request to a service whose probe keeps ending inconclusively (an upstream that answers the
// probe with a modern error, a node that cannot reach its target, a probe that times out after ProbeTimeout) would
// send another probe and wait for it, up to 10 s each. The closest faithful behaviour: an inconclusive probe leaves
// the era unknown, and for probeBackoff the requests that find it so are forwarded at once, without waiting for
// another probe; after it, the next one probes again.
const probeBackoff = 5 * time.Second

// eraState is what the gate learned of a service's upstream.
type eraState struct {
	era  mcpwire.Era
	at   time.Time
	caps json.RawMessage // a modern upstream's server/discover capabilities, after §2.5
}

// Prober sends the gate's own server/discover to the service's upstream and reads its era from the answer; for a
// modern one it also returns the result.
type Prober func(ctx context.Context) (mcpwire.DiscoverAnswer, json.RawMessage)

// Era returns the service's upstream era, ok false when it is not known. With no fresh era it waits for the probe
// in flight, or starts one with probe (nil: none is started), on a context of the cache's own bounded by
// ProbeTimeout, so the request that starts it neither ends it by leaving nor delays the others past the bound. An
// inconclusive probe answers ok false; ctx's end is ctx's error.
func (c *ToolsCache) Era(ctx context.Context, key ToolsKey, probe Prober) (mcpwire.Era, bool, error) {
	for {
		c.mu.Lock()
		now := c.now()
		if st, ok := c.eras[key]; ok && now.Sub(st.at) < RefreshTools {
			c.mu.Unlock()
			return st.era, true, nil
		}
		if ch := c.probes[key]; ch != nil {
			c.mu.Unlock()
			select {
			case <-ch:
				if c.inconclusive(key) {
					return 0, false, nil
				}
				continue
			case <-ctx.Done():
				return 0, false, ctx.Err()
			}
		}
		if probe == nil || now.Before(c.retryAt[key]) {
			c.mu.Unlock()
			return 0, false, nil
		}
		ch := make(chan struct{})
		c.probes[key] = ch
		gen := c.eraGen[key]
		bound := c.probeTimeout
		if bound <= 0 {
			bound = ProbeTimeout
		}
		c.mu.Unlock()
		go func() {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bound)
			ans, res := probe(pctx)
			cancel()
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.probes, key)
			defer close(ch)
			if c.eraGen[key] != gen {
				return // dropped while it ran: what it learned may be another node's
			}
			at := c.now()
			if st, ok := c.eras[key]; ok && st.era == mcpwire.EraModern && at.Sub(st.at) < RefreshTools {
				return // a client's server/discover result made it modern meanwhile; that stands
			}
			switch ans {
			case mcpwire.AnswerModern:
				c.eras[key] = eraState{era: mcpwire.EraModern, at: at, caps: capabilitiesOf(res)}
				delete(c.retryAt, key)
			case mcpwire.AnswerLegacy:
				c.eras[key] = eraState{era: mcpwire.EraLegacy, at: at}
				delete(c.retryAt, key)
			default:
				c.retryAt[key] = at.Add(probeBackoff)
			}
		}()
	}
}

// inconclusive reports whether the last probe of the service left its era unknown.
func (c *ToolsCache) inconclusive(key ToolsKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, known := c.eras[key]
	return !known && c.probes[key] == nil && c.now().Before(c.retryAt[key])
}

// LearnModern records a server/discover result the upstream returned (a client's, forwarded): the service is modern
// and these are its capabilities (§11.12, D177).
func (c *ToolsCache) LearnModern(key ToolsKey, result json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.eras[key] = eraState{era: mcpwire.EraModern, at: c.now(), caps: capabilitiesOf(result)}
	delete(c.retryAt, key)
}

// Capabilities are a modern service's server/discover capabilities as last learned, ok false when the service is
// not known to be modern.
func (c *ToolsCache) Capabilities(key ToolsKey) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.eras[key]
	if !ok || st.era != mcpwire.EraModern || c.now().Sub(st.at) >= RefreshTools {
		return nil, false
	}
	return st.caps, true
}

// DropEra forgets what the gate learned of a service's upstream (a node of it connected or disconnected on any
// replica, D177); a probe in flight installs nothing.
func (c *ToolsCache) DropEra(key ToolsKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropEra(key)
}

// DropEras forgets every service's era (the replica's LISTEN connection came back: a node's change may have gone
// unannounced while it was down).
func (c *ToolsCache) DropEras() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.eras {
		c.dropEra(key)
	}
	for key := range c.retryAt {
		c.dropEra(key)
	}
	for key := range c.probes {
		c.dropEra(key)
	}
}

func (c *ToolsCache) dropEra(key ToolsKey) {
	delete(c.eras, key)
	delete(c.retryAt, key)
	c.eraGen[key]++
}

// Forget drops everything the cache holds of a service: its lists and its era (its row changed: another backend,
// another policy).
func (c *ToolsCache) Forget(key ToolsKey) {
	c.Invalidate(key)
	c.DropEra(key)
}

// capabilitiesOf is a server/discover result's capabilities object, nil when it has none.
func capabilitiesOf(result json.RawMessage) json.RawMessage {
	caps, ok := mcpwire.Member(result, "capabilities")
	if !ok || firstByte(caps) != '{' {
		return nil
	}
	return bytes.Clone(caps)
}

// declaresTasks reports whether server/discover capabilities declare the tasks extension.
func declaresTasks(caps json.RawMessage) bool {
	ext, ok := mcpwire.Member(caps, "extensions")
	if !ok {
		return false
	}
	v, ok := mcpwire.Member(ext, mcpwire.ExtTasks)
	return ok && firstByte(v) == '{'
}

// era is the request's service's era (the probe, when it is not known, chosen and sent as for a modern request).
func (h *Handler) era(rq *request) (mcpwire.Era, bool, error) {
	return h.deps.Tools.Era(rq.ctx(), h.toolsKey(rq), h.prober(rq))
}

// discoverBody is the gate's own server/discover (2026-07-28 server/discover: no parameters beyond _meta).
func discoverBody() []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "kontra-discover", "method": mcpwire.MethodServerDiscover,
		"params": map[string]any{"_meta": map[string]any{
			mcpwire.MetaProtocolVersion:    mcpwire.V20260728,
			mcpwire.MetaClientCapabilities: map[string]any{},
			mcpwire.MetaClientInfo:         map[string]string{"name": "kontra-gate", "version": version.Get()},
		}}})
	return b
}

// prober sends the gate's own server/discover for rq's service (§11.12, D177) to a target chosen as for an initialize
// (§6.1: the direct backend, this replica's sessions first, else the peer hop), with no session headers and no MCP
// preamble fields (D180), and classifies the answer. A target that cannot be chosen, an exchange that fails, a node's
// own answer (Kontra-Node) and a body over the limit are inconclusive.
func (h *Handler) prober(rq *request) Prober {
	who := &request{svc: rq.svc, p: rq.p}
	return func(ctx context.Context) (mcpwire.DiscoverAnswer, json.RawMessage) {
		ans, res, why := h.probe(ctx, who)
		h.log.Info("mcp: the gate's own server/discover", "service", who.svc.Ref.Name, "era", answerName(ans), "why", why)
		return ans, res
	}
}

func answerName(a mcpwire.DiscoverAnswer) string {
	switch a {
	case mcpwire.AnswerModern:
		return "modern"
	case mcpwire.AnswerLegacy:
		return "legacy-only"
	}
	return "unknown"
}

func (h *Handler) probe(ctx context.Context, who *request) (mcpwire.DiscoverAnswer, json.RawMessage, string) {
	t, ref := h.chooseIn(ctx, who, nil, nil)
	if ref != nil {
		return mcpwire.AnswerUnknown, nil, "no target"
	}
	hdr := http.Header{}
	hdr.Set("Accept", "application/json, text/event-stream")
	hdr.Set("Content-Type", "application/json")
	hdr.Set(mcpwire.HeaderProtocolVersion, mcpwire.V20260728)
	hdr.Set(mcpwire.HeaderMethod, mcpwire.MethodServerDiscover)
	resp, err := h.send(ctx, t, who, http.MethodPost, discoverBody(), hdr, "", upstream.MCPExchange{})
	if err != nil {
		return mcpwire.AnswerUnknown, nil, "no exchange"
	}
	defer resp.Body.Close()
	if t.tunnelled() && tunnel.NodeReason(strings.TrimSpace(resp.Header.Get(tunnel.HeaderNode))) != "" {
		return mcpwire.AnswerUnknown, nil, "the node answered"
	}
	msg, err := probeAnswer(resp)
	if err != nil {
		return mcpwire.AnswerUnknown, nil, err.Error()
	}
	ans, res := mcpwire.ClassifyDiscover(resp.StatusCode, msg)
	if ans == mcpwire.AnswerModern {
		if res, err = mcpwire.ScrubResult(mcpwire.FromUpstream, mcpwire.MethodServerDiscover, res, false); err != nil {
			return mcpwire.AnswerUnknown, nil, "the result could not be read"
		}
	}
	return ans, res, "answered " + http.StatusText(resp.StatusCode)
}

// errProbeBody is a probe answer the gate could not read in time or within the limit.
var errProbeBody = errors.New("the answer could not be read")

// probeAnswer reads the answer to the gate's server/discover: the JSON body's message, or the first response in an
// event stream (as a client reads it), nil when the body holds no message Parse accepts. A body that cannot be read
// (cut, over upstream.MaxBody, the probe's time out) is an error: the outcome is unknown.
func probeAnswer(resp *http.Response) (*mcpwire.Message, error) {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" {
		r := newSSEReader(io.LimitReader(resp.Body, upstream.MaxBody))
		for {
			ev, err := r.next()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, nil
				}
				return nil, errProbeBody
			}
			if !ev.message() || !ev.hasData || strings.TrimSpace(ev.data) == "" {
				continue
			}
			m, err := mcpwire.Parse([]byte(ev.data), mcpwire.FromUpstream)
			if err != nil {
				return nil, nil
			}
			if m.Method == "" {
				return m, nil
			}
		}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, upstream.MaxBody+1))
	if err != nil || len(b) > upstream.MaxBody {
		return nil, errProbeBody
	}
	if mt != "application/json" {
		return nil, nil
	}
	m, err := mcpwire.Parse(b, mcpwire.FromUpstream)
	if err != nil {
		return nil, nil
	}
	return m, nil
}
