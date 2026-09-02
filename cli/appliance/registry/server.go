// server.go — the OCI registry the appliance serves from inside the binary, over the SAME
// content-addressed store everything else hydrates from (ADR 0032, issues 11 and 12). It is what
// removes `registry` and `registry-data` from docker-compose.yml.
//
// WHY THE LAYERS AND THE STORE ARE THE SAME THING. An OCI blob is addressed by
// `sha256:<hex>` over its own bytes, and `handler/internal/cas` addresses an object by sha256
// over its own bytes. They are not two stores that happen to agree; they are one address. So a
// layer PUT lands at `<data-dir>/cas/<ab>/<hex>` — the file the hydrator would have fetched, the
// file `sha256sum` confirms — and a push of an image whose layers are already there transfers
// nothing, because the daemon HEADs each digest first and this store already answers yes.
//
// THAT SHARING HAS ONE CONSEQUENCE WORTH SAYING OUT LOUD: BLOBS ARE GLOBAL. A real registry links
// each blob into each repository that uses it, so `GET /v2/other/blobs/<digest>` 404s for content
// only `mine` pushed. Here there is one CAS, so any repository can read any blob it can name — and
// naming it means already holding its sha256, which is the entire secret. That is deliberate: it
// is what makes a second actor image sharing the python base upload zero bytes, and it is only
// safe because this registry is single-tenant and unauthenticated by design, bound to loopback
// like the rest of the appliance (ADR 0031 §3). MANIFESTS are scoped per repository, because those
// are what a tag resolves to and a `docker pull kontra/a:1` must never be answered with `b`'s image.
//
// NO `distribution` DEPENDENCY, AND THAT IS A DECISION RATHER THAN A SHORTCUT. Its library entry
// point is `registry/handlers` over a `storagedriver`, and a storagedriver is a general
// filesystem-like store: GetContent/PutContent/Reader/append-Writer/Stat/List/Move/Delete/Walk
// over ITS path layout (`/docker/registry/v2/blobs/sha256/<ab>/<hex>/data`, `_uploads/<uuid>/data`,
// `_manifests/tags/<tag>/current/link`). Backing that with the CAS means implementing a general
// mutable filesystem and then special-casing the one path shape that happens to be a blob — which
// is a second store with a hole cut in it, against an internal layout that is not part of anyone's
// compatibility promise. Measured: 233 MB of module cache for the import, before the four-platform
// build this repo is heading for. What is served instead is the OCI distribution spec's pull and
// push surface directly — /v2/, blobs (incl. chunked, monolithic and cross-repo mount), manifests,
// tags, _catalog — which is the part `docker push` and `docker pull` actually speak.
//
// DELETION IS ABSENT ON PURPOSE. This store has other customers — the hydrated Node runtime, the
// built SPA, an actor's artifacts — so "delete this layer" is a garbage-collection question about
// the whole appliance, not a registry operation. Untagging without deleting bytes would be a lie
// about reclaimed disk; deleting bytes without asking the other customers is a corruption. The
// slice that adds retention answers it once, for the store.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/medmahmoudi26/kontra-local/handler/casstore"
)

// DefaultPort is the port actor images are pushed to and pulled from.
//
// 5000, unchanged from the compose `registry` service, because the port is half of an image
// REFERENCE: every `kontra/<name>:<ver>` an operator has already pushed is spelled
// `localhost:5000/<name>:<ver>`, and a different port would silently orphan all of them. It is
// also the number `cli/deploy.go`'s default and its own error message have always printed.
const DefaultPort = 5000

// maxManifestBytes bounds a manifest read. Manifests are lists of descriptors — the largest thing
// in this repo is a few kilobytes — and the body arrives before anything has authenticated,
// because nothing here authenticates. A cap is what stops one POST from being the whole memory
// budget.
const maxManifestBytes = 4 << 20

// addressFileName is where the bound address is published for the CLI to read. See
// ReadAddress: this file is the whole mechanism by which push and pull agree.
const addressFileName = "address"

// mediaTypeOctet is what a blob is served as. A layer is a tar.gz and a config is JSON, but the
// spec says a blob GET is opaque bytes and the client already knows from the manifest which it
// asked for.
const mediaTypeOctet = "application/octet-stream"

