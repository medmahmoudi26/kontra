// The GO ARM of shared/conformance/temporal_tls.json.
//
// Three languages derive one environment contract across eighteen call sites. What the corpus pins
// is the DECISION — TLS on or off, which material is present, and which misconfigurations are
// refusals — because that is what an operator configures and what must not disagree between the
// orchestrator, the hosts and the CLI. A deployment where one process reads the contract differently
// is one process talking plaintext to a server that accepts both, which looks like it works.
package temporaltls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
)

type corpusCase struct {
	Why        string            `json:"why"`
	Env        map[string]string `json:"env"`
	TLS        bool              `json:"tls"`
	CA         bool              `json:"ca"`
	ClientPair bool              `json:"client_pair"`
	ServerName string            `json:"server_name"`
}

type refusalCase struct {
	Why   string            `json:"why"`
	Env   map[string]string `json:"env"`
	Names []string          `json:"names"`
}

type corpusDoc struct {
	Truthy       []string      `json:"truthy"`
	Falsy        []string      `json:"falsy"`
	Cases        []corpusCase  `json:"cases"`
	RefusalCases []refusalCase `json:"refusal_cases"`
}

// Resolved from THIS FILE, never from the caller's directory — a relative `../shared/conformance/…`
// is correct in one package and wrong in the next, and that is a class of bug this repository has
// hit five times (tests/test_conformance_tree.py).
func corpusPath() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "shared", "conformance", "temporal_tls.json")
}

func loadCorpus(t *testing.T) corpusDoc {
	t.Helper()
	raw, err := os.ReadFile(corpusPath())
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc corpusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	// A corpus of nothing passes every assertion below.
	if len(doc.Cases) < 8 || len(doc.RefusalCases) < 3 || len(doc.Truthy) < 4 || len(doc.Falsy) < 4 {
		t.Fatalf("the corpus shrank: cases=%d refusals=%d truthy=%d falsy=%d",
			len(doc.Cases), len(doc.RefusalCases), len(doc.Truthy), len(doc.Falsy))
	}
	// The row that carries the whole point: a CA with the switch explicitly OFF must still be TLS.
	blob := string(raw)
	for _, want := range []string{"OFF DOES NOT OVERRIDE A CA", "SILENT DOWNGRADE"} {
		if !strings.Contains(blob, want) {
			t.Errorf("the corpus no longer carries %q", want)
		}
	}
	return doc
}

// A real, self-signed pair, generated per run. The corpus deliberately carries no key material —
// even a throwaway private key is not a thing to commit — and Go validates the pair at configuration
// time, so arbitrary bytes would not do.
func writePair(t *testing.T) (caPath, crtPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kontra-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}
	derKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key: %v", err)
	}

	write := func(name, blockType string, body []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: body}), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	caPath = write("ca.pem", "CERTIFICATE", der)
	crtPath = write("crt.pem", "CERTIFICATE", der)
	keyPath = write("key.pem", "EC PRIVATE KEY", derKey)
	return caPath, crtPath, keyPath
}

// Substitute the corpus's `@ca` / `@crt` / `@key` placeholders for this run's real paths, and turn
// the map into an Env. A value that is not a placeholder is passed through verbatim — that is how
// the refusal rows carry `/nope/missing-ca.pem`.
func envFor(m map[string]string, ca, crt, key string) Env {
	subs := map[string]string{"@ca": ca, "@crt": crt, "@key": key}
	return func(name string) string {
		v, ok := m[name]
		if !ok {
			return ""
		}
		if real, isPlaceholder := subs[v]; isPlaceholder {
			return real
		}
		return v
	}
}

