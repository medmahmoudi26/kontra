package config

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The CLI's arm of shared/conformance/fleets.json; the orchestrator's is
// control/orchestrator/src/infra/fleets.conformance.test.ts.
type fleetsCorpus struct {
	SecretFields []string `json:"secret_fields"`
	Valid        []struct {
		Why    string          `json:"why"`
		YAML   string          `json:"yaml"`
		Expect json.RawMessage `json:"expect"`
		Public json.RawMessage `json:"public"`
	} `json:"valid"`
	Invalid []struct {
		Why   string `json:"why"`
		YAML  string `json:"yaml"`
		Error string `json:"error"`
	} `json:"invalid"`
}

func loadFleetsCorpus(t *testing.T) fleetsCorpus {
	t.Helper()
	raw, err := os.ReadFile("../../../shared/conformance/fleets.json")
	if err != nil {
		t.Fatal(err)
	}
	var c fleetsCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Valid) == 0 || len(c.Invalid) == 0 {
		t.Fatal("fleets.json has no cases")
	}
	return c
}

// sameJSON compares two values by their JSON, the corpus's own representation.
func sameJSON(t *testing.T, got any, want json.RawMessage) {
	t.Helper()
	var g, w any
	b, _ := json.Marshal(got)
	_ = json.Unmarshal(b, &g)
	_ = json.Unmarshal(want, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestFleetsMatchTheCorpus(t *testing.T) {
	c := loadFleetsCorpus(t)
	for _, v := range c.Valid {
		t.Run("valid/"+v.Why, func(t *testing.T) {
			f, err := ParseFleets([]byte(v.YAML))
			if err != nil {
				t.Fatal(err)
			}
			sameJSON(t, f, v.Expect)
			def, profiles := PublicFleets(f)
			sameJSON(t, map[string]any{"default": def, "profiles": profiles}, v.Public)
		})
	}
	for _, v := range c.Invalid {
		t.Run("invalid/"+v.Why, func(t *testing.T) {
			_, err := ParseFleets([]byte(v.YAML))
			if !errors.Is(err, ErrFleetConfig) {
				t.Fatalf("want a fleet config refusal, got %v", err)
			}
			if !strings.Contains(err.Error(), v.Error) {
				t.Fatalf("refusal %q does not name %q", err, v.Error)
			}
		})
	}
}

func TestTheSecretFieldsAreTheCorpusList(t *testing.T) {
	c := loadFleetsCorpus(t)
	if !reflect.DeepEqual(FleetSecretFields, c.SecretFields) {
		t.Fatalf("FleetSecretFields %v, corpus %v", FleetSecretFields, c.SecretFields)
	}
}

func TestAMissingFileIsTheBuiltInLocalProfile(t *testing.T) {
	f, err := LoadFleets(t.TempDir() + "/kontra.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Default != "local" || f.Profiles["local"].Nodes != 1 {
		t.Fatalf("got %+v", f)
	}
}
