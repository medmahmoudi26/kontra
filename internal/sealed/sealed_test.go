package sealed

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/armor"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

func newID(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func kidsOf(ids ...*age.X25519Identity) []string {
	var out []string
	for _, id := range ids {
		out = append(out, tunnel.KeyID(id.Recipient().String()))
	}
	slices.Sort(out)
	return out
}

func recipientsOf(ids ...*age.X25519Identity) []age.Recipient {
	var out []age.Recipient
	for _, id := range ids {
		out = append(out, id.Recipient())
	}
	return out
}

func wantReason(t *testing.T, err error, r Reject) {
	t.Helper()
	got, ok := ReasonOf(err)
	if !ok || got != r {
		t.Fatalf("err = %v, want a *RejectError %s", err, r)
	}
}

func requestHeader(t *testing.T, kids []string) Header {
	t.Helper()
	rk, err := NewRK()
	if err != nil {
		t.Fatal(err)
	}
	return Header{
		V: Version, Dir: DirRequest, Service: "acme/hello", IAT: 1761490000, ContextID: "ctx-1", MessageID: "msg-1",
		Part: 0, Parts: 1, Kind: KindText, MediaType: "text/plain", Kids: kids,
		ReplyTo: newID(t).Recipient().String(), RK: EncodeRK(rk),
	}
}

func TestSealOpen(t *testing.T) {
	for _, n := range []int{1, 3} {
		var ids []*age.X25519Identity
		for range n {
			ids = append(ids, newID(t))
		}
		h := requestHeader(t, kidsOf(ids...))
		body := []byte("the buyer's secret \x00 with a NUL inside")
		ct, err := Seal(recipientsOf(ids...), h, body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ct, body[:10]) || bytes.HasPrefix(ct, []byte(armor.Header)) {
			t.Fatal("ciphertext holds plaintext or is armored")
		}
		if !bytes.HasPrefix(ct, []byte("age-encryption.org/v1\n")) {
			t.Fatalf("not a binary age file: %q", ct[:24])
		}
		for i, id := range ids {
			got, gotBody, err := Open([]age.Identity{id}, ct)
			if err != nil {
				t.Fatalf("%d recipients, identity %d: %v", n, i, err)
			}
			if !headersEqual(got, h) || !bytes.Equal(gotBody, body) {
				t.Fatalf("%d recipients: opened %+v %q", n, got, gotBody)
			}
		}
		// Any of several identities (a rotated key beside the active one) opens it.
		if _, _, err := Open([]age.Identity{newID(t), ids[n-1]}, ct); err != nil {
			t.Fatal(err)
		}
		// A non-recipient identity: undecryptable.
		_, _, err = Open([]age.Identity{newID(t)}, ct)
		wantReason(t, err, RejectUndecryptable)
	}
}

func headersEqual(a, b Header) bool {
	ca, err1 := a.Canonical()
	cb, err2 := b.Canonical()
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}

func TestSealRefusals(t *testing.T) {
	a, b := newID(t), newID(t)
	h := requestHeader(t, kidsOf(a))
	if _, err := Seal(nil, h, nil); err == nil {
		t.Fatal("sealed to nobody")
	}
	if _, err := Seal(recipientsOf(a, b), h, nil); err == nil {
		t.Fatal("kids naming one of two recipients are accepted")
	}
	h2 := h
	h2.Kids = kidsOf(b)
	if _, err := Seal(recipientsOf(a), h2, nil); err == nil {
		t.Fatal("kids naming another key are accepted")
	}
	pass, err := age.NewScryptRecipient("password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Seal([]age.Recipient{pass}, h, nil); err == nil {
		t.Fatal("a scrypt recipient is accepted")
	}
	h3 := h
	h3.Auth = "x"
	if _, err := Seal(recipientsOf(a), h3, nil); err == nil {
		t.Fatal("a request with auth is sealed")
	}
}

// encryptRaw seals arbitrary plaintext as a party that does not use Seal would.
func encryptRaw(t *testing.T, id *age.X25519Identity, plaintext []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plaintext)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestHeaderRefusals(t *testing.T) {
	id := newID(t)
	kid := kidsOf(id)[0]
	good := requestHeader(t, []string{kid})
	gb, err := good.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := jcs.Canonical(gb); !ok || err != nil {
		t.Fatalf("Canonical is not JCS: %s", gb)
	}
	g := string(gb)
	for _, c := range []struct{ name, hdr string }{
		{"unknown member", strings.Replace(g, `{"contextId"`, `{"color":"red","contextId"`, 1)},
		{"case-variant member", strings.Replace(g, `{"contextId"`, `{"ContextId":"x","contextId"`, 1)},
		{"case of a known member", strings.Replace(g, `"messageId"`, `"messageID"`, 1)},
		{"duplicate key", strings.Replace(g, `{"contextId":"ctx-1"`, `{"contextId":"ctx-1","contextId":"ctx-1"`, 1)},
		{"white space", strings.Replace(g, `,"dir"`, `, "dir"`, 1)},
		{"member order", strings.Replace(strings.Replace(g, `"contextId":"ctx-1",`, ``, 1), `"v":1}`, `"v":1,"contextId":"ctx-1"}`, 1)},
		{"non-ASCII value", strings.Replace(g, `"ctx-1"`, "\"ctx-é\"", 1)},
		{"escaped non-ASCII value", strings.Replace(g, `"ctx-1"`, `"ctx-\u00e9"`, 1)},
		{"escape JCS would not write", strings.Replace(g, `"ctx-1"`, `"ctx\u002d1"`, 1)},
		{"number form", strings.Replace(g, `"v":1`, `"v":1.0`, 1)},
		{"version 2", strings.Replace(g, `"v":1`, `"v":2`, 1)},
		{"empty optional member", strings.Replace(g, `{"contextId"`, `{"artifactId":"","contextId"`, 1)},
		{"zero iat", strings.Replace(g, `"iat":1761490000`, `"iat":0`, 1)},
		{"null chunk", strings.Replace(g, `{"contextId"`, `{"chunk":null,"contextId"`, 1)},
		{"string part", strings.Replace(g, `"part":0`, `"part":"0"`, 1)},
		{"part out of range", strings.Replace(g, `"part":0`, `"part":1`, 1)},
		{"unknown kind", strings.Replace(g, `"kind":"text"`, `"kind":"file"`, 1)},
		{"unknown dir", strings.Replace(g, `"dir":"req"`, `"dir":"rsp"`, 1)},
		{"missing kids", strings.Replace(g, `"kids":["`+kid+`"],`, ``, 1)},
		{"empty kids", strings.Replace(g, `"kids":["`+kid+`"]`, `"kids":[]`, 1)},
		{"unsorted kids", strings.Replace(g, `"kids":["`+kid+`"]`, `"kids":["`+kid+`","k1_0"]`, 1)},
		{"short rk", strings.Replace(g, `"rk":"`+good.RK+`"`, `"rk":"AAEC"`, 1)},
		{"auth on a request", strings.Replace(g, `"v":1}`, `"auth":"x","v":1}`, 1)},
		{"non-ASCII service", strings.Replace(g, `"service":"acme/hello"`, "\"service\":\"acme/h\u00e9llo\"", 1)},
		{"service out of order", strings.Replace(strings.Replace(g, `"service":"acme/hello",`, ``, 1), `{"contextId"`, `{"service":"acme/hello","contextId"`, 1)},
		{"case of service", strings.Replace(g, `"service"`, `"Service"`, 1)},
		{"empty service", strings.Replace(g, `"service":"acme/hello"`, `"service":""`, 1)},
		{"trailing data", g + " "},
		{"array", "[" + g + "]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.hdr == g {
				t.Fatal("the case does not change the header")
			}
			_, err := ParseHeader([]byte(c.hdr))
			wantReason(t, err, RejectBadHeader)
			// And the same header inside a sealed file.
			_, _, err = Open([]age.Identity{id}, encryptRaw(t, id, append([]byte(c.hdr+"\x00"), "body"...)))
			wantReason(t, err, RejectBadHeader)
		})
	}
	// Without the 0x00 separator.
	_, _, err = Open([]age.Identity{id}, encryptRaw(t, id, gb))
	wantReason(t, err, RejectBadHeader)
	// The good header parses back to itself.
	h, err := ParseHeader(gb)
	if err != nil || !headersEqual(h, good) {
		t.Fatalf("ParseHeader(good) = %+v, %v", h, err)
	}
}

