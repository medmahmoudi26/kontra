// Package temporaltls builds the TLS half of a Temporal client's options, for every Go binary in
// this repository that dials one.
//
// WHAT WAS WRONG. Nothing here could connect to a secured Temporal. Six Go call sites — four CLI
// commands, the handler and the Go host — plus the orchestrator's eight and both Python hosts all
// spelled the address and stopped there, so a self-hoster who put Temporal behind mTLS, which is the
// ordinary thing to do with a server holding every Run's history, had no way to point kontra at it.
// That is a gap in the open product and it is closed here rather than anywhere commercial.
//
// WHY THIS MODULE. `cli` already requires `runtime/handler` and imports `casstore`, `claimcheck` and
// `codecserver` from it, so this is the module the Go tools already share. `runtime/go` requires it
// for this package alone, which keeps ONE implementation rather than two agreeing copies — the thing
// a corpus exists to detect and not the thing it exists to permit.
//
// THE CONTRACT IS SHARED WITH TWO OTHER LANGUAGES and lives in shared/conformance/temporal_tls.json,
// which this package's test executes. Sixteen client connections is the whole difficulty: a change reaching
// twelve of them does not fail loudly, it produces a deployment that mostly works and has one
// process talking plaintext to a server that accepts both.
//
//	KONTRA_TEMPORAL_TLS              1|true|yes|on — TLS with the system trust store
//	KONTRA_TEMPORAL_TLS_CA           PEM path: the server's root CA, for a private CA
//	KONTRA_TEMPORAL_TLS_CERT         PEM path: this client's certificate   ┐ both, or neither
//	KONTRA_TEMPORAL_TLS_KEY          PEM path: this client's private key   ┘
//	KONTRA_TEMPORAL_TLS_SERVER_NAME  SNI override, for a proxy in front of the server
//
// PLAINTEXT REMAINS THE DEFAULT. With none of these set this returns a zero ConnectionOptions, which
// is exactly what every call site passed before — a compatibility promise, not a preference.
//
// ANY ONE OF THEM TURNS TLS ON, and the switch is not a master disable. Setting a CA and forgetting
// the switch would otherwise read a certificate, build nothing from it, and connect in the clear: a
// silent downgrade produced by configuration that looks complete.
//
// A MISCONFIGURATION IS A REFUSAL. It never falls back to plaintext — a security setting that
// degrades to off when it cannot be satisfied is the failure `watchdog.sh` shipped for years in this
// repository, and the shape ADR 0039 refuses for cosign. The error names the VARIABLE and the PATH,
// because an operator needs to know which setting pointed where and a path is a filesystem location.
// Key MATERIAL never reaches the message: this reads bytes and hands them to crypto/tls.
package temporaltls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"go.temporal.io/sdk/client"
)

// Vars is every environment variable this package reads. Exported so a test can assert that no
// other Go file consults one — one function, one reading, or there are two policies that agree
// today and drift later.
var Vars = []string{
	"KONTRA_TEMPORAL_TLS",
	"KONTRA_TEMPORAL_TLS_CA",
	"KONTRA_TEMPORAL_TLS_CERT",
	"KONTRA_TEMPORAL_TLS_KEY",
	"KONTRA_TEMPORAL_TLS_SERVER_NAME",
}

// Env is how the corpus driver supplies an environment without mutating the process. Production
// passes nil and gets os.Getenv.
type Env func(string) string

func (e Env) get(key string) string {
	if e == nil {
		return os.Getenv(key)
	}
	return e(key)
}

// truthy accepts 1, true, yes and on, case-insensitively and trimmed. Anything else is off.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Requested reports whether any TLS setting is present. ANY ONE turns TLS on — see the header.
func Requested(env Env) bool {
	if truthy(env.get("KONTRA_TEMPORAL_TLS")) {
		return true
	}
	for _, v := range Vars[1:] {
		if env.get(v) != "" {
			return true
		}
	}
	return false
}

// readPEM reads a certificate file, or refuses.
//
// The variable AND the path are named; the file's CONTENTS never are, and neither does this decrypt
// anything, so no passphrase can reach a message through here.
func readPEM(env Env, variable string) ([]byte, string, error) {
	path := env.get(variable)
	if path == "" {
		return nil, "", fmt.Errorf("%s is empty — set it to a PEM path or unset it entirely", variable)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf(
			"%s=%s could not be read (%v). Temporal TLS is configured, so this is a refusal rather "+
				"than a fall back to an unencrypted connection", variable, path, err)
	}
	return raw, path, nil
}

