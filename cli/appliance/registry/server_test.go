package registry

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// --- harness ---------------------------------------------------------------------------------

func startRegistry(t *testing.T) (*Server, string) {
	t.Helper()
	return startRegistryIn(t, t.TempDir())
}

// startRegistryIn starts one on a FREE port, never the default.
//
// `Port: 0` would be the default 5000 — the same rule every other service in this package uses —
// so a suite run on the development controller would fight the appliance, or the compose registry,
// that is already there. This was not a hypothetical: written as `Port: 0`, two tests here talked
// to something else on 5000 and one of them reported zero bytes served for a pull that had
// succeeded against a different process.
func startRegistryIn(t *testing.T, dir string) (*Server, string) {
	t.Helper()
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	srv, err := Start(Options{DataDir: dir, Port: port, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, dir
}

// ociClient is the half of the OCI push/pull protocol a test needs, written out rather than
// mocked, because the thing under test IS the protocol.
//
// NOT `client`: temporal_test.go imports go.temporal.io/sdk/client, and a package-level type of
// that name shadows the import for the whole test binary.
type ociClient struct {
	t    *testing.T
	base string
}

func (c *ociClient) do(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *ociClient) body(resp *http.Response) []byte {
	c.t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	return b
}

// errorCode reads the OCI error envelope out of a failed response.
func (c *ociClient) errorCode(resp *http.Response) (code, message string) {
	c.t.Helper()
	var env struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	b := c.body(resp)
	if err := json.Unmarshal(b, &env); err != nil || len(env.Errors) == 0 {
		c.t.Fatalf("not an OCI error envelope: %s", b)
	}
	return env.Errors[0].Code, env.Errors[0].Message
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// pushBlobChunked is the path `docker push` takes for a layer: open a session, PATCH the bytes in
// pieces, close with the digest.
func (c *ociClient) pushBlobChunked(repo string, data []byte, chunks int) string {
	c.t.Helper()
	resp := c.do(http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusAccepted {
		c.t.Fatalf("POST uploads: got %s, want 202", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		c.t.Fatal("POST uploads returned no Location")
	}
	size := len(data)
	step := size/chunks + 1
	at := 0
	for at < size {
		end := min(at+step, size)
		resp := c.do(http.MethodPatch, loc, bytes.NewReader(data[at:end]), map[string]string{
			"Content-Range": fmt.Sprintf("%d-%d", at, end-1),
			"Content-Type":  "application/octet-stream",
		})
		c.body(resp)
		if resp.StatusCode != http.StatusAccepted {
			c.t.Fatalf("PATCH upload at %d: got %s, want 202", at, resp.Status)
		}
		at = end
	}
	d := digestOf(data)
	resp = c.do(http.MethodPut, loc+"?digest="+d, nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("PUT upload: got %s, want 201", resp.Status)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != d {
		c.t.Fatalf("PUT upload reported %q, want %q", got, d)
	}
	return d
}

// pushBlobMonolithic is the single-POST form, which small config blobs take.
func (c *ociClient) pushBlobMonolithic(repo string, data []byte) string {
	c.t.Helper()
	d := digestOf(data)
	resp := c.do(http.MethodPost, "/v2/"+repo+"/blobs/uploads/?digest="+d, bytes.NewReader(data), nil)
	c.body(resp)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("monolithic POST: got %s, want 201", resp.Status)
	}
	return d
}

const testManifestType = "application/vnd.oci.image.manifest.v1+json"

func manifestFor(configDigest string, configSize int, layers []string, sizes []int) []byte {
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     testManifestType,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": configDigest, "size": configSize},
	}
	var ls []any
	for i, l := range layers {
		ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": l, "size": sizes[i]})
	}
	m["layers"] = ls
	b, _ := json.Marshal(m)
	return b
}

// pushImage is a whole push: config, layers, manifest. It returns the manifest digest the
// registry reported — the value ADR 0032 makes the actor's identity.
func (c *ociClient) pushImage(repo, tag string, config []byte, layers [][]byte) string {
	c.t.Helper()
	cd := c.pushBlobMonolithic(repo, config)
	var ds []string
	var sizes []int
	for _, l := range layers {
		ds = append(ds, c.pushBlobChunked(repo, l, 3))
		sizes = append(sizes, len(l))
	}
	man := manifestFor(cd, len(config), ds, sizes)
	resp := c.do(http.MethodPut, "/v2/"+repo+"/manifests/"+tag, bytes.NewReader(man), map[string]string{
		"Content-Type": testManifestType,
	})
	c.body(resp)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("PUT manifest: got %s, want 201", resp.Status)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		c.t.Fatal("PUT manifest reported no Docker-Content-Digest — the push has no identity to record")
	}
	return d
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

// --- the protocol ----------------------------------------------------------------------------

func TestRegistryPingAnnouncesV2(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	resp := c.do(http.MethodGet, "/v2/", nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v2/: got %s, want 200", resp.Status)
	}
	// The header a client uses to tell a v2 registry from a web server that happens to 200 the
	// path. Docker reads it off the first response it gets.
	if got := resp.Header.Get("Docker-Distribution-API-Version"); got != "registry/2.0" {
		t.Errorf("Docker-Distribution-API-Version = %q, want registry/2.0", got)
	}
}

// TestPushPullRoundTrip is the acceptance criterion in one function: everything that went in
// comes back out, byte for byte, by tag and by digest.
func TestPushPullRoundTrip(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}

	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layers := [][]byte{randomBytes(t, 40_000), randomBytes(t, 7_000)}
	digest := c.pushImage("kontra/beacon", "0.2.0", config, layers)

	// By tag.
	resp := c.do(http.MethodGet, "/v2/kontra/beacon/manifests/0.2.0", nil, nil)
	man := c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest by tag: got %s, want 200", resp.Status)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Errorf("tag resolves to %q, push reported %q", got, digest)
	}
	if got := resp.Header.Get("Content-Type"); got != testManifestType {
		t.Errorf("Content-Type = %q, want %q", got, testManifestType)
	}
	if digestOf(man) != digest {
		t.Errorf("manifest bytes hash to %s, not %s", digestOf(man), digest)
	}

	// By digest — the reference a run records.
	resp = c.do(http.MethodGet, "/v2/kontra/beacon/manifests/"+digest, nil, nil)
	byDigest := c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET manifest by digest: got %s, want 200", resp.Status)
	}
	if !bytes.Equal(man, byDigest) {
		t.Error("the tag and the digest resolve to different bytes")
	}

	// Every blob the manifest names comes back whole.
	for i, want := range append([][]byte{config}, layers...) {
		d := digestOf(want)
		resp := c.do(http.MethodGet, "/v2/kontra/beacon/blobs/"+d, nil, nil)
		got := c.body(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET blob %d: got %s, want 200", i, resp.Status)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("blob %d came back different (%d bytes in, %d out)", i, len(want), len(got))
		}
		if h := resp.Header.Get("Docker-Content-Digest"); h != d {
			t.Errorf("blob %d served as %q, want %q", i, h, d)
		}
	}
}

// TestPushDigestIsWhatAPullResolves pins ADR 0032's identity rule: the digest the push reported is
// the one a later HEAD resolves the tag to, and it is the hash of the bytes served.
func TestPushDigestIsWhatAPullResolves(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	digest := c.pushImage("kontra/beacon", "0.2.0", []byte(`{"os":"linux"}`), [][]byte{randomBytes(t, 1024)})

	resp := c.do(http.MethodHead, "/v2/kontra/beacon/manifests/0.2.0", nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD manifest: got %s, want 200", resp.Status)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != digest {
		t.Fatalf("HEAD resolves the tag to %q; the push reported %q", got, digest)
	}

	// A MOVED TAG MOVES THE DIGEST, which is the whole reason identity is not the tag. Re-push
	// different content under the same version and the tag resolves somewhere else.
	moved := c.pushImage("kontra/beacon", "0.2.0", []byte(`{"os":"linux","x":1}`), [][]byte{randomBytes(t, 1024)})
	if moved == digest {
		t.Fatal("two different images pushed to one tag produced one digest")
	}
	resp = c.do(http.MethodHead, "/v2/kontra/beacon/manifests/0.2.0", nil, nil)
	c.body(resp)
	if got := resp.Header.Get("Docker-Content-Digest"); got != moved {
		t.Errorf("after a re-push the tag resolves to %q, want %q", got, moved)
	}
	// …and the original is still reachable by ITS digest, which is what makes a recorded digest
	// worth recording.
	resp = c.do(http.MethodHead, "/v2/kontra/beacon/manifests/"+digest, nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the superseded manifest is gone: HEAD by digest got %s", resp.Status)
	}
}

// TestLayersLiveInTheSharedCAS is the "not a second store" criterion, checked on disk: the file a
// push produced is at the CAS address, and `sha256sum` of it is the digest.
func TestLayersLiveInTheSharedCAS(t *testing.T) {
	srv, dir := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	layer := randomBytes(t, 12_345)
	d := c.pushBlobChunked("kontra/beacon", layer, 4)
	sum := strings.TrimPrefix(d, "sha256:")

	path := filepath.Join(dir, "cas", sum[:2], sum)
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the layer is not at the CAS address %s: %v", path, err)
	}
	if !bytes.Equal(onDisk, layer) {
		t.Error("the file at the CAS address is not the layer that was pushed")
	}
	if digestOf(onDisk) != d {
		t.Errorf("the file at %s hashes to %s, not %s", path, digestOf(onDisk), d)
	}
	// The registry's own directory holds NAMING only — tags and manifest records. Anything blob-
	// sized in there would be the second store this criterion is about.
	var heavy []string
	_ = filepath.Walk(filepath.Join(dir, "registry"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi != nil && !fi.IsDir() && fi.Size() > 4096 {
			heavy = append(heavy, fmt.Sprintf("%s (%d bytes)", p, fi.Size()))
		}
		return nil
	})
	if len(heavy) > 0 {
		t.Errorf("the registry's index holds content, not just names: %v", heavy)
	}
}

