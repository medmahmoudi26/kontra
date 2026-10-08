package main

// registryauth.go — the credential `kontra deploy` pushes an actor image with.
//
// THE INSTALL HAS HAD USERS SINCE PHASE 8 AND NOTHING SENT ONE. `docker-compose.yml` generates three
// zot accounts at first boot — `push-actors`, `push-runtimes` and `pull` — with a per-repository
// policy: only `push-actors` may create in the actors namespace. The push side was
// `base64("{}")`, the Engine API's canonical "no credentials", which an ANONYMOUS registry accepts
// and an authenticated one answers with 401. So an install that had enabled auth could not deploy,
// and the way it said so was a push failure three layers down with no mention of a credential.
//
// A PREFLIGHT, NOT AN ERROR CLASSIFIER. The 401 is detected by asking `GET /v2/` with the credential
// BEFORE the build, rather than by pattern-matching whatever the builder prints after minutes of
// work. Two reasons: a buildpack build is expensive to spend on a push that cannot succeed, and
// parsing another tool's output for a cause is how a diagnostic ends up reporting what it assumed
// instead of what it observed.
//
// THE PASSWORD IS NEVER PRINTED, and the error messages below are written so that they cannot start:
// they name the USER, the REGISTRY and the VARIABLE, which is everything an operator needs and
// nothing a log should not hold.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const (
	// pushUserEnv / pushPasswordEnv are the two docker-compose.yml hands the `cli` service. The
	// password variable is the SAME one `registry-config` reads to write the htpasswd, so there is
	// one secret and not a copy of it.
	pushUserEnv     = "KONTRA_REGISTRY_PUSH_ACTORS_USER"
	pushPasswordEnv = "KONTRA_REGISTRY_PUSH_ACTORS_PASSWORD"
	// defaultPushUser is the account name docker-compose.yml writes. It is a default rather than a
	// constant because a registry that is not this install's has its own accounts.
	defaultPushUser = "push-actors"

	runtimesUserEnv     = "KONTRA_REGISTRY_PUSH_RUNTIMES_USER"
	runtimesPasswordEnv = "KONTRA_REGISTRY_PUSH_RUNTIMES_PASSWORD"
	defaultRuntimesUser = "push-runtimes"

	pullUserEnv     = "KONTRA_REGISTRY_PULL_USER"
	pullPasswordEnv = "KONTRA_REGISTRY_PULL_PASSWORD"
	defaultPullUser = "pull"
)

// registryRole is one of the install's push accounts: the namespace it may create in, the account
// docker-compose.yml writes, and the two variables that carry it.
//
// THERE ARE TWO BECAUSE THE SEPARATION IS THE POINT. `push-actors` may write `actors/**` and
// `push-runtimes` may write `kontra-runtimes/**`, so an actor build cannot replace the base every
// other actor is layered on. A refusal that named the wrong variable would send an operator to set
// a password that was never consulted, so the role travels with the credential.
type registryRole struct {
	Namespace   string
	DefaultUser string
	UserVar     string
	PasswordVar string
}

var (
	actorsRole = registryRole{
		Namespace: "actors", DefaultUser: defaultPushUser,
		UserVar: pushUserEnv, PasswordVar: pushPasswordEnv,
	}
	runtimesRole = registryRole{
		Namespace: "runtimes", DefaultUser: defaultRuntimesUser,
		UserVar: runtimesUserEnv, PasswordVar: runtimesPasswordEnv,
	}
	pullRole = registryRole{
		Namespace: "pull", DefaultUser: defaultPullUser,
		UserVar: pullUserEnv, PasswordVar: pullPasswordEnv,
	}
)

// ── READING AN AUTHENTICATED REGISTRY, WHICH IS NOT THE SAME PROBLEM AS WRITING ONE ─────────────
//
// WITH AUTH ON, THIS INSTALL'S ZOT PERMITS NO ANONYMOUS READ. The rendered `accessControl` gives
// every repository tree a `"defaultPolicy": []` and names the three accounts explicitly, so a
// manifest HEAD with no credential is 401 — not only a push. Every read on this path was
// unauthenticated, which under auth turns "does this tag exist" into an error and, worse, turns
// `versionDeployed`'s version-immutability check into "no".
//
// THE CREDENTIAL IS SENT ONLY TO THIS INSTALL'S REGISTRY. A read reaches ghcr as well, and sending
// an install's password to somebody else's registry on the strength of a 401 would be a credential
// leak — ghcr's 401 is answered by the Bearer dance below, with no credential at all.
//
// THE LEAST PRIVILEGED ONE THAT IS SET. `pull` may read and nothing else; `push-actors` and
// `push-runtimes` also carry read over `**`, and are what a service that only has a push credential
// falls back to.