// Reference grammar, from the OCI distribution spec. Both are also PATH SAFETY: the name becomes
// a directory under repositories/ and the tag becomes a file under it, and neither grammar can
// produce `.`, `..` or an absolute path. Nothing else in this file re-checks, so nothing else may
// build a path from an unvalidated one.
var (
	repoNameRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRe      = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Options configures the embedded registry. DataDir is the only required field.
type Options struct {
	// DataDir is the appliance's data directory — the same one Temporal, the object store and
	// the state store are given. The CAS lives at DataDir/cas and this registry's tag index at
	// DataDir/registry.
	DataDir string

	// Store is the shared CAS. nil opens one at DataDir, which is the same store `hydrate.Open`
	// would return for that root — one directory, one address space. A caller that already holds
	// one (the hydrator, in a later slice) passes it so the two share an open handle rather than
	// two views of one tree.
	Store *casstore.Local

	// BindIP is the address to listen on. Empty means 127.0.0.1.
	//
	// LOOPBACK IS THE SECURITY CONTROL, and here it is also a Docker one: the daemon refuses a
	// plaintext registry unless the host is loopback or is listed in `insecure-registries`. A
	// bind to a VPC address therefore needs daemon configuration on every host that pushes or
	// pulls — which is exactly the topology ADR 0032 declines for fleet Machines, since they run
	// Bundles natively and pull no images at all.
	BindIP string

	// Port is the registry port; 0 means DefaultPort.
	Port int

	// Logf receives failures the registry could not serve. nil means stderr.
	Logf func(format string, args ...any)
}

// Server is a started registry. It is returned already listening.
type Server struct {
	srv     *http.Server
	ln      net.Listener
	address string
	// loopbackLn is the SECOND listener, and it exists for one client: the Docker daemon.
	// See Start. nil when the bind address is already a loopback one.
	loopbackLn net.Listener
	// boundAddress is what the `--bind` listener answers on, which is what an operator sees
	// printed. `address` is what push and pull are told to use, and the two differ exactly when
	// the appliance was bound somewhere a `docker push` would insist on TLS.
	boundAddress string
	cas          *casstore.Local
	index        *registryIndex
	logf         func(string, ...any)

	uploads sync.Map // upload id -> *uploadSession

	blobsStored atomic.Uint64
	blobsServed atomic.Uint64
	bytesIn     atomic.Uint64
	bytesOut    atomic.Uint64
}

// Address is host:port — the registry half of an image reference, and the value push and pull
// must BOTH resolve to. On an appliance bound off-loopback this is the LOOPBACK address, which is
// deliberate: see Start.
func (r *Server) Address() string { return r.address }

// BoundAddress is what the `--bind` listener answers on. Equal to Address on a loopback
// appliance; on any other bind it is the second address the same registry serves, for a client
// that is not this host's Docker daemon.
func (r *Server) BoundAddress() string {
	if r.boundAddress == "" {
		return r.address
	}
	return r.boundAddress
}

// Endpoint is the base URL, for a caller that wants to probe /v2/ itself.
func (r *Server) Endpoint() string { return "http://" + r.address }

// CASRoot is the directory the layers actually live in, so `kontra up` can print it and an
// operator can `du -sh` it and find the same bytes a hydration would.
func (r *Server) CASRoot() string { return r.cas.Root() }

// BlobsStored counts completed blob UPLOADS — layers and config blobs. It is the number that
// stays flat when a push finds the store already holds every layer, which is the whole point of
// sharing the CAS. Manifests are not counted: they are kilobytes, and counting them would blur
// exactly the signal this exists to give.
func (r *Server) BlobsStored() uint64 { return r.blobsStored.Load() }

// BlobsServed is how many blob bodies this process has sent. A re-pull by a daemon that already
// has the layers leaves it unchanged; that is what "transfers nothing" means from this side.
func (r *Server) BlobsServed() uint64 { return r.blobsServed.Load() }

// BytesIn / BytesOut are blob bytes only — manifests are kilobytes and would only blur the
// signal these two exist to give.
func (r *Server) BytesIn() uint64  { return r.bytesIn.Load() }
func (r *Server) BytesOut() uint64 { return r.bytesOut.Load() }

// ReadAddress returns the address the registry running against dataDir bound, if one is.
//
// THE ADDRESS FILE IS HOW PUSH AND PULL AGREE, and it exists because the failure it prevents is
// the historically expensive one. `kontra deploy` tags an image with a registry address and
// pushes; `kontra scale` pulls by an address it resolves separately; and an image the daemon does
// not have does not auto-pull on create — so two addresses that disagree surface as `no such
// image`, which sends an operator to look at Docker. One writer, one reader, one string.
//
// A KILLED APPLIANCE LEAVES IT BEHIND, and that is the right failure rather than a missing one:
// the next deploy resolves the dead address, cannot reach /v2/ there, and says so NAMING it.
// Guessing that the file is stale and falling back would produce a push to a second registry that
// nobody asked for, which is the failure this whole mechanism exists to make impossible.
func ReadAddress(dataDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dataDir, "registry", addressFileName))
	if err != nil {
		return "", false
	}
	addr := strings.TrimSpace(string(b))
	return addr, addr != ""
}

