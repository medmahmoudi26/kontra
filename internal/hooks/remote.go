package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/version"
)

// ErrAdmissionUnavailable is an Admission hook that failed or answered nothing the gate may act on: a timeout, a
// non-2xx (a 3xx included), an answer that is not a valid decision, maps outside §13.2's rules, a malformed or expired
// quote. Every caller answers it -32603 "admission unavailable" (HTTP 503): Admission fails closed (spec §13.1).
var ErrAdmissionUnavailable = errors.New("hooks: admission unavailable")

// ErrInvalidDecision is a decision that is not valid against spec/schema/hooks.json's decision; it wraps into
// ErrAdmissionUnavailable when the remote hook answers one.
var ErrInvalidDecision = errors.New("hooks: not a valid Admission decision")

// MsgPricedTasks is the refusal of a quote outside operation task on a gate-owned task (spec §13.1).
const MsgPricedTasks = "priced tasks need tasks: durable"

// CodeInternalError is JSON-RPC's -32603, the code of every Admission refusal the gate itself makes.
const CodeInternalError = -32603

// MaxDecisionBytes bounds the hook's answer (spec §10.3, D284: 64 KiB; metadata and mcpMeta are 16 KiB each).
const MaxDecisionBytes = 64 << 10

// DefaultAdmissionTimeout is hooks.admission.timeout's default (spec §13.1).
const DefaultAdmissionTimeout = 5 * time.Second

// MaxAdmissionTimeout is hooks.admission.timeout's bound (spec §13.1, D284): the MCP relay's shared Admission session
// decision for modern clients runs at most 30 s (mcprelay's admitFlightTimeout), so a longer hook timeout would be cut
// there, silently, and not on A2A or legacy MCP.
const MaxAdmissionTimeout = 30 * time.Second

// RemoteAdmissionConfig is hooks.admission (spec/relay.md §13.1): the operator's control plane, never derived
// from a request and never a node.
type RemoteAdmissionConfig struct {
	URL        string        // https://, or http:// on a loopback host only
	Secret     string        // the value of secret_env, sent as Authorization: Bearer
	Timeout    time.Duration // default 5 s
	Operations []Operation   // which of task, session, call consult the hook (default task)
	Rules      KeyRules      // §13.2's key and size rules for metadata and mcpMeta
	// Client dials the hook. Nil is §10.3's operator-configured endpoint client (ssrf.NewOperatorClient, D284):
	// no address check, https:// or http:// to loopback, no redirects, no proxy, the timeout above. A client of the
	// caller's (tests) gets no redirects all the same.
	Client *http.Client
	// Now is the clock a quote's expiresAt is checked against; nil is time.Now.
	Now func() time.Time
}

// String shows the configuration without its secret, so that no log line or error carries it.
func (c RemoteAdmissionConfig) String() string {
	return fmt.Sprintf("hooks.admission{url: %s, secret: [redacted], timeout: %v, operations: %v}", c.URL, c.Timeout, c.Operations)
}

// GoString is String, for %#v.
func (c RemoteAdmissionConfig) GoString() string { return c.String() }

// LogValue is the configuration for slog, without its secret.
func (c RemoteAdmissionConfig) LogValue() slog.Value {
	return slog.GroupValue(slog.String("url", c.URL), slog.String("secret", "[redacted]"),
		slog.Duration("timeout", c.Timeout), slog.Any("operations", c.Operations))
}

// RemoteAdmission is the remote Admission hook (§13.1; built in M4, D275). It POSTs the AdmitRequest and reads a
// Decision, and fails closed: a timeout, a non-2xx, an unparsable answer, a key outside §13.2's rules or a map over
// 16 KiB is a deny with -32603 "admission unavailable" (HTTP 503). A quote for any operation but task on a
// gate-owned task (an A2A task, a hire, an MCP task with tasks: durable) is a deny with -32603 "priced tasks need
// tasks: durable". An operation the hook does not consult is answered as BuiltinAdmission answers it.
//
// The unavailable cases are returned as an error wrapping ErrAdmissionUnavailable (every caller maps an Admission
// error to -32603 "admission unavailable", 503), never as a Decision, so that no caller can mistake one for the
// operator's own deny. The hook is §10.3's operator-configured endpoint (D284), and its answer is read as
// spec/schema/hooks.json's decision with exact keys (ParseDecision); a quote must name an absolute https:// URL
// without userinfo and an RFC 3339 expiry in the future (CheckQuote). The approve URL is passed through untouched
// (D188).
type RemoteAdmission struct {
	url     string
	secret  string
	timeout time.Duration
	ops     []Operation
	rules   KeyRules
	client  *http.Client
	now     func() time.Time
}

