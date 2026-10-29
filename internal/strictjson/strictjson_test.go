package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func keyErr(t *testing.T, err error) *KeyError {
	t.Helper()
	var ke *KeyError
	if !errors.As(err, &ke) {
		t.Fatalf("err = %v, want a *KeyError", err)
	}
	return ke
}

func TestMembers(t *testing.T) {
	m, err := Members([]byte(` {"method": "tools/call", "params": {"name": "echo", "NAME": "x"}, "id": 1} `))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 || string(m["method"]) != `"tools/call"` || string(m["id"]) != `1` {
		t.Fatalf("members = %v", m)
	}
	// Below the top level nothing is checked by Members.
	if !strings.Contains(string(m["params"]), `"NAME"`) {
		t.Fatalf("params = %s", m["params"])
	}

	for _, c := range []struct {
		in, key string
	}{
		{`{"a":1,"a":2}`, "a"},
		{`{"a":1,"A":2}`, "A"},
		{`{"method":"tools/call","METHOD":"ping"}`, "METHOD"},
		{`{"method":"tools/call","\u006dethod":"ping"}`, "method"}, // the same key once unescaped
		{`{"name":"echo","NAME":"add"}`, "NAME"},
		{`{"scope":1,"ſcope":2}`, "ſcope"}, // U+017F folds to s
		{`{"kid":1,"\u212aid":2}`, "Kid"},  // the Kelvin sign folds to k
		{`{"isError":true,"ISERROR":false}`, "ISERROR"},
	} {
		_, err := Members([]byte(c.in))
		ke := keyErr(t, err)
		if ke.Key != c.key || len(ke.Path) != 0 {
			t.Errorf("Members(%s): KeyError{%v, %q}, want key %q at the top", c.in, ke.Path, ke.Key, c.key)
		}
	}

	for _, in := range []string{``, ` `, `[]`, `[{"a":1}]`, `"x"`, `1`, `null`, `true`, `{}{}`, `{} x`, `{"a":1`, `{"a"}`, `{1:2}`} {
		if _, err := Members([]byte(in)); !errors.Is(err, ErrNotObject) {
			t.Errorf("Members(%q) = %v, want ErrNotObject", in, err)
		}
	}
	if m, err := Members([]byte(`{}`)); err != nil || len(m) != 0 {
		t.Fatalf("Members({}) = %v, %v", m, err)
	}
}

func TestCheck(t *testing.T) {
	levels := []Path{{"params"}, {"params", "_meta"}, {"parts", "*"}, {"parts", "*", "metadata"}, {"result", "inputRequests", "*"}}

	pass := []string{
		// The same key at different levels.
		`{"a":1,"b":{"a":1}}`,
		`{"params":{"a":1,"x":{"a":1,"A":2}}}`,                     // a case variant below an unnamed level
		`{"params":{"_meta":{"k":1,"x":{"K":1,"k":2}}}}`,           // below _meta, not named
		`{"params":null,"parts":"not an array"}`,                   // absent, null and non-object levels are skipped
		`{"params":[{"a":1,"a":2}]}`,                               // params is an array, and params.* is not named
		`{"parts":[{"text":"x","metadata":{"k":1}},{"raw":"AA"}]}`, // every element checked, all fine
	}
	for _, in := range pass {
		if err := Check([]byte(in), levels...); err != nil {
			t.Errorf("Check(%s) = %v, want nil", in, err)
		}
	}

	for _, c := range []struct {
		in   string
		path []string
		key  string
	}{
		{`{"a":1,"a":2}`, nil, "a"},
		{`{"params":{"name":"echo","NAME":"add"}}`, []string{"params"}, "NAME"},
		{`{"params":{"a":1,"a":2}}`, []string{"params"}, "a"},
		{`{"params":{"a":1,"A":2}}`, []string{"params"}, "A"},
		{`{"params":{"_meta":{"x":1,"X":2}}}`, []string{"params", "_meta"}, "X"},
		// parts.* checks every element: the third one fails.
		{`{"parts":[{"a":1},{"b":1},{"mediaType":"x","mediaType":"y"}]}`, []string{"parts", "2"}, "mediaType"},
		{`{"parts":[{"a":1},{"metadata":{"k":1,"K":2}}]}`, []string{"parts", "1", "metadata"}, "K"},
		// "*" over an object: every member, and the object itself (its keys are the server's request keys).
		{`{"result":{"inputRequests":{"r1":{"m":1},"R1":{"m":2}}}}`, []string{"result", "inputRequests"}, "R1"},
		{`{"result":{"inputRequests":{"r1":{"m":1,"M":2}}}}`, []string{"result", "inputRequests", "r1"}, "M"},
	} {
		err := Check([]byte(c.in), levels...)
		ke := keyErr(t, err)
		if !slices.Equal(ke.Path, c.path) || ke.Key != c.key {
			t.Errorf("Check(%s): KeyError{%v, %q}, want {%v, %q}", c.in, ke.Path, ke.Key, c.path, c.key)
		}
	}

	// A traversed parent is decoded with exact keys even when it is not named.
	err := Check([]byte(`{"result":{"x":1,"X":2,"inputRequests":{}}}`), levels...)
	if ke := keyErr(t, err); !slices.Equal(ke.Path, []string{"result"}) {
		t.Fatalf("KeyError path %v, want [result]", ke.Path)
	}

	for _, in := range []string{`[{"a":1}]`, `{"a":1} {"b":2}`, `{"a":1}x`, `"x"`} {
		if err := Check([]byte(in), levels...); !errors.Is(err, ErrNotObject) {
			t.Errorf("Check(%q) = %v, want ErrNotObject", in, err)
		}
	}
}

