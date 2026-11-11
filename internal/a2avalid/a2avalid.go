// Package a2avalid checks A2A objects against the pinned A2A specification (the a2a-validation capability, D359), so the
// console can say of every card, request, answer, stream frame and logged event whether it is valid A2A and, if not,
// exactly why, as the A2A Inspector does.
//
// A2A 1.0 objects are checked against the specification's proto, the normative artefact of 1.0: a2a-go v2.7.0's
// a2apb/v1 descriptors, which are the pinned spec/relay.md §0 proto (specification/a2a.proto at 3303592…; D359 records
// the check). The JSON is walked against the descriptors as protojson reads it: every member must be a field of its
// message (by its JSON name or its proto name), of the field's JSON type; an enum by a value's name or number; at most
// one member of a oneof; and every field the proto marks google.api.field_behavior = REQUIRED present. Nothing is
// added to the proto's rules.
//
// A2A 0.3 objects (a 0.3 callee's) are checked against A2A 0.3.0's own JSON Schema, schema/a2a-v0.3.0.json:
// specification/json/a2a.json of a2aproject/A2A at tag v0.3.0 (210f03d4), Apache-2.0, sha256 97d6e243…, unchanged.
//
// Validation runs when a result is read, never on a call's path (the spec's "Validation Never Affects A Call"). Its
// cost (BenchmarkFrame256KiB, 11 Nov, 2 vCPU shared with a TCK run): a 256 KiB artifact frame, traffic.body_max's
// default, about 1.5 ms; the same size in 2000 small data parts, the walk's worst shape, about 9 to 13 ms.
package a2avalid

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Versions.
const (
	V10 = "1.0"
	V03 = "0.3"
)

// Rules an issue names.
const (
	RuleRequired  = "required member missing"
	RuleUnknown   = "unknown member"
	RuleType      = "wrong type"
	RuleEnum      = "unknown enum value"
	RuleOneof     = "more than one member of a oneof"
	RuleDuplicate = "duplicate member"
	RuleNotJSON   = "not JSON"
	RuleEnvelope  = "not a JSON-RPC 2.0 envelope"
	RuleSchema    = "schema"
)

