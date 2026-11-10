// Package publish is `kontra publish <listing.yaml>` (spec/tunnel.md §6.4, D323): it reads a listing file, checks it
// against spec/schema/listing.json, signs the person in at the gate's authorization server for the operator's control
// plane, and sends the listing to the control plane's PUT /v1/services/{name} (Kontra Cloud's, kontra-cloud D-M5-62,
// D-M5-95, D-M5-110), naming the version it replaces.
//
// It carries the publisher's declared listing and nothing else: a listing has no price, and the control plane decides
// what it accepts. Payment, when a seller charges, is the seller's own AP2, which neither the gate nor the CLI sees
// (D351).
package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	yaml "go.yaml.in/yaml/v3"
)

// MaxFile bounds a listing file.
const MaxFile = 64 << 10

// MaxTaskTimeoutSeconds bounds a listing's taskTimeoutSeconds (spec/schema/listing.json; Cloud's gate/listings:
// MAX_TASK_TIMEOUT); the control plane's default is DefaultTaskTimeoutSeconds.
const (
	MaxTaskTimeoutSeconds     = 604800
	DefaultTaskTimeoutSeconds = 3600
)

var serviceRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Listing is a listing file as spec/schema/listing.json describes it, its defaults filled in. It lists an A2A service:
// an MCP service is not listed (D350).
type Listing struct {
	Service    string
	Visibility string // public (default), unlisted or private
	Sealed     string // off (default), optional or required
	Published  bool   // true unless the file says false
	// TaskTimeoutSeconds is the service's task.timeout as the file writes it (nil: absent, the control plane's
	// DefaultTaskTimeoutSeconds).
	TaskTimeoutSeconds *int64
}

// Problems is a listing file the schema refuses: one line per problem, each naming where.
type Problems []string

func (p Problems) Error() string {
	return "the listing is not valid (spec/schema/listing.json):\n  " + strings.Join(p, "\n  ")
}

// Parse reads a listing file (YAML 1.2, or JSON): one document, a mapping with string keys, no key twice, at most
// MaxFile bytes. A file the schema refuses is Problems; one that is not YAML another error.
//
// A file whose first character is '{' is read as JSON first (RFC 8259, standard escapes and surrogate pairs included,
// which the YAML decoder refuses), and as YAML only when it is not JSON (a YAML flow mapping). A YAML integer must be
// written in decimal without leading zeros, underscores or a base prefix, and a float without underscores: the YAML
// decoder reads 0600 as octal 384 and 1_000 as 1000, where YAML 1.2's core schema reads decimal 600 and a string, so
// such a number is refused rather than sent as something the person did not write.
func Parse(b []byte) (*Listing, error) {
	if len(b) > MaxFile {
		return nil, fmt.Errorf("the listing file is over %d bytes", MaxFile)
	}
	doc, isJSON, err := parseJSON(b)
	if !isJSON {
		doc, err = parseYAML(b)
	}
	if err != nil {
		return nil, err
	}
	v := &checker{}
	l := v.listing(doc)
	if len(v.problems) > 0 {
		return nil, v.problems
	}
	return l, nil
}

// parseJSON reads b as one JSON value when it starts with '{' (after white space and a byte order mark): isJSON is
// false when it does not, or is not JSON at all (then it may be a YAML flow mapping). A key twice in an object, or
// anything after the value, is an error of a JSON file.
func parseJSON(b []byte) (doc any, isJSON bool, err error) {
	t := bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\ufeff")), " \t\r\n")
	if len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return nil, false, nil
	}
	if !utf8.Valid(t) { // encoding/json would read each invalid byte as U+FFFD
		return nil, true, errors.New("the listing file is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(t))
	doc, err = jsonValue(dec)
	if err != nil {
		return nil, true, fmt.Errorf("the listing file is not a listing in JSON: %w", err)
	}
	return doc, true, nil
}

// jsonValue reads one JSON value as the YAML decoder would give it (map[string]any, []any, string, float64, bool,
// nil), refusing a key twice in an object.
func jsonValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch tok {
	case json.Delim('{'):
		m := map[string]any{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			if _, dup := m[k]; dup {
				return nil, fmt.Errorf("the key %q is written twice in one object", k)
			}
			if m[k], err = jsonValue(dec); err != nil {
				return nil, err
			}
		}
		_, err := dec.Token() // }
		return m, err
	case json.Delim('['):
		xs := []any{}
		for dec.More() {
			x, err := jsonValue(dec)
			if err != nil {
				return nil, err
			}
			xs = append(xs, x)
		}
		_, err := dec.Token() // ]
		return xs, err
	}
	return tok, nil
}

// parseYAML reads b as one YAML document, refusing numbers the YAML decoder reads otherwise than YAML 1.2's core
// schema (yamlNumbers).
func parseYAML(b []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the listing file is empty")
		}
		return nil, fmt.Errorf("the listing file is not YAML: %w", err)
	}
	var more yaml.Node
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("the listing file holds more than one YAML document")
	}
	if err := yamlNumbers(&root); err != nil {
		return nil, err
	}
	var doc any
	if err := root.Decode(&doc); err != nil {
		return nil, fmt.Errorf("the listing file is not YAML: %w", err)
	}
	return doc, nil
}

var (
	yamlDecimalRE = regexp.MustCompile(`^[-+]?(0|[1-9][0-9]*)$`)
	yamlFloatRE   = regexp.MustCompile(`^([-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?|[-+]?\.(inf|Inf|INF)|\.(nan|NaN|NAN))$`)
)