func TestCheckMembersMatchesCheck(t *testing.T) {
	body := []byte(`{"params":{"_meta":{"a":1,"A":1}}}`)
	top, err := Members(body)
	if err != nil {
		t.Fatal(err)
	}
	e1 := Check(body, Path{"params", "_meta"})
	e2 := CheckMembers(top, Path{"params", "_meta"})
	k1, k2 := keyErr(t, e1), keyErr(t, e2)
	if !slices.Equal(k1.Path, k2.Path) || k1.Key != k2.Key {
		t.Fatalf("Check %v, CheckMembers %v", k1, k2)
	}
}

// FuzzMembers: Members never panics, and on success its keys are exactly the object's keys (as an exact-key
// decoder reads them), each with its value.
func FuzzMembers(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"a":1}`, `{"a":1,"a":2}`, `{"a":1,"A":2}`, `{"a":1,"b":[1,{"c":null}]}`, `[]`, `{"a":1}x`,
		`{"method":"tools/call","METHOD":"ping","params":{"name":"echo"}}`, "{\"\xff\":1}", `{"ſ":1,"s":2}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Members(b)
		if err != nil {
			var ke *KeyError
			if !errors.As(err, &ke) && !errors.Is(err, ErrNotObject) {
				t.Fatalf("Members(%q): unexpected error %v", b, err)
			}
			return
		}
		if !json.Valid(b) {
			t.Fatalf("Members accepted invalid JSON %q", b)
		}
		var ref map[string]json.RawMessage
		if err := json.Unmarshal(b, &ref); err != nil {
			t.Fatalf("Members accepted %q, which is not an object: %v", b, err)
		}
		if len(ref) != len(m) {
			t.Fatalf("Members(%q) has %d keys, the object %d", b, len(m), len(ref))
		}
		for k, v := range ref {
			got, ok := m[k]
			if !ok {
				t.Fatalf("Members(%q) lacks key %q", b, k)
			}
			var a, c bytes.Buffer
			if json.Compact(&a, got) != nil || json.Compact(&c, v) != nil || a.String() != c.String() {
				t.Fatalf("Members(%q)[%q] = %s, want %s", b, k, got, v)
			}
		}
		// No two keys are equal under case folding.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		for i := range keys {
			for j := i + 1; j < len(keys); j++ {
				if strings.EqualFold(keys[i], keys[j]) {
					t.Fatalf("Members(%q) kept %q and %q", b, keys[i], keys[j])
				}
			}
		}
	})
}

func TestFoldKeyIsEqualFold(t *testing.T) {
	words := []string{"a", "A", "ß", "ẞ", "ss", "k", "K", "K", "s", "S", "ſ", "Σ", "σ", "ς", "İ", "i", "ı", "I", "ǅ", "Ǆ", "ǆ"}
	for _, a := range words {
		for _, b := range words {
			if (foldKey(a) == foldKey(b)) != strings.EqualFold(a, b) {
				t.Errorf("foldKey(%q)==foldKey(%q) is %v, EqualFold %v", a, b, foldKey(a) == foldKey(b), strings.EqualFold(a, b))
			}
		}
	}
}

// TestKeyErrorIsBounded: a KeyError's text quotes at most a short prefix of the key, escaped, so that one
// request cannot write megabytes or a forged line into every log line that prints it.
func TestKeyErrorIsBounded(t *testing.T) {
	k := "k\ntime=2026 level=INFO msg=\"forged line\"" + strings.Repeat("x", 1<<20)
	k1, _ := json.Marshal(k)
	k2, _ := json.Marshal(strings.ToUpper(k))
	for _, err := range []error{
		func() error { _, err := Members([]byte(`{` + string(k1) + `:1,` + string(k2) + `:2}`)); return err }(),
		Check([]byte(`{"params":{`+string(k1)+`:1,`+string(k1)+`:2}}`), Path{"params"}),
		CheckReads(map[string]json.RawMessage{strings.ToUpper(k): json.RawMessage(`1`)}, nil, []Path{{k}}),
	} {
		ke := keyErr(t, err)
		if ke.Key == "" {
			t.Fatal("the KeyError lost its key")
		}
		s := err.Error()
		if len(s) > 160 || strings.ContainsAny(s, "\n\r") {
			t.Errorf("KeyError text is %d bytes (newline %v): %.200q", len(s), strings.ContainsAny(s, "\n\r"), s)
		}
		if !strings.Contains(s, `"K\nTIME`) && !strings.Contains(s, `"k\ntime`) {
			t.Errorf("KeyError text %q does not quote the key's start", s)
		}
	}
}

