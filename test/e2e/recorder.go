package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync"
)

// Recorder is a recording reverse proxy on 127.0.0.1 between a hosting node and a stock upstream (M3 plan, e2e
// harness): the node serves its URL, and the test reads every request that reached the upstream, for "no upstream
// traffic" assertions. It forwards bytes unchanged and streams answers (event streams) as they come.
type Recorder struct {
	URL string // http://127.0.0.1:<port><the target's path>

	l   net.Listener
	srv *http.Server

	mu   sync.Mutex
	reqs []RecordedRequest
}

// RecordedRequest is one request the upstream received.
type RecordedRequest struct {
	HTTPMethod string
	Header     http.Header
	Body       []byte
	Method     string // the JSON-RPC method, "" for a response, a GET or a DELETE
	Tool       string // params.name of a tools/call
	Task       bool   // a tools/call with params.task
}

// StartRecorder starts a proxy to target (an endpoint URL; its path is kept in URL).
func StartRecorder(target string) (*Recorder, error) {
	tu, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &Recorder{l: l}
	base := &url.URL{Scheme: tu.Scheme, Host: tu.Host}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = tu.Host
		},
		FlushInterval: -1,
		Transport:     &http.Transport{Proxy: nil},
	}
	r.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec := RecordedRequest{HTTPMethod: req.Method, Header: req.Header.Clone(), Body: body}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Name string          `json:"name"`
				Task json.RawMessage `json:"task"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &msg) == nil {
			rec.Method, rec.Tool, rec.Task = msg.Method, msg.Params.Name, len(msg.Params.Task) > 0
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, rec)
		r.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		proxy.ServeHTTP(w, req)
	})}
	go func() { _ = r.srv.Serve(l) }()
	r.URL = "http://" + l.Addr().String() + tu.EscapedPath()
	return r, nil
}

// Requests returns every request so far.
func (r *Recorder) Requests() []RecordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

// Len is the number of requests so far.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// Count is the number of requests of the JSON-RPC method.
func (r *Recorder) Count(method string) int {
	n := 0
	for _, q := range r.Requests() {
		if q.Method == method {
			n++
		}
	}
	return n
}

// Calls is the number of tools/call requests for tool ("" counts them all).
func (r *Recorder) Calls(tool string) int {
	n := 0
	for _, q := range r.Requests() {
		if q.Method == "tools/call" && (tool == "" || q.Tool == tool) {
			n++
		}
	}
	return n
}

// Close stops the proxy.
func (r *Recorder) Close() {
	_ = r.srv.Close()
	_ = r.l.Close()
}
