package config

// THE FLEET PROFILES — `fleets:` in the install's kontra.yaml (PRD §7, ADR 0066).
//
// The CLI's reader of the same block the orchestrator's infra role converges
// (control/orchestrator/src/infra/fleets.ts). The CLI only NAMES and LISTS profiles —
// `kontra fleet up --profile do-fra`, `kontra fleet profiles` — and refuses a file the infra role
// would refuse, so a typo is reported at the prompt rather than inside a converge activity.
// The two readers share no code; shared/conformance/fleets.json is what holds them to one answer.
//
// It never prints a secret: PublicFleets is the only shape the CLI shows, and it carries the NAMES
// of the secret fields that are set, never their values.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	yaml3 "gopkg.in/yaml.v3"
)

// Fleets is the parsed `fleets:` block: the default profile's name and every profile.
type Fleets struct {
	Default  string                  `json:"default"`
	Profiles map[string]FleetProfile `json:"profiles"`
}

// FleetProfile is one provider's inputs. Which fields are set depends on Provider; the JSON tags
// are the corpus's field names, and omitempty keeps a profile to its own provider's fields.
type FleetProfile struct {
	Provider          string            `json:"provider"`
	Nodes             int               `json:"nodes,omitempty"`
	Size              *FleetSize        `json:"size,omitempty"`
	IdleMinutes       int               `json:"idle_minutes,omitempty"`
	Region            string            `json:"region,omitempty"`
	Token             string            `json:"token,omitempty"`
	SSHKeyFingerprint string            `json:"ssh_key_fingerprint,omitempty"`
	VPCCIDR           string            `json:"vpc_cidr,omitempty"`
	Sizes             map[string]string `json:"sizes,omitempty"`
	Kubeconfig        string            `json:"kubeconfig,omitempty"`
}

// FleetSize is a local node's shape.
type FleetSize struct {
	CPUs   int    `json:"cpus"`
	Memory string `json:"memory"`
}

// FleetSecretFields are the fields whose values never leave the process (fleets.json).
var FleetSecretFields = []string{"token", "kubeconfig"}

// ErrFleetConfig marks a kontra.yaml the fleet rules refuse.
var ErrFleetConfig = errors.New("kontra.yaml fleets")

var (
	fleetName   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)
	fleetMemory = regexp.MustCompile(`^[1-9][0-9]*(Mi|Gi)$`)
	fleetKnown  = map[string][]string{
		"local":          {"provider", "nodes", "size", "idle_minutes"},
		"digital_ocean":  {"provider", "region", "token", "ssh_key_fingerprint", "vpc_cidr", "sizes", "nodes", "idle_minutes"},
		"byo_kubeconfig": {"provider", "kubeconfig"},
	}
)

const defaultVPCCIDR = "10.200.0.0/20"

func defaultLocalProfile() FleetProfile {
	return FleetProfile{Provider: "local", Nodes: 1, Size: &FleetSize{CPUs: 2, Memory: "4Gi"}, IdleMinutes: 15}
}

func fleetErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrFleetConfig, fmt.Sprintf(format, a...))
}

func positive(where, key string, v any, fallback int) (int, error) {
	if v == nil {
		return fallback, nil
	}
	n, ok := v.(int)
	if !ok || n < 1 {
		return 0, fleetErr("%s: %s must be a whole number of at least 1, got %v", where, key, v)
	}
	return n, nil
}