var _ Admission = (*RemoteAdmission)(nil)

// NewRemoteAdmission checks cfg (the URL's scheme and host, a non-empty secret, the operations).
func NewRemoteAdmission(cfg RemoteAdmissionConfig) (*RemoteAdmission, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("hooks: hooks.admission.url: not a URL")
	}
	if err := ssrf.CheckOperatorURL(u); err != nil {
		return nil, fmt.Errorf("hooks: hooks.admission.url %s: %w (https:// unless the host is loopback, spec §13.1)", shownURL(u), err)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, fmt.Errorf("hooks: hooks.admission.url %s: no fragment", shownURL(u))
	}
	if cfg.Secret == "" {
		return nil, errors.New("hooks: hooks.admission: the secret is empty (set the variable secret_env names)")
	}
	if strings.ContainsFunc(cfg.Secret, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		return nil, errors.New("hooks: hooks.admission: the secret holds white space or a control character")
	}
	timeout := cfg.Timeout
	switch {
	case timeout == 0:
		timeout = DefaultAdmissionTimeout
	case timeout < 0:
		return nil, errors.New("hooks: hooks.admission.timeout: must be positive")
	case timeout > MaxAdmissionTimeout:
		return nil, fmt.Errorf("hooks: hooks.admission.timeout %v: at most %v", timeout, MaxAdmissionTimeout)
	}
	ops := cfg.Operations
	if len(ops) == 0 {
		ops = []Operation{OpTask}
	}
	var seen []Operation
	for _, op := range ops {
		switch op {
		case OpTask, OpSession, OpCall:
		default:
			return nil, fmt.Errorf("hooks: hooks.admission.operations: %q is not task, session or call", op)
		}
		if !slices.Contains(seen, op) {
			seen = append(seen, op)
		}
	}
	var client *http.Client
	if cfg.Client == nil {
		client = ssrf.NewOperatorClient(ssrf.OperatorOptions{Timeout: timeout})
	} else {
		c := *cfg.Client
		c.CheckRedirect = ssrf.NoRedirects
		client = &c
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &RemoteAdmission{url: u.String(), secret: cfg.Secret, timeout: timeout, ops: seen, rules: cfg.Rules,
		client: client, now: now}, nil
}

// shownURL is u for an error or a log line, without its userinfo.
func shownURL(u *url.URL) string {
	c := *u
	c.User = nil
	return strings.TrimSpace(c.Redacted())
}

// Consults reports whether op is one the hook is asked about.
func (r *RemoteAdmission) Consults(op Operation) bool {
	return slices.Contains(r.ops, op)
}

// Operations are the operations the hook is asked about.
func (r *RemoteAdmission) Operations() []Operation { return slices.Clone(r.ops) }

// String shows the hook without its secret.
func (r *RemoteAdmission) String() string {
	return fmt.Sprintf("remote Admission %s (secret [redacted], operations %v, timeout %v)", r.url, r.ops, r.timeout)
}

// GoString is String, for %#v, which would otherwise print every field, the secret included.
func (r *RemoteAdmission) GoString() string { return r.String() }

// LogValue is the hook for slog, without its secret.
func (r *RemoteAdmission) LogValue() slog.Value {
	return slog.GroupValue(slog.String("url", r.url), slog.String("secret", "[redacted]"),
		slog.Duration("timeout", r.timeout), slog.Any("operations", r.ops))
}

