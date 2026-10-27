package sealing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"kontra.sh/kontra/internal/a2awire"
	"kontra.sh/kontra/internal/jcs"
	"kontra.sh/kontra/internal/jws"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealed"
	"kontra.sh/kontra/internal/strictjson"
	"kontra.sh/kontra/internal/tunnel"
)

// SealUsage is kontra seal's synopsis (spec/ext-sealed.md §5.1).
const SealUsage = `usage:
  kontra seal --service <service URL> [--api-key <key>|@<file>] [--context <id>] [--task <id>] [--media-type <type>] [file | -]
  kontra seal --open <part.json>
`

// ErrUsage is a kontra seal command line that is not one of SealUsage's (the CLI exits 2).
var ErrUsage = errors.New("usage")

// usageErr is a usage error with the synopsis.
func usageErr(why string) error {
	if why != "" {
		return fmt.Errorf("%w: %s\n%s", ErrUsage, why, SealUsage)
	}
	return fmt.Errorf("%w\n%s", ErrUsage, SealUsage)
}

// maxCardBytes bounds a card, a key set or an input file kontra seal reads.
const (
	maxCardBytes = 1 << 20
	maxSealInput = 64 << 20
)

// SealCommand is `kontra seal` (spec/ext-sealed.md §5.1, PRD §8): the escape hatch for scripting, not the path.
// It fetches the service's served card, verifies the gate's signature (jku = the service URL's origin +
// /.well-known/jwks.json) and the pin (§7), mints a per-job key and rk, and prints one A2A 1.0 Message JSON
// (role user, a fresh messageId, the contextId given or minted, taskId when given) whose single part is the
// sealed file, with metadata["kontra.sealed.replyTo"] set; the per-job key is kept under
// ~/.kontra/jobs/<gate host>/ so that --open can open the replies' parts. Sealing in-process without the CLI
// means reproducing §3 exactly; the example documents the age calls (Go filippo.io/age, Python pyrage), not an
// SDK.
//
// The file is sealed as one raw part (--media-type, default application/octet-stream; its name is not sent).
// --open reads one sealed part (or a Message, a Task or an artifact holding sealed parts) and writes the plaintext
// of each part a key of ~/.kontra/jobs opens, after checking dir res, the job key's id in kids and auth.
//
// SPEC-CONFLICT: §5.1 has kontra seal fetch the service's card with no word on a credential, but relay.md §2.1 answers
// a card GET of a service that neither is listed nor admits anonymous with 404 when no credential admits the caller,
// so kontra seal could seal for listed services only (and examples/sealing's flow, an allow: org:default service,
// failed). It sends an API key given explicitly with --api-key (or @file), never one read from the environment: a
// variable such as $KONTRA_API_KEY is tied to no gate, and the gate a --service names is not trusted (§7), so a key
// exported for one gate would reach every other one sealed for. As kontra connect sends a key only to the --gate it
// was given with, the key goes only to the --service's origin, and only where §2.1 needs a credential: the service's
// card, after an unauthenticated GET of it was 404, and then its upstream card; never the gate's key set
// (/.well-known/jwks.json is public), and never to a listed service, whose cards are public.
//
// SPEC-CONFLICT: §5.1 has kontra seal mint a per-job key and rk for every message it seals, --task included, but a
// follow-up's header must carry the job's replyTo and rk (§5 rule 3: wrong_job otherwise), so a follow-up sealed
// with a fresh key could never be accepted. --open binds a job to the gate task its replies name (renaming the job's
// key and state to the task id, as the hiring node names its jobs), and --task <id> seals with that task's job key,
// rk and contextId; a task with no job here is refused, saying to --open one of its replies first.
func SealCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return sealCommand(ctx, args, stdin, stdout, stderr, os.Getenv("HOME"), http.DefaultClient, time.Now)
}

func sealCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, home string,
	client *http.Client, now func() time.Time) error {
	fs := flag.NewFlagSet("kontra seal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	service := fs.String("service", "", "")
	ctxID := fs.String("context", "", "")
	task := fs.String("task", "", "")
	open := fs.String("open", "", "")
	mediaType := fs.String("media-type", "application/octet-stream", "")
	apiKey := fs.String("api-key", "", "")
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	var key string // only --api-key's: never the environment's (SealCommand's SPEC-CONFLICT)
	if *apiKey != "" {
		key = *apiKey
		if path, ok := strings.CutPrefix(key, "@"); ok {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("--api-key: %w", err)
			}
			key = strings.TrimSpace(string(b))
		}
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			return usageErr("--api-key: want the key, or @<file> holding it")
		}
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return errors.New("no home directory for ~/.kontra: set HOME")
		}
		home = h
	}
	if *open != "" {
		if *service != "" || fs.NArg() > 0 || *apiKey != "" {
			return usageErr("--open takes one part file and nothing else")
		}
		return openParts(*open, home, stdout)
	}
	if *service == "" || fs.NArg() > 1 {
		return usageErr("")
	}
	svcURL, err := url.Parse(*service)
	if err != nil || (svcURL.Scheme != "https" && svcURL.Scheme != "http") || svcURL.Host == "" {
		return usageErr(fmt.Sprintf("--service %q: want the service's URL, https://<gate>/agents/<name>", *service))
	}
	if svcURL.Scheme == "http" && !loopbackHost(svcURL.Hostname()) {
		return usageErr("--service: https:// except on localhost")
	}
	in := stdin
	if fs.NArg() == 1 && fs.Arg(0) != "-" {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	data, err := io.ReadAll(io.LimitReader(in, maxSealInput+1))
	if err != nil {
		return err
	}
	if len(data) > maxSealInput {
		return fmt.Errorf("the input is over %d bytes", maxSealInput)
	}
	origin, err := tunnel.CanonicalOrigin(svcURL)
	if err != nil {
		return err
	}
	svcPath := strings.TrimRight(svcURL.EscapedPath(), "/")
	cardURL := origin + svcPath + "/.well-known/agent-card.json"
	get := keyedGet(client, key, origin+svcPath+"/.well-known/agent-card.json", origin+svcPath+"/.well-known/agent-card.upstream.json")
	cardBody, err := get(ctx, cardURL)
	if err != nil {
		return fmt.Errorf("the service's card: %w", err)
	}
	card, err := VerifyCard(ctx, cardBody, origin, get)
	if err != nil {
		return err
	}
	ext, err := cardExtension(card.Body)
	if err != nil {
		return fmt.Errorf("the service's card: %w", err)
	}
	if ext == nil {
		return errors.New("the service is not sealed (its card carries no " + sealed.URI + ")")
	}
	recipients, kids, _, err := parseExtension(ext)
	if err != nil {
		return fmt.Errorf("the service's sealed extension: %w", err)
	}
	if len(recipients) == 0 {
		return errors.New("the sealed service has no recipient (no node is enrolled for it)")
	}
	ps, err := pins.Open(filepath.Join(home, nodekey.DirName, pins.FileName))
	if err != nil {
		return err
	}
	name := pinName(svcPath)
	outcome, err := ps.Check(origin, name, pins.Pin{Sealed: true, Recipients: kids, SellerKeys: card.SellerKeys, Signed: card.Signed}, pins.Options{})
	var ce *pins.ChangeError
	switch {
	case errors.As(err, &ce):
		return errors.New(tunnel.Printable(ce.Detail(pins.Options{})) + " (kontra connect --accept-change records it)")
	case err != nil:
		return err
	case outcome.First:
		fmt.Fprintf(stderr, "kontra seal: pins: %s at %s first seen: recipients %s, seller keys %s\n", name, origin,
			list(kids), list(card.SellerKeys))
	}
	jobs, err := nodekey.OpenJobKeys(home, svcURL.Host)
	if err != nil {
		return err
	}
	var (
		jobID  string
		jobKey *age.X25519Identity
		rk     []byte
	)
	if *task != "" {
		// A follow-up: the task's job key and rk (rule 3), bound by --open.
		var st hireJob
		jobID = *task
		if jobKey, err = jobs.Load(jobID); err == nil {
			err = readJSON(filepath.Join(jobs.Dir(), jobID+".json"), &st)
		}
		if err == nil && st.Service != name {
			err = fmt.Errorf("its job is for %s", st.Service)
		}
		if err == nil {
			rk, err = sealed.DecodeRK(st.RK)
		}
		if err != nil {
			return fmt.Errorf("no job of task %s for %s under %s (%v): open one of the task's replies with kontra seal --open "+
				"first, so that the follow-up is sealed with the task's job key (ext-sealed.md §5 rule 3)", *task, name, jobs.Dir(), err)
		}
		if *ctxID == "" {
			*ctxID = st.ContextID
		}
	} else {
		var rb [12]byte
		if _, err := io.ReadFull(randReader, rb[:]); err != nil {
			return err
		}
		jobID = "seal-" + hex.EncodeToString(rb[:])
		if jobKey, err = jobs.Mint(jobID, now()); err != nil {
			return err
		}
		if rk, err = sealed.NewRK(); err != nil {
			return err
		}
		if *ctxID == "" {
			*ctxID = uuid.NewString()
		}
		if err := writeJSON(jobs.Dir(), jobID+".json", &hireJob{ID: jobID, Service: name, RK: sealed.EncodeRK(rk),
			ContextID: *ctxID, Created: now()}); err != nil {
			return err
		}
	}
	msgID := uuid.NewString()
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, IAT: now().Unix(), TaskID: *task, ContextID: *ctxID,
		MessageID: msgID, Part: 0, Parts: 1, Kind: sealed.KindRaw, MediaType: *mediaType, Kids: kids,
		ReplyTo: jobKey.Recipient().String(), RK: sealed.EncodeRK(rk)}
	ct, err := sealed.Seal(recipients, hd, data)
	if err != nil {
		return err
	}
	part, err := sealed.MarshalPart(sealed.Part{Ciphertext: ct, Original: *mediaType})
	if err != nil {
		return err
	}
	msg := map[string]any{
		"messageId": msgID,
		"role":      "ROLE_USER",
		"contextId": *ctxID,
		"parts":     []json.RawMessage{part},
		"metadata":  map[string]string{sealed.MetadataReplyTo: jobKey.Recipient().String()},
	}
	if *task != "" {
		msg["taskId"] = *task
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(msg); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "kontra seal: sealed to %s; the reply key is %s (job %s)\n", list(kids), tunnel.KeyID(jobKey.Recipient().String()), jobID)
	return nil
}