// needsLoopbackListener decides whether a registry bound to `ip` must open a SECOND listener on
// 127.0.0.1 for the Docker daemon's sake. See Start for why the daemon needs one.
//
// TWO ADDRESSES ARE ALREADY COVERED AND MUST NOT GET A SECOND LISTENER, and one of them is not
// obvious. A loopback bind is the trivial case. The other is the WILDCARD: `0.0.0.0` already
// accepts on 127.0.0.1, so a second `net.Listen` there fails with `address already in use` — and
// it would fail from inside Start, turning a perfectly ordinary `--bind 0.0.0.0` into a
// refusal that blames the Docker daemon. Measured, by a test written before this function existed.
func needsLoopbackListener(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified()
}

// Start boots the registry and returns once it is serving.
func Start(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("appliance: DataDir is required (the registry stores layers in the appliance's shared CAS)")
	}
	if opts.BindIP == "" {
		opts.BindIP = "127.0.0.1"
	}
	if net.ParseIP(opts.BindIP) == nil {
		return nil, fmt.Errorf("appliance: bind address %q is not an IP (use 127.0.0.1, not a hostname)", opts.BindIP)
	}
	if opts.Port == 0 {
		opts.Port = DefaultPort
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "kontra-registry: "+format+"\n", args...) }
	}

	store := opts.Store
	if store == nil {
		var err error
		store, err = casstore.NewLocal(opts.DataDir)
		if err != nil {
			return nil, fmt.Errorf("appliance: %w", err)
		}
	}
	index, err := newRegistryIndex(filepath.Join(opts.DataDir, "registry"))
	if err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}

	addr := hostPort(opts.BindIP, opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Named, like every other embedded service's: an operator whose older compose stack is up
		// with `--profile extras` owns 5000 through the `registry` service this replaces, and two
		// registries on one port serve different image sets depending on start order.
		return nil, fmt.Errorf("appliance: cannot listen on %s for the registry — "+
			"another process (the compose `registry` service, or a hand-started `kontra-registry`?) has it: %w", addr, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		addr = hostPort(opts.BindIP, tcp.Port)
	}

	// THE SECOND LISTENER, AND THE ONE CLIENT IT IS FOR: THIS HOST'S DOCKER DAEMON.
	//
	// `kontra deploy` does not speak to this registry itself — it hands an image reference to the
	// Docker daemon and the DAEMON pushes, and later pulls. And the daemon refuses plain HTTP to
	// anything it does not consider an insecure registry, a list that by default contains exactly
	// `127.0.0.0/8` and `::1`. So an appliance bound anywhere else answers a push with
	//
	//     http: server gave HTTP response to HTTPS client
	//
	// MEASURED, and it is not an edge case: `--bind 172.17.0.1` is REQUIRED for actor Workers,
	// because a worker container cannot reach this host's loopback (appliance/appliance.go). The
	// bind that makes the actor path possible is the bind that broke the deploy half of it.
	//
	// A loopback listener costs nothing and is the narrowest possible answer. It is not a
	// weakening: 127.0.0.1 is reachable from this machine and nowhere else, which is strictly less
	// than the `--bind` listener beside it. The alternative — telling every operator to edit
	// `/etc/docker/daemon.json` and restart the daemon — is the "a control plane that asks you to
	// hand-start one of its own components is not installed, it is assembled" that ADR 0031 §1
	// removed, with a daemon restart on top.
	//
	// THE PUBLISHED ADDRESS IS THE LOOPBACK ONE, because the reader of that file is `kontra
	// deploy` and `kontra serve --mode docker` on this host, and both of them are driving the
	// daemon. An operator pointing a DIFFERENT machine at this registry passes `--registry` or
	// KONTRA_REGISTRY and gets the wide listener, unchanged.
	var loopbackLn net.Listener
	publish := addr
	if needsLoopbackListener(net.ParseIP(opts.BindIP)) {
		port := opts.Port
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			port = tcp.Port
		}
		loopback := hostPort("127.0.0.1", port)
		loopbackLn, err = net.Listen("tcp", loopback)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("appliance: the registry is bound on %s and also needs %s, because "+
				"the Docker daemon will not push plain HTTP to anything but a loopback address — and that "+
				"port is taken: %w", addr, loopback, err)
		}
		publish = loopback
	}

	r := &Server{ln: ln, address: publish, boundAddress: addr, loopbackLn: loopbackLn, cas: store, index: index, logf: logf}
	// An upload session is process state (see uploadSession), so anything left in the directory
	// belongs to a run that is over. Clearing it on the way up is what stops a killed push from
	// costing disk forever — the alternative is a sweeper, for debris with no reader.
	if n, err := index.clearUploads(); err != nil {
		logf("could not clear stale upload sessions: %v", err)
	} else if n > 0 {
		logf("discarded %d unfinished upload(s) from a previous run", n)
	}
	if err := index.publishAddress(publish); err != nil {
		_ = ln.Close()
		if loopbackLn != nil {
			_ = loopbackLn.Close()
		}
		return nil, fmt.Errorf("appliance: %w", err)
	}
	r.srv = &http.Server{
		Handler: r,
		// No ReadTimeout, for the object store's reason: a layer is hundreds of megabytes and a
		// slow push is not a broken one. The HEADER deadline is bounded, because a client that
		// has not finished its request line is not pushing anything.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		if err := r.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("listener stopped: %v", err)
		}
	}()
	// ONE http.Server, TWO listeners — the same handler, the same CAS, the same tag index. Two
	// servers would be two sets of upload sessions, and a push whose PATCHes landed on one and
	// whose PUT landed on the other would fail on a digest nobody could explain.
	if loopbackLn != nil {
		go func() {
			if err := r.srv.Serve(loopbackLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("loopback listener stopped: %v", err)
			}
		}()
	}
	return r, nil
}