// Admit implements Admission.
func (r *RemoteAdmission) Admit(ctx context.Context, req AdmitRequest) (Decision, error) {
	if req.Operation == "" {
		return Decision{}, errors.New("hooks: an admission request needs its operation")
	}
	if !r.Consults(req.Operation) {
		return BuiltinAdmission{}.Admit(ctx, req)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, r.unavailable("the request is not JSON", err)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, r.unavailable("the request", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("Authorization", "Bearer "+r.secret)
	hr.Header.Set("User-Agent", "kontra-gate/"+version.Get())
	resp, err := r.client.Do(hr)
	if err != nil {
		return Decision{}, r.unavailable("the hook did not answer", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return Decision{}, r.unavailable(fmt.Sprintf("the hook answered HTTP %d", resp.StatusCode), nil)
	}
	b, err := ssrf.ReadBody(resp.Body, MaxDecisionBytes)
	if err != nil {
		return Decision{}, r.unavailable("the answer", err)
	}
	d, err := ParseDecision(b)
	if err != nil {
		return Decision{}, r.unavailable("the answer", err)
	}
	if err := r.rules.CheckOperatorMaps(d.Metadata, d.MCPMeta); err != nil {
		return Decision{}, r.unavailable("the decision's maps", err)
	}
	if d.Outcome == Quote {
		if req.Operation != OpTask {
			// A quote is valid only for a gate-owned task (§13.1); a passthrough task, which the hook is also asked
			// about with operation task, is refused the same way by the MCP relay.
			return Decision{Outcome: Deny, Refusal: &Refusal{Code: CodeInternalError, Message: MsgPricedTasks}}, nil
		}
		if err := CheckQuote(d.Quote, r.now()); err != nil {
			return Decision{}, r.unavailable("the quote", err)
		}
	}
	return d, nil
}

// unavailable is the error of a hook that failed: ErrAdmissionUnavailable with what failed and why. Its text never
// holds the secret: a cause that might quote it (a hook that echoes the Authorization header into a key of its answer,
// which an error quotes, escaped, up to 32 bytes or whole) is withheld.
func (r *RemoteAdmission) unavailable(what string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrAdmissionUnavailable, what)
	}
	msg := cause.Error()
	if quotesSecret(msg, r.secret) {
		msg = "[the detail is withheld: it may quote the hook's secret]"
	}
	return fmt.Errorf("%w: %s: %s", ErrAdmissionUnavailable, what, msg)
}

// quotesSecret reports whether msg holds the secret or any 8 consecutive bytes of it (all of it when shorter), as
// written or in a form an error quotes it in: Go-quoted (strconv.Quote, %q and strictjson.Quote, which escape " and
// \), ASCII-quoted (%+q) or JSON-escaped. A window of an escaped form may stand for fewer than 8 bytes of the secret,
// which only withholds more.
func quotesSecret(msg, secret string) bool {
	if secret == "" {
		return false
	}
	for _, form := range secretForms(secret) {
		n := min(8, len(form))
		for i := 0; i+n <= len(form); i++ {
			if strings.Contains(msg, form[i:i+n]) {
				return true
			}
		}
	}
	return false
}

// secretForms are the secret as written and as each quoting an error may use escapes it, without the quotes.
func secretForms(secret string) []string {
	forms := []string{secret}
	add := func(quoted string) {
		if s := quoted[1 : len(quoted)-1]; !slices.Contains(forms, s) {
			forms = append(forms, s)
		}
	}
	add(strconv.Quote(secret))
	add(strconv.QuoteToASCII(secret))
	if b, err := json.Marshal(secret); err == nil {
		add(string(b))
	}
	return forms
}

// decisionMembers are the members of hooks.json's decision; refusalMembers and quoteMembers those of its refusal and
// quote (additionalProperties: false, exact keys).
var (
	decisionMembers = []string{"outcome", "refusal", "metadata", "mcpMeta", "quote"}
	refusalMembers  = []string{"code", "message"}
	quoteMembers    = []string{"approveUrl", "expiresAt"}
)

// ParseDecision reads b as spec/schema/hooks.json's decision with exact keys: one JSON object; no repeated or
// case-variant key at its level or in refusal, quote, metadata and mcpMeta; no member the schema does not name;
// outcome allow, deny or quote; a deny with its refusal {code: integer, message: string}, a quote with its terms
// {approveUrl: string, expiresAt: string}; metadata and mcpMeta objects, mcpMeta's keys under sh.kontra/. A
// refusal on another outcome than deny, and quote terms on another than quote, are dropped. Anything else is
// ErrInvalidDecision. The quote's terms are checked by CheckQuote, the maps' keys and sizes by
// KeyRules.CheckOperatorMaps.
func ParseDecision(b []byte) (Decision, error) {
	top, err := strictjson.Members(b)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	if err := strictjson.CheckMembers(top, strictjson.Path{"refusal"}, strictjson.Path{"quote"},
		strictjson.Path{"metadata"}, strictjson.Path{"mcpMeta"}); err != nil {
		return Decision{}, fmt.Errorf("%w: %v", ErrInvalidDecision, err)
	}
	if err := onlyMembers(top, decisionMembers, ""); err != nil {
		return Decision{}, err
	}
	var d Decision
	raw, ok := top["outcome"]
	if !ok {
		return Decision{}, fmt.Errorf("%w: no outcome", ErrInvalidDecision)
	}
	var outcome string
	if err := json.Unmarshal(raw, &outcome); err != nil {
		return Decision{}, fmt.Errorf("%w: outcome is not a string", ErrInvalidDecision)
	}
	switch d.Outcome = Outcome(outcome); d.Outcome {
	case Allow, Deny, Quote:
	default:
		return Decision{}, fmt.Errorf("%w: outcome %s is not allow, deny or quote", ErrInvalidDecision, strictjson.Quote(outcome))
	}
	if raw, ok := top["refusal"]; ok {
		m, err := object(raw, "refusal")
		if err != nil {
			return Decision{}, err
		}
		if err := onlyMembers(m, refusalMembers, "refusal."); err != nil {
			return Decision{}, err
		}
		var rf Refusal
		code, okc := m["code"]
		msg, okm := m["message"]
		if !okc || !okm {
			return Decision{}, fmt.Errorf("%w: a refusal needs its code and message", ErrInvalidDecision)
		}
		n, isInt := integer(code)
		if !isInt {
			return Decision{}, fmt.Errorf("%w: refusal.code is not an integer", ErrInvalidDecision)
		}
		rf.Code = n
		if err := json.Unmarshal(msg, &rf.Message); err != nil || !isString(msg) {
			return Decision{}, fmt.Errorf("%w: refusal.message is not a string", ErrInvalidDecision)
		}
		d.Refusal = &rf
	}
	if raw, ok := top["quote"]; ok {
		m, err := object(raw, "quote")
		if err != nil {
			return Decision{}, err
		}
		if err := onlyMembers(m, quoteMembers, "quote."); err != nil {
			return Decision{}, err
		}
		var q QuoteTerms
		au, oka := m["approveUrl"]
		ex, oke := m["expiresAt"]
		if !oka || !oke {
			return Decision{}, fmt.Errorf("%w: a quote needs its approveUrl and expiresAt", ErrInvalidDecision)
		}
		if !isString(au) || json.Unmarshal(au, &q.ApproveURL) != nil {
			return Decision{}, fmt.Errorf("%w: quote.approveUrl is not a string", ErrInvalidDecision)
		}
		if !isString(ex) || json.Unmarshal(ex, &q.ExpiresAt) != nil {
			return Decision{}, fmt.Errorf("%w: quote.expiresAt is not a string", ErrInvalidDecision)
		}
		d.Quote = &q
	}
	for name, dst := range map[string]*map[string]any{"metadata": &d.Metadata, "mcpMeta": &d.MCPMeta} {
		raw, ok := top[name]
		if !ok {
			continue
		}
		if _, err := object(raw, name); err != nil {
			return Decision{}, err
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return Decision{}, fmt.Errorf("%w: %s: %v", ErrInvalidDecision, name, err)
		}
		*dst = m
	}
	for k := range d.MCPMeta {
		if !strings.HasPrefix(k, MCPMetaPrefix) {
			return Decision{}, fmt.Errorf("%w: mcpMeta key %s is not under %s", ErrInvalidDecision, strictjson.Quote(k), MCPMetaPrefix)
		}
	}
	switch d.Outcome {
	case Deny:
		if d.Refusal == nil {
			return Decision{}, fmt.Errorf("%w: a deny needs its refusal", ErrInvalidDecision)
		}
		d.Quote = nil
	case Quote:
		if d.Quote == nil {
			return Decision{}, fmt.Errorf("%w: a quote needs its terms", ErrInvalidDecision)
		}
		d.Refusal = nil
	default:
		d.Refusal, d.Quote = nil, nil
	}
	return d, nil
}

// object returns raw's members, refusing anything but a JSON object.
func object(raw json.RawMessage, name string) (map[string]json.RawMessage, error) {
	m, err := strictjson.Members(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not an object", ErrInvalidDecision, name)
	}
	return m, nil
}

// onlyMembers refuses a member of m that allowed does not name (exact keys).
func onlyMembers(m map[string]json.RawMessage, allowed []string, at string) error {
	for k := range m {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("%w: unknown member %s%s", ErrInvalidDecision, at, strictjson.Quote(k))
		}
	}
	return nil
}