// yamlNumbers refuses an integer the YAML decoder would read in another base or with underscores removed (0600 is
// 384 to it, 600 to YAML 1.2; 1_000 is 1000 to it, a string to YAML 1.2; 0x258, 0o1130 and 0b1 are refused too, so
// every integer is written one way), and a float written with underscores.
func yamlNumbers(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.ShortTag() {
		case "!!int":
			if !yamlDecimalRE.MatchString(n.Value) {
				return fmt.Errorf("the listing file, line %d: %q: write an integer in decimal, without leading zeros, "+
					"underscores or a base prefix (YAML 1.1 and 1.2 read it differently)", n.Line, n.Value)
			}
		case "!!float":
			if !yamlFloatRE.MatchString(n.Value) {
				return fmt.Errorf("the listing file, line %d: %q: write a number in decimal, without underscores", n.Line, n.Value)
			}
		}
	}
	for _, c := range n.Content {
		if err := yamlNumbers(c); err != nil {
			return err
		}
	}
	return nil
}

// checker collects problems as it reads a document.
type checker struct{ problems Problems }

func (v *checker) add(at, format string, args ...any) {
	v.problems = append(v.problems, at+": "+fmt.Sprintf(format, args...))
}

// object reads a mapping with string keys; nil (and a problem) otherwise.
func (v *checker) object(at string, x any) map[string]any {
	m, ok := x.(map[string]any)
	if !ok {
		v.add(at, "want a mapping")
		return nil
	}
	return m
}

// unknown names the keys of m that are not in known.
func (v *checker) unknown(at string, m map[string]any, known ...string) {
	var extra []string
	for k := range m {
		if !contains(known, k) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		v.add(at, "%q is not a key of a listing here (the keys: %s)", k, strings.Join(known, ", "))
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// str reads a string member; ok false when absent.
func (v *checker) str(m map[string]any, at, key string) (string, bool) {
	x, present := m[key]
	if !present {
		return "", false
	}
	s, isStr := x.(string)
	if !isStr {
		hint := ""
		switch x.(type) {
		case float64, int:
			hint = ` (quote it)`
		}
		v.add(join(at, key), "want a string%s", hint)
		return "", false
	}
	return s, true
}

// enum reads a string member that must be one of values; def when absent.
func (v *checker) enum(m map[string]any, at, key, def string, values ...string) string {
	s, ok := v.str(m, at, key)
	if !ok {
		return def
	}
	if !contains(values, s) {
		v.add(join(at, key), "%q: want %s", s, strings.Join(values, ", "))
		return def
	}
	return s
}

// integer reads an integer member within [lo, hi]; nil when absent or refused.
func (v *checker) integer(m map[string]any, at, key string, lo, hi int64) *int64 {
	x, present := m[key]
	if !present {
		return nil
	}
	var n int64
	switch t := x.(type) {
	case int:
		n = int64(t)
	case int64:
		n = t
	case uint64:
		if t > math.MaxInt64 {
			v.add(join(at, key), "want an integer from %d to %d", lo, hi)
			return nil
		}
		n = int64(t)
	case float64:
		// JSON Schema's integer: a number with no fraction.
		if t != math.Trunc(t) || t < math.MinInt64 || t > math.MaxInt64 {
			v.add(join(at, key), "want an integer from %d to %d", lo, hi)
			return nil
		}
		n = int64(t)
	default:
		v.add(join(at, key), "want an integer from %d to %d", lo, hi)
		return nil
	}
	if n < lo || n > hi {
		v.add(join(at, key), "%d: want an integer from %d to %d", n, lo, hi)
		return nil
	}
	return &n
}

func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

// listing reads the document's top level (spec/schema/listing.json).
func (v *checker) listing(doc any) *Listing {
	m := v.object("the listing", doc)
	if m == nil {
		return nil
	}
	// kind, tools and tasks were an MCP listing's: listings are A2A only (D350), so they are unknown members; price was
	// escrow's, and a listing has none (D351).
	v.unknown("the listing", m, "service", "visibility", "sealed", "published", "taskTimeoutSeconds")
	l := &Listing{Published: true}
	if s, ok := v.str(m, "", "service"); !ok {
		if _, present := m["service"]; !present {
			v.add("service", "required: the service's name at the gate")
		}
	} else if !serviceRE.MatchString(s) {
		v.add("service", "%q: want 1 to 63 of [a-z0-9-], not starting or ending with '-'", s)
	} else {
		l.Service = s
	}
	l.Visibility = v.enum(m, "", "visibility", "public", "public", "unlisted", "private")
	l.Sealed = v.enum(m, "", "sealed", "off", "off", "optional", "required")
	if x, present := m["published"]; present {
		b, ok := x.(bool)
		if !ok {
			v.add("published", "want true or false")
		}
		l.Published = !ok || b
	}
	l.TaskTimeoutSeconds = v.integer(m, "", "taskTimeoutSeconds", 1, MaxTaskTimeoutSeconds)
	return l
}

// Body is the request PUT /v1/services/{name} takes (Cloud's ListingPut, D-M5-95): the listing, whether the gate
// serves it, and the version it replaces (0: none). taskTimeoutSeconds is sent only when the file wrote it (the
// control plane's default otherwise). kind is always "a2a": the file has none, and ListingPut still names it.
func (l *Listing) Body(version int64) ([]byte, error) {
	body := struct {
		Kind               string `json:"kind"`
		Visibility         string `json:"visibility"`
		Sealed             string `json:"sealed"`
		Published          bool   `json:"published"`
		TaskTimeoutSeconds *int64 `json:"taskTimeoutSeconds,omitempty"`
		Version            int64  `json:"version"`
	}{
		Kind: "a2a", Visibility: l.Visibility, Sealed: l.Sealed, Published: l.Published,
		TaskTimeoutSeconds: l.TaskTimeoutSeconds, Version: version,
	}
	return json.Marshal(body)
}