// Stop shuts the listener down, letting in-flight requests finish, and withdraws the published
// address — a stale address file would point `kontra deploy` at a port nothing is on, and the
// resulting connection refused is a worse message than "no registry is running".
func (r *Server) Stop() error {
	_ = r.index.withdrawAddress()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.srv.Shutdown(ctx); err != nil {
		return r.srv.Close()
	}
	return nil
}

// --- routing ------------------------------------------------------------------------------

func (r *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// On EVERY response, not just /v2/. It is how a client tells a v2 registry from a web server
	// that happens to 200 the path, and Docker's own check reads it off the first thing it gets.
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	p := req.URL.EscapedPath()
	if p == "/v2" || p == "/v2/" {
		r.ping(w, req)
		return
	}
	if !strings.HasPrefix(p, "/v2/") {
		r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "UNSUPPORTED",
			Message: "not a registry path: " + p + " (this is the OCI registry; the object store is a different port)"})
		return
	}
	rest, err := unescapePath(strings.TrimPrefix(p, "/v2/"))
	if err != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "NAME_INVALID", Message: err.Error()})
		return
	}
	if rest == "_catalog" {
		r.catalog(w, req)
		return
	}

	name, verb, ref := routeV2(rest)
	if verb == "" {
		r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "UNSUPPORTED",
			Message: "not a registry endpoint: /v2/" + rest})
		return
	}
	if !repoNameRe.MatchString(name) {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "NAME_INVALID",
			Message: fmt.Sprintf("repository name %q is not a valid OCI name", name)})
		return
	}

	switch verb {
	case "blobs":
		r.serveBlob(w, req, name, ref)
	case "uploads":
		r.serveUpload(w, req, name, ref)
	case "manifests":
		r.serveManifest(w, req, name, ref)
	case "tags":
		r.serveTags(w, req, name)
	}
}