// isString reports whether raw is a JSON string.
func isString(raw json.RawMessage) bool {
	raw = bytes.TrimLeft(raw, " \t\r\n")
	return len(raw) > 0 && raw[0] == '"'
}

// integer reads raw, one JSON value, as hooks.json's integer: a JSON number whose value has no fractional part (JSON
// Schema 2020-12, so -32001.0 and 1e2 are integers and 1.5 is not), read exactly and within int's range. Nothing else
// is one: not null, which json.Unmarshal leaves an int untouched for (a refusal read as code 0), nor a string or a
// boolean.
func integer(raw json.RawMessage) (int, bool) {
	s := string(bytes.TrimSpace(raw))
	if s == "" || s[0] != '-' && (s[0] < '0' || s[0] > '9') || !json.Valid([]byte(s)) {
		return 0, false
	}
	// A JSON number: -?digits(.digits)?([eE][+-]?digits)?, whose value is ±(whole frac) × 10^(exp - len(frac)).
	neg := s[0] == '-'
	s = strings.TrimPrefix(s, "-")
	mant, expText, hasExp := strings.Cut(strings.ToLower(s), "e")
	whole, frac, _ := strings.Cut(mant, ".")
	digits := strings.TrimLeft(whole+frac, "0")
	if digits == "" {
		return 0, true // zero, however it is written
	}
	exp := 0
	if hasExp {
		e, err := strconv.Atoi(expText)
		if err != nil || e < -1<<20 || e > 1<<20 {
			return 0, false // past int's range, or a fraction no 64 KiB answer can write the zeros of
		}
		exp = e
	}
	for exp -= len(frac); exp < 0; exp++ {
		if digits[len(digits)-1] != '0' {
			return 0, false // a fractional part
		}
		digits = digits[:len(digits)-1] // never empties: digits starts with a non-zero digit
	}
	if len(digits)+exp > 19 {
		return 0, false // past int64's range
	}
	digits += strings.Repeat("0", exp)
	if neg {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, 10, strconv.IntSize)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// ErrBadQuote is a quote the gate may not create a task from.
var ErrBadQuote = errors.New("hooks: the quote's terms are not usable")

// rfc3339 is RFC 3339's date-time (§5.6) as every reader of a quote's expiresAt parses it, with time.RFC3339 (the A2A
// queue, the MCP relay): upper-case T and Z, a fraction only after a period, an offset up to ±23:59. time.Parse alone
// also takes a comma fraction, a one-digit hour and an offset of +24:00 or with minute 60 (Go issue 54580), which
// RFC 3339 and hooks.json's format date-time do not. Lower-case t and z, which they allow, no reader parses: refused,
// failing closed. The ranges of the date and time fields are time.Parse's.
var rfc3339 = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

// CheckQuote checks a quote decision's terms (spec §13.1, D284): approveUrl an absolute https:// URL (the scheme
// written in lowercase, as hooks.json's pattern says) with a host, no userinfo, a valid port and nothing but URI
// characters; expiresAt an RFC 3339 time (rfc3339) after now. The URL is otherwise the operator's and passed through
// untouched (D188).
func CheckQuote(q *QuoteTerms, now time.Time) error {
	if q == nil {
		return fmt.Errorf("%w: no terms", ErrBadQuote)
	}
	if !strings.HasPrefix(q.ApproveURL, "https://") {
		return fmt.Errorf("%w: approveUrl is not an https:// URL", ErrBadQuote)
	}
	if strings.ContainsFunc(q.ApproveURL, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) {
		return fmt.Errorf("%w: approveUrl holds a character a URI cannot", ErrBadQuote)
	}
	u, err := url.Parse(q.ApproveURL)
	switch {
	case err != nil:
		return fmt.Errorf("%w: approveUrl is not a URL", ErrBadQuote)
	case u.User != nil:
		return fmt.Errorf("%w: approveUrl carries userinfo", ErrBadQuote)
	case u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "":
		return fmt.Errorf("%w: approveUrl is not an absolute https:// URL", ErrBadQuote)
	}
	if err := ssrf.CheckOperatorURL(u); err != nil {
		return fmt.Errorf("%w: approveUrl: %v", ErrBadQuote, err)
	}
	if !rfc3339.MatchString(q.ExpiresAt) {
		return fmt.Errorf("%w: expiresAt is not an RFC 3339 time", ErrBadQuote)
	}
	exp, err := time.Parse(time.RFC3339, q.ExpiresAt)
	if err != nil {
		return fmt.Errorf("%w: expiresAt is not an RFC 3339 time", ErrBadQuote)
	}
	if !exp.After(now) {
		return fmt.Errorf("%w: expiresAt %s has passed", ErrBadQuote, exp.UTC().Format(time.RFC3339))
	}
	return nil
}
