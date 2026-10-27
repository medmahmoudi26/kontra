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
  kontra seal --service <service URL> [--context <id>] [--task <id>] [--media-type <type>] [file | -]
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
	if err := fs.Parse(args); err != nil {
		return usageErr(err.Error())
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return errors.New("no home directory for ~/.kontra: set HOME")
		}
		home = h
	}
	if *open != "" {
		if *service != "" || fs.NArg() > 0 {
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
	cardBody, err := httpGet(ctx, client, cardURL)
	if err != nil {
		return fmt.Errorf("the service's card: %w", err)
	}
	card, err := VerifyCard(ctx, cardBody, origin, func(ctx context.Context, u string) ([]byte, error) {
		return httpGet(ctx, client, u)
	})
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
	var rb [12]byte
	if _, err := io.ReadFull(randReader, rb[:]); err != nil {
		return err
	}
	jobID := "seal-" + hex.EncodeToString(rb[:])
	key, err := jobs.Mint(jobID, now())
	if err != nil {
		return err
	}
	rk, err := sealed.NewRK()
	if err != nil {
		return err
	}
	if *ctxID == "" {
		*ctxID = uuid.NewString()
	}
	if err := writeJSON(jobs.Dir(), jobID+".json", &hireJob{ID: jobID, Service: name, RK: sealed.EncodeRK(rk),
		ContextID: *ctxID, Created: now()}); err != nil {
		return err
	}
	msgID := uuid.NewString()
	hd := sealed.Header{V: sealed.Version, Dir: sealed.DirRequest, IAT: now().Unix(), TaskID: *task, ContextID: *ctxID,
		MessageID: msgID, Part: 0, Parts: 1, Kind: sealed.KindRaw, MediaType: *mediaType, Kids: kids,
		ReplyTo: key.Recipient().String(), RK: sealed.EncodeRK(rk)}
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
		"metadata":  map[string]string{sealed.MetadataReplyTo: key.Recipient().String()},
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
	fmt.Fprintf(stderr, "kontra seal: sealed to %s; the reply key is %s (job %s)\n", list(kids), tunnel.KeyID(key.Recipient().String()), jobID)
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
// data as they are, raw as its bytes, one after another.
func openParts(path, home string, out io.Writer) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ids, rks, err := allJobKeys(home)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("no per-job key under ~/.kontra/jobs")
	}
	var parts []json.RawMessage
	if _, ok, _ := sealed.ParsePart(b); ok {
		parts = append(parts, b)
	} else {
		w := a2awire.Walker{Container: func(c *a2awire.Container) error {
			for _, p := range c.Parts {
				parts = append(parts, a2awire.Encode(p))
			}
			return nil
		}}
		if _, _, err := w.WalkReply(b); err != nil {
			return fmt.Errorf("%s holds no sealed part: %w", path, err)
		}
	}
	opened := 0
	for _, p := range parts {
		sp, ok, err := sealed.ParsePart(p)
		if !ok {
			continue
		}
		if err != nil {
			return err
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
	}
	if opened == 0 {
		return fmt.Errorf("%s holds no sealed part", path)
	}
	return nil
}

// allJobKeys loads every per-job key under ~/.kontra/jobs/*/ with its rk (the job's state file), by key id.
func allJobKeys(home string) ([]age.Identity, map[string][]byte, error) {
	root := filepath.Join(home, nodekey.DirName, nodekey.JobsDirName)
	gates, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var ids []age.Identity
	rks := map[string][]byte{}
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
			rks[tunnel.KeyID(key.Recipient().String())] = rk
		}
	}
	return ids, rks, nil
}

func httpGet(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered HTTP %d", u, res.StatusCode)
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
