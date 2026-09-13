package creds

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The GO ARM of shared/conformance/login.json — the WRITER's side.
//
// This package mints the hash at install; `control/orchestrator/src/auth/password.ts` verifies it at
// login. A hash one writes and the other cannot read is a login nobody can pass, and neither suite
// would notice: each half is correct on its own. That is ADR 0035 rule two.

type vector struct {
	Why      string `json:"why"`
	Password string `json:"password"`
	SaltB64  string `json:"salt_b64"`
	Encoded  string `json:"encoded"`
}

type loginCorpus struct {
	Cost struct {
		N, R, P, KeyLen, SaltLen int `json:"-"`
	} `json:"-"`
	Raw     map[string]int `json:"cost"`
	Vectors []vector       `json:"vectors"`
}

// Resolved from THIS FILE, never the caller's directory — a relative climb is correct in one
// package and wrong in the next (tests/test_conformance_tree.py).
func corpusPath() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "shared", "conformance", "login.json")
}

func load(t *testing.T) loginCorpus {
	t.Helper()
	raw, err := os.ReadFile(corpusPath())
	if err != nil {
		t.Fatalf("read the corpus: %v", err)
	}
	var doc loginCorpus
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the corpus: %v", err)
	}
	if len(doc.Vectors) < 4 {
		t.Fatalf("the corpus shrank to %d vectors", len(doc.Vectors))
	}
	// The rows that break a naive implementation, by name. A corpus of one happy row passes an
	// implementation that splits on `$` carelessly and never sees a non-ASCII or empty password.
	body := string(raw)
	for _, want := range []string{`"a$b$c$d"`, "café-naïve", "maxmem"} {
		if !strings.Contains(body, want) {
			t.Errorf("the corpus no longer carries %s", want)
		}
	}
	return doc
}

func TestTheHashesThisPackageWritesAreTheCorpusVectors(t *testing.T) {
	doc := load(t)
	for _, v := range doc.Vectors {
		t.Run(v.Why, func(t *testing.T) {
			salt, err := base64.RawStdEncoding.DecodeString(v.SaltB64)
			if err != nil {
				t.Fatalf("salt: %v", err)
			}
			got, err := hashWithSalt(v.Password, salt, doc.Raw["N"], doc.Raw["r"], doc.Raw["p"])
			if err != nil {
				t.Fatal(err)
			}
			if got != v.Encoded {
				t.Errorf("encoding drifted from the corpus:\n  got  %s\n  want %s", got, v.Encoded)
			}
			ok, err := Verify(v.Password, v.Encoded)
			if err != nil || !ok {
				t.Errorf("this package cannot verify its own corpus row: ok=%v err=%v", ok, err)
			}
			// NON-VACUOUS: a Verify returning true unconditionally passes the line above.
			if wrong, _ := Verify(v.Password+"x", v.Encoded); wrong {
				t.Error("a different password verified")
			}
		})
	}
}

func TestTheCostInTheCorpusIsTheCostThisPackageWrites(t *testing.T) {
	// The corpus states N/r/p and so does this package. If they drift, a fresh install writes a
	// hash at one cost while both arms assert another — green suites, and the only symptom would be
	// verification getting slower or weaker with nobody choosing it.
	doc := load(t)
	if doc.Raw["N"] != CostN || doc.Raw["r"] != CostR || doc.Raw["p"] != CostP {
		t.Errorf("cost drift: corpus N=%d r=%d p=%d, package N=%d r=%d p=%d",
			doc.Raw["N"], doc.Raw["r"], doc.Raw["p"], CostN, CostR, CostP)
	}
	if doc.Raw["key_len"] != KeyLen || doc.Raw["salt_len"] != SaltLen {
		t.Errorf("length drift: corpus key=%d salt=%d, package key=%d salt=%d",
			doc.Raw["key_len"], doc.Raw["salt_len"], KeyLen, SaltLen)
	}
}
