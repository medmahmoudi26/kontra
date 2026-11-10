package a2ahttp

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/ids"
)

// The local development profile (auth.NewLocalAuthenticator, services all allow anonymous): a request with a bogus
// Authorization header is served as the one anonymous caller; on a team gate the same request is 401.
func TestLocalProfileSharedCaller(t *testing.T) {
	f := newFixture(t, false)
	body := rpc("SendMessage", msg, 1)

	if w := f.do(req{"POST", "/agents/open", body, v1("bogus")}); w.Code != http.StatusUnauthorized {
		t.Fatalf("team gate, bogus credential: %d, want 401", w.Code)
	}

	f.srv = New(Deps{Services: f.reg, Handlers: handlers{f.h}, SSE: f.st, Auth: auth.NewLocalAuthenticator()})
	want, _ := ids.AnonymousBuyer(uuid.Nil)
	for _, token := range []string{"bogus", "alice", ""} {
		w := f.do(req{"POST", "/agents/open", body, v1(token)})
		if r := decodeRPC(t, w); w.Code != 200 || r.Error != nil {
			t.Fatalf("local, token %q: %d %s", token, w.Code, w.Body)
		}
		c, _ := f.h.last()
		if c.principal.Kind != auth.KindAnonymous || c.principal.BuyerID != want {
			t.Fatalf("local, token %q: principal %+v, want the anonymous caller %s", token, c.principal, want)
		}
	}
}