// routeV2 splits `<name>/<verb>/<ref>` where the NAME may itself contain slashes, so the split is
// found from the right. `<name>/blobs/uploads/<id>` is matched before `<name>/blobs/<digest>`
// because an upload id is not a digest and the two share a prefix.
func routeV2(rest string) (name, verb, ref string) {
	if s, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		return s, "tags", ""
	}
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		return rest[:i], "manifests", rest[i+len("/manifests/"):]
	}
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		tail := rest[i+len("/blobs/"):]
		// `uploads` without the trailing slash is the same endpoint. Falling through would send it
		// to the blob handler, which would report an invalid DIGEST for the word "uploads" — an
		// error about the wrong thing entirely.
		if tail == "uploads" {
			return rest[:i], "uploads", ""
		}
		if u, ok := strings.CutPrefix(tail, "uploads/"); ok {
			return rest[:i], "uploads", u
		}
		return rest[:i], "blobs", tail
	}
	return "", "", ""
}

func (r *Server) ping(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		r.fail(w, req, errMethod(req.Method, "/v2/"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write([]byte("{}"))
	}
}

// --- blobs --------------------------------------------------------------------------------

func (r *Server) serveBlob(w http.ResponseWriter, req *http.Request, name, ref string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		// DELETE is the one a caller might reasonably expect. See the file header for why it is
		// not here — the answer is about the shared store, not about this endpoint.
		r.fail(w, req, errMethod(req.Method, "/v2/"+name+"/blobs/"+ref))
		return
	}
	sum, err := parseDigest(ref)
	if err != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: err.Error()})
		return
	}
	path, err := r.cas.Path(sum)
	if err != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: err.Error()})
		return
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		r.fail(w, req, errBlobUnknown(ref))
		return
	}
	if err != nil {
		r.fail(w, req, r.internal("open blob", err))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		r.fail(w, req, r.internal("stat blob", err))
		return
	}

	w.Header().Set("Content-Type", mediaTypeOctet)
	w.Header().Set("Docker-Content-Digest", ref)
	// ETag is the digest, which is the one case where an entity tag is EXACTLY right: the content
	// is immutable and named by its own hash, so a conditional request can be answered without
	// reading a byte.
	w.Header().Set("ETag", `"`+ref+`"`)
	if req.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	// THE COUNTERS ARE HANDED TO THE WRITER, NOT UPDATED AFTER IT. See countingWriter: an
	// increment on the line below `ServeContent` can be read as zero by a client that already
	// holds the whole body.
	cw := &countingWriter{ResponseWriter: w, blobs: &r.blobsServed, bytes: &r.bytesOut}
	// ServeContent for Range and the conditional handling, because a resumed pull over a bad link
	// is a real thing and re-implementing byte ranges is not this file's job.
	http.ServeContent(cw, req, "", st.ModTime(), f)
}

// --- manifests ----------------------------------------------------------------------------

// revision is what this repository knows about one manifest: its media type, so a GET can answer
// with the Content-Type the pusher declared, and its size. The BYTES are in the CAS under the
// same digest as any other blob — this is the naming, not a copy.
type revision struct {
	MediaType string `json:"mediaType"`
	Size      int64  `json:"size"`
}

func (r *Server) serveManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	switch req.Method {
	case http.MethodPut:
		r.putManifest(w, req, name, ref)
	case http.MethodGet, http.MethodHead:
		r.getManifest(w, req, name, ref)
	default:
		r.fail(w, req, errMethod(req.Method, "/v2/"+name+"/manifests/"+ref))
	}
}