// readCredential is the credential for reading `reg`, or anonymous when `reg` is not this install's
// registry or no account is configured.
func readCredential(reg string) registryCredential {
	if !isInstallRegistry(reg) {
		return registryCredential{}
	}
	for _, r := range []registryRole{pullRole, actorsRole, runtimesRole} {
		if c := credentialFor(r); !c.anonymous() {
			return c
		}
	}
	return registryCredential{}
}

// isInstallRegistry reports whether `reg` names the registry this install configures — by any of
// the spellings `registryProbeBases` already knows reach it, because `127.0.0.1:5000`,
// `registry:5000` and `host.docker.internal:5000` are one registry seen from three places.
func isInstallRegistry(reg string) bool {
	host, _ := registryHost(reg)
	for _, base := range registryProbeBases(registryAddress("")) {
		if h, _ := registryHost(base); h == host {
			return true
		}
	}
	return false
}

// registryCredential is a username and password for one registry. Empty means anonymous.
type registryCredential struct {
	User     string
	Password string
	Role     registryRole
}

func (c registryCredential) anonymous() bool { return c.Password == "" }

// role is what the refusals name. A credential built without one is the ACTORS credential: that is
// the push every install makes, and it is what a caller constructing this struct by hand means.
func (c registryCredential) role() registryRole {
	if c.Role.PasswordVar == "" {
		return actorsRole
	}
	return c.Role
}

// pushCredential reads the credential for the actors namespace out of the environment.
func pushCredential() registryCredential { return credentialFor(actorsRole) }

// runtimesPushCredential reads the credential `kontra runtime import` writes the mirrored runtimes
// with. A separate account, not a convenience alias — see registryRole.
func runtimesPushCredential() registryCredential { return credentialFor(runtimesRole) }

func credentialFor(r registryRole) registryCredential {
	user := strings.TrimSpace(os.Getenv(r.UserVar))
	if user == "" {
		user = r.DefaultUser
	}
	return registryCredential{User: user, Password: os.Getenv(r.PasswordVar), Role: r}
}

// registryAdmits asks whether this credential may talk to this registry at all.
//
// `GET /v2/` IS THE WHOLE PROTOCOL FOR THIS QUESTION: the distribution spec makes it the endpoint a
// client probes, and a registry with auth enabled answers 401 with a `WWW-Authenticate` header.
// 200 means admitted; anything else is reported as itself rather than guessed at.
func registryAdmits(reg string, c registryCredential) error {
	var lastErr error
	for _, base := range registryProbeBases(reg) {
		req, err := http.NewRequest(http.MethodGet, base+"/v2/", nil)
		if err != nil {
			return err
		}
		if !c.anonymous() {
			req.SetBasicAuth(c.User, c.Password)
		}
		resp, err := registryHTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		role := c.role()
		switch {
		case resp.StatusCode == http.StatusOK:
			return nil
		case resp.StatusCode == http.StatusUnauthorized && c.anonymous():
			return fmt.Errorf("the registry at %s requires a credential and this process has none.\n"+
				"  Set %s (and %s if the account is not %q). On the compose install those are the\n"+
				"  same values `registry-config` writes the zot htpasswd from, so the `cli` service\n"+
				"  has to be given them too.", reg, role.PasswordVar, role.UserVar, role.DefaultUser)
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("the registry at %s refused the credential for user %q.\n"+
				"  The password comes from %s. On the compose install it must match the one\n"+
				"  `registry-config` wrote the htpasswd from — a recreated registry volume with an\n"+
				"  old password set, or the reverse, is the usual cause.", reg, c.User, role.PasswordVar)
		case resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("the registry at %s authenticated user %q and will not let it push.\n"+
				"  Only %q may create in the %s namespace (docker-compose.yml's accessControl);\n"+
				"  %s names the account.", reg, c.User, role.DefaultUser, role.Namespace, role.UserVar)
		default:
			// NOT AN ERROR, because plenty of registries answer /v2/ with something else and still
			// work. Reported by the push if it matters; this probe is only here to turn a credential
			// problem into a sentence.
			return nil
		}
	}
	if lastErr != nil {
		return fmt.Errorf("could not reach the registry at %s to check the credential: %w", reg, lastErr)
	}
	return nil
}

