package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	fhttp "github.com/sw33tLie/http"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// TestH2RawNewlineInHeaderValue answers the one empirical question that decides
// whether a whole probe family is reachable from Go.
//
// THE QUESTION. HTTP/2 -> HTTP/1.1 downgrade is the highest-value header-injection
// surface: the frontend MUST re-serialise an h2 header value into an HTTP/1.1 header
// line, and if it does not validate, a bare LF in that value splits the backend
// request. Reaching it requires a client that will actually put an LF in an h2 header
// value on the wire.
//
// WHY IT MIGHT WORK HERE. The fork patches ValidHeaderFieldValue to return true, and
// that function is what internal/httpcommon.validateHeaders and h2_bundle.go both
// call. Critically, headerNewlineToSpace - the sanitiser that defeats the HTTP/1.1
// Header map path - appears ONLY in header.go writeSubset, which the h2 encoder never
// touches.
//
// The server side decodes the HEADERS frame with a bare hpack.Decoder rather than
// Framer.ReadMetaHeaders, because ReadMetaHeaders runs UNPATCHED x/net validation and
// would reject the very value being measured, destroying the evidence.
func TestH2RawNewlineInHeaderValue(t *testing.T) {
	const probeHeader = "x-probe"
	const probeValue = "canary\r\nx-injected: yes"

	got := make(chan []hpack.HeaderField, 1)
	ln := h2Listener(t, got)

	tr := &fhttp.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}},
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := fhttp.NewRequestWithContext(ctx, "GET", "https://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Probe", probeValue)

	// The server never answers, so an error here is expected and uninteresting. What
	// matters is what reached the wire before it.
	resp, rtErr := tr.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	select {
	case fields := <-got:
		var found bool
		for _, f := range fields {
			if f.Name == probeHeader {
				found = true
				t.Logf("wire value: %q", f.Value)
				switch {
				case strings.Contains(f.Value, "\n"):
					t.Logf("VERDICT: raw LF SURVIVES into the HTTP/2 header block. " +
						"The h2 -> h1 downgrade probe family is reachable with this fork.")
				default:
					t.Fatalf("VERDICT: LF was stripped or flattened (%q) - the h2 path "+
						"sanitises after all; this family needs a hand-driven "+
						"http2.Framer + hpack.Encoder instead", f.Value)
				}
			}
		}
		if !found {
			var names []string
			for _, f := range fields {
				names = append(names, f.Name)
			}
			t.Fatalf("probe header never reached the wire; frame carried %v (roundtrip err: %v)", names, rtErr)
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("no HEADERS frame observed - the client refused to send the value "+
			"(roundtrip err: %v)", rtErr)
	}
}

// h2Listener starts a TLS listener negotiating h2 that decodes the first HEADERS
// frame it sees and publishes the decoded fields.
func h2Listener(t *testing.T, out chan<- []hpack.HeaderField) net.Listener {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{selfSigned(t)},
		NextProtos:   []string{"h2"},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(8 * time.Second))

		if _, err := io.ReadFull(conn, make([]byte, len(http2.ClientPreface))); err != nil {
			return
		}
		fr := http2.NewFramer(conn, conn)
		_ = fr.WriteSettings()

		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			switch v := f.(type) {
			case *http2.SettingsFrame:
				if !v.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.HeadersFrame:
				fields, err := hpack.NewDecoder(4096, nil).DecodeFull(v.HeaderBlockFragment())
				if err != nil {
					return
				}
				out <- fields
				return
			}
		}
	}()
	return ln
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