func (r *Server) putManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxManifestBytes+1))
	if err != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID",
			Message: "could not read the manifest body: " + err.Error()})
		return
	}
	if len(body) > maxManifestBytes {
		r.fail(w, req, &regError{Status: http.StatusRequestEntityTooLarge, Code: "MANIFEST_INVALID",
			Message: fmt.Sprintf("manifest is larger than %d bytes; a manifest is a list of descriptors, not content", maxManifestBytes)})
		return
	}
	sum := casstore.Sha256Hex(body)
	digest := "sha256:" + sum

	// A digest reference is an ASSERTION about the bytes, so it is checked against them rather
	// than used to name them.
	if isDigestRef(ref) {
		want, derr := parseDigest(ref)
		if derr != nil {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: derr.Error()})
			return
		}
		if want != sum {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID",
				Message: fmt.Sprintf("manifest was PUT as %s but its bytes hash to %s", ref, digest)})
			return
		}
	} else if !tagRe.MatchString(ref) {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "TAG_INVALID",
			Message: fmt.Sprintf("tag %q is not a valid OCI tag", ref)})
		return
	}

	mediaType := strings.TrimSpace(req.Header.Get("Content-Type"))
	parsed, perr := parseManifest(body)
	if perr != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID", Message: perr.Error()})
		return
	}
	if mediaType == "" {
		mediaType = parsed.MediaType
	}
	if mediaType == "" {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID",
			Message: "the manifest has no Content-Type and no mediaType field, so a pull could not be told what it is"})
		return
	}

	// EVERY DESCRIPTOR IT NAMES MUST ALREADY BE HERE. Accepting a manifest whose config or layers
	// were never uploaded produces an image that pushes clean and fails at pull with a message
	// about a blob, three commands away from the deploy that caused it.
	for _, d := range parsed.refs {
		msum, derr := parseDigest(d)
		if derr != nil {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "MANIFEST_INVALID",
				Message: fmt.Sprintf("manifest references %q, which is not a digest", d)})
			return
		}
		has, herr := r.cas.Has(msum)
		if herr != nil {
			r.fail(w, req, r.internal("check referenced blob", herr))
			return
		}
		if !has {
			r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "MANIFEST_BLOB_UNKNOWN",
				Message: fmt.Sprintf("manifest references %s, which this registry does not hold", d),
				Detail:  map[string]any{"digest": d}})
			return
		}
	}

	if _, _, err := r.cas.Put(bytes.NewReader(body), "manifest "+name+":"+ref); err != nil {
		r.fail(w, req, r.internal("store manifest", err))
		return
	}
	if err := r.index.putRevision(name, sum, revision{MediaType: mediaType, Size: int64(len(body))}); err != nil {
		r.fail(w, req, r.internal("record manifest", err))
		return
	}
	if !isDigestRef(ref) {
		if err := r.index.putTag(name, ref, digest); err != nil {
			r.fail(w, req, r.internal("record tag", err))
			return
		}
	}

	w.Header().Set("Location", "/v2/"+name+"/manifests/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (r *Server) getManifest(w http.ResponseWriter, req *http.Request, name, ref string) {
	digest := ref
	if !isDigestRef(ref) {
		if !tagRe.MatchString(ref) {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "TAG_INVALID",
				Message: fmt.Sprintf("tag %q is not a valid OCI tag", ref)})
			return
		}
		d, ok, err := r.index.tag(name, ref)
		if err != nil {
			r.fail(w, req, r.internal("read tag", err))
			return
		}
		if !ok {
			r.fail(w, req, errManifestUnknown(name, ref))
			return
		}
		digest = d
	}
	sum, err := parseDigest(digest)
	if err != nil {
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: err.Error()})
		return
	}
	// SCOPED PER REPOSITORY, unlike a blob: a tag is a name, and answering `a`'s pull with `b`'s
	// manifest because the bytes happen to be in the store is how a run records the wrong actor.
	rev, ok, err := r.index.revision(name, sum)
	if err != nil {
		r.fail(w, req, r.internal("read manifest record", err))
		return
	}
	if !ok {
		r.fail(w, req, errManifestUnknown(name, ref))
		return
	}
	body, err := r.cas.ReadVerified(sum, "manifest")
	if err != nil {
		// The record says this repository has it and the store does not, which is a store problem
		// and not a client one — say so with a 5xx rather than a 404 that reads as "never pushed".
		r.fail(w, req, r.internal("read manifest "+digest, err))
		return
	}

	w.Header().Set("Content-Type", rev.MediaType)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// manifestRefs is the descriptor digests a manifest names, whichever shape it is: an image
// manifest's config and layers, or an index's child manifests. Anything else parses to nothing,
// which is the right answer for a media type this registry does not need to understand — it
// stores and serves bytes, and only the existence check needs to look inside.
type manifestRefs struct {
	MediaType string
	refs      []string
}

