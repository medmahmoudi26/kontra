// Package publish is `kontra publish <listing.yaml>` (spec/tunnel.md §6.4, D323): it reads a listing file, checks it
// against spec/schema/listing.json, signs the person in at the gate's authorization server for the operator's control
// plane, and sends the listing to the control plane's PUT /v1/services/{name} (Kontra Cloud's, kontra-cloud D-M5-62,
// D-M5-95, D-M5-110), naming the version it replaces.
//
// It carries the operator's declared listing and nothing else: no amount is computed, converted, rounded or compared
// with another here; the control plane decides what it accepts, and the gate holds no price (README, "no money code").
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

	yaml "go.yaml.in/yaml/v3"
)

// MaxFile bounds a listing file.
const MaxFile = 64 << 10

// Bounds a listing's values take (spec/schema/listing.json; Cloud's gate/listings: MAX_REVIEW_WINDOW,
// MAX_TASK_TIMEOUT, MAX_TOOL_PRICES, its 64 globs and a week's idle bound).
const (
	MaxWindowSeconds = 604800 // reviewWindowSeconds, taskTimeoutSeconds, idleTimeoutSeconds
	MaxToolPrices    = 100
	MaxGlobs         = 64
	MaxFreePerMonth  = 1_000_000_000
	maxAmountLen     = 24
)

// Price models.
const (
	ModelFree       = "free"
	ModelPerTask    = "per_task"
	ModelPerCall    = "per_call"
	ModelPerSession = "per_session"
)