func TestTheDecisionMatchesTheCorpus(t *testing.T) {
	ca, crt, key := writePair(t)
	for _, c := range loadCorpus(t).Cases {
		t.Run(c.Why, func(t *testing.T) {
			env := envFor(c.Env, ca, crt, key)

			if got := Requested(env); got != c.TLS {
				t.Fatalf("Requested = %v, corpus says %v", got, c.TLS)
			}
			opts, err := ConnectionOptions(env)
			if err != nil {
				t.Fatalf("ConnectionOptions: %v", err)
			}
			if (opts.TLS != nil) != c.TLS {
				t.Fatalf("TLS config present = %v, corpus says %v", opts.TLS != nil, c.TLS)
			}
			if !c.TLS {
				// PLAINTEXT IS THE DEFAULT, and it must be the ZERO value — byte-identical to what
				// every call site passed before this package existed, not merely equivalent.
				// DeepEqual because ConnectionOptions holds a []grpc.DialOption and is not
				// comparable — the assertion is still "the zero value", not "TLS happens to be nil".
				if !reflect.DeepEqual(opts, client.ConnectionOptions{}) {
					t.Errorf("plaintext must return the zero ConnectionOptions, got %+v", opts)
				}
				return
			}
			if (opts.TLS.RootCAs != nil) != c.CA {
				t.Errorf("private CA = %v, corpus says %v", opts.TLS.RootCAs != nil, c.CA)
			}
			if (len(opts.TLS.Certificates) > 0) != c.ClientPair {
				t.Errorf("client pair = %v, corpus says %v", len(opts.TLS.Certificates) > 0, c.ClientPair)
			}
			if opts.TLS.ServerName != c.ServerName {
				t.Errorf("ServerName = %q, corpus says %q", opts.TLS.ServerName, c.ServerName)
			}
		})
	}
}

func TestARefusalNamesWhatIsWrongAndNeverFallsBack(t *testing.T) {
	ca, crt, key := writePair(t)
	for _, c := range loadCorpus(t).RefusalCases {
		t.Run(c.Why, func(t *testing.T) {
			_, err := ConnectionOptions(envFor(c.Env, ca, crt, key))
			if err == nil {
				t.Fatal("a misconfiguration returned no error — this is the silent fall back to " +
					"plaintext the whole package exists to refuse")
			}
			for _, want := range c.Names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %q: %v", want, err)
				}
			}
		})
	}
}

func TestTheSwitchIsReadTheSameWayEverywhere(t *testing.T) {
	doc := loadCorpus(t)
	for _, v := range doc.Truthy {
		if !Requested(envFor(map[string]string{"KONTRA_TEMPORAL_TLS": v}, "", "", "")) {
			t.Errorf("%q should turn TLS on", v)
		}
	}
	for _, v := range doc.Falsy {
		if Requested(envFor(map[string]string{"KONTRA_TEMPORAL_TLS": v}, "", "", "")) {
			t.Errorf("%q should not turn TLS on", v)
		}
	}
}

func TestKeyMaterialNeverReachesAMessage(t *testing.T) {
	// The PATH is named on purpose — an operator needs to know which setting pointed where, and a
	// path is a filesystem location. The BYTES are the secret.
	_, _, key := writePair(t)
	raw, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	env := envFor(map[string]string{
		"KONTRA_TEMPORAL_TLS_CERT": "/nope/missing.pem",
		"KONTRA_TEMPORAL_TLS_KEY":  "@key",
	}, "", "", key)
	if _, err := ConnectionOptions(env); err == nil {
		t.Fatal("expected a refusal for the unreadable certificate")
	} else {
		body := strings.TrimSpace(string(raw))
		for _, line := range strings.Split(body, "\n") {
			if len(line) > 20 && strings.Contains(err.Error(), line) {
				t.Fatalf("the error carries key material: %q", line)
			}
		}
		if !strings.Contains(err.Error(), "KONTRA_TEMPORAL_TLS_CERT") {
			t.Errorf("the error should name the variable that failed: %v", err)
		}
	}
}

func TestDescribeNamesTheModeAndNotTheMaterial(t *testing.T) {
	ca, crt, key := writePair(t)
	plain, err := ConnectionOptions(envFor(map[string]string{}, ca, crt, key))
	if err != nil {
		t.Fatal(err)
	}
	if got := Describe("localhost:7233", plain); got != "localhost:7233 (plaintext)" {
		t.Errorf("plaintext should say so plainly, got %q", got)
	}
	full, err := ConnectionOptions(envFor(map[string]string{
		"KONTRA_TEMPORAL_TLS_CA":          "@ca",
		"KONTRA_TEMPORAL_TLS_CERT":        "@crt",
		"KONTRA_TEMPORAL_TLS_KEY":         "@key",
		"KONTRA_TEMPORAL_TLS_SERVER_NAME": "temporal.internal",
	}, ca, crt, key))
	if err != nil {
		t.Fatal(err)
	}
	want := "t:1 (TLS, private CA, client certificate, SNI temporal.internal)"
	if got := Describe("t:1", full); got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
}