func parseManifest(body []byte) (manifestRefs, error) {
	var m struct {
		MediaType string `json:"mediaType"`
		Config    struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return manifestRefs{}, fmt.Errorf("manifest is not JSON: %w", err)
	}
	out := manifestRefs{MediaType: m.MediaType}
	if m.Config.Digest != "" {
		out.refs = append(out.refs, m.Config.Digest)
	}
	for _, l := range m.Layers {
		if l.Digest != "" {
			out.refs = append(out.refs, l.Digest)
		}
	}
	for _, c := range m.Manifests {
		if c.Digest != "" {
			out.refs = append(out.refs, c.Digest)
		}
	}
	return out, nil
}

// --- tags and catalog ---------------------------------------------------------------------

func (r *Server) serveTags(w http.ResponseWriter, req *http.Request, name string) {
	if req.Method != http.MethodGet {
		r.fail(w, req, errMethod(req.Method, "/v2/"+name+"/tags/list"))
		return
	}
	tags, err := r.index.tags(name)
	if err != nil {
		r.fail(w, req, r.internal("list tags", err))
		return
	}
	// A REPOSITORY THAT WAS NEVER PUSHED IS A 404, not an empty list. `cli/deploy.go`'s re-deploy
	// guard reads this endpoint, and "no such repository" and "this repository has no tags" are
	// the same answer to it — but they are not the same answer to an operator asking where their
	// image went.
	if len(tags) == 0 {
		known, kerr := r.index.hasRepository(name)
		if kerr != nil {
			r.fail(w, req, r.internal("list tags", kerr))
			return
		}
		if !known {
			r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "NAME_UNKNOWN",
				Message: fmt.Sprintf("repository %q is not in this registry", name)})
			return
		}
	}
	tags = page(tags, req.URL.Query())
	writeJSON(w, http.StatusOK, struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{name, tags})
}

func (r *Server) catalog(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		r.fail(w, req, errMethod(req.Method, "/v2/_catalog"))
		return
	}
	repos, err := r.index.repositories()
	if err != nil {
		r.fail(w, req, r.internal("list repositories", err))
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Repositories []string `json:"repositories"`
	}{page(repos, req.URL.Query())})
}

// page applies the spec's `n` and `last` to an already-sorted list. No Link header: this
// registry's lists are an actor catalog, not a public index, and a client that asks for a page
// gets one rather than a promise of more.
func page(all []string, q url.Values) []string {
	if last := q.Get("last"); last != "" {
		i := sort.SearchStrings(all, last)
		if i < len(all) && all[i] == last {
			i++
		}
		all = all[i:]
	}
	if n := q.Get("n"); n != "" {
		if limit, err := strconv.Atoi(n); err == nil && limit >= 0 && limit < len(all) {
			all = all[:limit]
		}
	}
	if all == nil {
		return []string{}
	}
	return all
}

// --- errors and small helpers ---------------------------------------------------------------

// regError is the OCI distribution error envelope. The CODE is what a client branches on and the
// MESSAGE is what a person reads, so both are always set.
type regError struct {
	Status  int
	Code    string
	Message string
	Detail  any
}

func (e *regError) Error() string { return e.Code + ": " + e.Message }

// errEnvelope is the wire shape of an OCI error: always a LIST, even for one, because that is
// what every client parses.
type errEnvelope struct {
	Errors []errItem `json:"errors"`
}

type errItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (r *Server) fail(w http.ResponseWriter, req *http.Request, e *regError) {
	if e.Status >= 500 {
		r.logf("%s %s: %s", req.Method, req.URL.Path, e.Message)
	}
	writeJSON(w, e.Status, errEnvelope{[]errItem{{e.Code, e.Message, e.Detail}}})
}

// internal turns a store failure into a 5xx that names what the registry was doing. The store's
// own errors already name the path and classify a full disk (cas/errors.go), so wrapping is all
// this has to do.
func (r *Server) internal(doing string, err error) *regError {
	return &regError{Status: http.StatusInternalServerError, Code: "UNKNOWN",
		Message: fmt.Sprintf("could not %s: %v", doing, err)}
}

// uploadError distinguishes a client that stopped sending from a store that could not write. The
// first is a 400 and the operator's own network; the second is a 500 and the appliance's disk.
func (r *Server) uploadError(err error) *regError {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return &regError{Status: http.StatusBadRequest, Code: "BLOB_UPLOAD_INVALID",
			Message: "the upload body ended early: " + err.Error()}
	}
	return r.internal("write the upload", err)
}

func errMethod(method, path string) *regError {
	return &regError{Status: http.StatusMethodNotAllowed, Code: "UNSUPPORTED",
		Message: method + " " + path + " is not a request this registry answers"}
}