// randReader is crypto/rand (tests may replace it).
var randReader = cryptoRand{}

// VerifyCard verifies the gate's signature on a served card (spec/relay.md §3): one signature that verifies with the
// key set at jku = origin + /.well-known/jwks.json, fetched from that origin only; a card none of whose signatures
// does is refused. When the gate says it verified a seller signature (kontra.upstream.verified), the upstream card
// is fetched from kontra.upstream.url (on the same origin), its JCS hash checked against kontra.upstream.sha256, and
// its signatures' key ids are the seller keys the pins record (ext-sealed.md §7).
func VerifyCard(ctx context.Context, card []byte, origin string, get func(ctx context.Context, u string) ([]byte, error)) (*Card, error) {
	if len(card) > maxCardBytes {
		return nil, errors.New("the card is over 1 MiB")
	}
	jku := origin + jws.JWKSPath
	m, err := strictjson.Members(card)
	if err != nil {
		return nil, fmt.Errorf("the card is not one JSON object with exact keys: %w", err)
	}
	var sigs []jws.Signature
	if raw, ok := m["signatures"]; ok {
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil {
			return nil, errors.New("the card's signatures is not an array")
		}
		for _, e := range elems {
			sm, err := strictjson.Members(e)
			if err != nil {
				return nil, err
			}
			var s jws.Signature
			if json.Unmarshal(sm["protected"], &s.Protected) != nil || json.Unmarshal(sm["signature"], &s.Signature) != nil {
				return nil, errors.New("a signature without protected and signature strings")
			}
			s.Header = sm["header"]
			sigs = append(sigs, s)
		}
	}
	if len(sigs) == 0 {
		return nil, errors.New("the card is not signed by the gate (relay.md §3): refused")
	}
	kb, err := get(ctx, jku)
	if err != nil {
		return nil, fmt.Errorf("the gate's key set at %s: %w", jku, err)
	}
	keys, err := jws.ParseJWKS(kb)
	if err != nil {
		return nil, err
	}
	var hdr *jws.Header
	for _, s := range sigs {
		h, err := jws.Verify(card, s, keys)
		if err == nil && h.JKU == jku {
			hdr = &h
			break
		}
	}
	if hdr == nil {
		return nil, fmt.Errorf("the card's gate signature does not verify with the key set at %s: refused", jku)
	}
	out := &Card{Body: card}
	if up := hdr.Upstream; up != nil && up.Verified {
		u, err := url.Parse(up.URL)
		if err != nil {
			return nil, errors.New("kontra.upstream.url is not a URL")
		}
		if o, err := tunnel.CanonicalOrigin(u); err != nil || o != origin {
			return nil, errors.New("kontra.upstream.url is not on the gate's origin")
		}
		ub, err := get(ctx, up.URL)
		if err != nil {
			return nil, fmt.Errorf("the upstream card: %w", err)
		}
		ids, err := SellerKeyIDs(ub, up.SHA256)
		if err != nil {
			return nil, err
		}
		out.SellerKeys, out.Signed = ids, true
	}
	return out, nil
}