func required(where, key string, v any) (string, error) {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", fleetErr("%s: %s is required and must be a non-empty string", where, key)
	}
	return s, nil
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func fleetProfileOf(name string, raw any) (FleetProfile, error) {
	where := "fleets." + name
	if !fleetName.MatchString(name) {
		return FleetProfile{}, fleetErr("%s: a profile name must match %s — it becomes part of a stack name and a Kubernetes label", where, fleetName.String())
	}
	m, ok := asMap(raw)
	if !ok {
		return FleetProfile{}, fleetErr("%s: a profile is a map of its provider's inputs", where)
	}
	provider, _ := m["provider"].(string)
	known, ok := fleetKnown[provider]
	if !ok {
		return FleetProfile{}, fleetErr("%s: provider %q has no program — one of local, digital_ocean, byo_kubeconfig", where, fmt.Sprint(m["provider"]))
	}
	for key := range m {
		found := false
		for _, k := range known {
			found = found || k == key
		}
		if !found {
			return FleetProfile{}, fleetErr("%s: %s is not a %s setting (known: %s)", where, key, provider, strings.Join(known, ", "))
		}
	}
	var err error
	switch provider {
	case "local":
		p := defaultLocalProfile()
		size := map[string]any{}
		if m["size"] != nil {
			if size, ok = asMap(m["size"]); !ok {
				return p, fleetErr("%s: size is a map of cpus and memory", where)
			}
		}
		for key := range size {
			if key != "cpus" && key != "memory" {
				return p, fleetErr("%s: size.%s is not a size setting", where, key)
			}
		}
		if size["memory"] != nil {
			mem, ok := size["memory"].(string)
			if !ok || !fleetMemory.MatchString(mem) {
				return p, fleetErr("%s: size.memory must be a quantity like 4Gi or 512Mi, got %v", where, size["memory"])
			}
			p.Size.Memory = mem
		}
		if p.Size.CPUs, err = positive(where, "size.cpus", size["cpus"], 2); err != nil {
			return p, err
		}
		if p.Nodes, err = positive(where, "nodes", m["nodes"], 1); err != nil {
			return p, err
		}
		if p.IdleMinutes, err = positive(where, "idle_minutes", m["idle_minutes"], 15); err != nil {
			return p, err
		}
		return p, nil
	case "digital_ocean":
		p := FleetProfile{Provider: provider, VPCCIDR: defaultVPCCIDR}
		sizes, ok := asMap(m["sizes"])
		if !ok || len(sizes) == 0 {
			return p, fleetErr("%s: sizes is required — a map of size letter to droplet slug", where)
		}
		p.Sizes = map[string]string{}
		for letter, slug := range sizes {
			if p.Sizes[letter], err = required(where, "sizes."+letter, slug); err != nil {
				return p, err
			}
		}
		if p.Region, err = required(where, "region", m["region"]); err != nil {
			return p, err
		}
		if p.Token, err = required(where, "token", m["token"]); err != nil {
			return p, err
		}
		if p.SSHKeyFingerprint, err = required(where, "ssh_key_fingerprint", m["ssh_key_fingerprint"]); err != nil {
			return p, err
		}
		if m["vpc_cidr"] != nil {
			if p.VPCCIDR, err = required(where, "vpc_cidr", m["vpc_cidr"]); err != nil {
				return p, err
			}
		}
		if p.Nodes, err = positive(where, "nodes", m["nodes"], 1); err != nil {
			return p, err
		}
		if p.IdleMinutes, err = positive(where, "idle_minutes", m["idle_minutes"], 15); err != nil {
			return p, err
		}
		return p, nil
	default: // byo_kubeconfig
		p := FleetProfile{Provider: provider}
		p.Kubeconfig, err = required(where, "kubeconfig", m["kubeconfig"])
		return p, err
	}
}

// ParseFleets reads the `fleets:` block of a kontra.yaml. No block is the built-in `local` profile.
func ParseFleets(text []byte) (Fleets, error) {
	var doc any
	if err := yaml3.Unmarshal(text, &doc); err != nil {
		return Fleets{}, fleetErr("kontra.yaml is not valid YAML: %v", err)
	}
	var block any
	if m, ok := asMap(doc); ok {
		block = m["fleets"]
	}
	if block == nil {
		return Fleets{Default: "local", Profiles: map[string]FleetProfile{"local": defaultLocalProfile()}}, nil
	}
	m, ok := asMap(block)
	if !ok {
		return Fleets{}, fleetErr("fleets must be a map of profile names to profiles")
	}
	out := Fleets{Profiles: map[string]FleetProfile{}}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic: the first refusal named is the same on every run
	for _, name := range names {
		if name == "default" {
			continue
		}
		p, err := fleetProfileOf(name, m[name])
		if err != nil {
			return Fleets{}, err
		}
		out.Profiles[name] = p
	}
	if chosen, present := m["default"]; present {
		name, _ := chosen.(string)
		if _, ok := out.Profiles[name]; ok {
			out.Default = name
			return out, nil
		}
		if name == "local" {
			out.Profiles["local"] = defaultLocalProfile()
			out.Default = "local"
			return out, nil
		}
		return Fleets{}, fleetErr("fleets.default names %q, which is not a profile here", fmt.Sprint(chosen))
	}
	if _, ok := out.Profiles["local"]; ok {
		out.Default = "local"
		return out, nil
	}
	return Fleets{}, fleetErr("fleets.default is required when there is no local profile — a fleet nobody chose is not chosen silently")
}

// LoadFleets reads the install's kontra.yaml at path; a missing file is the built-in `local` profile.
func LoadFleets(path string) (Fleets, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ParseFleets(nil)
	}
	if err != nil {
		return Fleets{}, err
	}
	return ParseFleets(b)
}

// PublicFleetProfile is a profile as it may be printed: secrets replaced by the names of those set.
type PublicFleetProfile struct {
	FleetProfile
	Secrets []string `json:"secrets"`
}

// PublicFleets is what the CLI may print or send anywhere.
func PublicFleets(f Fleets) (string, map[string]PublicFleetProfile) {
	out := map[string]PublicFleetProfile{}
	for name, p := range f.Profiles {
		pub := PublicFleetProfile{FleetProfile: p, Secrets: []string{}}
		if p.Token != "" {
			pub.Secrets = append(pub.Secrets, "token")
			pub.Token = ""
		}
		if p.Kubeconfig != "" {
			pub.Secrets = append(pub.Secrets, "kubeconfig")
			pub.Kubeconfig = ""
		}
		out[name] = pub
	}
	return f.Default, out
}