// TestSyntaxIsNotObject: input that is not JSON at all is ErrSyntax (which is also ErrNotObject); valid JSON
// that is not one object is ErrNotObject only, so a reader can answer them differently (-32700, -32600).
func TestSyntaxIsNotObject(t *testing.T) {
	for _, in := range []string{``, ` `, `{"a":1`, `{"a"}`, `{1:2}`, `{} x`, `{}{}`, `{"a":1} {"b":2}`, `[1,`, `nul`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}`} {
		_, err := Members([]byte(in))
		if !errors.Is(err, ErrSyntax) || !errors.Is(err, ErrNotObject) {
			t.Errorf("Members(%q) = %v, want ErrSyntax (and ErrNotObject)", in, err)
		}
		if err := Check([]byte(in), Path{"params"}); !errors.Is(err, ErrSyntax) {
			t.Errorf("Check(%q) = %v, want ErrSyntax", in, err)
		}
	}
	for _, in := range []string{`[]`, `[{"a":1}]`, `"x"`, `1`, `null`, `true`, ` [1, {"a": [2]}] `} {
		_, err := Members([]byte(in))
		if !errors.Is(err, ErrNotObject) || errors.Is(err, ErrSyntax) {
			t.Errorf("Members(%q) = %v, want ErrNotObject and not ErrSyntax", in, err)
		}
	}
}

// TestCheckReads: a lone key equal under case folding to a member the reader reads at that level, without being
// it, is a *KeyError naming the member it stands for; the exact member, other keys, and variants at levels or of
// names that are not read pass.
func TestCheckReads(t *testing.T) {
	levels := []Path{{"params"}, {"params", "_meta"}, {"result"}, {"result", "tools", "*"}}
	reads := []Path{{"method"}, {"id"}, {"params"}, {"params", "name"}, {"params", "task"}, {"params", "_meta"},
		{"result", "isError"}, {"result", "tools", "*", "name"}}
	check := func(in string) error {
		top, err := Members([]byte(in))
		if err != nil {
			t.Fatalf("Members(%s): %v", in, err)
		}
		return CheckReads(top, levels, reads)
	}
	for _, in := range []string{
		`{"method":"tools/call","id":1,"params":{"name":"echo","task":{},"_meta":{}}}`,
		`{"method":"x","params":{"name":"echo","arguments":{"NAME":"y"}}}`,         // arguments is not a level here
		`{"method":"x","params":{"name":"echo","Message":"m","_meta":{"NAME":1}}}`, // name is not read in _meta
		`{"result":{"content":[{"ISERROR":true}],"isError":false}}`,                // content items are not a level
		`{"result":{"tools":[{"name":"a","Title":"t"},{"name":"b"}]}}`,
		`{"params":null,"result":"x"}`, // absent, null and non-object levels are skipped
	} {
		if err := check(in); err != nil {
			t.Errorf("CheckReads(%s) = %v, want nil", in, err)
		}
	}
	for _, c := range []struct {
		in      string
		path    []string
		key, of string
	}{
		{`{"METHOD":"tools/call","id":1}`, nil, "METHOD", "method"},
		{`{"method":"tools/call","Id":1}`, nil, "Id", "id"},
		{`{"method":"tools/call","PARAMS":{"name":"rm"}}`, nil, "PARAMS", "params"},
		{`{"method":"tools/call","params":{"NAME":"rm"}}`, []string{"params"}, "NAME", "name"},
		{"{\"method\":\"tools/call\",\"params\":{\"name\":\"echo\",\"tasK\":{}}}", []string{"params"}, "tasK", "task"},
		{`{"params":{"name":"echo","_Meta":{}}}`, []string{"params"}, "_Meta", "_meta"},
		{`{"result":{"content":[],"ISERROR":true}}`, []string{"result"}, "ISERROR", "isError"},
		{`{"result":{"tools":[{"name":"a"},{"NAME":"rm"}]}}`, []string{"result", "tools", "1"}, "NAME", "name"},
	} {
		ke := keyErr(t, check(c.in))
		if !slices.Equal(ke.Path, c.path) || ke.Key != c.key || ke.Of != c.of {
			t.Errorf("CheckReads(%s): KeyError{%v, %q, of %q}, want {%v, %q, of %q}", c.in, ke.Path, ke.Key, ke.Of,
				c.path, c.key, c.of)
		}
	}
	// A repeated or case-variant pair is still refused at every level, and the top's problems come first.
	top, _ := Members([]byte(`{"METHOD":"x","params":{"a":1,"A":2}}`))
	if ke := keyErr(t, CheckReads(top, levels, reads)); len(ke.Path) != 0 || ke.Of != "method" {
		t.Errorf("KeyError %+v, want the top level's METHOD first", ke)
	}
	top, _ = Members([]byte(`{"method":"x","params":{"a":1,"A":2}}`))
	if ke := keyErr(t, CheckReads(top, levels, reads)); !slices.Equal(ke.Path, []string{"params"}) || ke.Of != "" {
		t.Errorf("KeyError %+v, want the pair in params", ke)
	}
}
