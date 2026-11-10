package gate

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/console"
	"kontra.sh/kontra/internal/edge"
)

// The console is mounted in the local development profile only: on a team gate /, /v1/tasks and the page's files
// reach the closed default route's empty 404.
func TestConsoleOnlyInTheProfile(t *testing.T) {
	for _, local := range []bool{false, true} {
		cfg := &config.Config{PublicURL: "http://127.0.0.1:8080"}
		if local {
			cfg.Profile = config.ProfileLocal
		}
		routes := edge.NewRouter()
		if got := mountConsole(cfg, routes, console.Deps{Config: cfg}); got != local {
			t.Fatalf("local=%v: mounted %v", local, got)
		}
		srv := httptest.NewServer(edge.Middleware(edge.MiddlewareConfig{}, routes))
		paths := []string{"/", "/v1/tasks", "/v1/info", "/js/main.js"}
		if local {
			paths = []string{"/", "/js/main.js"} // the API needs a database; internal/console tests it
		}
		for _, p := range paths {
			resp, err := http.Get(srv.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			closed := resp.StatusCode == http.StatusNotFound && len(b) == 0
			if closed == local {
				t.Errorf("local=%v GET %s = %d (%d bytes)", local, p, resp.StatusCode, len(b))
			}
		}
		srv.Close()
	}
}

// The profile's start warning names what it does not protect and gives the console's URL.
func TestLocalBanner(t *testing.T) {
	var logs bytes.Buffer
	LocalBanner(slog.New(slog.NewTextHandler(&logs, nil)), "http://127.0.0.1:8080")
	out := logs.String()
	for _, want := range []string{"level=WARN", "without credentials", "every conversation", "console=http://127.0.0.1:8080/"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner %q lacks %q", out, want)
		}
	}
}
