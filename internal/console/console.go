// Package console is the local development profile's console (spec/relay.md §18; the console capability): the page
// at / (the web package's files, embedded) and the console API under /v1/, whose paths and JSON shapes follow Kontra
// Cloud's control plane, so the same frontend serves both. It is mounted only when the gate runs with profile: local.
//
// The API reads the gate's own tables: tasks and their event log, the services and the tunnel sessions serving
// them, the A2A traffic record (a2a_exchanges) and the MCP calls. It shows every caller's tasks: in the profile every
// client is the one anonymous caller and the gate is on loopback. It never reads a push configuration's URL or token
// or a work item's payload: no file of this package names either table (TestNeverReadsSecrets, spec §13.5).
//
// Prompting (start a task, reply, cancel, continue) is a real A2A request to the gate's own public listener
// (prompt.go), so it passes the edge, the version and extension checks, Admission and the traffic record as any
// client's would. The console acts only on the tasks it started (console_tasks): it watches every other one.
//
// Browser hardening (hardening.go): the edge answers 421 to a Host that is not loopback; the API sends no CORS
// header, refuses a request whose Origin is another site's (403) and a state-changing one that is not
// application/json (403); the page carries a CSP that runs only its own scripts.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/config"
	"kontra.sh/kontra/internal/edge"
	"kontra.sh/kontra/internal/store"
	"kontra.sh/kontra/web"
)

// Deps are the console's collaborators.
type Deps struct {
	Config *config.Config
	DB     *pgxpool.Pool
	Log    *store.Log
	Waker  store.TaskWaker // pg.Listener: wakes an events stream on NOTIFY kontra_task_events
	Blobs  blob.Store      // nil without a bucket: no part is stored apart
	// Dropped is the traffic record's drop count since the replica started (traffic.Recorder.Dropped).
	Dropped func() int64
	// HTTP dials the gate's own public listener for prompting; nil uses a client with no proxy and no redirects.
	HTTP   *http.Client
	Logger *slog.Logger
}

// Console is the page and the API.
type Console struct {
	d   Deps
	api *http.ServeMux
}

// New returns the console.
func New(d Deps) *Console {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Dropped == nil {
		d.Dropped = func() int64 { return 0 }
	}
	if d.HTTP == nil {
		d.HTTP = &http.Client{
			Transport: &http.Transport{Proxy: nil, MaxIdleConns: 16, IdleConnTimeout: 90 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 90 * time.Second,
		}
	}
	c := &Console{d: d, api: http.NewServeMux()}
	for pattern, h := range map[string]http.HandlerFunc{
		"GET /v1/info":                                       c.info,
		"GET /v1/tasks":                                      c.tasks,
		"POST /v1/tasks":                                     c.startTask,
		"GET /v1/tasks/{task_id}":                            c.task,
		"GET /v1/tasks/{task_id}/events":                     c.taskEvents,
		"GET /v1/tasks/{task_id}/files/{ref}":                c.taskFile,
		"POST /v1/tasks/{task_id}/messages":                  c.taskMessage,
		"POST /v1/tasks/{task_id}/cancel":                    c.cancelTask,
		"POST /v1/tasks/{task_id}/continue":                  c.continueTask,
		"GET /v1/conversations":                              c.conversations,
		"GET /v1/conversations/{service}/{caller}/{context}": c.conversation,
		"GET /v1/services":                                   c.services,
		"GET /v1/services/{name}":                            c.service,
		"GET /v1/services/{name}/form":                       c.startForm,
		"GET /v1/tunnels":                                    c.tunnels,
		"GET /v1/exchanges":                                  c.exchanges,
		"GET /v1/calls":                                      c.calls,
	} {
		c.api.HandleFunc(pattern, h)
	}
	return c
}

// Paths are the console API's route patterns, as Register mounts them (the operations of web/static/js/routes.js).
var Paths = []string{
	"GET /v1/info", "GET /v1/tasks", "POST /v1/tasks", "GET /v1/tasks/{task_id}", "GET /v1/tasks/{task_id}/events",
	"GET /v1/tasks/{task_id}/files/{ref}", "POST /v1/tasks/{task_id}/messages", "POST /v1/tasks/{task_id}/cancel",
	"POST /v1/tasks/{task_id}/continue", "GET /v1/conversations", "GET /v1/conversations/{service}/{caller}/{context}",
	"GET /v1/services", "GET /v1/services/{name}", "GET /v1/services/{name}/form", "GET /v1/tunnels",
	"GET /v1/exchanges", "GET /v1/calls",
}

// pagePaths are the page and its files (web.FS): / and the paths under it the page loads.
var pagePaths = []string{"GET /{$}", "GET /console.css", "GET /favicon.svg", "GET /js/", "GET /vendor/"}

// Register mounts the page and the API on the gate's router. Anything else stays the closed router's empty 404.
func (c *Console) Register(rt *edge.Router) {
	page := pageHeaders(web.Handler())
	for _, p := range pagePaths {
		rt.Handle(p, page)
	}
	api := c.guard(c.api)
	for _, p := range Paths {
		rt.Handle(p, api)
	}
}

// ---------------------------------------------------------------- answers

// apiError is the console API's error body, as Kontra Cloud's: {"error": {"code", "message"}}.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func fail(status int, code, message string) error { return &apiError{status, code, message} }

var errNotFound = fail(http.StatusNotFound, "task_not_found", "no such task: it does not exist, or retention deleted it")

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		status, b = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"the answer could not be written"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// answer writes v, or err as the API's error body (an error of the gate's own is logged and answered 500).
func (c *Console) answer(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		if r.Context().Err() == nil {
			c.d.Logger.Warn("console: request failed", "path", r.URL.Path, "error", err)
		}
		ae = &apiError{http.StatusInternalServerError, "internal", "the gate could not answer: see its log"}
	}
	writeJSON(w, ae.status, map[string]any{"error": map[string]string{"code": ae.code, "message": ae.message}})
}

// ---------------------------------------------------------------- GET /v1/info

// Features the console API offers (GET /v1/info): the frontend hides any other (sign-in, money, sealing).
var Features = []string{"tasks", "conversations", "thread", "prompt", "services", "tunnels", "traffic", "calls", "a2ui", "ap2"}

func (c *Console) info(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := c.d.DB.Query(ctx, `select s.name, s.kind, s.retention_s from kontra.services s
		where s.disabled_at is null order by s.name`)
	if err != nil {
		c.answer(w, r, nil, err)
		return
	}
	services := map[string]int64{}
	for rows.Next() {
		var (
			name, kind string
			ret        int64
		)
		if err := rows.Scan(&name, &kind, &ret); err != nil {
			rows.Close()
			c.answer(w, r, nil, err)
			return
		}
		services[name] = ret
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		c.answer(w, r, nil, err)
		return
	}
	cfg := c.d.Config
	c.answer(w, r, map[string]any{
		"profile":    string(cfg.Profile),
		"public_url": cfg.PublicURL,
		"features":   Features,
		"retention": map[string]any{
			"traffic":  int64(cfg.Traffic.Retention.D() / time.Second), // 0: kept forever
			"services": services,                                       // seconds; 0: kept forever
		},
		"traffic": map[string]any{"recording": cfg.Traffic.Recording(), "dropped": c.d.Dropped()},
	}, nil)
}

// ctxTimeout bounds one API read.
const ctxTimeout = 10 * time.Second

func readCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), ctxTimeout)
}