var (
	serviceRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	centsRE   = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})(\.[0-9]{1,2})?$`)
	microsRE  = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.[0-9]{1,6})?$`)
	zeroRE    = regexp.MustCompile(`^0(\.0+)?$`)
	toolRE    = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,128}$`)
)

// Listing is a listing file as spec/schema/listing.json describes it, its defaults filled in.
type Listing struct {
	Service    string
	Kind       string // a2a (default) or mcp
	Visibility string // public (default), unlisted or private
	Price      Price
	Tools      *ToolsPolicy // mcp only
	Tasks      string       // mcp only; "" when the file names none
	Sealed     string       // off (default), optional or required
	Published  bool         // true unless the file says false
}

// Price is the listing's price, its members as the file writes them (nil: absent).
type Price struct {
	Model               string
	Amount              *string
	ReviewWindowSeconds *int64
	TaskTimeoutSeconds  *int64
	IdleTimeoutSeconds  *int64
	Tools               map[string]string // per_call: a tool's own amount
	FreePerMonth        *int64
	CapPerMonth         *string
}

// ToolsPolicy is an MCP service's tools policy (relay.md §11.3).
type ToolsPolicy struct {
	Allow []string // nil: absent (every tool)
	Deny  []string
}

// Problems is a listing file the schema refuses: one line per problem, each naming where.
type Problems []string

func (p Problems) Error() string {
	return "the listing is not valid (spec/schema/listing.json):\n  " + strings.Join(p, "\n  ")
}

// Parse reads a listing file (YAML 1.2, or JSON): one document, a mapping with string keys, no key twice, at most
// MaxFile bytes. A file the schema refuses is Problems; one that is not YAML another error.
func Parse(b []byte) (*Listing, error) {
	if len(b) > MaxFile {
		return nil, fmt.Errorf("the listing file is over %d bytes", MaxFile)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the listing file is empty")
		}
		return nil, fmt.Errorf("the listing file is not YAML: %w", err)
	}
	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return nil, errors.New("the listing file holds more than one YAML document")
	}
	v := &checker{}
	l := v.listing(doc)
	if len(v.problems) > 0 {
		return nil, v.problems
	}
	return l, nil
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
		if _, num := x.(float64); num {
			hint = ` (quote it: "1.00", not 1.00)`
		} else if _, num := x.(int); num {
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
	v.unknown("the listing", m, "service", "kind", "visibility", "price", "tools", "tasks", "sealed", "published")
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
	l.Kind = v.enum(m, "", "kind", "a2a", "a2a", "mcp")
	l.Visibility = v.enum(m, "", "visibility", "public", "public", "unlisted", "private")
	l.Sealed = v.enum(m, "", "sealed", "off", "off", "optional", "required")
	if x, present := m["published"]; present {
		b, ok := x.(bool)
		if !ok {
			v.add("published", "want true or false")
		}
		l.Published = !ok || b
	}
	if x, present := m["price"]; !present {
		v.add("price", "required: {model: free | per_task | per_call | per_session, amount, …}")
	} else if pm := v.object("price", x); pm != nil {
		l.Price = v.price(pm)
	}
	if x, present := m["tools"]; present {
		if l.Kind != "mcp" {
			v.add("tools", "an MCP service's (kind: mcp)")
		} else if tm := v.object("tools", x); tm != nil {
			l.Tools = v.tools(tm)
		}
	}
	if _, present := m["tasks"]; present {
		if l.Kind != "mcp" {
			v.add("tasks", "an MCP service's (kind: mcp)")
		} else {
			l.Tasks = v.enum(m, "", "tasks", "", "durable", "required", "passthrough", "off")
		}
	}
	switch model := l.Price.Model; {
	case (model == ModelPerCall || model == ModelPerSession) && l.Kind != "mcp":
		v.add("price.model", "%s prices an MCP service's calls: kind: mcp", model)
	case l.Kind == "mcp" && model == ModelPerTask && l.Tasks != "required":
		v.add("tasks", "per_task on an MCP service needs tasks: required, so that every call is a task its quote covers "+
			"(ext-escrow.md §6, D322)")
	case l.Kind == "mcp" && model != "" && model != ModelFree && l.Tasks == "passthrough":
		v.add("tasks", "a priced MCP service is never passthrough: the gate does not observe those calls (relay.md §11.5)")
	}
	return l
}

// price reads the price mapping (spec/schema/listing.json $defs/price).
func (v *checker) price(m map[string]any) Price {
	v.unknown("price", m, "model", "amount", "currency", "reviewWindowSeconds", "taskTimeoutSeconds",
		"idleTimeoutSeconds", "tools", "freePerMonth", "capPerMonth")
	var p Price
	if _, present := m["model"]; !present {
		v.add("price.model", "required: free, per_task, per_call or per_session")
	}
	p.Model = v.enum(m, "price", "model", "", ModelFree, ModelPerTask, ModelPerCall, ModelPerSession)
	if c, ok := v.str(m, "price", "currency"); ok && c != "USD" {
		v.add("price.currency", "%q: listings are priced in USD", c)
	}
	if a, ok := v.str(m, "price", "amount"); ok {
		if len(a) > maxAmountLen {
			v.add("price.amount", "at most %d characters", maxAmountLen)
		} else {
			p.Amount = &a
		}
	}
	p.ReviewWindowSeconds = v.integer(m, "price", "reviewWindowSeconds", 0, MaxWindowSeconds)
	p.TaskTimeoutSeconds = v.integer(m, "price", "taskTimeoutSeconds", 1, MaxWindowSeconds)
	p.IdleTimeoutSeconds = v.integer(m, "price", "idleTimeoutSeconds", 1, MaxWindowSeconds)
	p.FreePerMonth = v.integer(m, "price", "freePerMonth", 0, MaxFreePerMonth)
	if c, ok := v.str(m, "price", "capPerMonth"); ok {
		if !microsRE.MatchString(c) || zeroRE.MatchString(c) {
			v.add("price.capPerMonth", "%q: want USD above 0 with at most 6 decimals", c)
		} else {
			p.CapPerMonth = &c
		}
	}
	if x, present := m["tools"]; present {
		if tm := v.object("price.tools", x); tm != nil {
			if len(tm) > MaxToolPrices {
				v.add("price.tools", "at most %d tools", MaxToolPrices)
			}
			p.Tools = map[string]string{}
			names := make([]string, 0, len(tm))
			for k := range tm {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				at := "price.tools[" + fmt.Sprintf("%q", k) + "]"
				a, ok := tm[k].(string)
				switch {
				case k == "*" || !toolRE.MatchString(k):
					v.add(at, "not a tool name: the price's amount is every other tool's")
				case !ok:
					v.add(at, `want a string (quote it: "0.01")`)
				case !microsRE.MatchString(a) || zeroRE.MatchString(a):
					v.add(at, "%q: want USD above 0 with at most 6 decimals", a)
				default:
					p.Tools[k] = a
				}
			}
		}
	}
	_, amountWritten := m["amount"]
	switch p.Model {
	case ModelFree:
		if p.Amount != nil && !contains([]string{"0", "0.0", "0.00"}, *p.Amount) {
			v.add("price.amount", "%q: a free listing has no amount", *p.Amount)
		}
	case ModelPerTask:
		switch {
		case p.Amount == nil && !amountWritten:
			v.add("price.amount", "required: a task's price in USD, e.g. \"1.00\"")
		case p.Amount == nil:
		case !centsRE.MatchString(*p.Amount) || zeroRE.MatchString(*p.Amount):
			v.add("price.amount", "%q: want USD above 0 with at most 2 decimals (a task is charged in cents)", *p.Amount)
		}
	case ModelPerCall, ModelPerSession:
		switch {
		case p.Amount == nil && !amountWritten:
			v.add("price.amount", "required: the price of a %s in USD, e.g. \"0.002\"", strings.TrimPrefix(p.Model, "per_"))
		case p.Amount == nil:
		case !microsRE.MatchString(*p.Amount) || zeroRE.MatchString(*p.Amount):
			v.add("price.amount", "%q: want USD above 0 with at most 6 decimals (metered amounts are micro-USD)", *p.Amount)
		}
	}
	if p.Model != ModelPerTask && p.ReviewWindowSeconds != nil && *p.ReviewWindowSeconds != 0 {
		v.add("price.reviewWindowSeconds", "a per_task price's")
	}
	if p.Model != ModelPerCall {
		for _, k := range []string{"tools", "freePerMonth", "capPerMonth"} {
			if _, present := m[k]; present {
				v.add("price."+k, "a per_call price's")
			}
		}
	}
	return p
}

// tools reads an MCP service's tools policy.
func (v *checker) tools(m map[string]any) *ToolsPolicy {
	v.unknown("tools", m, "allow", "deny")
	t := &ToolsPolicy{Deny: []string{}}
	globs := func(key string) []string {
		x, present := m[key]
		if !present {
			return nil
		}
		xs, ok := x.([]any)
		if !ok {
			v.add("tools."+key, "want a list of tool globs")
			return nil
		}
		out := []string{}
		for i, g := range xs {
			s, ok := g.(string)
			if !ok || !toolRE.MatchString(s) {
				v.add(fmt.Sprintf("tools.%s[%d]", key, i), "want a tool glob: 1 to 128 characters, * for any run")
				continue
			}
			out = append(out, s)
		}
		return out
	}
	t.Allow = globs("allow")
	if d := globs("deny"); d != nil {
		t.Deny = d
	}
	if len(t.Allow)+len(t.Deny) > MaxGlobs {
		v.add("tools", "at most %d globs in allow and deny together", MaxGlobs)
	}
	return t
}

// Body is the request PUT /v1/services/{name} takes (Cloud's ListingPut, D-M5-95): the listing, whether the gate
// serves it, and the version it replaces (0: none). Every key the file left out is the control plane's default, and
// the keys it wrote are sent as written.
func (l *Listing) Body(version int64) ([]byte, error) {
	type price struct {
		Model               string            `json:"model"`
		Amount              *string           `json:"amount,omitempty"`
		Currency            string            `json:"currency"`
		Tools               map[string]string `json:"tools,omitempty"`
		FreePerMonth        *int64            `json:"freePerMonth,omitempty"`
		CapPerMonth         *string           `json:"capPerMonth,omitempty"`
		ReviewWindowSeconds *int64            `json:"reviewWindowSeconds,omitempty"`
		TaskTimeoutSeconds  *int64            `json:"taskTimeoutSeconds,omitempty"`
		IdleTimeoutSeconds  *int64            `json:"idleTimeoutSeconds,omitempty"`
	}
	type tools struct {
		Allow *[]string `json:"allow,omitempty"` // nil: absent (every tool); [] allows none, and is sent
		Deny  []string  `json:"deny"`
	}
	body := struct {
		Kind       string `json:"kind"`
		Visibility string `json:"visibility"`
		Price      price  `json:"price"`
		Tools      *tools `json:"tools,omitempty"`
		Tasks      string `json:"tasks,omitempty"`
		Sealed     string `json:"sealed"`
		Published  bool   `json:"published"`
		Version    int64  `json:"version"`
	}{
		Kind: l.Kind, Visibility: l.Visibility, Sealed: l.Sealed, Published: l.Published, Version: version, Tasks: l.Tasks,
		Price: price{Model: l.Price.Model, Amount: l.Price.Amount, Currency: "USD", Tools: l.Price.Tools,
			FreePerMonth: l.Price.FreePerMonth, CapPerMonth: l.Price.CapPerMonth,
			ReviewWindowSeconds: l.Price.ReviewWindowSeconds, TaskTimeoutSeconds: l.Price.TaskTimeoutSeconds,
			IdleTimeoutSeconds: l.Price.IdleTimeoutSeconds},
	}
	if l.Tools != nil {
		body.Tools = &tools{Deny: l.Tools.Deny}
		if l.Tools.Allow != nil {
			allow := l.Tools.Allow
			body.Tools.Allow = &allow
		}
	}
	return json.Marshal(body)
}