func errBlobUnknown(digest string) *regError {
	return &regError{Status: http.StatusNotFound, Code: "BLOB_UNKNOWN",
		Message: "this registry does not hold " + digest, Detail: map[string]any{"digest": digest}}
}

func errManifestUnknown(name, ref string) *regError {
	return &regError{Status: http.StatusNotFound, Code: "MANIFEST_UNKNOWN",
		Message: fmt.Sprintf("%s:%s is not in this registry", name, ref),
		Detail:  map[string]any{"name": name, "reference": ref}}
}

// parseDigest accepts only `sha256:<64 lower-hex>` and returns the hex, which IS the CAS address.
// The algorithm is checked rather than accepted-and-ignored: sha512 content would store under a
// hex string the store cannot re-verify, and "unsupported" now is better than an integrity error
// on the pull.
func parseDigest(ref string) (string, error) {
	alg, sum, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("%q is not a digest (want sha256:<64 lower-hex>)", ref)
	}
	if alg != "sha256" {
		return "", fmt.Errorf("digest algorithm %q is not supported; this registry addresses content by sha256", alg)
	}
	if err := casstore.ValidateDigest(sum); err != nil {
		return "", err
	}
	return sum, nil
}

func isDigestRef(ref string) bool { return strings.HasPrefix(ref, "sha256:") }

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// countingWriter counts the body bytes ServeContent actually sent, so BytesOut reflects transfer
// rather than intent — a 304 or a HEAD writes none, and those are exactly the cases the "transfers
// nothing" claim is about.
//
// IT COUNTS BEFORE THE BYTES GO OUT, AND THAT ORDERING IS THE WHOLE REASON THE ACCOUNTING LIVES
// HERE RATHER THAN AFTER `ServeContent` RETURNS. `ResponseWriter.Write` may flush the last of the
// body to the client from inside the call, so an increment on the next line races a reader that
// already holds every byte. That is not a test artefact: every caller of these counters is a
// second goroutine by construction, because the only way to know a body was served is to have
// received it. OBSERVED on CI 2026-08-27 — `TestRePullTransfersNoLayers` read 0 blobs / 0 bytes on
// a pull whose 60,000-byte body had arrived complete, on the assertion after the one that checked
// the body's length and passed.
type countingWriter struct {
	http.ResponseWriter
	blobs *atomic.Uint64
	bytes *atomic.Uint64
	n     int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return c.ResponseWriter.Write(p)
	}
	// A BLOB IS COUNTED ON ITS FIRST BODY BYTE. `cw.n > 0` used to be the same test, made once at
	// the end; made here it cannot be beaten to the answer by the client.
	first := c.n == 0
	if first {
		c.blobs.Add(1)
	}
	c.bytes.Add(uint64(len(p)))
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	// THE ONE CASE COUNTING EARLY GETS WRONG, PUT BACK. A short write means the connection died
	// mid-body; the bytes that did not land are given back so `BytesOut` stays what it claims to
	// be — transfer, not intent. Late is safe here and ONLY here — nobody can observe bytes that
	// were never sent — which is also why a first write that landed NOTHING gives the blob back
	// too: no client saw a body, so there is no body to have served.
	if n < len(p) {
		c.bytes.Add(negate(uint64(len(p) - n)))
		if first && n == 0 {
			c.blobs.Add(negate(1))
		}
	}
	return n, err
}

// negate is `-v` for an atomic.Uint64, which has no Sub. The stdlib's own idiom (see the
// atomic package docs on Add), named so the bit-twiddling is not read as a bug at the call site.
func negate(v uint64) uint64 { return ^(v - 1) }

// unescapePath decodes the percent-encoding once, for the same reason splitPath does it in the
// object store: the path arrives escaped and every name below is matched against a grammar that
// has no percent in it.
func unescapePath(escaped string) (string, error) {
	out, err := url.PathUnescape(escaped)
	if err != nil {
		return "", fmt.Errorf("path %q is not valid percent-encoding", escaped)
	}
	return out, nil
}

// hostPort is net.JoinHostPort with an int, as every listening role in this tree spells it. One
// line, copied rather than shared: the package that would hold it for all of them is the parent,
// and the parent imports the roles, not the other way round.
func hostPort(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }
