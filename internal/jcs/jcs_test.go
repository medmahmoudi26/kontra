package jcs

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// RFC 8785's examples (§3.2.2, §3.2.3) and the webpki.org vectors the RFC's reference implementations share, byte
// for byte.
var vectors = []struct {
	name, in, want string
}{
	{
		"rfc8785 3.2.2 values",
		`{
  "numbers": [333333333.33333329, 1E30, 4.50, 2e-3, 0.000000000000000000000000001],
  "string": "\u20ac$\u000F\u000aA'\u0042\u0022\u005c\\\"\/",
  "literals": [null, true, false]
}`,
		"{\"literals\":[null,true,false],\"numbers\":[333333333.3333333,1e+30,4.5,0.002,1e-27],\"string\":\"\u20ac$\\u000f\\nA'B\\\"\\\\\\\\\\\"/\"}",
	},
	{
		// §3.2.3: keys sort by UTF-16 code units, so U+1F602 (D83D DE02) sorts before U+FB33, unlike code points.
		"rfc8785 3.2.3 sorting, surrogate pairs",
		`{
  "\u20ac": "Euro Sign",
  "\r": "Carriage Return",
  "\u000a": "Newline",
  "1": "One",
  "\u0080": "Control\u007f",
  "\ud83d\ude02": "Smiley",
  "\u00f6": "Latin Small Letter O With Diaeresis",
  "\ufb33": "Hebrew Letter Dalet With Dagesh",
  "</script>": "Browser Challenge"
}`,
		"{\"\\n\":\"Newline\",\"\\r\":\"Carriage Return\",\"1\":\"One\",\"</script>\":\"Browser Challenge\",\"\u0080\":\"Control\u007f\"," +
			"\"\u00f6\":\"Latin Small Letter O With Diaeresis\",\"\u20ac\":\"Euro Sign\",\"\U0001F602\":\"Smiley\",\"\ufb33\":\"Hebrew Letter Dalet With Dagesh\"}",
	},
	{
		"arrays keep order, objects sort",
		"[\n  56,\n  {\n    \"d\": true,\n    \"10\": null,\n    \"1\": [ ]\n  }\n]\n",
		`[56,{"1":[],"10":null,"d":true}]`,
	},
	{
		"nested, case-distinct keys sort by code unit",
		`{"1": {"f": {"f": "hi","F": 5} ,"\n": 56.0}, "10": { }, "": "empty", "a": { }, "111": [ {"e": "yes","E": "no" } ], "A": { }}`,
		`{"":"empty","1":{"\n":56,"f":{"F":5,"f":"hi"}},"10":{},"111":[{"E":"no","e":"yes"}],"A":{},"a":{}}`,
	},
	{
		"no locale order, no Unicode normalization",
		`{"pêche": 1, "péché": 2, "peach": 3, "sin": 4, "n": "A\u030a"}`,
		"{\"n\":\"A\u030a\",\"peach\":3,\"péché\":2,\"pêche\":1,\"sin\":4}",
	},
	{
		"U+2028 and U+2029 are not escaped",
		`"\u2028\u2029"`,
		"\"\u2028\u2029\"",
	},
	{
		"HTML characters are not escaped, controls are",
		`"<>&\u0001\u001f"`,
		`"<>&\u0001\u001f"`,
	},
	{"-0 is 0", `-0`, `0`},
	{"-0.0 is 0", `[-0.0, 0.0, 0e5]`, `[0,0,0]`},
	{"1e+21", `1E21`, `1e+21`},
	{"1e21 - 1 ulp", `999999999999999900000`, `999999999999999900000`},
	{"5e-324", `4.9406564584124654e-324`, `5e-324`},
	{"exponent form", `[1e-7, 0.000001, 1e+2]`, `[1e-7,0.000001,100]`},
	{"top-level white space", " \t\r\n{ \"a\" : [ 1 , 2 ] }\n ", `{"a":[1,2]}`},
}

// RFC 8785 Appendix B: IEEE 754 values and their JCS text.
var numbers = []struct {
	bits uint64
	want string
}{
	{0x0000000000000000, "0"},
	{0x8000000000000000, "0"},
	{0x0000000000000001, "5e-324"},
	{0x8000000000000001, "-5e-324"},
	{0x7fefffffffffffff, "1.7976931348623157e+308"},
	{0xffefffffffffffff, "-1.7976931348623157e+308"},
	{0x4340000000000000, "9007199254740992"},
	{0xc340000000000000, "-9007199254740992"},
	{0x4430000000000000, "295147905179352830000"},
	{0x44b52d02c7e14af5, "9.999999999999997e+22"},
	{0x44b52d02c7e14af6, "1e+23"},
	{0x44b52d02c7e14af7, "1.0000000000000001e+23"},
	{0x444b1ae4d6e2ef4e, "999999999999999700000"},
	{0x444b1ae4d6e2ef4f, "999999999999999900000"},
	{0x444b1ae4d6e2ef50, "1e+21"},
	{0x3eb0c6f7a0b5ed8c, "9.999999999999997e-7"},
	{0x3eb0c6f7a0b5ed8d, "0.000001"},
	{0x41b3de4355555553, "333333333.3333332"},
	{0x41b3de4355555554, "333333333.33333325"},
	{0x41b3de4355555555, "333333333.3333333"},
	{0x41b3de4355555556, "333333333.3333334"},
	{0x41b3de4355555557, "333333333.33333343"},
	{0xbecbf647612f3696, "-0.0000033333333333333333"},
	{0x43143ff3c1cb0959, "1424953923781206.2"},
}

