package ids

import (
	"bytes"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
)

// TCK CORE-GET-002 sends a task id that is not a UUID: TaskNotFound, never a parse error.
func TestParseTaskIDMalformedIsTaskNotFound(t *testing.T) {
	for _, s := range []string{
		"tck-nonexistent-get-002",
		"",
		"not-a-uuid",
		"0199A3B2-7C4E-7D10-8F00-2B9A6E1C0D11",   // uppercase: not the form the gate mints
		"0199a3b27c4e7d108f002b9a6e1c0d11",       // no hyphens
		"{0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11}", // braces
		"urn:uuid:0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11", // URN
		"0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d1x",
		"0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11 ",
		"'; select 1; --",
	} {
		u, err := ParseTaskID(s)
		if !errors.Is(err, a2a.ErrTaskNotFound) {
			t.Errorf("ParseTaskID(%q) = %v, %v; want an error wrapping a2a.ErrTaskNotFound", s, u, err)
			continue
		}
		var ae *a2a.Error
		if !errors.As(err, &ae) || ae.Message != "task not found" {
			t.Errorf("ParseTaskID(%q): error %#v, want *a2a.Error with the unknown-id message", s, err)
		}
		if u != uuid.Nil {
			t.Errorf("ParseTaskID(%q) returned %v with its error", s, u)
		}
	}
}

func TestParseTaskIDRoundTrip(t *testing.T) {
	for range 100 {
		id, err := NewTaskID()
		if err != nil {
			t.Fatal(err)
		}
		u, err := ParseTaskID(string(id))
		if err != nil || u.String() != string(id) {
			t.Fatalf("ParseTaskID(%q) = %v, %v", id, u, err)
		}
		if u.Version() != 7 || u.Variant() != uuid.RFC4122 {
			t.Fatalf("%s: version %d variant %v, want a UUIDv7", id, u.Version(), u.Variant())
		}
	}
}

// UUIDv7 ids sort by creation, as strings and as bytes, even within one millisecond.
func TestUUIDv7SortByCreation(t *testing.T) {
	const n = 5000
	tasks := make([]string, 0, n)
	contexts := make([]string, 0, n)
	items := make([]uuid.UUID, 0, n)
	for range n {
		tid, err := NewTaskID()
		if err != nil {
			t.Fatal(err)
		}
		cid, err := NewContextID()
		if err != nil {
			t.Fatal(err)
		}
		wid, err := NewWorkItemID()
		if err != nil {
			t.Fatal(err)
		}
		tasks, contexts, items = append(tasks, string(tid)), append(contexts, cid), append(items, wid)
	}
	for name, ids := range map[string][]string{"task": tasks, "context": contexts} {
		if !slices.IsSorted(ids) {
			t.Errorf("%s ids are not in creation order", name)
		}
		if len(slices.Compact(slices.Clone(ids))) != n {
			t.Errorf("%s ids repeat", name)
		}
	}
	if !slices.IsSortedFunc(items, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		t.Error("work item ids are not in creation order")
	}
	// Interleaved minting across kinds shares one clock: a task minted after a context sorts after it.
	if tasks[n-1] <= contexts[0] {
		t.Error("ids of different kinds do not share one order")
	}
}

// Derived ids are fixed: these values are stored in tasks.caller_id and must never change. They were
// computed independently with Python's uuid.uuid5 and the recipe in Derive's doc.
func TestUUIDv5DerivationsAreStable(t *testing.T) {
	if got := Namespace.String(); got != "a89faf13-3f2e-5f6d-8f26-999810d1be71" {
		t.Errorf("Namespace = %s", got)
	}
	tenant := uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11")
	team, _ := AnonymousCaller(uuid.Nil)
	multi, _ := AnonymousCaller(tenant)
	oidc, err := OIDCCaller("https://login.acme.io", "auth0|42")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		got  uuid.UUID
		want string
	}{
		"anonymous, team gate":         {team, "86ac12bd-54bd-5c48-95fc-5fca3c9a43db"},
		"anonymous, multi-tenant gate": {multi, "e629ddd4-0927-5baa-80e1-96e6d71a3bd3"},
		"oidc":                         {oidc, "d15aa7e8-4ab3-5925-bae8-94ec473a5229"},
		"derive":                       {Derive("x", "a", ""), "95029081-b3b1-525b-a251-1e8ad351f15a"},
	} {
		if tc.got.String() != tc.want {
			t.Errorf("%s = %s, want %s", name, tc.got, tc.want)
		}
		if tc.got.Version() != 5 {
			t.Errorf("%s: version %d, want 5", name, tc.got.Version())
		}
	}
	// The encoding is unambiguous: moving a boundary changes the id.
	if Derive("oidc", "https://a", "b") == Derive("oidc", "https://ab", "") || Derive("oidc", "a,b") == Derive("oidc", "a", "b") {
		t.Error("Derive is ambiguous across part boundaries")
	}
	if team == multi {
		t.Error("the team gate's anonymous caller equals a tenant's")
	}
	other, _ := AnonymousCaller(uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d12"))
	if other == multi {
		t.Error("two tenants share an anonymous caller")
	}
	if _, err := OIDCCaller("", "x"); err == nil {
		t.Error("OIDCCaller accepted an empty issuer")
	}
}

