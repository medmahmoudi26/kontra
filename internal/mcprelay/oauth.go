package mcprelay

import "net/http"

// The MCP OAuth resource server on /mcp/{svc} (spec/relay.md §12.1, DM4-9, DM4-29; M4 WP-06): the Authenticator
// verifies a bearer JWT for the route's canonical URI (auth.AuthenticateMCP), and a 401 carries the route's challenge
// naming its RFC 9728 document (internal/oidc).

// routeSvc is the route's service as its canonical URI names it: name, or tenant/name on a multi-tenant gate, read from
// the path as written (the resource server checks that it is well formed before it names it anywhere).
func routeSvc(r *http.Request, multi bool) string {
	if !multi {
		return r.PathValue("name")
	}
	return r.PathValue("tenant") + "/" + r.PathValue("name")
}

// unauthorized writes the 401 of a request on the route svc: WWW-Authenticate is the resource server's challenge
// (Bearer resource_metadata=…, scope="mcp"; error="invalid_token" when a credential was refused), or Bearer alone
// without auth.mcp_oauth; a POST gets a JSON-RPC error without id.
func (h *Handler) unauthorized(rq *request, svc string, refused bool) {
	rq.w.Header().Set("WWW-Authenticate", h.deps.Auth.MCPChallenge(svc, refused))
	if rq.r.Method == http.MethodPost {
		writeError(rq.w, http.StatusUnauthorized, nil, codeUnauthenticated, "unauthenticated", nil)
	} else {
		writeEmpty(rq.w, http.StatusUnauthorized)
	}
}

// notServed answers a request the route does not serve: an unknown, disabled or other-kind service, or one whose
// allow: does not admit the principal. It is the unknown-service 404 (§2.1, D50), except that with auth.mcp_oauth a
// request without a credential gets the route's 401 challenge instead, whether or not the service exists, so an
// OAuth client can sign in and nothing tells it which services exist (§12.1, DM4-29). A service whose allow: admits
// anonymous serves a request without a credential before this is reached.
//
// SPEC-CONFLICT (relay.md §12.1 with §2.3 and tunnel.md §6.1, M4 WP-06): §12.1 reads "a request without a credential
// is 401" on every /mcp/{svc} once the resource server is configured, while §2.3 gives a request without a credential
// the anonymous principal on a service whose allow: contains anonymous, and §6.1's --public MCP services are "callable
// by anyone". The closest faithful behaviour: a service that admits anonymous still serves such a request; every other
// one (existing or not) is 401 with the challenge. §12.1 says so now (DM4-29).
func (h *Handler) notServed(rq *request, svc string, authenticated bool) {
	if !authenticated && h.deps.Auth.MCPOAuth() {
		h.unauthorized(rq, svc, false)
		return
	}
	h.unknownService(rq)
}
