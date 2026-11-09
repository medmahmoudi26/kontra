package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// M4 (WP-07): an operator's Admission server on loopback (spec/relay.md §13.1; D275, D284), which the replicas reach
// as hooks.admission (http:// on a loopback host, operations [task]). It allows every request but those to the
// services it denies (Deny); a test can make it answer a raw decision instead (Set), make each answer wait (SetDelay),
// or stop it (Down: the listener closed, so the gate's request fails) and serve it again (Up). It records each request
// (Requests).

// AdmissionSecretEnv is the variable the replicas read hooks.admission's secret from (secret_env).
const AdmissionSecretEnv = "KONTRA_E2E_ADMISSION_SECRET"

// AdmissionRequest is one request the double received: the operation, the service, the pre-minted task id, the
// principal and the activated extensions, as hooks.json's admitRequest names them, and its Authorization header.
type AdmissionRequest struct {
	Operation  string
	Service    string // the service's name
	Tenant     string // the service's tenant slug
	TaskID     string
	Principal  string
	Via        string
	Extensions []string
	Auth       string
}

// admissionDenial is the decision the double answers for a service it denies.
const admissionDenial = `{"outcome":"deny","refusal":{"code":-32001,"message":"denied by the e2e Admission double"}}`

// Admission is the double.
type Admission struct {
	Secret string // the shared secret the replicas send (Authorization: Bearer)

	mu       sync.Mutex
	deny     map[string]bool // the service names it denies
	raw      string          // a decision answered as is, for every request, when not ""
	delay    time.Duration   // how long each answer waits (a slow operator's server)
	requests []AdmissionRequest
	l        net.Listener
	srv      *http.Server
	addr     string
}

// StartAdmission serves the double on 127.0.0.1 until Close; it allows every request.
func StartAdmission() (*Admission, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	a := &Admission{Secret: randomHex(24), deny: map[string]bool{}, l: l, addr: l.Addr().String()}
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
		Extensions []string `json:"extensions"`
	}
	_ = json.Unmarshal(b, &req)
	a.mu.Lock()
	a.requests = append(a.requests, AdmissionRequest{Operation: req.Operation, Service: req.Service.Name, Tenant: req.Service.Tenant,
		TaskID: req.TaskID, Principal: req.Principal.ID, Via: req.Principal.Via, Extensions: req.Extensions, Auth: r.Header.Get("Authorization")})
	raw, deny, delay := a.raw, a.deny[req.Service.Name], a.delay
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case raw != "":
		_, _ = io.WriteString(w, raw)
	case deny:
		_, _ = io.WriteString(w, admissionDenial)
	default:
		_, _ = io.WriteString(w, `{"outcome":"allow"}`)
	}
}

// Set makes the double answer raw, as is, to every request ("" answers by service again: deny or allow).
func (a *Admission) Set(raw string) {
	a.mu.Lock()
	a.raw = raw
	a.mu.Unlock()
}

// Deny makes the double deny the services named, and allow every other (none: allow all).
func (a *Admission) Deny(services ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deny = map[string]bool{}
	for _, s := range services {
		a.deny[s] = true
	}
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