// TestSecondPushTransfersNoLayers is dedup from the push side: a client that HEADs before it
// uploads finds everything already there, and nothing is stored twice.
func TestSecondPushTransfersNoLayers(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	layers := [][]byte{randomBytes(t, 20_000), randomBytes(t, 5_000)}
	config := []byte(`{"os":"linux"}`)
	c.pushImage("kontra/beacon", "0.2.0", config, layers)

	stored := srv.BlobsStored()
	bytesIn := srv.BytesIn()
	if stored != 3 {
		t.Fatalf("first push stored %d blobs, want 3 (a config and two layers)", stored)
	}

	// The second push, by the book: HEAD each blob first.
	for _, b := range append([][]byte{config}, layers...) {
		resp := c.do(http.MethodHead, "/v2/kontra/beacon/blobs/"+digestOf(b), nil, nil)
		c.body(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD of an already-pushed blob: got %s, want 200 (the client would re-upload it)", resp.Status)
		}
	}
	if srv.BlobsStored() != stored || srv.BytesIn() != bytesIn {
		t.Errorf("a re-push wrote %d blobs / %d bytes; want 0 of each",
			srv.BlobsStored()-stored, srv.BytesIn()-bytesIn)
	}

	// A DIFFERENT REPOSITORY sharing those layers — the actual case, since every python actor
	// image shares the same base. Cross-repo mount answers 201 with no body at all.
	for _, b := range layers {
		d := digestOf(b)
		resp := c.do(http.MethodPost, "/v2/kontra/other/blobs/uploads/?mount="+d+"&from=kontra/beacon", nil, nil)
		c.body(resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("cross-repo mount: got %s, want 201", resp.Status)
		}
		if got := resp.Header.Get("Docker-Content-Digest"); got != d {
			t.Errorf("mount reported %q, want %q", got, d)
		}
	}
	if srv.BlobsStored() != stored || srv.BytesIn() != bytesIn {
		t.Errorf("mounting into a second repository wrote %d blobs / %d bytes; want 0 of each",
			srv.BlobsStored()-stored, srv.BytesIn()-bytesIn)
	}
}

