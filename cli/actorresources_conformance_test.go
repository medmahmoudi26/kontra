package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI ARM of shared/conformance/resources.json. The Python arm is
// tests/test_resources_conformance.py, and it reads the same file through the worker's own loader.
//
// DRIVEN THROUGH readManifest AND NOT THROUGH resourcesOf, because the file is the contract: what
// matters is what `kontra deploy` and `kontra serve` make of an actor.json on disk, and a test of
// the helper alone would pass while the struct tags that feed it were wrong.

type resourcesCorpus struct {
	Keys  []string `json:"keys"`
	Cases []struct {
		Why       string          `json:"why"`
		Manifest  json.RawMessage `json:"manifest"`
		Resources json.RawMessage `json:"resources"`
		Refused   []string        `json:"refused"`
	} `json:"cases"`
}

func loadResourcesCorpus(t *testing.T) resourcesCorpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "shared", "conformance", "resources.json"))
	if err != nil {
		t.Fatalf("the corpus must be readable from cli/ — a driver that cannot read it is one edit "+
			"away from one that skips: %v", err)
	}
	var c resourcesCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("the corpus is not valid JSON: %v", err)
	}
	return c
}

// manifestWith writes an actor.json carrying the identity readManifest requires plus the case's keys.
func manifestWith(t *testing.T, keys json.RawMessage) string {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(keys, &body); err != nil {
		t.Fatalf("a case's manifest is not an object: %v", err)
	}
	body["schemaVersion"] = json.RawMessage(`"kontra.actor.v1"`)
	body["name"] = json.RawMessage(`"enrich"`)
	body["version"] = json.RawMessage(`"0.3.0"`)
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "actor.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheResourcesCorpusStillCarriesTheInputsThatDiverge(t *testing.T) {
	c := loadResourcesCorpus(t)
	if len(c.Cases) < 30 {
		t.Fatalf("the corpus has %d cases; a corpus that shrank passes every reader", len(c.Cases))
	}
	// The two inputs that are the reason the file exists: Docker's `256m`, which Kubernetes reads as
	// millibytes, and `true`, which Python reads as the number 1.
	var all strings.Builder
	for _, k := range c.Cases {
		all.Write(k.Manifest)
	}
	for _, want := range []string{`"256m"`, `"cpus": true`, `"needs"`, `"resources"`} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("the corpus no longer carries %s", want)
		}
	}
	if strings.Join(c.Keys, ",") != "cpus,memory" {
		t.Errorf("the normalised key set is %v; actorResources has cpus and memory", c.Keys)
	}
}

func TestActorJSONResourcesMatchTheCorpus(t *testing.T) {
	for _, k := range loadResourcesCorpus(t).Cases {
		t.Run(k.Why, func(t *testing.T) {
			m, err := readManifest(manifestWith(t, k.Manifest))
			if len(k.Refused) > 0 {
				if err == nil {
					t.Fatalf("accepted, normalised to %+v; the corpus refuses it", m.Resources)
				}
				for _, word := range k.Refused {
					if !strings.Contains(err.Error(), word) {
						t.Errorf("the refusal must name %q:\n%v", word, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			var want *actorResources
			if err := json.Unmarshal(k.Resources, &want); err != nil {
				t.Fatal(err)
			}
			switch {
			case want == nil && m.Resources != nil:
				t.Errorf("normalised to %+v; the corpus says nothing was stated", *m.Resources)
			case want != nil && m.Resources == nil:
				t.Errorf("normalised to nothing; want %+v", *want)
			case want != nil && *want != *m.Resources:
				t.Errorf("normalised to %+v; want %+v", *m.Resources, *want)
			}
		})
	}
}

// The examples this repo ships use the alias in Docker's units. They are the first thing an author
// copies, so they are asserted by path rather than trusted to resemble a corpus row.
func TestTheShippedExamplesStillRead(t *testing.T) {
	for _, dir := range []string{
		filepath.Join("..", "examples", "python", "hello"),
		filepath.Join("..", "examples", "python", "firstactor"),
		filepath.Join("..", "testdata", "fixtureactor"),
	} {
		m, err := readManifest(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if m.Resources == nil || *m.Resources != (actorResources{CPUs: 1, Memory: "256Mi"}) {
			t.Errorf("%s: resources = %+v, want 1 CPU and 256Mi", dir, m.Resources)
		}
	}
}
