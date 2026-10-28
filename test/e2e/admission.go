package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.temporal.io/sdk/client"

	"kontra.sh/kontra/internal/lifecycle"
)

// M4 (WP-07): an operator's Admission server on loopback (spec/relay.md §13.1, §13.2; DM4-8, DM4-17), which the
// replicas reach as hooks.admission (http:// on a loopback host, operations [task]). For the services it quotes it
// answers a quote whose approveUrl names the pre-minted taskId the gate sent (Cloud's thread URL
// https://app.kontra.sh/#/t/<taskId> by default, D188; a team gate's own page with URLFor), expiring ExpiresIn from
// the request; every other service is allowed. A test can make it answer a raw decision, or stop it (Down: the
// listener closed, so the gate's request fails). Approvals are the operator's kontra.approval Update, sent through
// the harness's Temporal client (Approve), as a control plane sends it after a person accepts.

// AdmissionSecretEnv is the variable the replicas read hooks.admission's secret from (secret_env).
const AdmissionSecretEnv = "KONTRA_E2E_ADMISSION_SECRET"

// AdmissionRequest is one request the double received: the operation, the service, the pre-minted task id and the
// principal, as hooks.json's admitRequest names them, and its Authorization header.
type AdmissionRequest struct {
	Operation string
	Service   string // the service's name
	Tenant    string // the service's tenant slug
	TaskID    string
	Principal string
	Via       string
	Auth      string
}

// Admission is the double.
type Admission struct {
	Secret string // the shared secret the replicas send (Authorization: Bearer)

	mu        sync.Mutex
	quotes    map[string]bool            // the service names it quotes
	urlFor    func(taskID string) string // the quote's approveUrl
	expiresIn time.Duration
	raw       string        // a decision answered as is, for every request, when not ""
	delay     time.Duration // how long each answer waits (a slow operator's server)
	requests  []AdmissionRequest
	l         net.Listener
	srv       *http.Server
	addr      string
}

// StartAdmission serves the double on 127.0.0.1 until Close; it quotes the services named in quotes.
func StartAdmission(quotes ...string) (*Admission, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	a := &Admission{Secret: randomHex(24), quotes: map[string]bool{}, expiresIn: 10 * time.Minute, l: l, addr: l.Addr().String(),
		urlFor: func(id string) string { return "https://app.kontra.sh/#/t/" + id }}
	for _, q := range quotes {
		a.quotes[q] = true
	}
	srv := &http.Server{Handler: http.HandlerFunc(a.serve), ReadHeaderTimeout: 5 * time.Second}
	a.srv = srv
	go func() { _ = srv.Serve(l) }() // srv, not a.srv: Down may clear a.srv first
	return a, nil
}

// URL is the hook's URL (hooks.admission.url).
func (a *Admission) URL() string { return "http://" + a.addr + "/admit" }

func (a *Admission) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		Operation string `json:"operation"`
		TaskID    string `json:"taskId"`
		Service   struct {
			Tenant string `json:"tenant"`
			Name   string `json:"name"`
		} `json:"service"`
		Principal struct {
			ID  string `json:"id"`
			Via string `json:"via"`
		} `json:"principal"`
	}
	_ = json.Unmarshal(b, &req)
	a.mu.Lock()
	a.requests = append(a.requests, AdmissionRequest{Operation: req.Operation, Service: req.Service.Name, Tenant: req.Service.Tenant,
		TaskID: req.TaskID, Principal: req.Principal.ID, Via: req.Principal.Via, Auth: r.Header.Get("Authorization")})
	raw, quote, urlFor, in, delay := a.raw, a.quotes[req.Service.Name], a.urlFor, a.expiresIn, a.delay
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case raw != "":
		_, _ = io.WriteString(w, raw)
	case quote && req.Operation == "task" && req.TaskID != "":
		fmt.Fprintf(w, `{"outcome":"quote","quote":{"approveUrl":%q,"expiresAt":%q}}`, urlFor(req.TaskID),
			time.Now().Add(in).UTC().Format(time.RFC3339))
	default:
		_, _ = io.WriteString(w, `{"outcome":"allow"}`)
	}
}

// Set changes how the double answers: urlFor (nil keeps it) builds approveUrl from the task id, in is how long a
// quote lasts (0 keeps it), raw is a decision answered as is to every request ("" answers by service again).
func (a *Admission) Set(urlFor func(taskID string) string, in time.Duration, raw string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if urlFor != nil {
		a.urlFor = urlFor
	}
	if in > 0 {
		a.expiresIn = in
	}
	a.raw = raw
}

// SetDelay makes each answer wait d (0: none).
func (a *Admission) SetDelay(d time.Duration) {
	a.mu.Lock()
	a.delay = d
	a.mu.Unlock()
}

// Requests are the requests the double received, in order.
func (a *Admission) Requests() []AdmissionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AdmissionRequest(nil), a.requests...)
}

// Down stops the double: the gate's next request is refused (connection refused), as when the operator's server is
// down. Up serves it again on the same address.
func (a *Admission) Down() {
	a.mu.Lock()
	srv := a.srv
	a.srv = nil
	a.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Up serves the double again on its address after Down.
func (a *Admission) Up() error {
	l, err := net.Listen("tcp", a.addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: http.HandlerFunc(a.serve), ReadHeaderTimeout: 5 * time.Second}
	a.mu.Lock()
	a.srv, a.l = srv, l
	a.mu.Unlock()
	go func() { _ = srv.Serve(l) }()
	return nil
}

// Close stops the double.
func (a *Admission) Close() error {
	a.Down()
	return nil
}

// configYAML is hooks.admission as kontra.yaml writes it.
func (a *Admission) configYAML() string {
	return fmt.Sprintf("hooks:\n  admission: { url: %q, secret_env: %s, timeout: 3s, operations: [task] }\n", a.URL(), AdmissionSecretEnv)
}

// Approve sends the operator's kontra.approval Update to a quoted task's workflow (spec §13.2, D38, D45): accepted
// true holds and starts it, false declines it. It waits until the workflow has completed the Update.
func (h *Harness) Approve(ctx context.Context, taskID string, accepted bool) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u, err := h.TC.UpdateWorkflow(cctx, client.UpdateWorkflowOptions{
		WorkflowID: lifecycle.WorkflowID(a2a.TaskID(taskID)), UpdateName: lifecycle.UpdateApproval,
		Args: []any{lifecycle.ApprovalUpdate{Accepted: accepted}}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return fmt.Errorf("e2e: kontra.approval of %s: %w", taskID, err)
	}
	var res lifecycle.ApprovalResult
	if err := u.Get(cctx, &res); err != nil {
		return fmt.Errorf("e2e: kontra.approval of %s: %w", taskID, err)
	}
	return nil
}