// TestRePullTransfersNoLayers is the same property from the pull side. A client that already holds
// the content asks and is told nothing new — by HEAD, and by a conditional GET, neither of which
// moves a layer byte.
func TestRePullTransfersNoLayers(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	layer := randomBytes(t, 60_000)
	d := c.pushBlobChunked("kontra/beacon", layer, 2)

	// The first pull moves the bytes.
	resp := c.do(http.MethodGet, "/v2/kontra/beacon/blobs/"+d, nil, nil)
	if got := len(c.body(resp)); got != len(layer) {
		t.Fatalf("first pull served %d bytes, want %d", got, len(layer))
	}
	served, out := srv.BlobsServed(), srv.BytesOut()
	if served != 1 || out != uint64(len(layer)) {
		t.Fatalf("after one pull: %d blobs / %d bytes served, want 1 / %d", served, out, len(layer))
	}

	// A daemon that already has the layer HEADs and stops.
	resp = c.do(http.MethodHead, "/v2/kontra/beacon/blobs/"+d, nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD: got %s, want 200", resp.Status)
	}
	// A client that revalidates instead gets a 304 with no body, because the ETag is the digest
	// and content named by its own hash cannot have changed.
	resp = c.do(http.MethodGet, "/v2/kontra/beacon/blobs/"+d, nil, map[string]string{"If-None-Match": `"` + d + `"`})
	body := c.body(resp)
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET: got %s, want 304", resp.Status)
	}
	if len(body) != 0 {
		t.Errorf("a 304 carried %d bytes of body", len(body))
	}
	if srv.BlobsServed() != served || srv.BytesOut() != out {
		t.Errorf("a re-pull transferred %d blobs / %d bytes; want 0 of each",
			srv.BlobsServed()-served, srv.BytesOut()-out)
	}
}

func TestBlobDigestMismatchIsRefused(t *testing.T) {
	srv, dir := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}

	resp := c.do(http.MethodPost, "/v2/kontra/beacon/blobs/uploads/", nil, nil)
	c.body(resp)
	loc := resp.Header.Get("Location")
	resp = c.do(http.MethodPatch, loc, strings.NewReader("the wrong bytes"), nil)
	c.body(resp)

	lie := digestOf([]byte("the right bytes"))
	resp = c.do(http.MethodPut, loc+"?digest="+lie, nil, nil)
	code, msg := c.errorCode(resp)
	if resp.StatusCode != http.StatusBadRequest || code != "DIGEST_INVALID" {
		t.Fatalf("got %s/%s, want 400/DIGEST_INVALID", resp.Status, code)
	}
	// BOTH sides named: "integrity check failed" would leave an operator unable to tell a stale
	// pin from a truncated transfer.
	for _, want := range []string{strings.TrimPrefix(lie, "sha256:"), strings.TrimPrefix(digestOf([]byte("the wrong bytes")), "sha256:")} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not name %s: %s", want, msg)
		}
	}
	// And nothing was stored under either address.
	for _, d := range []string{lie, digestOf([]byte("the wrong bytes"))} {
		sum := strings.TrimPrefix(d, "sha256:")
		if _, err := os.Stat(filepath.Join(dir, "cas", sum[:2], sum)); err == nil {
			t.Errorf("the refused blob was stored anyway at %s", d)
		}
	}
	if srv.BlobsStored() != 0 {
		t.Errorf("BlobsStored = %d after a refused upload, want 0", srv.BlobsStored())
	}
}

