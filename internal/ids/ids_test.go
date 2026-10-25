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

// Derived ids are fixed: these values are stored in tasks.buyer_id and must never change. They were
// computed independently with Python's uuid.uuid5 and the recipe in Derive's doc.
func TestUUIDv5DerivationsAreStable(t *testing.T) {
	if got := Namespace.String(); got != "a89faf13-3f2e-5f6d-8f26-999810d1be71" {
		t.Errorf("Namespace = %s", got)
	}
	tenant := uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d11")
	team, _ := AnonymousBuyer(uuid.Nil)
	multi, _ := AnonymousBuyer(tenant)
	oidc, err := OIDCBuyer("https://login.acme.io", "auth0|42")
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
		t.Error("the team gate's anonymous buyer equals a tenant's")
	}
	other, _ := AnonymousBuyer(uuid.MustParse("0199a3b2-7c4e-7d10-8f00-2b9a6e1c0d12"))
	if other == multi {
		t.Error("two tenants share an anonymous buyer")
	}
	if _, err := OIDCBuyer("", "x"); err == nil {
		t.Error("OIDCBuyer accepted an empty issuer")
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
