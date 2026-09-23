// Package creds is the console login credential: how it is hashed, how it is written, and how it is
// checked. The CLI MINTS one at install; the orchestrator VERIFIES it at login, in TypeScript.
//
// WHY A LOGIN EXISTS AT ALL. The CLI authenticates by reading `~/.kontra/config.yaml`. A browser
// cannot — so until now the console got in by having a bearer BAKED INTO ITS BUNDLE at build time
// (`VITE_KONTRA_EXPLORE_TOKEN`). That is a credential in a build artifact, it breaks on every
// rotation, and a build run for an unrelated reason silently replaced the served bundle with one
// that had no token and answered `query: unauthorized` for every query. The login page is where the
// browser's token comes from instead: sign in with the credential on the filesystem, get the token,
// send it as `Authorization` from then on.
//
// SCRYPT, AND WHY THAT ONE. It is the only password KDF both sides have WITHOUT A NEW DEPENDENCY —
// `golang.org/x/crypto/scrypt` here and `node:crypto`'s builtin there. bcrypt and argon2 would each
// add an npm package to the orchestrator to check a password on a single-instance install, and the
// one thing this file must not do is become a reason to pull native code into the control plane.
//
// THE ENCODED FORM IS SELF-DESCRIBING, so a hash written by an older kontra keeps verifying after
// the parameters are raised — the cost lives in the string, not in a constant two languages have to
// agree about at the same moment:
//
//	scrypt$32768$8$1$<salt-b64>$<hash-b64>
//
// `shared/conformance/login.json` pins that format and carries vectors both sides execute, because
// a hash the CLI writes and the orchestrator cannot read is a login nobody can pass and neither
// suite would notice.
package creds

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// The cost parameters a fresh hash is written with. Raising them is safe: the encoded form carries
// its own, so old hashes keep verifying and only new ones get the new cost.
//
// N=32768 is 32 MiB of memory per verification (128*N*r). That is deliberately more than Node's
// DEFAULT `maxmem` of 32 MiB, which is why the TypeScript peer passes `maxmem` explicitly — a
// default that silently caps the work factor is exactly the kind of quiet weakening this comment
// exists to prevent.
const (
	CostN   = 32768
	CostR   = 8
	CostP   = 1
	KeyLen  = 32
	SaltLen = 16
)

// Scheme is the prefix every encoded hash carries, so a value in a config file says what it is.
const Scheme = "scrypt"

// Hash derives an encoded hash for `password`, with a fresh random salt.
func Hash(password string) (string, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("no entropy for a password salt: %w", err)
	}
	return hashWithSalt(password, salt, CostN, CostR, CostP)
}

func hashWithSalt(password string, salt []byte, n, r, p int) (string, error) {
	sum, err := scrypt.Key([]byte(password), salt, n, r, p, KeyLen)
	if err != nil {
		return "", fmt.Errorf("scrypt: %w", err)
	}
	return strings.Join([]string{
		Scheme,
		strconv.Itoa(n), strconv.Itoa(r), strconv.Itoa(p),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	}, "$"), nil
}

// Verify reports whether `password` produces `encoded`.
//
// CONSTANT-TIME ON THE COMPARISON, and it matters here for the same reason it does in `auth.ts`:
// a byte-by-byte compare returns as soon as two bytes differ, which turns brute force into a
// per-character search. Everything before the compare — parsing, deriving — is not secret.
//
// A MALFORMED HASH IS A FAILURE, NEVER A PASS. `false, error` and not `true`: a config whose hash
// somebody hand-edited into nonsense must lock the console, not open it.
func Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != Scheme {
		return false, fmt.Errorf("not a %s hash: expected %s$N$r$p$salt$hash", Scheme, Scheme)
	}
	n, err1 := strconv.Atoi(parts[1])
	r, err2 := strconv.Atoi(parts[2])
	p, err3 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || err3 != nil || n <= 1 || r <= 0 || p <= 0 {
		return false, fmt.Errorf("%s hash has unusable cost parameters", Scheme)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("%s hash has an undecodable salt", Scheme)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("%s hash is undecodable", Scheme)
	}
	got, err := scrypt.Key([]byte(password), salt, n, r, p, len(want))
	if err != nil {
		return false, fmt.Errorf("scrypt: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// : The alphabet a generated password is drawn from.
//
// UNAMBIGUOUS ON PURPOSE: no `O`/`0`, no `l`/`1`/`I`. This value is PRINTED ONCE at install and
// typed into a browser by a person reading it off a terminal, and a character pair that two fonts
// disagree about turns a working install into a support conversation.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword returns a fresh password of `n` characters.
//
// REJECTION SAMPLING, not `% len(alphabet)`. The modulo form is one line shorter and biases toward
// the first `256 % 56` characters — a real reduction in entropy for a value that is the whole of
// what protects the console.
func GeneratePassword(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("password length must be positive, got %d", n)
	}
	const size = len(passwordAlphabet)
	limit := byte(256 - (256 % size)) // bytes at or above this would bias the result
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("no entropy for a password: %w", err)
		}
		for _, b := range buf {
			if b < limit {
				out = append(out, passwordAlphabet[int(b)%size])
				if len(out) == n {
					break
				}
			}
		}
	}
	return string(out), nil
}