func TestChunkOutOfOrderIsRefused(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	resp := c.do(http.MethodPost, "/v2/kontra/beacon/blobs/uploads/", nil, nil)
	c.body(resp)
	loc := resp.Header.Get("Location")

	resp = c.do(http.MethodPatch, loc, strings.NewReader("0123456789"), map[string]string{"Content-Range": "0-9"})
	c.body(resp)
	if got := resp.Header.Get("Range"); got != "0-9" {
		t.Fatalf("Range after 10 bytes = %q, want 0-9", got)
	}
	// A chunk that claims to start past the end — a lost chunk. Answering 202 here would produce
	// a blob that fails its digest at the very end of a long upload.
	resp = c.do(http.MethodPatch, loc, strings.NewReader("xxxx"), map[string]string{"Content-Range": "50-53"})
	code, _ := c.errorCode(resp)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("out-of-order chunk: got %s, want 416", resp.Status)
	}
	if code != "BLOB_UPLOAD_INVALID" {
		t.Errorf("code = %s, want BLOB_UPLOAD_INVALID", code)
	}
	if got := resp.Header.Get("Range"); got != "0-9" {
		t.Errorf("the 416 should tell the client where the upload actually is; Range = %q", got)
	}
}

func TestManifestReferencingAMissingBlobIsRefused(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	config := []byte(`{"os":"linux"}`)
	cd := c.pushBlobMonolithic("kontra/beacon", config)
	missing := digestOf([]byte("a layer nobody uploaded"))

	man := manifestFor(cd, len(config), []string{missing}, []int{23})
	resp := c.do(http.MethodPut, "/v2/kontra/beacon/manifests/0.1.0", bytes.NewReader(man),
		map[string]string{"Content-Type": testManifestType})
	code, msg := c.errorCode(resp)
	if resp.StatusCode != http.StatusNotFound || code != "MANIFEST_BLOB_UNKNOWN" {
		t.Fatalf("got %s/%s, want 404/MANIFEST_BLOB_UNKNOWN", resp.Status, code)
	}
	if !strings.Contains(msg, missing) {
		t.Errorf("the refusal does not name the missing blob: %s", msg)
	}
	// The tag must not exist afterwards: a push that half-succeeded is the state that fails at
	// pull time on another machine.
	resp = c.do(http.MethodGet, "/v2/kontra/beacon/manifests/0.1.0", nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the tag was written despite the refusal: GET got %s", resp.Status)
	}
}

func TestManifestPutUnderAWrongDigestIsRefused(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	config := []byte(`{"os":"linux"}`)
	cd := c.pushBlobMonolithic("kontra/beacon", config)
	man := manifestFor(cd, len(config), nil, nil)

	wrong := digestOf([]byte("not this manifest"))
	resp := c.do(http.MethodPut, "/v2/kontra/beacon/manifests/"+wrong, bytes.NewReader(man),
		map[string]string{"Content-Type": testManifestType})
	code, _ := c.errorCode(resp)
	if resp.StatusCode != http.StatusBadRequest || code != "DIGEST_INVALID" {
		t.Fatalf("got %s/%s, want 400/DIGEST_INVALID", resp.Status, code)
	}
}

// TestManifestsAreScopedPerRepository is the one place the shared CAS must NOT be shared: a tag is
// a name, and one repository's name may never resolve to another's image.
func TestManifestsAreScopedPerRepository(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	digest := c.pushImage("kontra/beacon", "0.2.0", []byte(`{"os":"linux"}`), [][]byte{randomBytes(t, 900)})

	// Same digest, different repository: the bytes are in the store and the manifest is still not
	// this repository's to serve.
	resp := c.do(http.MethodGet, "/v2/kontra/other/manifests/"+digest, nil, nil)
	code, _ := c.errorCode(resp)
	if resp.StatusCode != http.StatusNotFound || code != "MANIFEST_UNKNOWN" {
		t.Fatalf("another repository resolved the manifest: got %s/%s, want 404/MANIFEST_UNKNOWN", resp.Status, code)
	}
	// The tag likewise.
	resp = c.do(http.MethodGet, "/v2/kontra/other/manifests/0.2.0", nil, nil)
	c.body(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("another repository resolved the tag: %s", resp.Status)
	}
}