// TestInputRequestHeaders: D175's headers round-trip canonically: an input request the hosting node seals like a
// reply, and the hiring node's answer, sealed like a request, with the same session, tool and messageId.
func TestInputRequestHeaders(t *testing.T) {
	hosting, perJob := newID(t), newID(t)
	session := strings.Repeat("ab", 32)
	in := Header{V: Version, Dir: DirReply, Session: session, Tool: "book", MessageID: "7", Op: OpInput, Part: 0, Parts: 1,
		Kind: KindData, MediaType: "", Kids: kidsOf(perJob)}
	rk, _ := NewRK()
	auth, err := Auth(rk, in, []byte(`{"message":"Which date?"}`))
	if err != nil {
		t.Fatal(err)
	}
	in.Auth = auth
	answer := Header{V: Version, Dir: DirRequest, IAT: 1761490100, Session: session, Tool: "book", MessageID: "7",
		Op: OpAnswer, Part: 0, Parts: 1, Kind: KindData, MediaType: "", Kids: kidsOf(hosting),
		ReplyTo: perJob.Recipient().String(), RK: EncodeRK(rk)}
	for _, c := range []struct {
		h    Header
		want string
	}{
		{in, `{"auth":"` + auth + `","dir":"res","kids":["` + in.Kids[0] + `"],"kind":"data","mediaType":"","messageId":"7","op":"input","part":0,"parts":1,"session":"` + session + `","tool":"book","v":1}`},
		{answer, `{"dir":"req","iat":1761490100,"kids":["` + answer.Kids[0] + `"],"kind":"data","mediaType":"","messageId":"7","op":"answer","part":0,"parts":1,"replyTo":"` + answer.ReplyTo + `","rk":"` + answer.RK + `","session":"` + session + `","tool":"book","v":1}`},
	} {
		b, err := c.h.Canonical()
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Fatalf("Canonical = %s\nwant %s", b, c.want)
		}
		p, err := ParseHeader(b)
		if err != nil {
			t.Fatal(err)
		}
		b2, _ := p.Canonical()
		if !bytes.Equal(b, b2) || p.MessageID != "7" || p.Session != session || p.Tool != "book" {
			t.Fatalf("round trip: %s / %+v", b2, p)
		}
	}
	// The input request travels to the per-job key, the answer to the hosting node, and both open there.
	ct, err := Seal(recipientsOf(perJob), in, []byte(`{"message":"Which date?"}`))
	if err != nil {
		t.Fatal(err)
	}
	h, body, err := Open([]age.Identity{perJob}, ct)
	if err != nil || VerifyAuth(rk, h, body) != nil {
		t.Fatalf("input request: %v / %v", err, VerifyAuth(rk, h, body))
	}
	ct, err = Seal(recipientsOf(hosting), answer, []byte(`{"action":"accept"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open([]age.Identity{hosting}, ct); err != nil {
		t.Fatal(err)
	}
}

// TestAgeInterop: Seal's output is a plain age file: filippo.io/age with an identity parsed from an identity file
// (the format age-keygen writes, so `age -d -i identity.key` opens it) gives JCS(header) ‖ 0x00 ‖ body; and a
// file the age CLI encrypted (testdata/cli-sealed.age, made with `printf '<header>\0<body>' | age -r <recipient>`,
// age v1.3.2; made again with its header's service member, D221) opens with Open.
func TestAgeInterop(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "identity.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ParseIdentities: %v, %d", err, len(ids))
	}
	x := ids[0].(*age.X25519Identity)

	h := requestHeader(t, kidsOf(x))
	body := []byte(`{"order":42}`)
	ct, err := Seal(recipientsOf(x), h, body)
	if err != nil {
		t.Fatal(err)
	}
	r, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := h.Canonical()
	if want := append(append(hb, 0), body...); !bytes.Equal(plain, want) {
		t.Fatalf("plaintext = %q\nwant %q", plain, want)
	}

	cli, err := os.ReadFile(filepath.Join("testdata", "cli-sealed.age"))
	if err != nil {
		t.Fatal(err)
	}
	ch, cbody, err := Open(ids, cli)
	if err != nil {
		t.Fatal(err)
	}
	if string(cbody) != "hello from the age CLI" || ch.Dir != DirRequest || ch.MessageID != "msg-1" || ch.Service != "hello" ||
		!slices.Equal(ch.Kids, kidsOf(x)) || ch.IAT != 1761490000 {
		t.Fatalf("the CLI's file opened as %+v %q", ch, cbody)
	}
}

func TestOpenRefusesDamagedFiles(t *testing.T) {
	id := newID(t)
	h := requestHeader(t, kidsOf(id))
	body := bytes.Repeat([]byte("x"), 200<<10) // several 64 KiB chunks
	ct, err := Seal(recipientsOf(id), h, body)
	if err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte {
		c := bytes.Clone(ct)
		c[i] ^= 1
		return c
	}
	var armored bytes.Buffer
	aw := armor.NewWriter(&armored)
	aw.Write(ct)
	aw.Close()
	for name, c := range map[string][]byte{
		"truncated by a chunk":   ct[:len(ct)-(64<<10)],
		"truncated by a byte":    ct[:len(ct)-1],
		"last byte flipped":      flip(len(ct) - 1),
		"first chunk flipped":    flip(len(ct) - 150<<10),
		"header MAC flipped":     flip(bytes.Index(ct, []byte("\n---")) + 6),
		"trailing data":          append(bytes.Clone(ct), 'x'),
		"armored":                armored.Bytes(),
		"empty":                  nil,
		"not an age file":        []byte("plain text"),
		"two files concatenated": append(bytes.Clone(ct), ct...),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Open([]age.Identity{id}, c)
			wantReason(t, err, RejectUndecryptable)
		})
	}
	if _, _, err := Open(nil, ct); err == nil {
		t.Fatal("opened with no identity")
	}
}

// The reply MAC's fixed vector, computed outside this package (Python's hmac over the header's JCS written out
// by hand, 0x00 and the body).
const (
	vectorRK   = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8" // bytes 0..31
	vectorAuth = "atb6ij4m4l_v535_Ev6HzWSszHpR3QWdBaCw-c4GQF8"
)

func TestAuthVector(t *testing.T) {
	rk, err := DecodeRK(vectorRK)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range rk {
		if int(b) != i {
			t.Fatal("rk is not 0..31")
		}
	}
	h := Header{V: Version, Dir: DirReply, TaskID: "t-1", ContextID: "c-1", MessageID: "m-1", Part: 0, Parts: 1,
		Kind: KindText, MediaType: "text/plain", Kids: []string{"k1_aaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	body := []byte("hello world")
	auth, err := Auth(rk, h, body)
	if err != nil {
		t.Fatal(err)
	}
	if auth != vectorAuth {
		t.Fatalf("Auth = %s, want %s", auth, vectorAuth)
	}
	h.Auth = auth
	if err := VerifyAuth(rk, h, body); err != nil {
		t.Fatal(err)
	}
	// auth is not part of what it authenticates: computing it again over the header carrying it is the same.
	if again, _ := Auth(rk, h, body); again != vectorAuth {
		t.Fatalf("Auth over a header with auth = %s", again)
	}

	flipped := bytes.Clone(body)
	flipped[0] ^= 1
	wantReason(t, VerifyAuth(rk, h, flipped), RejectBadHeader)
	other := h
	other.MessageID = "m-2"
	wantReason(t, VerifyAuth(rk, other, body), RejectBadHeader)
	wrongKey := bytes.Clone(rk)
	wrongKey[31] ^= 1
	wantReason(t, VerifyAuth(wrongKey, h, body), RejectBadHeader)
	missing := h
	missing.Auth = ""
	wantReason(t, VerifyAuth(rk, missing, body), RejectBadHeader)
	padded := h
	padded.Auth = vectorAuth + "="
	wantReason(t, VerifyAuth(rk, padded, body), RejectBadHeader)
	truncated := h
	truncated.Auth = vectorAuth[:40]
	wantReason(t, VerifyAuth(rk, truncated, body), RejectBadHeader)
	if _, err := Auth(rk[:16], h, body); err == nil {
		t.Fatal("a 16-byte reply key is used")
	}
}

func TestRK(t *testing.T) {
	a, err := NewRK()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewRK()
	if len(a) != RKSize || bytes.Equal(a, b) {
		t.Fatalf("NewRK: %x, %x", a, b)
	}
	s := EncodeRK(a)
	if len(s) != 43 || strings.ContainsAny(s, "=+/") {
		t.Fatalf("EncodeRK = %s", s)
	}
	back, err := DecodeRK(s)
	if err != nil || !bytes.Equal(back, a) {
		t.Fatalf("DecodeRK = %x, %v", back, err)
	}
	for _, bad := range []string{"", s + "=", s[:42], base64.StdEncoding.EncodeToString(a), EncodeRK(a[:31])} {
		if _, err := DecodeRK(bad); err == nil {
			t.Errorf("DecodeRK(%q) succeeds", bad)
		}
	}
}

func TestReasons(t *testing.T) {
	// spec/ext-sealed.md §6, in its order.
	want := []string{"undecryptable", "bad_header", "wrong_task", "wrong_context", "wrong_message", "wrong_job", "replayed", "not_a_recipient"}
	var got []string
	for _, r := range Reasons {
		got = append(got, string(r))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Reasons = %v", got)
	}
	err := error(&RejectError{Reason: RejectReplayed, Detail: "secret detail"})
	if err.Error() != "sealed part rejected: replayed" {
		t.Fatalf("Error() = %q: the message is what §6 sends", err)
	}
	if r, ok := ReasonOf(errors.Join(errors.New("x"), err)); !ok || r != RejectReplayed {
		t.Fatal("ReasonOf does not unwrap")
	}
	if _, ok := ReasonOf(errors.New("x")); ok {
		t.Fatal("ReasonOf of a plain error")
	}
}

func TestParts(t *testing.T) {
	if blobScheme != blob.URIScheme {
		t.Fatalf("blobScheme %q != blob.URIScheme %q", blobScheme, blob.URIScheme)
	}
	ct := []byte("age-encryption.org/v1\n-> X25519 ...")
	p := Part{Ciphertext: ct, Filename: "contract.pdf", Original: "application/pdf"}
	raw, err := MarshalPart(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"raw":"` + base64.StdEncoding.EncodeToString(ct) + `","mediaType":"application/vnd.kontra.sealed+age;v=1","filename":"contract.pdf","metadata":{"kontra.sealed":{"original":"application/pdf"}}}`
	if string(raw) != want {
		t.Fatalf("MarshalPart = %s\nwant %s", raw, want)
	}
	got, ok, err := ParsePart(raw)
	if !ok || err != nil || !bytes.Equal(got.Ciphertext, ct) || got.Filename != p.Filename || got.Original != p.Original || got.BlobRef != "" {
		t.Fatalf("ParsePart = %+v, %v, %v", got, ok, err)
	}
	// Without a filename.
	raw2, _ := MarshalPart(Part{Ciphertext: ct, Original: "text"})
	if strings.Contains(string(raw2), "filename") {
		t.Fatalf("an empty filename is written: %s", raw2)
	}
	if got, ok, err := ParsePart(raw2); !ok || err != nil || got.Original != "text" {
		t.Fatalf("ParsePart = %+v, %v, %v", got, ok, err)
	}
	// metadata may carry other keys, and kontra.sealed may be absent.
	if got, ok, err := ParsePart(json.RawMessage(`{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age;v=1","metadata":{"x":1}}`)); !ok || err != nil || got.Original != "" {
		t.Fatalf("ParsePart = %+v, %v, %v", got, ok, err)
	}

	// The blob form: written for nodes, read by ParseDeliveredPart only.
	braw, err := MarshalPart(Part{BlobRef: "abc234", Original: "image/png"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(braw), `{"url":"kontra-blob:abc234","mediaType":"`+MediaType+`"`) {
		t.Fatalf("blob part = %s", braw)
	}
	if bp, ok, err := ParseDeliveredPart(braw); !ok || err != nil || bp.BlobRef != "abc234" || bp.Original != "image/png" || bp.Ciphertext != nil {
		t.Fatalf("ParseDeliveredPart = %+v, %v, %v", bp, ok, err)
	}
	if _, ok, err := ParsePart(braw); !ok || err == nil {
		t.Fatalf("ParsePart of a kontra-blob part = %v, %v; want ok with an error", ok, err)
	}
	if dp, ok, err := ParseDeliveredPart(raw); !ok || err != nil || !bytes.Equal(dp.Ciphertext, ct) {
		t.Fatalf("ParseDeliveredPart of a raw part = %+v, %v, %v", dp, ok, err)
	}
	for _, bad := range []Part{{}, {Ciphertext: ct}, {Ciphertext: ct, BlobRef: "abc", Original: "x"}, {BlobRef: "../x", Original: "x"}} {
		if _, err := MarshalPart(bad); err == nil {
			t.Errorf("MarshalPart(%+v) succeeds", bad)
		}
	}

	env := `"mediaType":"application/vnd.kontra.sealed+age;v=1"`
	for _, c := range []struct {
		name, part string
	}{
		{"text form", `{"text":"hello",` + env + `}`},
		{"url form", `{"url":"https://example.com/x",` + env + `}`},
		{"data form", `{"data":{"a":1},` + env + `}`},
		{"raw and text", `{"raw":"AAAA","text":"x",` + env + `}`},
		{"no content", `{` + env + `}`},
		{"case of the value", `{"raw":"AAAA","mediaType":"Application/Vnd.Kontra.Sealed+Age;V=1"}`},
		{"white space", `{"raw":"AAAA","mediaType":" application/vnd.kontra.sealed+age; v=1"}`},
		{"quoted v", `{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age;v=\"1\""}`},
		{"another parameter", `{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age;v=1;x=y"}`},
		{"unreadable media type", `{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age;v=1;v=1"}`},
		{"case of the key", `{"raw":"AAAA","MediaType":"application/vnd.kontra.sealed+age;v=1"}`},
		{"two media types", `{"raw":"AAAA","mediaType":"text/plain","MEDIATYPE":"application/vnd.kontra.sealed+age;v=1"}`},
		{"repeated mediaType", `{"raw":"AAAA",` + env + `,` + env + `}`},
		{"extra member", `{"raw":"AAAA",` + env + `,"x":1}`},
		{"raw not base64", `{"raw":"not base64",` + env + `}`},
		{"raw base64url", `{"raw":"_-_-",` + env + `}`},
		{"raw empty", `{"raw":"",` + env + `}`},
		{"raw a number", `{"raw":1,` + env + `}`},
		{"filename a number", `{"raw":"AAAA",` + env + `,"filename":1}`},
		{"metadata repeated key", `{"raw":"AAAA",` + env + `,"metadata":{"kontra.sealed":{"original":"a"},"kontra.sealed":{"original":"b"}}}`},
		{"kontra.sealed false", `{"raw":"AAAA",` + env + `,"metadata":{"kontra.sealed":false}}`},
		{"kontra.sealed extra", `{"raw":"AAAA",` + env + `,"metadata":{"kontra.sealed":{"original":"a","x":1}}}`},
		{"original a number", `{"raw":"AAAA",` + env + `,"metadata":{"kontra.sealed":{"original":1}}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, ok, err := ParsePart(json.RawMessage(c.part))
			if !ok || err == nil {
				t.Fatalf("ParsePart = ok %v, err %v; want ok true with an error", ok, err)
			}
		})
	}
	for _, plain := range []string{
		`{"text":"hello"}`,
		`{"text":"hello","mediaType":"text/plain"}`,
		`{"raw":"AAAA","mediaType":"application/octet-stream","metadata":{"kontra.sealed":false}}`,
		`{"data":{"mediaType":"application/vnd.kontra.sealed+age;v=1"}}`, // inside data is the content's business
		`{"text":"x","mediaType":5}`,
		`{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age;v=2"}`, // another envelope version
		`{"raw":"AAAA","mediaType":"application/vnd.kontra.sealed+age"}`,     // no version
	} {
		if _, ok, err := ParsePart(json.RawMessage(plain)); ok || err != nil {
			t.Errorf("ParsePart(%s) = ok %v, %v; want not sealed", plain, ok, err)
		}
	}
	for _, notPart := range []string{`[]`, `"x"`, `{"a":1} {}`, ``} {
		if _, ok, err := ParsePart(json.RawMessage(notPart)); ok || err == nil {
			t.Errorf("ParsePart(%q) = ok %v, %v; want ok false with an error", notPart, ok, err)
		}
	}
}

func TestMCPForms(t *testing.T) {
	ct := []byte("age-encryption.org/v1\nciphertext")
	for _, isError := range []bool{false, true} {
		raw, err := MCPResult(ct, isError)
		if err != nil {
			t.Fatal(err)
		}
		// isError stays outside, readable by anyone.
		m, err := strictjson.Members(raw)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]string{true: "true", false: "false"}[isError]; string(m["isError"]) != want {
			t.Fatalf("isError = %s, want %s", m["isError"], want)
		}
		if _, has := m["structuredContent"]; has {
			t.Fatal("structuredContent is kept")
		}
		var r struct {
			Content []struct {
				Type     string `json:"type"`
				Resource struct {
					URI, MimeType, Blob string
				} `json:"resource"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || len(r.Content) != 1 || r.Content[0].Type != "resource" ||
			r.Content[0].Resource.URI != ResourceURI || r.Content[0].Resource.MimeType != MediaType {
			t.Fatalf("MCPResult = %s", raw)
		}
		got, ok, err := ParseMCPResult(raw)
		if !ok || err != nil || !bytes.Equal(got, ct) {
			t.Fatalf("ParseMCPResult = %q, %v, %v", got, ok, err)
		}
	}
	// A result with _meta beside it (a related-task key) is still one.
	withMeta := `{"content":[{"type":"resource","resource":{"uri":"kontra:sealed","mimeType":"` + MediaType + `","blob":"AAAA"}}],"isError":false,"_meta":{"io.modelcontextprotocol/related-task":{"taskId":"t"}}}`
	if _, ok, err := ParseMCPResult(json.RawMessage(withMeta)); !ok || err != nil {
		t.Fatalf("with _meta: %v, %v", ok, err)
	}
	for _, plain := range []string{`{"content":[{"type":"text","text":"hi"}],"isError":false}`, `{"content":[]}`, `{"tools":[]}`,
		`{"content":[{"type":"resource","resource":{"uri":"file:///a","mimeType":"text/plain","text":"x"}}]}`} {
		if _, ok, err := ParseMCPResult(json.RawMessage(plain)); ok || err != nil {
			t.Errorf("ParseMCPResult(%s) = %v, %v; want not sealed", plain, ok, err)
		}
	}
	res := func(inner string) string { return `{"type":"resource","resource":{` + inner + `}}` }
	good := `"uri":"kontra:sealed","mimeType":"` + MediaType + `","blob":"AAAA"`
	for _, c := range []struct{ name, result string }{
		{"two items", `{"content":[` + res(good) + `,{"type":"text","text":"leak"}]}`},
		{"structuredContent", `{"content":[` + res(good) + `],"structuredContent":{"leak":1}}`},
		{"wrong mimeType", `{"content":[` + res(`"uri":"kontra:sealed","mimeType":"text/plain","blob":"AAAA"`) + `]}`},
		{"text resource", `{"content":[` + res(`"uri":"kontra:sealed","mimeType":"`+MediaType+`","text":"x"`) + `]}`},
		{"envelope type, another uri", `{"content":[` + res(`"uri":"file:///x","mimeType":"`+MediaType+`","blob":"AAAA"`) + `]}`},
		{"case-variant key", `{"content":[` + res(good+`,"URI":"x"`) + `]}`},
		{"case of the key", `{"content":[{"type":"resource","resource":{"URI":"kontra:sealed","mimeType":"` + MediaType + `","blob":"AAAA"}}]}`},
		{"blob not base64", `{"content":[` + res(`"uri":"kontra:sealed","mimeType":"`+MediaType+`","blob":"%%%"`) + `]}`},
		{"isError a string", `{"content":[` + res(good) + `],"isError":"false"}`},
		{"item extra member", `{"content":[{"type":"resource","annotations":{},"resource":{` + good + `}}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok, err := ParseMCPResult(json.RawMessage(c.result)); !ok || err == nil {
				t.Fatalf("ParseMCPResult = ok %v, %v; want ok with an error", ok, err)
			}
		})
	}

	// The envelope: NewMCPEnvelope's JSON is exactly {v, envelope}, and opens back to the ciphertext.
	b, err := json.Marshal(NewMCPEnvelope(ct))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"v":1,"envelope":"`+base64.StdEncoding.EncodeToString(ct)+`"}` {
		t.Fatalf("envelope = %s", b)
	}
	back, err := ParseMCPEnvelope(b)
	if err != nil || !bytes.Equal(back, ct) {
		t.Fatalf("ParseMCPEnvelope = %q, %v", back, err)
	}
}

// TestRejectDetailIsBounded: a refusal's Detail, which goes to the node's log, never copies the sender's bytes
// unbounded or with a raw line break, whatever part of the header carries them.
func TestRejectDetailIsBounded(t *testing.T) {
	id := newID(t)
	g, err := requestHeader(t, kidsOf(id)).Canonical()
	if err != nil {
		t.Fatal(err)
	}
	big := "y\ntime=2026 level=INFO msg=forged " + strings.Repeat("y", 1<<20)
	bigKey, _ := json.Marshal(big)
	bigUpper, _ := json.Marshal(strings.ToUpper(big))
	for _, c := range []struct{ name, hdr string }{
		{"unknown member", `{"v":1,` + string(bigKey) + `:1}`},
		{"case-variant pair", `{"v":1,` + string(bigKey) + `:1,` + string(bigUpper) + `:2}`},
		{"long dir", strings.Replace(string(g), `"dir":"req"`, `"dir":`+string(bigKey), 1)},
		{"long kind", strings.Replace(string(g), `"kind":"text"`, `"kind":`+string(bigKey), 1)},
		{"long number", strings.Replace(string(g), `"iat":1761490000`, `"iat":1`+strings.Repeat("0", 1<<20), 1)},
		{"long string for a number", strings.Replace(string(g), `"part":0`, `"part":`+string(bigKey), 1)},
		{"not JSON", `{"v":1,` + string(bigKey)},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseHeader([]byte(c.hdr))
			wantReason(t, err, RejectBadHeader)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatal(err)
			}
			if len(re.Detail) > 300 || strings.ContainsAny(re.Detail, "\n\r") || re.Detail == "" {
				t.Fatalf("Detail is %d bytes (line break %v): %.300q", len(re.Detail),
					strings.ContainsAny(re.Detail, "\n\r"), re.Detail)
			}
		})
	}
}

func TestBoundDetail(t *testing.T) {
	for _, in := range []string{"", "plain", strings.Repeat("\x00", 1000), strings.Repeat("é", 1000),
		"a\nb\rc\x1b[31m", strings.Repeat("x", 255) + "é"} {
		out := boundDetail(in)
		if len(out) > maxDetail+len("…") || strings.IndexFunc(out, unicode.IsControl) >= 0 || !utf8.ValidString(out) {
			t.Errorf("boundDetail(%.40q) = %.60q (%d bytes)", in, out, len(out))
		}
	}
	if got := boundDetail("plain detail"); got != "plain detail" {
		t.Errorf("a short detail changed: %q", got)
	}
}