func TestNewReplicaID(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?-[0-9a-f]{8}$`)
	for host, prefix := range map[string]string{
		"gate-1":                    "gate-1",
		"Gate_1.Example.COM":        "gate-1-example-com",
		"":                          "gate",
		"...":                       "gate",
		strings.Repeat("h", 80):     strings.Repeat("h", 48),
		"kontra-gate-7d9f8b6c5-x2v": "kontra-gate-7d9f8b6c5-x2v",
	} {
		id, err := NewReplicaID(host)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(id) || !strings.HasPrefix(id, prefix+"-") || len(id) != len(prefix)+9 {
			t.Errorf("NewReplicaID(%q) = %q, want %q-<8 hex>", host, id, prefix)
		}
	}
	a, _ := NewReplicaID("h")
	b, _ := NewReplicaID("h")
	if a == b {
		t.Errorf("two starts on one host got the same replica id %q", a)
	}
}

// NodeCaller is UUIDv5(gate namespace ‖ key_id) (spec/relay.md §2.3), fixed forever like the other callers: the
// value was computed independently with Python's uuid.uuid5 and Derive's recipe, kind "node".
func TestNodeCaller(t *testing.T) {
	const kid = "k1_abcdefghijklmnopqrstuvwxyz"
	b, err := NodeCaller(Namespace, kid)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != "7333cc86-7e09-5473-9922-6e39f7551bed" || b.Version() != 5 {
		t.Fatalf("NodeCaller = %s (version %d)", b, b.Version())
	}
	if b != Derive("node", kid) {
		t.Fatal("NodeCaller(Namespace, k) is not Derive(\"node\", k)")
	}
	other, err := NodeCaller(Namespace, "k1_bbcdefghijklmnopqrstuvwxyz")
	if err != nil || other == b {
		t.Fatalf("two keys share a caller: %s %v", other, err)
	}
	if again, _ := NodeCaller(Namespace, kid); again != b {
		t.Fatal("NodeCaller is not stable")
	}
	// Another namespace is another caller; a key id of the wrong shape and no namespace are errors.
	if o, _ := NodeCaller(uuid.NameSpaceURL, kid); o == b {
		t.Fatal("the namespace does not enter the id")
	}
	for _, bad := range []string{"", "k1_ABCDEFGHIJKLMNOPQRSTUVWXYZ", "k1_abc", "key:kk_x", "k1_abcdefghijklmnopqrstuvwxyz0"} {
		if _, err := NodeCaller(Namespace, bad); err == nil {
			t.Errorf("NodeCaller(%q) accepted", bad)
		}
	}
	if _, err := NodeCaller(uuid.Nil, kid); err == nil {
		t.Error("NodeCaller accepted no namespace")
	}
}

// relay.md §2.3's table of derived callers (D319) is what an operator computes the ids from: each example it writes is
// the id the gate stores. scripts/check_specs.py recomputes the same values with Python's uuid.uuid5.
func TestDerivedCallersAsRelayWritesThem(t *testing.T) {
	node, err := NodeCaller(Namespace, "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	oidc, err := OIDCCaller("https://api.kontra.sh", "alice")
	if err != nil {
		t.Fatal(err)
	}
	team, _ := AnonymousCaller(uuid.Nil)
	multi, _ := AnonymousCaller(uuid.MustParse("01900000-0000-7000-8000-000000000001"))
	for name, tc := range map[string]struct {
		got  uuid.UUID
		want string
	}{
		"namespace":                    {Namespace, "a89faf13-3f2e-5f6d-8f26-999810d1be71"},
		"node":                         {node, "70c9ed92-910e-5b48-b506-040034a1d192"},
		"oidc":                         {oidc, "cd0b4bc7-5da9-50b2-aecd-b390e706e09a"},
		"anonymous, team gate":         {team, "86ac12bd-54bd-5c48-95fc-5fca3c9a43db"},
		"anonymous, multi-tenant gate": {multi, "ff015874-8db2-5a37-8ea7-f3e3516c1461"},
	} {
		if tc.got.String() != tc.want {
			t.Errorf("%s = %s, want relay.md §2.3's %s", name, tc.got, tc.want)
		}
	}
	// The names as relay.md writes them, netstrings of the kind and its parts.
	if netstrings("node", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa") != "4:node,29:k1_aaaaaaaaaaaaaaaaaaaaaaaaaa," ||
		netstrings("anonymous") != "9:anonymous," {
		t.Errorf("netstrings: %q %q", netstrings("node", "k1_aaaaaaaaaaaaaaaaaaaaaaaaaa"), netstrings("anonymous"))
	}
}