func TestTagsListAndCatalog(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	for _, v := range []string{"0.3.0", "0.1.0", "0.2.0"} {
		c.pushImage("kontra/beacon", v, []byte(`{"v":"`+v+`"}`), nil)
	}
	c.pushImage("kontra/crawl", "1.0.0", []byte(`{"os":"linux"}`), nil)

	resp := c.do(http.MethodGet, "/v2/kontra/beacon/tags/list", nil, nil)
	var tags struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(c.body(resp), &tags); err != nil {
		t.Fatalf("tags/list: %v", err)
	}
	if tags.Name != "kontra/beacon" || strings.Join(tags.Tags, ",") != "0.1.0,0.2.0,0.3.0" {
		t.Errorf("tags/list = %+v, want kontra/beacon with sorted tags", tags)
	}

	// `cli/deploy.go`'s re-deploy guard reads this endpoint. A repository nobody pushed is a 404,
	// not an empty list — the two are the same answer to the guard and different answers to an
	// operator asking where their image went.
	resp = c.do(http.MethodGet, "/v2/kontra/nothing/tags/list", nil, nil)
	code, _ := c.errorCode(resp)
	if resp.StatusCode != http.StatusNotFound || code != "NAME_UNKNOWN" {
		t.Errorf("unknown repository: got %s/%s, want 404/NAME_UNKNOWN", resp.Status, code)
	}

	resp = c.do(http.MethodGet, "/v2/_catalog", nil, nil)
	var cat struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(c.body(resp), &cat); err != nil {
		t.Fatalf("_catalog: %v", err)
	}
	if strings.Join(cat.Repositories, ",") != "kontra/beacon,kontra/crawl" {
		t.Errorf("_catalog = %v, want both repositories", cat.Repositories)
	}
}

// TestRepositoryNamesCannotEscapeTheIndex is the path-safety guard. `_tags` and `_manifests` begin
// with an underscore, which no OCI name component may — so no repository can be named such that
// its directory collides with another's bookkeeping, and no `..` gets that far either.
func TestRepositoryNamesCannotEscapeTheIndex(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	for _, bad := range []string{"../escape", "kontra/../../etc", "UPPER", "_tags"} {
		resp := c.do(http.MethodGet, "/v2/"+bad+"/tags/list", nil, nil)
		body := c.body(resp)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("repository %q was accepted: %s", bad, body)
		}
	}
}

func TestUnknownEndpointsAreNamedRatherThanBlank(t *testing.T) {
	srv, _ := startRegistry(t)
	c := &ociClient{t: t, base: srv.Endpoint()}
	resp := c.do(http.MethodGet, "/kontra/beacon", nil, nil)
	code, msg := c.errorCode(resp)
	if resp.StatusCode != http.StatusNotFound || code != "UNSUPPORTED" {
		t.Fatalf("got %s/%s, want 404/UNSUPPORTED", resp.Status, code)
	}
	// The object store is a different port on the same box, and a client pointed at the wrong one
	// should be told which one it hit.
	if !strings.Contains(msg, "registry") {
		t.Errorf("the 404 does not say what this server is: %s", msg)
	}
}

// --- the address ------------------------------------------------------------------------------

