// Package ids mints and parses the gate's identifiers (spec/relay.md §2.3, §4).
//
// Every id is minted in Go: task and context ids are UUIDv7 (Postgres before 18 has no uuidv7(), D49),
// derived buyer ids are UUIDv5. Anything a caller sends as a task id is parsed here before any query,
// so a malformed id is TaskNotFound and never a 500 (TCK CORE-GET-002).
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/medmahmoudi26/kontra/internal/unimpl"
)

// NewTaskID mints a gate task id (UUIDv7). A caller can never choose one (spec §3.4.1). UUIDv7 ids of
// one process sort by creation, in their string form and their bytes alike (google/uuid keeps a
// monotonic 12-bit sub-millisecond sequence).
func NewTaskID() (a2a.TaskID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("ids: task id: %w", err)
	}
	return a2a.TaskID(u.String()), nil
}

// NewContextID mints a gate context id (UUIDv7) for a message that carries none (D3).
func NewContextID() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("ids: context id: %w", err)
	}
	return u.String(), nil
}

// NewWorkItemID mints a work_items id (UUIDv7); it is also the kontra.message Update id (spec §14.1).
func NewWorkItemID() (uuid.UUID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("ids: work item id: %w", err)
	}
	return u, nil
}

// ParseTaskID parses a caller-supplied task id. Only the canonical form the gate mints (36 characters,
// lowercase hex, hyphenated) is a task id; anything else, including another spelling of a UUID, returns
// an *a2a.Error wrapping a2a.ErrTaskNotFound (spec §4) whose message is the same as for an unknown id,
// so the caller cannot tell a malformed id from an unknown one.
func ParseTaskID(s string) (uuid.UUID, error) {
	if len(s) != 36 {
		return uuid.Nil, notFound()
	}
	u, err := uuid.Parse(s)
	if err != nil || u.String() != s {
		return uuid.Nil, notFound()
	}
	return u, nil
}

func notFound() error { return a2a.NewError(a2a.ErrTaskNotFound, "task not found") }

// Namespace is the UUIDv5 namespace of every id the gate derives: UUIDv5(URL namespace,
// "https://kontra.sh/ns/v1"). Fixed forever: a buyer id derived from it is stored in tasks.buyer_id.
var Namespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://kontra.sh/ns/v1"))

// Derive is UUIDv5(Namespace, name) where name encodes a kind and parts unambiguously as netstrings:
// "<len>:<bytes>," for the kind and then for each part (len in bytes, decimal). The same inputs give the
// same id in every process and every release; any language can recompute it (Python:
// uuid.uuid5(ns, "".join(f"{len(p.encode())}:{p}," for p in [kind, *parts]))).
func Derive(kind string, parts ...string) uuid.UUID {
	var b strings.Builder
	for _, p := range append([]string{kind}, parts...) {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
		b.WriteByte(',')
	}
	return uuid.NewSHA1(Namespace, []byte(b.String()))
}

// AnonymousBuyer is the buyer of the `anonymous` principal (spec §2.3, D20): on a team gate one fixed id
// for every anonymous caller (pass uuid.Nil), on a multi-tenant gate UUIDv5(service tenant ‖ "anonymous").
func AnonymousBuyer(serviceTenant uuid.UUID) (uuid.UUID, error) {
	if serviceTenant == uuid.Nil {
		return Derive("anonymous"), nil
	}
	return Derive("anonymous", serviceTenant.String()), nil
}

// OIDCBuyer is UUIDv5(issuer ‖ sub) for an `oidc:sub:` principal (spec §2.3).
func OIDCBuyer(issuer, sub string) (uuid.UUID, error) {
	if issuer == "" || sub == "" {
		return uuid.Nil, fmt.Errorf("ids: an OIDC buyer needs an issuer and a subject")
	}
	return Derive("oidc", issuer, sub), nil
}

// NodeBuyer is UUIDv5(gate namespace ‖ key_id) for a node hiring without an API key (spec §2.3, M2).
func NodeBuyer(gateNamespace uuid.UUID, keyID string) (uuid.UUID, error) {
	return uuid.Nil, unimpl.Error("ids.NodeBuyer", "M2")
}

// NewReplicaID returns a replica id unique per process start, "<host>-<8 hex>" (spec §14.2): it names the
// replica's own Temporal queues kontra-tunnel-<id> and kontra-ctl-<id>. The host part is lowercased, any
// character outside [a-z0-9-] becomes '-', and it is cut to 48 characters ("gate" when nothing is left).
func NewReplicaID(host string) (string, error) {
	var r [4]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", fmt.Errorf("ids: replica id: %w", err)
	}
	return sanitizeHost(host) + "-" + hex.EncodeToString(r[:]), nil
}

func sanitizeHost(host string) string {
	b := []byte(strings.ToLower(host))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '-'
		}
	}
	s := strings.Trim(string(b), "-")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "-")
	}
	if s == "" {
		return "gate"
	}
	return s
}