// Issue is one way an object breaks the specification: where (a JSON path from $), the rule, and the
// specification's name for the member (Message.messageId).
type Issue struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Member string `json:"member,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Result is what validation says of one object.
type Result struct {
	Version   string  `json:"version"`        // the specification checked against: 1.0 or 0.3
	Validated bool    `json:"validated"`      // false: not checked (Note says why)
	Valid     bool    `json:"valid"`          // with Validated: no issue
	Note      string  `json:"note,omitempty"` // why it was not validated, or what was checked
	Issues    []Issue `json:"issues,omitempty"`
}

// Notes of an object that is not validated.
const (
	NoteTruncated = "not validated: the body was kept only up to traffic.body_max"
	NoteEmpty     = "not validated: no body"
	NoteVersion   = "not validated: no A2A specification is pinned for this version"
	NoteMethod    = "not validated: not an A2A method of this version"
)

func notValidated(version, note string) Result { return Result{Version: version, Note: note} }

func result(version string, issues []Issue) Result {
	if len(issues) > maxIssues {
		issues = append(issues[:maxIssues], Issue{Path: "$", Rule: "more issues", Detail: "only the first 50 are listed"})
	}
	return Result{Version: version, Validated: true, Valid: len(issues) == 0, Issues: issues}
}

const maxIssues = 50

// Normalize is the specification an object of version v is checked against: 1.0 for 1.0 and 1.0.x (and for an
// unknown or absent version on the caller's leg, which the gate serves as 1.0 alone), 0.3 for 0.3 and 0.3.x.
func Normalize(v string) string {
	switch {
	case v == "" || v == "1" || v == "1.0" || strings.HasPrefix(v, "1.0."):
		return V10
	case v == "0.3" || strings.HasPrefix(v, "0.3."):
		return V03
	}
	return v
}

// ---------------------------------------------------------------- 1.0: the proto

var (
	once     sync.Once
	file     protoreflect.FileDescriptor
	methods  map[string]protoreflect.MethodDescriptor
	pathVars map[string]map[string]bool // per method, the request fields an HTTP+JSON path carries
	varRE    = regexp.MustCompile(`\{([a-z_]+)`)
)

func load() {
	file = a2apb.File_a2av1_proto
	methods = map[string]protoreflect.MethodDescriptor{}
	pathVars = map[string]map[string]bool{}
	svc := file.Services().Get(0)
	for i := 0; i < svc.Methods().Len(); i++ {
		m := svc.Methods().Get(i)
		name := string(m.Name())
		methods[name] = m
		vars := map[string]bool{}
		if opts, ok := m.Options().(*descriptorpb.MethodOptions); ok && opts != nil {
			if rule, ok := proto.GetExtension(opts, annotations.E_Http).(*annotations.HttpRule); ok && rule != nil {
				for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
					for _, p := range []string{r.GetGet(), r.GetPost(), r.GetDelete(), r.GetPut(), r.GetPatch()} {
						for _, v := range varRE.FindAllStringSubmatch(p, -1) {
							vars[v[1]] = true
						}
					}
				}
			}
		}
		pathVars[name] = vars
	}
}

func message(name string) protoreflect.MessageDescriptor {
	once.Do(load)
	return file.Messages().ByName(protoreflect.Name(name))
}

// Card checks an agent card.
func Card(data []byte, version string) Result {
	switch Normalize(version) {
	case V10:
		return object(V10, data, message("AgentCard"), nil)
	case V03:
		return schema(data, "AgentCard")
	}
	return notValidated(version, NoteVersion)
}

// Event checks a logged task event's payload: a 1.0 StreamResponse with gate ids.
func Event(payload []byte) Result { return object(V10, payload, message("StreamResponse"), nil) }

// Request checks a request body of method on binding ("jsonrpc" or "rest") in version.
func Request(version, binding, method string, body []byte, truncated bool) Result {
	return check(version, binding, method, body, truncated, partRequest)
}

// Response checks an answer body (not a stream).
func Response(version, binding, method string, body []byte, truncated bool) Result {
	return check(version, binding, method, body, truncated, partResponse)
}

// Frame checks one stream frame's data.
func Frame(version, binding, method string, data []byte, truncated bool) Result {
	return check(version, binding, method, data, truncated, partFrame)
}

type part int

const (
	partRequest part = iota
	partResponse
	partFrame
)

func check(version, binding, method string, body []byte, truncated bool, p part) Result {
	v := Normalize(version)
	if truncated {
		return notValidated(v, NoteTruncated)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return notValidated(v, NoteEmpty)
	}
	switch v {
	case V10:
		return check10(binding, method, body, p)
	case V03:
		return check03(binding, method, body, p)
	}
	return notValidated(version, NoteVersion)
}

func check10(binding, method string, body []byte, p part) Result {
	once.Do(load)
	m, ok := methods[method]
	if !ok {
		return notValidated(V10, NoteMethod)
	}
	n, err := parse(body)
	if err != nil {
		return result(V10, []Issue{{Path: "$", Rule: RuleNotJSON, Detail: err.Error()}})
	}
	target, skip := m.Output(), map[string]bool(nil)
	if p == partRequest {
		target = m.Input()
		if binding != "jsonrpc" {
			skip = pathVars[method] // carried by the path, not the body
		}
	}
	var w walker
	switch {
	case binding == "jsonrpc":
		w.envelope(n, p, target)
	case p != partRequest && isStatusError(n):
		w.statusError(n)
	default:
		w.message(n, target, skip)
	}
	return result(V10, w.issues)
}

// member returns the value of an object's member key, nil when it has none.
func (n *node) get(key string) *node {
	for _, m := range n.members {
		if m.key == key {
			return m.val
		}
	}
	return nil
}

// isStatusError reports whether an HTTP+JSON answer is an error: {"error": google.rpc.Status}.
func isStatusError(n *node) bool {
	return n.kind == kObject && len(n.members) == 1 && n.members[0].key == "error"
}

// statusError checks an HTTP+JSON error: {"error": google.rpc.Status} (AIP-193, as A2A 1.0's HTTP+JSON binding).
func (w *walker) statusError(n *node) {
	e := n.get("error")
	w.push("error")
	defer w.pop()
	if e == nil || e.kind != kObject {
		w.add(RuleType, "google.rpc.Status", "an object")
		return
	}
	if c := e.get("code"); c == nil || c.kind != kNumber || !intLike(c.str) {
		w.at("code", RuleType, "google.rpc.Status.code", "an integer")
	}
	if m := e.get("message"); m == nil || m.kind != kString {
		w.at("message", RuleType, "google.rpc.Status.message", "a string")
	}
}

// envelope checks a JSON-RPC 2.0 request (part request: jsonrpc, id, method, params) or answer (result or error), the
// params or result as target.
func (w *walker) envelope(n *node, p part, target protoreflect.MessageDescriptor) {
	if n.kind != kObject {
		w.add(RuleEnvelope, "", "an object")
		return
	}
	if v := n.get("jsonrpc"); v == nil || v.kind != kString || v.str != "2.0" {
		w.at("jsonrpc", RuleEnvelope, "", `jsonrpc must be "2.0"`)
	}
	if id := n.get("id"); id != nil && id.kind != kString && id.kind != kNumber && id.kind != kNull {
		w.at("id", RuleEnvelope, "", "id must be a string, a number or null")
	}
	if p == partRequest {
		if m := n.get("method"); m == nil || m.kind != kString {
			w.at("method", RuleEnvelope, "", "method must be a string")
		}
		params := n.get("params")
		if params == nil {
			w.at("params", RuleRequired, "", "params")
			return
		}
		w.push("params")
		w.message(params, target, nil)
		w.pop()
		return
	}
	res, errv := n.get("result"), n.get("error")
	switch {
	case (res == nil) == (errv == nil):
		w.add(RuleEnvelope, "", "exactly one of result and error")
	case errv != nil:
		c, m := (*node)(nil), (*node)(nil)
		if errv.kind == kObject {
			c, m = errv.get("code"), errv.get("message")
		}
		if c == nil || c.kind != kNumber || !intLike(c.str) || m == nil || m.kind != kString {
			w.at("error", RuleEnvelope, "", "error must have an integer code and a string message")
		}
	default:
		w.push("result")
		w.message(res, target, nil)
		w.pop()
	}
}

// object checks body as one message md, the fields in skip not required.
func object(version string, body []byte, md protoreflect.MessageDescriptor, skip map[string]bool) Result {
	if md == nil {
		return notValidated(version, NoteMethod)
	}
	n, err := parse(body)
	if err != nil {
		return result(version, []Issue{{Path: "$", Rule: RuleNotJSON, Detail: err.Error()}})
	}
	var w walker
	w.message(n, md, skip)
	return result(version, w.issues)
}

// walker collects issues; path is the current JSON path as segments (".key" or "[i]"), joined only for an issue.
type walker struct {
	issues []Issue
	path   []string
}

func (w *walker) push(key string) { w.path = append(w.path, "."+key) }
func (w *walker) index(i int)     { w.path = append(w.path, "["+strconv.Itoa(i)+"]") }
func (w *walker) pop()            { w.path = w.path[:len(w.path)-1] }

func (w *walker) add(rule, member, detail string) {
	if len(w.issues) <= maxIssues {
		w.issues = append(w.issues, Issue{Path: "$" + strings.Join(w.path, ""), Rule: rule, Member: member, Detail: detail})
	}
}

// at adds an issue at the member key of the current object.
func (w *walker) at(key, rule, member, detail string) {
	w.push(key)
	w.add(rule, member, detail)
	w.pop()
}

func memberName(md protoreflect.MessageDescriptor, fd protoreflect.FieldDescriptor) string {
	return string(md.Name()) + "." + fd.JSONName()
}

var requiredCache sync.Map // protoreflect.FullName -> bool

func required(fd protoreflect.FieldDescriptor) bool {
	if v, ok := requiredCache.Load(fd.FullName()); ok {
		return v.(bool)
	}
	r := requiredOf(fd)
	requiredCache.Store(fd.FullName(), r)
	return r
}

func requiredOf(fd protoreflect.FieldDescriptor) bool {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil {
		return false
	}
	for _, b := range proto.GetExtension(opts, annotations.E_FieldBehavior).([]annotations.FieldBehavior) {
		if b == annotations.FieldBehavior_REQUIRED {
			return true
		}
	}
	return false
}

// message checks n as a message md at the current path.
func (w *walker) message(n *node, md protoreflect.MessageDescriptor, skip map[string]bool) {
	if wk := wellKnown(md); wk != nil {
		wk(w, n)
		return
	}
	if n.kind != kObject {
		w.add(RuleType, string(md.Name()), "an object")
		return
	}
	fields := md.Fields()
	var seen map[protoreflect.FieldNumber]string
	var oneofs map[protoreflect.Name]string
	for _, m := range n.members {
		fd := fields.ByJSONName(m.key)
		if fd == nil {
			fd = fields.ByName(protoreflect.Name(m.key))
		}
		if fd == nil {
			w.at(m.key, RuleUnknown, string(md.Name())+"."+m.key, "")
			continue
		}
		if seen == nil {
			seen = map[protoreflect.FieldNumber]string{}
		}
		if prev, dup := seen[fd.Number()]; dup {
			w.at(m.key, RuleDuplicate, memberName(md, fd), "also given as "+prev)
			continue
		}
		seen[fd.Number()] = m.key
		if m.val.kind == kNull && (fd.Message() == nil || fd.Message().FullName() != "google.protobuf.Value") {
			continue // null is the field's default
		}
		if od := fd.ContainingOneof(); od != nil && !od.IsSynthetic() {
			if oneofs == nil {
				oneofs = map[protoreflect.Name]string{}
			}
			if other, ok := oneofs[od.Name()]; ok {
				w.at(m.key, RuleOneof, string(md.Name())+"."+string(od.Name()), "also "+other)
			}
			oneofs[od.Name()] = m.key
		}
		w.push(m.key)
		w.field(m.val, md, fd)
		w.pop()
	}
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if _, ok := seen[fd.Number()]; ok || !required(fd) || skip[string(fd.Name())] {
			continue
		}
		w.at(fd.JSONName(), RuleRequired, memberName(md, fd), "")
	}
}

func (w *walker) field(n *node, md protoreflect.MessageDescriptor, fd protoreflect.FieldDescriptor) {
	switch {
	case fd.IsMap():
		if n.kind != kObject {
			w.add(RuleType, memberName(md, fd), "an object (a map)")
			return
		}
		for _, m := range n.members {
			w.push(m.key)
			w.single(m.val, fd.MapValue(), memberName(md, fd))
			w.pop()
		}
	case fd.IsList():
		if n.kind != kArray {
			w.add(RuleType, memberName(md, fd), "an array")
			return
		}
		for i, it := range n.items {
			w.index(i)
			w.single(it, fd, memberName(md, fd))
			w.pop()
		}
	default:
		w.single(n, fd, memberName(md, fd))
	}
}

func (w *walker) single(n *node, fd protoreflect.FieldDescriptor, member string) {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		w.message(n, fd.Message(), nil)
	case protoreflect.EnumKind:
		switch n.kind {
		case kString:
			if fd.Enum().Values().ByName(protoreflect.Name(n.str)) == nil {
				w.add(RuleEnum, member, fmt.Sprintf("%q is not a value of %s", n.str, fd.Enum().Name()))
			}
		case kNumber:
			if !intLike(n.str) {
				w.add(RuleEnum, member, "not an integer")
			}
		default:
			w.add(RuleType, member, "an enum value name")
		}
	case protoreflect.StringKind:
		if n.kind != kString {
			w.add(RuleType, member, "a string")
		}
	case protoreflect.BoolKind:
		if n.kind != kBool {
			w.add(RuleType, member, "a boolean")
		}
	case protoreflect.BytesKind:
		if n.kind != kString || !base64OK(n.str) {
			w.add(RuleType, member, "a base64 string")
		}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		if !(n.kind == kNumber || n.kind == kString && floatString(n.str)) {
			w.add(RuleType, member, "a number")
		}
	default: // the integer kinds
		if !(n.kind == kNumber && intLike(n.str) || n.kind == kString && intLike(n.str)) {
			w.add(RuleType, member, "an integer")
		}
	}
}

// wellKnown checks the well-known types as protojson maps them.
func wellKnown(md protoreflect.MessageDescriptor) func(*walker, *node) {
	switch md.FullName() {
	case "google.protobuf.Struct":
		return func(w *walker, n *node) {
			if n.kind != kObject {
				w.add(RuleType, "google.protobuf.Struct", "an object")
			}
		}
	case "google.protobuf.Value":
		return func(*walker, *node) {}
	case "google.protobuf.ListValue":
		return func(w *walker, n *node) {
			if n.kind != kArray {
				w.add(RuleType, "google.protobuf.ListValue", "an array")
			}
		}
	case "google.protobuf.Timestamp":
		return func(w *walker, n *node) {
			if n.kind != kString {
				w.add(RuleType, "google.protobuf.Timestamp", "an RFC 3339 string")
				return
			}
			if _, err := time.Parse(time.RFC3339Nano, n.str); err != nil {
				w.add(RuleType, "google.protobuf.Timestamp", "an RFC 3339 string")
			}
		}
	case "google.protobuf.Empty":
		return func(w *walker, n *node) {
			if n.kind != kObject || len(n.members) != 0 {
				w.add(RuleType, "google.protobuf.Empty", "an empty object")
			}
		}
	}
	return nil
}

func intLike(s string) bool {
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseUint(s, 10, 64); err == nil {
		return true
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == math.Trunc(f) && !math.IsInf(f, 0)
}

func floatString(s string) bool {
	if s == "NaN" || s == "Infinity" || s == "-Infinity" {
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func base64OK(s string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if _, err := enc.DecodeString(s); err == nil {
			return true
		}
	}
	return false
}