// TestAddressIsPublishedAndWithdrawn covers the mechanism that makes push and pull agree: the
// running registry writes the address it bound where the CLI looks, and takes it back on stop, so
// a stale file cannot point `kontra deploy` at a dead port.
func TestAddressIsPublishedAndWithdrawn(t *testing.T) {
	dir := t.TempDir()
	if addr, ok := ReadAddress(dir); ok {
		t.Fatalf("an address was published before anything started: %q", addr)
	}
	port, err := freePort("127.0.0.1")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	srv, err := Start(Options{DataDir: dir, Port: port, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr, ok := ReadAddress(dir)
	if !ok || addr != srv.Address() {
		t.Fatalf("published address %q (found=%v), want %q", addr, ok, srv.Address())
	}
	// The BOUND port, not the DEFAULT one: publishing 5000 while listening somewhere else is the
	// exact mismatch this whole mechanism exists to prevent.
	if addr != hostPort("127.0.0.1", port) {
		t.Errorf("published %q, but the listener is on port %d", addr, port)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if addr, ok := ReadAddress(dir); ok {
		t.Errorf("the address outlived the registry: %q", addr)
	}
}

func TestStaleUploadsAreClearedOnStart(t *testing.T) {
	dir := t.TempDir()
	debris := filepath.Join(dir, "registry", "_uploads")
	if err := os.MkdirAll(debris, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(debris, "half-a-layer"), randomBytes(t, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	startRegistryIn(t, dir)
	entries, err := os.ReadDir(debris)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a killed push's debris survived the restart: %d file(s)", len(entries))
	}
}

func TestTakenPortNamesTheCollision(t *testing.T) {
	first, dir := startRegistry(t)
	_, port, _ := strings.Cut(first.Address(), ":")
	n := 0
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatal(err)
	}
	_, err := Start(Options{DataDir: dir, Port: n, Logf: func(string, ...any) {}})
	if err == nil {
		t.Fatal("two registries bound one port")
	}
	// The address, because "the appliance is broken" and "you are running two registries" are one
	// sentence apart.
	if !strings.Contains(err.Error(), first.Address()) {
		t.Errorf("the bind failure does not name the address: %v", err)
	}
}

// --- the real thing ---------------------------------------------------------------------------

// TestDockerPushPullRoundTrip is the slice's actual claim, driven by the client it exists for.
// It builds a tiny image with no base (so nothing is fetched from Docker Hub), pushes it, checks
// the digest, proves a re-pull moves nothing, then deletes it locally and pulls it back — and
// compares the layer diff IDs, which are hashes of the uncompressed content, so "the same image"
// means the same bytes rather than the same name.
//
// It is skipped without a daemon rather than failed: this package has to build and test on a box
// that has never installed Docker, which is the appliance's whole claim (ADR 0032).
func TestDockerPushPullRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: this one drives a real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker CLI on PATH")
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Skipf("no reachable Docker daemon: %s", strings.TrimSpace(string(out)))
	}
	srv, _ := startRegistry(t)

	ref := srv.Address() + "/kontra-registry-selftest:1"
	ctx := t.TempDir()
	// THREE layers, not one, because the daemon uploads them CONCURRENTLY — which is the only
	// thing exercising the per-session locking and the store's publish-by-link race.
	df := "FROM scratch\n"
	for i := range 3 {
		name := fmt.Sprintf("payload%d", i)
		if err := os.WriteFile(filepath.Join(ctx, name), randomBytes(t, 256*1024), 0o644); err != nil {
			t.Fatal(err)
		}
		df += "COPY " + name + " /" + name + "\n"
	}
	// FROM scratch: no base image, so this build needs no network and cannot spend the Docker Hub
	// rate limit this repo has already measured.
	if err := os.WriteFile(filepath.Join(ctx, "Dockerfile"), []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	dockerRun("build", "-q", "-t", ref, ctx)
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", ref).Run() })

	before := strings.TrimSpace(dockerRun("image", "inspect", "--format", "{{json .RootFS.Layers}}", ref))

	// PUSH. The digest in the output is the identity `deployResult` records.
	pushOut := dockerRun("push", ref)
	pushed := parsePushedDigest(pushOut)
	if pushed == "" {
		t.Fatalf("no digest in the push output:\n%s", pushOut)
	}

	// …and it is what the registry resolves the tag to, which is the check `confirmPushed` makes.
	c := &ociClient{t: t, base: srv.Endpoint()}
	resp := c.do(http.MethodHead, "/v2/kontra-registry-selftest/manifests/1", nil, map[string]string{
		"Accept": "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json",
	})
	c.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD after push: got %s", resp.Status)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != pushed {
		t.Fatalf("docker pushed %s; the registry resolves the tag to %s", pushed, got)
	}

	// A PULL OF WHAT THE DAEMON ALREADY HAS TRANSFERS NOTHING.
	served := srv.BlobsServed()
	dockerRun("pull", ref)
	if srv.BlobsServed() != served {
		t.Errorf("a pull of an image the daemon already holds served %d blob(s)", srv.BlobsServed()-served)
	}

	// Now really pull it: drop it locally, including the digest reference the push added.
	_ = exec.Command("docker", "rmi", "-f", ref).Run()
	_ = exec.Command("docker", "rmi", "-f", srv.Address()+"/kontra-registry-selftest@"+pushed).Run()
	dockerRun("pull", ref)
	if srv.BlobsServed() <= served {
		t.Error("a cold pull served no blobs at all — the daemon cannot have got the content from here")
	}
	after := strings.TrimSpace(dockerRun("image", "inspect", "--format", "{{json .RootFS.Layers}}", ref))
	if after != before {
		t.Errorf("the image came back different:\n  before %s\n  after  %s", before, after)
	}
	digests := strings.TrimSpace(dockerRun("image", "inspect", "--format", "{{json .RepoDigests}}", ref))
	if !strings.Contains(digests, pushed) {
		t.Errorf("the pulled image does not carry the pushed digest %s: %s", pushed, digests)
	}
}

// parsePushedDigest reads the `<tag>: digest: sha256:… size: N` line docker prints at the end of a
// push. The API's `aux` message is what cli/deploy.go reads; the CLI prints this.
func parsePushedDigest(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "digest: sha256:"); i >= 0 {
			f := strings.Fields(line[i+len("digest: "):])
			if len(f) > 0 {
				return f[0]
			}
		}
	}
	return ""
}