// ConnectionOptions is the TLS half of client.Options, built from the environment.
//
// It deliberately does NOT resolve the address or the namespace. The CLI reads both from
// `.kontra/config.yaml` as well as the environment (`config.TemporalAddress`), and folding that in
// here would give this package a second, poorer answer to a question something else already answers
// well. Every caller keeps its own address resolution and gains TLS.
func ConnectionOptions(env Env) (client.ConnectionOptions, error) {
	if !Requested(env) {
		return client.ConnectionOptions{}, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if name := env.get("KONTRA_TEMPORAL_TLS_SERVER_NAME"); name != "" {
		cfg.ServerName = name
	}

	if env.get("KONTRA_TEMPORAL_TLS_CA") != "" {
		raw, path, err := readPEM(env, "KONTRA_TEMPORAL_TLS_CA")
		if err != nil {
			return client.ConnectionOptions{}, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(raw) {
			// A file that exists and holds no certificate is the same class of failure as one that
			// is missing: proceeding would silently fall back to the system trust store, which is
			// not what an operator who set a private CA asked for.
			return client.ConnectionOptions{}, fmt.Errorf(
				"KONTRA_TEMPORAL_TLS_CA=%s contains no PEM certificate — refusing rather than "+
					"falling back to the system trust store", path)
		}
		cfg.RootCAs = pool
	}

	// BOTH OR NEITHER. A certificate without a key is not a partial configuration that could be
	// completed at dial time — it is mTLS that will not authenticate, and the server's rejection
	// arrives as a handshake failure naming neither variable.
	hasCert := env.get("KONTRA_TEMPORAL_TLS_CERT") != ""
	hasKey := env.get("KONTRA_TEMPORAL_TLS_KEY") != ""
	if hasCert != hasKey {
		missing := "KONTRA_TEMPORAL_TLS_KEY"
		if !hasCert {
			missing = "KONTRA_TEMPORAL_TLS_CERT"
		}
		return client.ConnectionOptions{}, fmt.Errorf(
			"KONTRA_TEMPORAL_TLS_CERT and KONTRA_TEMPORAL_TLS_KEY must be set together — %s is "+
				"missing. A client certificate without its key cannot authenticate, and the server "+
				"would refuse the handshake without naming either", missing)
	}
	if hasCert && hasKey {
		crt, crtPath, err := readPEM(env, "KONTRA_TEMPORAL_TLS_CERT")
		if err != nil {
			return client.ConnectionOptions{}, err
		}
		key, _, err := readPEM(env, "KONTRA_TEMPORAL_TLS_KEY")
		if err != nil {
			return client.ConnectionOptions{}, err
		}
		pair, err := tls.X509KeyPair(crt, key)
		if err != nil {
			// NAMES THE CERTIFICATE'S PATH AND NOT THE KEY'S, and carries no bytes of either:
			// `tls.X509KeyPair`'s own error text is about structure ("failed to find any PEM data",
			// "private key does not match public key") and never quotes the material.
			return client.ConnectionOptions{}, fmt.Errorf(
				"KONTRA_TEMPORAL_TLS_CERT=%s and its key are not a usable pair (%v)", crtPath, err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}

	return client.ConnectionOptions{TLS: cfg}, nil
}

// Describe is one line for a boot log: which Temporal, encrypted or not, with a client certificate
// or not. It names the MODE and never the material, which is the whole of what is safe to print and
// exactly what an operator needs when a worker is not polling.
func Describe(address string, opts client.ConnectionOptions) string {
	if opts.TLS == nil {
		return fmt.Sprintf("%s (plaintext)", address)
	}
	parts := []string{"TLS"}
	if opts.TLS.RootCAs != nil {
		parts = append(parts, "private CA")
	}
	if len(opts.TLS.Certificates) > 0 {
		parts = append(parts, "client certificate")
	}
	if opts.TLS.ServerName != "" {
		parts = append(parts, "SNI "+opts.TLS.ServerName)
	}
	return fmt.Sprintf("%s (%s)", address, strings.Join(parts, ", "))
}