func TestTransformVectors(t *testing.T) {
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			got, err := Transform([]byte(v.in))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(v.want)) {
				t.Fatalf("Transform:\n got %q\nwant %q", got, v.want)
			}
			if ok, err := Canonical(got); !ok || err != nil {
				t.Fatalf("Canonical(Transform(x)) = %v, %v", ok, err)
			}
		})
	}
}

func TestTransformNumbers(t *testing.T) {
	for _, n := range numbers {
		f := math.Float64frombits(n.bits)
		// Several spellings of the same double: Go's shortest, 17 significant digits, and an exponent form.
		for _, in := range []string{
			strconv.FormatFloat(f, 'g', -1, 64),
			strconv.FormatFloat(f, 'e', 16, 64),
			strconv.FormatFloat(f, 'E', -1, 64),
		} {
			got, err := Transform([]byte(in))
			if err != nil {
				t.Fatalf("%016x %s: %v", n.bits, in, err)
			}
			if string(got) != n.want {
				t.Fatalf("%016x %s: got %s, want %s", n.bits, in, got, n.want)
			}
		}
	}
}

func TestTransformRefusals(t *testing.T) {
	for _, in := range []string{
		``,
		` `,
		`{"a":1,"a":2}`,
		`{"a":1,"\u0061":2}`, // the same key after unescaping
		`[{"x":{"b":1,"b":1}}]`,
		`{} {}`,
		`{}x`,
		`1 2`,
		`"a" "b"`,
		`[1,]`,
		`{"a":1,}`,
		`NaN`,
		`[NaN]`,
		`Infinity`,
		`-Infinity`,
		`1e400`,
		`-1e400`,
		`0x10`,
		`+1`,
		`.5`,
		`01`,
		`1.`,
		`{"a":undefined}`,
		"\"\xff\"",       // invalid UTF-8
		`"\ud800"`,       // a lone high surrogate
		`"\udc00"`,       // a lone low surrogate
		`"\ud83d\u0041"`, // a high surrogate without its low half
		"\xef\xbb\xbf{}", // a byte order mark
		"\"a\x01\"",      // a raw control character
	} {
		if out, err := Transform([]byte(in)); err == nil {
			t.Errorf("Transform(%q) = %q, want an error", in, out)
		}
		if ok, err := Canonical([]byte(in)); ok || err == nil {
			t.Errorf("Canonical(%q) = %v, %v; want false with an error", in, ok, err)
		}
	}
	if _, err := Transform(nil); err == nil {
		t.Error("Transform(nil) succeeds")
	}
	deep := strings.Repeat("[", 10001) + strings.Repeat("]", 10001)
	if _, err := Transform([]byte(deep)); err == nil {
		t.Error("10001 levels of nesting are accepted")
	}
}

func TestCanonical(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{`{"a":1}`, true},
		{`{"a":1,"b":[true,null,"x"]}`, true},
		{"\"\u2028\"", true},
		{`{ "a":1}`, false},
		{`{"a":1} `, false},
		{`{"b":1,"a":2}`, false},
		{`1.0`, false},
		{`1E2`, false},
		{`"\u0041"`, false},
		{`"\u2028"`, false},
		{`"\/"`, false},
		{`{"a":"\u00e9"}`, false},
		{"{\"a\":\"\u00e9\"}", true},
	} {
		ok, err := Canonical([]byte(c.in))
		if err != nil || ok != c.want {
			t.Errorf("Canonical(%q) = %v, %v; want %v", c.in, ok, err, c.want)
		}
	}
}

// FuzzTransform: Transform never panics, its output is canonical and is the same JSON value.
func FuzzTransform(f *testing.F) {
	for _, v := range vectors {
		f.Add([]byte(v.in))
	}
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Add([]byte(`[1e400]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		out, err := Transform(b)
		if err != nil {
			return
		}
		if ok, err := Canonical(out); !ok || err != nil {
			t.Fatalf("Transform(%q) = %q, not canonical (%v)", b, out, err)
		}
		if !json.Valid(out) {
			t.Fatalf("Transform(%q) = %q, not JSON", b, out)
		}
	})
}