// THE DOCKER DAEMON IS THE REGISTRY'S ONLY LOCAL CLIENT, AND IT WILL NOT SPEAK PLAIN HTTP TO A
// NON-LOOPBACK ADDRESS.
//
// This is not a hypothetical. `--bind 172.17.0.1` is REQUIRED for the local actor path, because a
// worker container cannot reach this host's loopback — and with a single listener on that address
// `kontra deploy` died on
//
//	http: server gave HTTP response to HTTPS client
//
// which reads as a TLS misconfiguration and is really the daemon's insecure-registry list, whose
// default membership is 127.0.0.0/8 and ::1. So the registry serves a second listener there and
// PUBLISHES that address, since the reader of the address file is the deploy/scale pair driving
// the daemon.
func TestRegistryAlsoServesLoopbackWhenBoundElsewhere(t *testing.T) {
	bind := aNonLoopbackIPv4(t)
	dir := t.TempDir()
	r, err := Start(Options{DataDir: dir, BindIP: bind, Port: aFreePort(t, bind), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("Start on %s: %v", bind, err)
	}
	defer r.Stop()

	_, port, err := net.SplitHostPort(r.BoundAddress())
	if err != nil {
		t.Fatalf("BoundAddress %q is not host:port: %v", r.BoundAddress(), err)
	}
	if want := net.JoinHostPort("127.0.0.1", port); r.Address() != want {
		t.Fatalf("Address() = %q, want the loopback one %q — that is what push and pull are told", r.Address(), want)
	}
	if published, ok := ReadAddress(dir); !ok || published != r.Address() {
		t.Fatalf("published address = %q (ok=%v), want %q", published, ok, r.Address())
	}

	// BOTH LISTENERS ARE THE SAME REGISTRY, which is the half a "there are two addresses" test
	// could pass without: one http.Server over two listeners, so a push's PATCHes and its PUT
	// cannot land on two different sets of upload sessions.
	for _, addr := range []string{r.Address(), r.BoundAddress()} {
		resp, err := http.Get("http://" + addr + "/v2/")
		if err != nil {
			t.Fatalf("GET http://%s/v2/: %v", addr, err)
		}
		status := resp.StatusCode
		version := resp.Header.Get("Docker-Distribution-API-Version")
		resp.Body.Close()
		if status != http.StatusOK {
			t.Errorf("http://%s/v2/ answered %d, want 200", addr, status)
		}
		if version != "registry/2.0" {
			t.Errorf("http://%s/v2/ did not identify as a v2 registry (%q)", addr, version)
		}
	}
}

// THE WILDCARD ALREADY COVERS LOOPBACK. `--bind 0.0.0.0` is an ordinary thing to type, and a
// second listener on 127.0.0.1 would fail with `address already in use` — from inside
// Start, as a refusal blaming the Docker daemon for a bind that was fine.
func TestRegistryOnWildcardOpensNoSecondListener(t *testing.T) {
	dir := t.TempDir()
	r, err := Start(Options{DataDir: dir, BindIP: "0.0.0.0", Port: aFreePort(t, "0.0.0.0"), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("Start on the wildcard: %v", err)
	}
	defer r.Stop()
	if r.loopbackLn != nil {
		t.Error("a wildcard bind already accepts on 127.0.0.1; a second listener there cannot bind")
	}
	if r.Address() != r.BoundAddress() {
		t.Errorf("wildcard: Address() %q and BoundAddress() %q must be the same string", r.Address(), r.BoundAddress())
	}
}

// aNonLoopbackIPv4 is an address on this machine that is neither loopback nor the wildcard —
// docker0, eth0, whatever this box has. SKIPPED rather than failed when there is none: a network
// namespace with only `lo` is a legitimate place to run a unit suite, and the property being
// pinned is about an address that does not exist there.
func aNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	t.Skip("no non-loopback IPv4 on this machine; the two-listener case cannot arise here")
	return ""
}

// aFreePort asks the kernel for one on a SPECIFIC address. Named apart from `temporal.go`'s
// `freePort`, which is the package's own helper and takes no address. `Port: 0` is not the way to
// ask here: Start reads it as DefaultPort (5000), which is the right default for
// the command and the wrong one for a test that must not collide with a developer's registry.
func aFreePort(t *testing.T, ip string) int {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// A LOOPBACK BIND GETS ONE LISTENER AND NOTHING CHANGES. The second listener is a fix for a
// specific bind, not a new default shape — and a second `net.Listen` on the address already held
// would fail rather than be skipped.
func TestRegistryOnLoopbackKeepsOneAddress(t *testing.T) {
	dir := t.TempDir()
	r, err := Start(Options{DataDir: dir, BindIP: "127.0.0.1", Port: 0, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop()
	if r.Address() != r.BoundAddress() {
		t.Fatalf("a loopback appliance must have ONE registry address, got %q and %q", r.Address(), r.BoundAddress())
	}
	if r.loopbackLn != nil {
		t.Error("a loopback bind must not open a second listener on the address it already holds")
	}
}

// TestBlobCountsLandBeforeTheBytesDo is the ordering `TestRePullTransfersNoLayers` depends on and
// cannot itself prove.
//
// THAT TEST IS PROBABILISTIC AND THIS ONE IS NOT. It pulls a blob and then reads the counters from
// the CLIENT goroutine, which is the only goroutine that can know a body arrived — so whether it
// passes depends on whether the server reached its accounting before the last bytes were flushed.
// It passed for months and then read `0 blobs / 0 bytes` on CI (2026-08-27) after an assertion on
// the same response's length had already succeeded. Fixing that by counting inside the writer is
// only a fix if the counting really happens before the write, which is what this asserts directly:
// a ResponseWriter that samples the counters at the moment it is handed bytes must already see
// them include those bytes.
func TestBlobCountsLandBeforeTheBytesDo(t *testing.T) {
	var blobs, bytesOut atomic.Uint64
	seen := &samplingWriter{blobs: &blobs, bytes: &bytesOut}
	cw := &countingWriter{ResponseWriter: seen, blobs: &blobs, bytes: &bytesOut}

	if _, err := cw.Write([]byte("first-chunk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := cw.Write([]byte("second")); err != nil {
		t.Fatalf("write: %v", err)
	}

	// AT THE MOMENT THE FIRST BYTES WERE HANDED OVER, the blob was already counted and its bytes
	// were already in the total. A reader who received them cannot be ahead of the accounting.
	if seen.blobsAt[0] != 1 {
		t.Errorf("the first chunk went out with %d blobs counted, want 1 — a client holding it could read zero",
			seen.blobsAt[0])
	}
	if seen.bytesAt[0] != uint64(len("first-chunk")) {
		t.Errorf("the first chunk went out with %d bytes counted, want %d", seen.bytesAt[0], len("first-chunk"))
	}
	// AND THE LAST CHUNK IS THE ONE THAT MATTERS, because it is the one whose flush ends the body.
	if seen.bytesAt[1] != uint64(len("first-chunk")+len("second")) {
		t.Errorf("the last chunk went out with %d bytes counted, want %d",
			seen.bytesAt[1], len("first-chunk")+len("second"))
	}
	// One blob, not one per chunk.
	if got := blobs.Load(); got != 1 {
		t.Errorf("two chunks of one blob counted %d blobs, want 1", got)
	}
}

// TestAShortWriteIsNotCountedAsTransferred is the other half: counting early is only honest if the
// bytes that never landed are given back. A dead connection must not inflate BytesOut, or "this
// re-pull transferred nothing" stops being a claim about transfer.
func TestAShortWriteIsNotCountedAsTransferred(t *testing.T) {
	var blobs, bytesOut atomic.Uint64
	cw := &countingWriter{ResponseWriter: &shortWriter{accept: 4}, blobs: &blobs, bytes: &bytesOut}
	n, err := cw.Write([]byte("ten-bytes!"))
	if n != 4 || err == nil {
		t.Fatalf("short write: got n=%d err=%v, want 4 and an error", n, err)
	}
	if got := bytesOut.Load(); got != 4 {
		t.Errorf("a write that landed 4 of 10 bytes counted %d, want 4", got)
	}
	if got := blobs.Load(); got != 1 {
		t.Errorf("a body that landed 4 bytes counted %d blobs, want 1 — some of it was served", got)
	}
}

// TestABodyThatNeverStartedIsNotAServedBlob is the far edge of counting early: a connection that
// died before the first byte. Optimism is safe only because it can be taken back, and it has to be
// taken back for BOTH counters — "1 blob / 0 bytes" would be a re-pull claim that reads as a
// transfer that never happened.
func TestABodyThatNeverStartedIsNotAServedBlob(t *testing.T) {
	var blobs, bytesOut atomic.Uint64
	cw := &countingWriter{ResponseWriter: &shortWriter{accept: 0}, blobs: &blobs, bytes: &bytesOut}
	if n, err := cw.Write([]byte("nothing lands")); n != 0 || err == nil {
		t.Fatalf("dead write: got n=%d err=%v, want 0 and an error", n, err)
	}
	if got := blobs.Load(); got != 0 {
		t.Errorf("a body that sent no byte counted %d blobs, want 0", got)
	}
	if got := bytesOut.Load(); got != 0 {
		t.Errorf("a body that sent no byte counted %d bytes, want 0", got)
	}
}

// samplingWriter records what the counters said at the instant each chunk was handed to it — the
// instant after which a client may hold those bytes.
type samplingWriter struct {
	blobs   *atomic.Uint64
	bytes   *atomic.Uint64
	blobsAt []uint64
	bytesAt []uint64
	header  http.Header
}

func (s *samplingWriter) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}

func (s *samplingWriter) Write(p []byte) (int, error) {
	s.blobsAt = append(s.blobsAt, s.blobs.Load())
	s.bytesAt = append(s.bytesAt, s.bytes.Load())
	return len(p), nil
}

func (s *samplingWriter) WriteHeader(int) {}

// shortWriter accepts a fixed prefix and then reports the connection gone, which is what a client
// that hung up mid-body looks like from inside a handler.
type shortWriter struct {
	accept int
	header http.Header
}

func (w *shortWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) <= w.accept {
		return len(p), nil
	}
	return w.accept, io.ErrClosedPipe
}

func (w *shortWriter) WriteHeader(int) {}