// writeDockerConfig writes a `config.json` holding this credential, and returns the DIRECTORY that
// holds it — which is what `DOCKER_CONFIG` names and what `pack` is handed.
//
// A DIRECTORY THIS PROCESS MADE, never the operator's `~/.docker`. `pack` reads the whole config and
// the lifecycle's exporter gets it; writing into a shared one would hand an actor's build every
// credential on the machine, and editing it would be a side effect on a file somebody else owns.
//
// 0600 ON THE FILE AND 0700 ON THE DIRECTORY. The file holds a password in a form that is base64 and
// not encryption, so the mode is the only thing protecting it.
func writeDockerConfig(dir, reg string, c registryCredential) error {
	if c.anonymous() {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Password))
	doc := map[string]any{"auths": map[string]any{reg: map[string]string{"auth": auth}}}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600)
}

// ── THE BEARER-TOKEN DANCE, WHICH A PUBLIC REGISTRY STILL REQUIRES ──────────────────────────────
//
// "ANONYMOUS PULL" DOES NOT MEAN "NO AUTHORIZATION HEADER". ghcr answers an unauthenticated manifest
// read with
//
//	HTTP/2 401
//	www-authenticate: Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:<repo>:pull"
//
// — measured, on a package that is public and pulls anonymously with `docker pull`. What makes that
// pull work is the client fetching a token from the realm and retrying; `docker` and `pack` both do
// it, and this CLI did not. The symptom was a runtime that "is not in the registry" for a reference
// a human can open in a browser.
//
// TOKEN-FOR-A-CHALLENGE ONLY. This follows a challenge the registry issued, to the realm the
// registry named, for the scope the registry asked for. It invents no credential and sends none: a
// private repository answers the token request with 401 of its own, which surfaces as the original
// failure rather than as a second confusing one.

// bearerChallenge is the `realm`/`service`/`scope` of a `WWW-Authenticate: Bearer` header.
type bearerChallenge struct{ realm, service, scope string }

// parseBearerChallenge reads a `WWW-Authenticate` value, or reports that it is not a Bearer one.
func parseBearerChallenge(header string) (bearerChallenge, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return bearerChallenge{}, false
	}
	var c bearerChallenge
	for _, part := range strings.Split(header[len(prefix):], ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		switch strings.ToLower(k) {
		case "realm":
			c.realm = v
		case "service":
			c.service = v
		case "scope":
			c.scope = v
		}
	}
	// THE REALM IS THE ONLY REQUIRED FIELD, and it has to be a URL this process will fetch — so a
	// header naming something else is not a challenge this follows.
	if !strings.HasPrefix(c.realm, "https://") && !strings.HasPrefix(c.realm, "http://") {
		return bearerChallenge{}, false
	}
	return c, true
}

// registryToken exchanges a challenge for a token, or returns "" when the realm will not issue one.
func registryToken(c bearerChallenge) string {
	req, err := http.NewRequest(http.MethodGet, c.realm, nil)
	if err != nil {
		return ""
	}
	q := req.URL.Query()
	if c.service != "" {
		q.Set("service", c.service)
	}
	if c.scope != "" {
		q.Set("scope", c.scope)
	}
	req.URL.RawQuery = q.Encode()
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	// BOTH SPELLINGS. The distribution spec says `token`; OAuth2 says `access_token`, and real
	// registries answer with one or the other.
	if body.Token != "" {
		return body.Token
	}
	return body.AccessToken
}

// bearerChallengeFor re-reads the `WWW-Authenticate` a URL answers with, for a caller that has
// already closed the response that carried it.
func bearerChallengeFor(url string) (bearerChallenge, bool) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return bearerChallenge{}, false
	}
	req.Header.Set("Accept", manifestAccept)
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return bearerChallenge{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return bearerChallenge{}, false
	}
	return parseBearerChallenge(resp.Header.Get("WWW-Authenticate"))
}

// registryAuthHeader is the `X-Registry-Auth` value the Engine API wants: base64 of an
// AuthConfig JSON document.
//
// THE ENGINE STILL REQUIRES THE HEADER FOR AN ANONYMOUS PUSH, which is why the old code sent
// base64("{}") — the canonical "no credentials". That is correct against a registry with no users
// and a 401 against one with them, and sending it was the whole bug: an install that had turned
// auth on could not deploy, and said so from inside a push.
func registryAuthHeader(reg string, c registryCredential) string {
	doc := map[string]string{"serveraddress": reg}
	if !c.anonymous() {
		doc["username"] = c.User
		doc["password"] = c.Password
	}
	body, err := json.Marshal(doc)
	if err != nil {
		// Marshalling a map of strings cannot fail; the anonymous form keeps the signature simple
		// for callers that have nothing to do about it.
		return base64.URLEncoding.EncodeToString([]byte("{}"))
	}
	return base64.URLEncoding.EncodeToString(body)
}
