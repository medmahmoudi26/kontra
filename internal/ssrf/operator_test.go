package ssrf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An operator-configured endpoint (spec §10.3, D284) is dialed without the address check: an https:// server on
// 127.0.0.1, which NewClient refuses, answers; http:// to a loopback host answers too.
func TestOperatorClientSkipsTheAddressCheck(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "tls") }))
	defer tlsSrv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	c := NewOperatorClient(OperatorOptions{TLS: &tls.Config{RootCAs: pool}})
	resp, err := c.Get(tlsSrv.URL)
	if err != nil {
		t.Fatalf("https to 127.0.0.1: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "tls" {
		t.Errorf("body %q", b)
	}
	// The SSRF dialer would refuse the same address (the control).
	if _, err := NewClient(Policy{}).Get(tlsSrv.URL); !errors.Is(err, ErrAddress) {
		t.Errorf("control: NewClient on 127.0.0.1 = %v, want ErrAddress", err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "plain") }))
	defer plain.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(plain.URL, "http://"))
	for _, host := range []string{"127.0.0.1", "localhost"} {
		resp, err := c.Get("http://" + host + ":" + port + "/")
		if err != nil {
			t.Fatalf("http to %s: %v", host, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "plain" {
			t.Errorf("http to %s: body %q", host, b)
		}
	}
}

// http:// off loopback, a scheme other than https and http, userinfo and a bad port are refused before any
// connection.
func TestOperatorURLRules(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want error
	}{
		{"http://cp.example.com/admit", ErrScheme},
		{"http://10.0.0.7:8080/admit", ErrScheme},
		{"http://[::ffff:10.0.0.7]/admit", ErrScheme},
		{"ftp://cp.example.com/", ErrScheme},
		{"https://user:pw@cp.example.com/admit", ErrURL},
		{"https://cp.example.com:0/admit", ErrURL},
		{"https:///admit", ErrURL},
	} {
		u := mustURL(t, tc.url)
		if err := CheckOperatorURL(u); !errors.Is(err, tc.want) {
			t.Errorf("CheckOperatorURL(%s) = %v, want %v", tc.url, err, tc.want)
		}
		_, err := NewOperatorClient(OperatorOptions{}).Get(tc.url)
		if !errors.Is(err, tc.want) {
			t.Errorf("GET %s = %v, want %v", tc.url, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("GET %s: the error shows the userinfo: %v", tc.url, err)
		}
	}
	for _, ok := range []string{"https://cp.example.com/admit", "https://10.0.0.7/admit", "http://localhost:7000/admit",
		"http://127.0.0.2/admit", "http://[::1]:9/admit", "HTTPS://cp.example.com/"} {
		if err := CheckOperatorURL(mustURL(t, ok)); err != nil {
			t.Errorf("CheckOperatorURL(%s) = %v, want nil", ok, err)
		}
	}
}

// The http:// dialer connects to loopback addresses only, whatever a name resolved to: its Control refuses any
// other address before the connection is made.
func TestOperatorLoopbackDialerRefusesOtherAddresses(t *testing.T) {
	for _, addr := range []string{"192.0.2.1:9", "10.0.0.7:80", "[2001:db8::1]:80"} {
		if err := loopbackOnly("tcp", addr, nil); !errors.Is(err, ErrAddress) {
			t.Errorf("loopbackOnly(%s) = %v, want ErrAddress", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:9", "[::1]:9", "127.3.4.5:80"} {
		if err := loopbackOnly("tcp", addr, nil); err != nil {
			t.Errorf("loopbackOnly(%s) = %v, want nil", addr, err)
		}
	}
}

// A 3xx is returned as the response, and its target receives no connection; the environment's proxy is never
// consulted (the transports have none).
func TestOperatorClientNoRedirectNoProxy(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, code)
		}))
		resp, err := NewOperatorClient(OperatorOptions{}).Post(srv.URL, "application/json", strings.NewReader("{}"))
		srv.Close()
		if err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("status %d, want the %d itself", resp.StatusCode, code)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("the redirect target received %d requests", hits.Load())
	}
	ot := NewOperatorClient(OperatorOptions{}).Transport.(operatorTransport)
	for name, rt := range map[string]http.RoundTripper{"https": ot.https, "http": ot.loopback} {
		if tr := rt.(*http.Transport); tr.Proxy != nil {
			t.Errorf("the %s transport has a proxy function", name)
		}
	}
}

// A request that takes longer than the timeout fails; the default timeout is 5 s.
func TestOperatorClientTimeout(t *testing.T) {
	if c := NewOperatorClient(OperatorOptions{}); c.Timeout != OperatorTimeout || OperatorTimeout != 5*time.Second {
		t.Errorf("default timeout %v", c.Timeout)
	}
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	start := time.Now()
	_, err := NewOperatorClient(OperatorOptions{Timeout: 150 * time.Millisecond}).Get(slow.URL)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a slow endpoint: %v after %v", err, time.Since(start))
	}
}

// ReadBody reads up to its bound and refuses one byte more.
func TestReadBody(t *testing.T) {
	b, err := ReadBody(strings.NewReader(strings.Repeat("a", 10)), 10)
	if err != nil || len(b) != 10 {
		t.Errorf("at the bound: %d bytes, %v", len(b), err)
	}
	if _, err := ReadBody(strings.NewReader(strings.Repeat("a", 11)), 10); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("over the bound: %v", err)
	}
	var read atomic.Int64
	r := readerFunc(func(p []byte) (int, error) {
		read.Add(int64(len(p)))
		for i := range p {
			p[i] = 'x'
		}
		return len(p), nil
	})
	if _, err := ReadBody(r, 1<<10); !errors.Is(err, ErrBodyTooLarge) || read.Load() > 1<<10+1 {
		t.Errorf("an endless body: %v after %d bytes", err, read.Load())
	}
	if MaxOperatorBody != 256<<10 {
		t.Errorf("MaxOperatorBody = %d", MaxOperatorBody)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST.": true, "127.0.0.1": true, "127.9.9.9": true,
		"::1": true, "[::1]": true, "::ffff:127.0.0.1": true, "10.0.0.1": false, "localhost.example": false, "": false,
		"fe80::1%eth0": false} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
