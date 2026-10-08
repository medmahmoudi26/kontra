package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE FAILURE THESE TESTS PIN. The install generates three zot accounts and the push side sent
// `base64("{}")` — the Engine API's "no credentials", which an anonymous registry accepts and an
// authenticated one answers with 401. Every assertion here is about the SENTENCE that comes out,
// because the thing that was missing was never the HTTP status: it was anyone saying "credential".

func TestThePushUserDefaultsToTheAccountComposeWrites(t *testing.T) {
	t.Setenv(pushUserEnv, "")
	t.Setenv(pushPasswordEnv, "")
	c := pushCredential()
	if c.User != defaultPushUser {
		t.Errorf("user = %q, want the account docker-compose.yml writes (%q)", c.User, defaultPushUser)
	}
	if !c.anonymous() {
		t.Error("no password must read as anonymous, so the refusal below can say so")
	}
	t.Setenv(pushUserEnv, "ci-pusher")
	t.Setenv(pushPasswordEnv, "s3cret")
	if c := pushCredential(); c.User != "ci-pusher" || c.anonymous() {
		t.Errorf("the environment must override both halves, got %+v", registryCredential{User: c.User})
	}
}

// probe serves `/v2/` with one status and records whether a credential arrived.
func probe(t *testing.T, status int) (addr string, seen *string) {
	t.Helper()
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok {
			got = u + ":" + p
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	return strings.TrimPrefix(ts.URL, "http://"), &got
}

func TestAnAnonymousPushAgainstAnAuthenticatedRegistryNamesTheVariable(t *testing.T) {
	reg, _ := probe(t, http.StatusUnauthorized)
	err := registryAdmits(reg, registryCredential{User: defaultPushUser})
	if err == nil {
		t.Fatal("a 401 against an anonymous client must be refused, not ignored")
	}
	// THE VARIABLE, BECAUSE THAT IS THE ACTION. "unauthorized" sends an operator to the registry;
	// the name of the environment variable sends them to the thing they can change.
	for _, want := range []string{pushPasswordEnv, pushUserEnv, reg} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

func TestAWrongPasswordNamesTheUserAndNotThePassword(t *testing.T) {
	reg, seen := probe(t, http.StatusUnauthorized)
	err := registryAdmits(reg, registryCredential{User: "push-actors", Password: "wrong-pw"})
	if err == nil {
		t.Fatal("a 401 against a credential must be refused")
	}
	if !strings.Contains(err.Error(), "push-actors") {
		t.Errorf("the refusal must name the user that was rejected:\n%v", err)
	}
	// THE PASSWORD IS NEVER IN THE MESSAGE. This is the assertion that stops a well-meaning "got %q"
	// from being added to the error above, which is how a secret reaches a CI log.
	if strings.Contains(err.Error(), "wrong-pw") {
		t.Errorf("the refusal leaks the password:\n%v", err)
	}
	if *seen != "push-actors:wrong-pw" {
		t.Errorf("the probe did not send basic auth at all, so this test proved nothing (saw %q)", *seen)
	}
}

func TestAForbiddenNamesTheAccountThatMayPush(t *testing.T) {
	reg, _ := probe(t, http.StatusForbidden)
	err := registryAdmits(reg, registryCredential{User: "pull", Password: "pw"})
	if err == nil || !strings.Contains(err.Error(), defaultPushUser) {
		t.Errorf("403 must say which account may create in the actors namespace, got: %v", err)
	}
}

func TestA200Admits(t *testing.T) {
	reg, seen := probe(t, http.StatusOK)
	if err := registryAdmits(reg, registryCredential{User: "push-actors", Password: "pw"}); err != nil {
		t.Errorf("200 must admit: %v", err)
	}
	if *seen == "" {
		t.Error("the credential was not sent, so a registry with auth would have 401'd here")
	}
}

// AN UNEXPECTED STATUS IS NOT A CREDENTIAL PROBLEM. Plenty of registries answer /v2/ with something
// else and still work; refusing here would turn a working install into a failed deploy, which is the
// opposite of the bug this file fixes.
func TestAnUnexpectedStatusIsNotTreatedAsARefusal(t *testing.T) {
	reg, _ := probe(t, http.StatusNotFound)
	if err := registryAdmits(reg, registryCredential{User: "u", Password: "p"}); err != nil {
		t.Errorf("404 on /v2/ must not be read as a credential refusal: %v", err)
	}
}

func TestTheDockerConfigIsWrittenPrivatelyAndIsWhatPackReads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	const reg = "127.0.0.1:5000"
	if err := writeDockerConfig(dir, reg, registryCredential{User: "push-actors", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// THE MODE IS THE ONLY PROTECTION. `auth` is base64 and not encryption, so a world-readable
	// config.json is a password anybody on the box can read.
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config.json mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(dir); err == nil && di.Mode().Perm() != 0o700 {
		t.Errorf("the directory mode = %v, want 0700", di.Mode().Perm())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Auths map[string]struct{ Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.json is not valid JSON: %v", err)
	}
	// KEYED BY THE REGISTRY THIS DEPLOY PUSHES TO, not by a host derived a second way: docker and
	// pack both look the credential up by the reference's registry component, so a key that is
	// nearly right is a credential that is never sent.
	entry, ok := doc.Auths[reg]
	if !ok {
		t.Fatalf("no entry for %q; config.json has %v", reg, doc.Auths)
	}
	dec, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		t.Fatalf("the auth field is not base64: %v", err)
	}
	if string(dec) != "push-actors:pw" {
		t.Errorf("auth decodes to %q, want user:password", dec)
	}
}

// AN ANONYMOUS CREDENTIAL WRITES NOTHING. `pack` with a DOCKER_CONFIG holding an empty `auths` would
// send an Authorization header for no user, which is a different failure from sending none.
func TestAnAnonymousCredentialWritesNoFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := writeDockerConfig(dir, "127.0.0.1:5000", registryCredential{User: "push-actors"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Error("an anonymous credential must leave no config.json behind")
	}
}

// ── THE BEARER CHALLENGE ─────────────────────────────────────────────────────────────────────────

func TestABearerChallengeIsParsedAndAnythingElseIsNot(t *testing.T) {
	const real = `Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:acme/rt:pull"`
	c, ok := parseBearerChallenge(real)
	if !ok {
		t.Fatalf("the header ghcr actually sends was not recognised: %s", real)
	}
	if c.realm != "https://ghcr.io/token" || c.service != "ghcr.io" || c.scope != "repository:acme/rt:pull" {
		t.Errorf("parsed %+v", c)
	}
	// LOWER CASE, because net/http canonicalises header NAMES and not values, and a registry is
	// free to send `bearer`.
	if _, ok := parseBearerChallenge(`bearer realm="https://x/token"`); !ok {
		t.Error("a lower-case scheme must still parse")
	}
	// A REALM THIS PROCESS WOULD NOT FETCH IS NOT A CHALLENGE IT FOLLOWS. Without this guard the
	// token exchange would try to GET whatever a registry put there.
	for _, bad := range []string{
		`Basic realm="registry"`,
		`Bearer service="ghcr.io"`,
		`Bearer realm="file:///etc/passwd"`,
		"",
	} {
		if _, ok := parseBearerChallenge(bad); ok {
			t.Errorf("%q must not be followed as a bearer challenge", bad)
		}
	}
}

// THE WHOLE FLOW, OFFLINE: 401 with a challenge, a token from the realm, then the digest.
//
// This is the shape ghcr really answers with for a PUBLIC repository — verified live against
// ghcr.io/medmahmoudi26/kontra-runtimes/python:1 — and the shape this CLI could not read, so a
// runtime a human can open in a browser reported as "not in the registry".
func TestAPublicRegistryThatChallengesIsStillRead(t *testing.T) {
	const digest = "sha256:712b8d2e71e383b1c554464e7d45e5bee3a7f1acce4b69ab47194e396c03399e"
	var issued, authedReads int

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			// THE SCOPE IS ECHOED BACK AS A CHECK: a client that dropped it would get a token for
			// nothing, which is how this silently half-works.
			if r.URL.Query().Get("scope") != "repository:acme/rt:pull" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			issued++
			_, _ = w.Write([]byte(`{"token":"t0ken"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer t0ken" {
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="test",scope="repository:acme/rt:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authedReads++
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	got, err := registryManifestDigest(strings.TrimPrefix(srv.URL, "http://"), "acme/rt", "1")
	if err != nil {
		t.Fatalf("a challenged read must still resolve: %v", err)
	}
	if got != digest {
		t.Errorf("digest = %q, want %q", got, digest)
	}
	if issued == 0 {
		t.Error("no token was fetched, so this passed without exercising the challenge at all")
	}
	if authedReads == 0 {
		t.Error("the retry never carried the token")
	}
}

// A 404 IS STILL A 404. The challenge path must not turn "this tag does not exist" into a token
// problem, because `resolveRuntime` keys its "available runtimes" message on errNotInRegistry.
func TestAMissingTagIsNotMistakenForAChallenge(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	if _, err := registryManifestDigest(strings.TrimPrefix(ts.URL, "http://"), "acme/rt", "9"); err != errNotInRegistry {
		t.Errorf("want errNotInRegistry, got %v", err)
	}
}

// THE ENGINE API HEADER, WHICH IS THE LINE THAT WAS WRONG. `base64("{}")` is the canonical "no
// credentials" and it is what was sent for as long as the install has had zot accounts.
func TestTheEngineAuthHeaderCarriesTheCredential(t *testing.T) {
	const reg = "127.0.0.1:5000"
	decode := func(h string) map[string]string {
		t.Helper()
		raw, err := base64.URLEncoding.DecodeString(h)
		if err != nil {
			t.Fatalf("the header is not URL-safe base64, which is what the Engine API reads: %v", err)
		}
		var doc map[string]string
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("the header is not an AuthConfig document: %v", err)
		}
		return doc
	}

	got := decode(registryAuthHeader(reg, registryCredential{User: "push-actors", Password: "pw"}))
	if got["username"] != "push-actors" || got["password"] != "pw" {
		t.Errorf("the credential did not reach the header: %v", got)
	}
	if got["serveraddress"] != reg {
		t.Errorf("serveraddress = %q, want the registry being pushed to", got["serveraddress"])
	}

	// ANONYMOUS STILL SENDS A HEADER, because the Engine requires one for every push — it just
	// carries no user. This is the shape that works against a registry with no accounts.
	anon := decode(registryAuthHeader(reg, registryCredential{User: "push-actors"}))
	if _, ok := anon["password"]; ok {
		t.Errorf("an anonymous header must carry no password: %v", anon)
	}
	if anon["serveraddress"] != reg {
		t.Errorf("even anonymously the header names the registry, got %v", anon)
	}
}