// SellerKeyIDs checks an upstream card against the hex SHA-256 of its JCS that the gate signed (kontra.upstream.sha256)
// and returns the key ids (kid) of its signatures, sorted: what the pins record of the seller (ext-sealed.md §7).
func SellerKeyIDs(upstream []byte, sha string) ([]string, error) {
	c, err := jcs.Transform(upstream)
	if err != nil {
		return nil, fmt.Errorf("the upstream card has no canonical form: %w", err)
	}
	sum := sha256.Sum256(c)
	if hex.EncodeToString(sum[:]) != strings.ToLower(sha) {
		return nil, errors.New("the upstream card does not match the hash the gate signed (kontra.upstream.sha256)")
	}
	m, err := strictjson.Members(upstream)
	if err != nil {
		return nil, err
	}
	var elems []json.RawMessage
	if raw, ok := m["signatures"]; ok {
		if json.Unmarshal(raw, &elems) != nil {
			return nil, errors.New("the upstream card's signatures is not an array")
		}
	}
	var ids []string
	for _, e := range elems {
		sm, err := strictjson.Members(e)
		if err != nil {
			continue
		}
		var prot string
		if json.Unmarshal(sm["protected"], &prot) != nil {
			continue
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(prot, "="))
		if err != nil {
			continue
		}
		pm, err := strictjson.Members(b)
		if err != nil {
			continue
		}
		if kid, ok := a2awire.String(pm, "kid"); ok && kid != "" && !slices.Contains(ids, kid) {
			ids = append(ids, kid)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// openParts is kontra seal --open: every sealed part of the file that a per-job key under ~/.kontra/jobs opens is
// checked (dir res, the key's id in kids, auth with the job's rk) and its plaintext written to out: text, url and
// data as they are, raw as its bytes, one after another. A sealed part none of the job keys opens is an error, except
// in the client's own message in a Task's history (a ROLE_USER entry), which is left out.
func openParts(path, home string, out io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ids, rks, where, err := allJobKeys(home)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no per-job key under ~/.kontra/jobs")
	}
	type found struct {
		part json.RawMessage
		own  bool // in the client's own message in a Task's history
	}
	var parts []found
	if _, ok, _ := sealed.ParsePart(b); ok {
		parts = append(parts, found{part: b})
	} else {
		w := a2awire.Walker{Container: func(c *a2awire.Container) error {
			role, _ := a2awire.String(c.Members, "role")
			for _, p := range c.Parts {
				parts = append(parts, found{part: a2awire.Encode(p), own: role == "ROLE_USER" && c.Where == a2awire.WhereHistory})
			}
			return nil
		}}
		if _, _, err := w.WalkReply(b); err != nil {
			return fmt.Errorf("%s holds no sealed part: %w", path, err)
		}
	}
	opened := 0
	for _, f := range parts {
		sp, ok, err := sealed.ParsePart(f.part)
		if !ok {
			continue
		}
		if err != nil {
			return err
		}
		// A Task's history holds the client's own sealed message as the gate stored it, sealed dir req to the hosting
		// nodes, which none of the job keys is a recipient of: it is left out, as the hiring node leaves it out
		// (hiring.go). Anywhere else, a part that does not open is an error.
		if f.own {
			if _, err := age.Decrypt(bytes.NewReader(sp.Ciphertext), ids...); err != nil {
				var nomatch *age.NoIdentityMatchError
				if errors.As(err, &nomatch) {
					continue
				}
			}
		}
		hd, plain, err := sealed.Open(ids, sp.Ciphertext)
		if err != nil {
			return fmt.Errorf("a part does not open with this node's job keys: %w", err)
		}
		if hd.Dir != sealed.DirReply || len(hd.Kids) != 1 {
			return errors.New("a part that is not a reply (dir res, one key id)")
		}
		rk, ok := rks[hd.Kids[0]]
		if !ok {
			return errors.New("no job state (rk) for the reply's key")
		}
		if err := sealed.VerifyAuth(rk, hd, plain); err != nil {
			return errors.New("the reply's auth does not verify: refused")
		}
		if _, err := out.Write(plain); err != nil {
			return err
		}
		opened++
		// The reply names the gate task its job opened: the job is bound to it, so that --task seals a follow-up
		// with this key and rk (SealCommand's SPEC-CONFLICT on §5.1).
		if loc, ok := where[hd.Kids[0]]; ok && hd.TaskID != "" && loc.id != hd.TaskID {
			if err := bindJob(loc.jobs, loc.id, hd.TaskID); err != nil {
				return err
			}
			loc.id = hd.TaskID
		}
	}
	if opened == 0 {
		return fmt.Errorf("%s holds no sealed part a job key here opens", path)
	}
	return nil
}

// jobLocation is where a per-job key is kept: its gate's directory and its job id.
type jobLocation struct {
	jobs *nodekey.JobKeys
	id   string
}

// bindJob renames a kontra seal job (seal-…) to the gate task id its replies name: its key, then its state, whose
// id it rewrites. A job already named for the task, or a task that already has another job here, is left as it is.
func bindJob(jobs *nodekey.JobKeys, from, task string) error {
	if !strings.HasPrefix(from, "seal-") {
		return nil
	}
	if _, err := jobs.Load(task); err == nil {
		return nil
	}
	var st hireJob
	if err := readJSON(filepath.Join(jobs.Dir(), from+".json"), &st); err != nil {
		return err
	}
	if err := jobs.Rename(from, task); err != nil {
		return fmt.Errorf("bind job %s to task %s: %w", from, task, err)
	}
	st.ID = task
	if err := writeJSON(jobs.Dir(), task+".json", &st); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(jobs.Dir(), from+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// allJobKeys loads every per-job key under ~/.kontra/jobs/*/ with its rk (the job's state file) and where it is
// kept, by key id.
func allJobKeys(home string) ([]age.Identity, map[string][]byte, map[string]*jobLocation, error) {
	root := filepath.Join(home, nodekey.DirName, nodekey.JobsDirName)
	gates, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	var ids []age.Identity
	rks := map[string][]byte{}
	where := map[string]*jobLocation{}
	for _, g := range gates {
		if !g.IsDir() {
			continue
		}
		jobs, err := nodekey.OpenJobKeys(home, g.Name())
		if err != nil {
			continue
		}
		ents, err := os.ReadDir(jobs.Dir())
		if err != nil {
			continue
		}
		for _, e := range ents {
			id, ok := strings.CutSuffix(e.Name(), ".key")
			if !ok {
				continue
			}
			key, err := jobs.Load(id)
			if err != nil {
				continue
			}
			var st hireJob
			if readJSON(filepath.Join(jobs.Dir(), id+".json"), &st) != nil {
				continue
			}
			rk, err := sealed.DecodeRK(st.RK)
			if err != nil {
				continue
			}
			ids = append(ids, key)
			kid := tunnel.KeyID(key.Recipient().String())
			rks[kid] = rk
			where[kid] = &jobLocation{jobs: jobs, id: id}
		}
	}
	return ids, rks, where, nil
}

// keyedGet is kontra seal's GET (SealCommand's SPEC-CONFLICT on a credential): key, --api-key's or "", goes as a
// bearer credential only to the URLs of keyed (the service's card and upstream card, on the --service's origin,
// compared in canonical form), and only once a GET of one of them without it was 404, the unknown-service answer of
// relay.md §2.1 to a caller no credential admits; the later GETs of keyed send it at once. Any other URL (the gate's
// key set) never gets it.
func keyedGet(client *http.Client, key string, keyed ...string) func(ctx context.Context, u string) ([]byte, error) {
	needed := false
	return func(ctx context.Context, u string) ([]byte, error) {
		mayKey := key != "" && slices.Contains(keyed, canonicalURL(u))
		if mayKey && needed {
			return httpGet(ctx, client, u, key)
		}
		b, err := httpGet(ctx, client, u, "")
		var se *statusError
		if !mayKey || !errors.As(err, &se) || se.code != http.StatusNotFound {
			return b, err
		}
		needed = true
		return httpGet(ctx, client, u, key)
	}
}

// canonicalURL is u as its canonical origin and escaped path, "" when it has none or carries a query or a fragment.
func canonicalURL(u string) string {
	pu, err := url.Parse(u)
	if err != nil || pu.RawQuery != "" || pu.ForceQuery || pu.Fragment != "" {
		return ""
	}
	o, err := tunnel.CanonicalOrigin(pu)
	if err != nil {
		return ""
	}
	return o + pu.EscapedPath()
}

// statusError is a GET answered with a status other than 200.
type statusError struct {
	url  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("%s answered HTTP %d", e.url, e.code) }

// httpGet GETs u, sending key as a bearer credential when it is set (keyedGet decides where it may go).
func httpGet(ctx context.Context, client *http.Client, u, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &statusError{url: u, code: res.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxCardBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxCardBytes {
		return nil, fmt.Errorf("%s answered over %d bytes", u, maxCardBytes)
	}
	return bytes.TrimSpace(b), nil
}

func loopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// cryptoRand reads crypto/rand.
type cryptoRand struct{}

func (cryptoRand) Read(p []byte) (int, error) { return rand.Read(p) }
