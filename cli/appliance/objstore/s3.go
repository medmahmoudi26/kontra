// s3.go — the S3 API the appliance serves over its own data directory, in process, in place of
// the `seaweed` container (ADR 0031 §1 and §1a).
//
// WHY AN S3 API AND NOT JUST A DIRECTORY. "Replace SeaweedFS with the filesystem" is the obvious
// move and it is wrong, because three consumers need an ADDRESS rather than a path, and none of
// them is the browser — the SPA stopped range-reading parquet (ADR 0031, finding 8):
//
//   - The three SDK object stores (handler/internal/objectstore, sdk/python's casstore and
//     unitstore, backend/src/codec/objectStore.ts) all speak SigV4 to an endpoint. Their key
//     layout is pinned across languages by shared/conformance/blobkey.json, so the store has to
//     be the thing those clients already talk to, byte for byte.
//   - Remote fleet workers reach the controller over the VPC. `KONTRA_S3_ENDPOINT=http://$CONTROLLER:8333`
//     is handed to every Machine by cloud-init; a filesystem path means nothing on another host.
//   - `kontra explore` presigns GETs to an operator's workstation, where the URL is the whole
//     credential. Dropping presigning would not degrade that surface, it would delete it.
//
// A fourth, quieter one: DuckDB's httpfs. The materializer writes DuckLake parquet to an s3://
// DATA_PATH and the API reads it back, both through httpfs — which is why Range GET, LIST with a
// delimiter (its glob), and multipart upload are in this file and are not optional extras. All
// three were driven against this store with DuckDB 1.5.5 before it was called done.
//
// SIGNATURES ARE PARSED AND NOT ENFORCED, ON PURPOSE. SeaweedFS runs here with no identities
// config, so every request it receives today is authorised regardless of what it carries — and
// changing that while changing the topology would be a security change smuggled inside a
// packaging change. ADR 0031 §1a records it as open. The exposure control today is the BINDING —
// 127.0.0.1 by default, ${KONTRA_BIND} when a fleet needs it — exactly as it was for the
// container's published port.
//
// THE ANONYMOUS-GET DEPENDENCY THIS BLOCK USED TO NAME HAS MOVED, and it is worth recording where
// rather than deleting the sentence. A fleet Machine's cloud-init used to fetch its Bundle from
// `http://<controller>:8333/kontra-bundles/<key>` with a bare `curl` and no credentials, which
// made unsigned GET a live requirement of the fleet. A Bundle is an OCI artifact now (ADR 0036),
// so that `curl` points at the registry and this store no longer has a caller who cannot sign.
// What has NOT changed is that turning signatures on is still its own decision — the browser's
// presigned parquet reads and DuckDB's httpfs are the surfaces that would notice.
//
// PATH-STYLE ADDRESSING ONLY. Every kontra client is configured that way already (`UsePathStyle`,
// `forcePathStyle`, `s3_url_style='path'`), and virtual-host style would need DNS for a service
// that is an IP and a port.
package objstore

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The defaults below are not this file's choices. They are the values the KONTRA_S3_* environment
// already falls back to in all three SDKs (objectstore.go's ConfigFromEnv, objectStore.ts's
// constructor, casstore.py's from_env), so an appliance that binds these serves the clients that
// were never configured at all.
const (
	// DefaultPort is the port SeaweedFS's gateway published. Kept, for the reason
	// temporalsrv.DefaultPort is kept for Temporal: cloud-init hands fleet Machines `…:8333`,
	// docker-compose's
	// KONTRA_SERVE_ENV names it, and an appliance on a different port is a store every existing
	// client fails to find.
	DefaultPort = 8333

	DefaultS3Bucket = "kontra"
	DefaultS3Region = "us-east-1"

	// The dev credential pair, matching KONTRA_S3_ACCESS_KEY / KONTRA_S3_SECRET_KEY's fallbacks.
	// They are what presigned URLs are signed with; they authenticate nothing (see the header).
	DefaultS3AccessKey = "kontra"
	DefaultS3SecretKey = "kontra"

	s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"
)

// Options configures the embedded object store. DataDir is the only required field.
type Options struct {
	// DataDir is the directory the store owns. Objects live under DataDir/objects/<bucket>/…
	// Required, for the reason temporalsrv.Options.DataDir is: a store with a defaulted location is
	// a store an operator cannot find, back up, or measure.
	DataDir string

	// BindIP is the address to listen on. Empty means 127.0.0.1 (ADR 0031 §3). A fleet needs the
	// VPC address here, which is the same decision `${KONTRA_BIND}` encoded in compose.
	BindIP string

	// Port is the S3 port; 0 means DefaultPort.
	Port int

	Region    string
	AccessKey string
	SecretKey string

	// PublicEndpoint is the base URL presigned links are signed FOR, when it differs from the
	// address this server binds — the split horizon KONTRA_S3_PUBLIC_ENDPOINT names. Empty means
	// the bound address.
	PublicEndpoint string

	// Logf receives failures the store could not serve. nil means stderr.
	//
	// It exists because of one specific bug: a full disk made SeaweedFS answer every write with a
	// bare 500 while the run's own counters reported nothing wrong, so nobody looked at the store
	// for days. A 5xx that nothing logs is a 5xx nobody knows about.
	Logf func(format string, args ...any)
}

// Server is a started object store. It is returned already listening.
type Server struct {
	store    *objectStore
	srv      *http.Server
	ln       net.Listener
	address  string
	public   string
	region   string
	access   string
	secret   string
	dataDir  string
	logf     func(string, ...any)
	requests atomic.Uint64
}

// Address is host:port — what goes in KONTRA_S3_ENDPOINT, with a scheme.
func (s *Server) Address() string { return s.address }

// Endpoint is the full base URL clients configure.
func (s *Server) Endpoint() string { return "http://" + s.address }

// DataDir is the directory holding the objects. Named so an operator can `du -sh` it and so a
// test can assert an object survived a restart.
func (s *Server) DataDir() string { return s.dataDir }

// PresignGet mints a presigned GET for one object, signed for the public endpoint.
func (s *Server) PresignGet(bucket, key string, ttl time.Duration) (string, error) {
	return Presign(PresignOptions{
		Endpoint: s.public, Bucket: bucket, Key: key, Region: s.region,
		AccessKey: s.access, SecretKey: s.secret, Expires: ttl,
	})
}

// Stop shuts the listener down, letting in-flight requests finish.
//
// Bounded, and graceful FIRST rather than only: a multipart part can be tens of megabytes over a
// VPC link from a fleet worker, and killing that connection turns a clean stop into a failed
// dataset write on the far end. Five seconds, then the connections go anyway — a control plane
// that will not stop is its own problem.
func (s *Server) Stop() error {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(c); err != nil {
		return s.srv.Close()
	}
	return nil
}

// Start boots the object store and returns once it is serving.
func Start(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("appliance: DataDir is required (the embedded object store owns its own data directory)")
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
	if opts.Region == "" {
		opts.Region = DefaultS3Region
	}
	if opts.AccessKey == "" {
		opts.AccessKey = DefaultS3AccessKey
	}
	if opts.SecretKey == "" {
		opts.SecretKey = DefaultS3SecretKey
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, "kontra-s3: "+format+"\n", args...) }
	}

	store, err := newObjectStore(filepath.Join(opts.DataDir, "objects"))
	if err != nil {
		return nil, err
	}
	// DefaultS3Bucket EXISTS BEFORE THE LISTENER DOES, and that is the `seaweed-bucket` one-shot
	// deleted. A fresh SeaweedFS had no buckets, and its answer to a write into one that does not
	// exist is `403 AccessDenied` — which reads as a credentials problem, is not one, and cost a
	// fresh droplet's first `kontra dataset create`. A bucket is a directory here, made on the way
	// up, so the state that produced that 403 cannot exist.
	//
	// A CONFIGURABLE LIST OF EXTRA BUCKETS STOOD HERE AND HAD ONE CALLER: `kontra up` naming
	// `kontra-bundles`. A Bundle is an OCI artifact now (ADR 0036), so that caller is gone, and an
	// option nothing passes is a second answer to "which buckets exist" waiting to disagree with
	// this one. Every other bucket is created on first write.
	if err := store.createBucket(DefaultS3Bucket); err != nil {
		return nil, fmt.Errorf("appliance: %w", err)
	}

	addr := hostPort(opts.BindIP, opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// THE PORT CHECK IS THE ERROR MESSAGE, same as the embedded Temporal's. An operator who
		// still has the compose stack up owns 8333 already, and the difference between "the
		// appliance is broken" and "you are running two object stores" is this sentence.
		return nil, fmt.Errorf("appliance: cannot listen on %s for the object store — "+
			"another process (the compose `seaweed` service?) has it: %w", addr, err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		addr = hostPort(opts.BindIP, tcp.Port)
	}

	s := &Server{
		store: store, ln: ln, address: addr, region: opts.Region,
		access: opts.AccessKey, secret: opts.SecretKey, dataDir: opts.DataDir, logf: logf,
	}
	s.public = opts.PublicEndpoint
	if s.public == "" {
		s.public = "http://" + addr
	}
	s.srv = &http.Server{
		Handler: s,
		// No ReadTimeout: a fleet worker on a slow VPC link uploading a large part must not be
		// cut off mid-body. IdleTimeout still reclaims dead keep-alives.
		IdleTimeout: 2 * time.Minute,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logf("listener stopped: %v", err)
		}
	}()
	return s, nil
}

// --- routing ------------------------------------------------------------------------------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strconv.FormatUint(s.requests.Add(1), 10)
	w.Header().Set("x-amz-request-id", id)
	w.Header().Set("Server", "kontra-appliance")

	// Parsed and dropped. See this file's header: the claim is available, the check is not made.
	if _, err := parseSigV4(r); err != nil {
		s.fail(w, r, id, &s3Error{Code: "AuthorizationHeaderMalformed", Status: 400, Message: err.Error()})
		return
	}

	bucket, key, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	switch {
	case bucket == "":
		if r.Method != http.MethodGet {
			s.fail(w, r, id, &s3Error{Code: "MethodNotAllowed", Status: 405,
				Message: r.Method + " / is not a request this store answers"})
			return
		}
		s.listBuckets(w, r, id)
	case key == "":
		s.serveBucket(w, r, id, bucket)
	default:
		s.serveObject(w, r, id, bucket, key)
	}
}

// splitPath takes `/bucket/some/key` apart. The path arrives percent-encoded; each part is
// decoded once, which is why `%2F` inside a key and a real separator are indistinguishable here —
// the same thing SeaweedFS does, and no kontra key layout produces one (keys are validated
// identifiers: [a-z0-9/=._-]).
func splitPath(escaped string) (bucket, key string, err error) {
	p := strings.TrimPrefix(escaped, "/")
	b, rest, _ := strings.Cut(p, "/")
	bucket, uerr := url.PathUnescape(b)
	if uerr != nil {
		return "", "", errInvalid("bucket name %q is not valid percent-encoding", b)
	}
	key, uerr = url.PathUnescape(rest)
	if uerr != nil {
		return "", "", errInvalid("object key %q is not valid percent-encoding", rest)
	}
	return bucket, key, nil
}

func (s *Server) serveBucket(w http.ResponseWriter, r *http.Request, id, bucket string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodHead:
		if bad := unknownSubresource(q); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, bucket, bad))
			return
		}
		if !s.store.hasBucket(bucket) {
			s.fail(w, r, id, errNoSuchBucket(bucket))
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if q.Has("location") {
			writeXML(w, http.StatusOK, locationConstraint{XMLNS: s3XMLNS, Location: s.region})
			return
		}
		if bad := unknownSubresource(q, listQueryKeys...); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, bucket, bad))
			return
		}
		if err := s.listObjects(w, r, bucket, q); err != nil {
			s.fail(w, r, id, err)
		}
	case http.MethodPut:
		// CreateBucket takes no sub-resource, so anything in the query string is a DIFFERENT
		// operation — `?cors`, `?policy`, `?lifecycle`. Creating the bucket and answering 200
		// would tell the caller its CORS rules (or its retention policy) were applied.
		if bad := unknownSubresource(q); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, bucket, bad))
			return
		}
		// Idempotent on purpose: all three SDK object stores call CreateBucket best-effort before
		// their first write (`ensureBucket`), so returning BucketAlreadyOwnedByYou would make
		// every one of them log an error on a healthy path.
		if err := s.store.createBucket(bucket); err != nil {
			s.fail(w, r, id, err)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	default:
		s.fail(w, r, id, s.unimplemented(r, bucket, ""))
	}
}

// listQueryKeys are the parameters ListObjects (v1 and v2) is allowed to carry. Everything else
// in a bucket GET names another operation.
var listQueryKeys = []string{
	"list-type", "prefix", "delimiter", "max-keys", "continuation-token",
	"start-after", "marker", "encoding-type", "fetch-owner",
}

// unknownSubresource returns the first query parameter that names an operation this store does
// not implement, so a refusal can say WHICH one.
//
// A whitelist, not a blacklist, and that direction is the whole value: S3 hangs three dozen
// operations off query parameters on the same method and path, and a store that ignores the ones
// it does not know answers `PUT /kontra?cors` by creating a bucket and reporting success. The
// caller then believes it configured something.
func unknownSubresource(q url.Values, allowed ...string) string {
	for k := range q {
		if strings.HasPrefix(k, "X-Amz-") || strings.HasPrefix(k, "x-amz-") || strings.HasPrefix(k, "response-") {
			continue // presigning material and response-header overrides, not operations
		}
		// `x-id=PutObject` is an aws-sdk-go-v2 convention on EVERY request — it names the
		// operation for tracing and addresses nothing. It arrives on the happy path, so it is
		// skipped rather than refused.
		if k == "x-id" {
			continue
		}
		if slices.Contains(allowed, k) {
			continue
		}
		return k
	}
	return ""
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request, id string) {
	bs, err := s.store.buckets()
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	out := listAllMyBucketsResult{XMLNS: s3XMLNS}
	out.Owner.ID = "kontra"
	out.Owner.DisplayName = "kontra"
	for _, b := range bs {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketXML{Name: b.Name, CreationDate: iso(b.Created)})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) listObjects(w http.ResponseWriter, _ *http.Request, bucket string, q url.Values) error {
	v2 := q.Get("list-type") == "2"
	p := listParams{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter")}
	if v2 {
		if t := q.Get("continuation-token"); t != "" {
			p.After = t
		} else {
			p.After = q.Get("start-after")
		}
	} else {
		p.After = q.Get("marker")
	}
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil {
		p.MaxKeys = n
	}
	if p.MaxKeys <= 0 || p.MaxKeys > maxMaxKeys {
		p.MaxKeys = defaultMaxKeys // clamped here too, so the echoed MaxKeys is the one applied
	}
	res, err := s.store.list(bucket, p)
	if err != nil {
		return err
	}

	out := listBucketResult{
		XMLNS: s3XMLNS, Name: bucket, Prefix: p.Prefix, Delimiter: p.Delimiter,
		MaxKeys: p.MaxKeys, IsTruncated: res.Truncated, KeyCount: len(res.Objects) + len(res.CommonPrefixes),
	}
	for _, o := range res.Objects {
		out.Contents = append(out.Contents, contentsXML{
			Key: o.Key, LastModified: iso(o.Modified), ETag: o.ETag, Size: o.Size, StorageClass: "STANDARD",
		})
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefixXML{Prefix: cp})
	}
	if v2 {
		out.ContinuationToken = q.Get("continuation-token")
		out.StartAfter = q.Get("start-after")
		out.NextContinuationToken = res.NextToken
	} else {
		out.Marker = p.After
		out.NextMarker = res.NextToken
	}
	writeXML(w, http.StatusOK, out)
	return nil
}

func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, id, bucket, key string) {
	q := r.URL.Query()
	resource := bucket + "/" + key
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if bad := unknownSubresource(q); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, resource, bad))
			return
		}
		s.getObject(w, r, id, bucket, key)
	case http.MethodPut:
		if bad := unknownSubresource(q, "uploadId", "partNumber"); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, resource, bad))
			return
		}
		switch {
		case q.Has("uploadId"):
			s.uploadPart(w, r, id, q)
		case r.Header.Get("x-amz-copy-source") != "":
			s.copyObject(w, r, id, bucket, key)
		default:
			s.putObject(w, r, id, bucket, key)
		}
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.createMultipart(w, r, id, bucket, key)
		case q.Has("uploadId"):
			s.completeMultipart(w, r, id, bucket, key, q.Get("uploadId"))
		default:
			s.fail(w, r, id, s.unimplemented(r, resource, firstKey(q)))
		}
	case http.MethodDelete:
		if bad := unknownSubresource(q, "uploadId"); bad != "" {
			s.fail(w, r, id, s.unimplemented(r, resource, bad))
			return
		}
		if uid := q.Get("uploadId"); uid != "" {
			if err := s.store.abortUpload(uid); err != nil {
				s.fail(w, r, id, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := s.store.delete(bucket, key); err != nil {
			s.fail(w, r, id, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.fail(w, r, id, s.unimplemented(r, resource, ""))
	}
}

func firstKey(q url.Values) string {
	for k := range q {
		return k
	}
	return ""
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, id, bucket, key string) {
	f, info, err := s.store.open(bucket, key)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	defer f.Close()
	w.Header().Set("ETag", info.ETag)
	w.Header().Set("Content-Type", contentType(key))
	// http.ServeContent owns Range, If-Range, If-None-Match, 206 and Content-Range. Reproducing
	// that by hand is where a store gets range reads subtly wrong, and DuckDB reads a parquet
	// footer by range before it reads anything else.
	http.ServeContent(w, r, "", info.Modified, f)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, id, bucket, key string) {
	// The bucket is created rather than demanded. Both halves of that choice matter: every SDK
	// object store already calls CreateBucket before its first write, so refusing here would only
	// ever fire for a client that did not — and the failure mode being replaced is precisely a
	// missing bucket answering 403, which is the same one `Start` creates DefaultS3Bucket for.
	if err := s.store.createBucket(bucket); err != nil {
		s.fail(w, r, id, err)
		return
	}
	body, err := requestBody(r)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	info, err := s.store.put(bucket, key, body)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	w.Header().Set("ETag", info.ETag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, id, bucket, key string) {
	src := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/")
	srcBucket, srcKey, _ := strings.Cut(src, "/")
	if unescaped, err := url.PathUnescape(srcKey); err == nil {
		srcKey = unescaped
	}
	if srcBucket == "" || srcKey == "" {
		s.fail(w, r, id, errInvalid("x-amz-copy-source %q is not <bucket>/<key>", r.Header.Get("x-amz-copy-source")))
		return
	}
	info, err := s.store.copyObject(srcBucket, srcKey, bucket, key)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	writeXML(w, http.StatusOK, copyObjectResult{XMLNS: s3XMLNS, ETag: info.ETag, LastModified: iso(info.Modified)})
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, id, bucket, key string) {
	uploadID, err := s.store.newUpload(bucket, key)
	if err != nil {
		s.fail(w, r, id, err)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{
		XMLNS: s3XMLNS, Bucket: bucket, Key: key, UploadID: uploadID,
	})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, id string, q url.Values) {
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		s.fail(w, r, id, errInvalid("partNumber %q is not a number", q.Get("partNumber")))
		return
	}
	body, berr := requestBody(r)
	if berr != nil {
		s.fail(w, r, id, berr)
		return
	}
	etag, perr := s.store.putPart(q.Get("uploadId"), n, body)
	if perr != nil {
		s.fail(w, r, id, perr)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, id, bucket, key, uploadID string) {
	var req completeMultipartUpload
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		s.fail(w, r, id, errInvalid("reading the CompleteMultipartUpload body: %v", err))
		return
	}
	if err := xml.Unmarshal(body, &req); err != nil {
		s.fail(w, r, id, errInvalid("parsing the CompleteMultipartUpload body: %v", err))
		return
	}
	parts := make([]completedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, completedPart{PartNumber: p.PartNumber, ETag: p.ETag})
	}
	info, cerr := s.store.completeUpload(uploadID, parts)
	if cerr != nil {
		s.fail(w, r, id, cerr)
		return
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		XMLNS: s3XMLNS, Bucket: bucket, Key: key, ETag: info.ETag,
		Location: s.Endpoint() + "/" + bucket + "/" + key,
	})
}

// unimplemented names the operation rather than 404-ing it. A store that answers an operation it
// does not implement with "not found" sends whoever hits it looking for a missing object.
func (s *Server) unimplemented(r *http.Request, resource, sub string) *s3Error {
	if sub != "" {
		sub = "?" + sub
	}
	return &s3Error{Code: "NotImplemented", Status: 501,
		Message: fmt.Sprintf("%s /%s%s: the appliance's object store implements what kontra uses — "+
			"GET, PUT, DELETE, LIST, multipart upload and copy. This operation is not among them.",
			r.Method, resource, sub)}
}

// --- bodies -------------------------------------------------------------------------------

// requestBody returns the object bytes from a PUT, unwrapping aws-chunked framing when the client
// used it.
//
// THIS IS NOT DEFENSIVE PROGRAMMING, it is a silent-corruption guard. When an AWS SDK streams a
// body with a trailing checksum it wraps the payload in `aws-chunked` framing — hex length lines,
// per-chunk signatures, a trailer — and declares it with `x-amz-content-sha256: STREAMING-…`. A
// store that ignores that header stores the FRAMING as the object, returns 200, and the corruption
// surfaces far away as a sha256 mismatch in the CAS. Whether a given SDK uses it depends on the
// body being seekable and the transport being TLS, which means a version bump or an https endpoint
// can turn it on with no code change here.
func requestBody(r *http.Request) (io.Reader, error) {
	enc := r.Header.Get("Content-Encoding")
	if !strings.HasPrefix(r.Header.Get("x-amz-content-sha256"), "STREAMING-") &&
		!strings.Contains(enc, "aws-chunked") {
		return r.Body, nil
	}
	declared := int64(-1)
	if v := r.Header.Get("x-amz-decoded-content-length"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, errInvalid("x-amz-decoded-content-length %q is not a length", v)
		}
		declared = n
	}
	return &awsChunkedReader{br: bufio.NewReader(r.Body), remaining: declared}, nil
}

// awsChunkedReader decodes `<hex-size>[;chunk-signature=…]\r\n<size bytes>\r\n`, repeated, ending
// with a zero-length chunk and optional trailer headers.
type awsChunkedReader struct {
	br        *bufio.Reader
	chunk     int64 // bytes left in the current chunk
	remaining int64 // declared decoded length, or -1
	done      bool
	err       error
}

func (c *awsChunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	for c.chunk == 0 {
		if c.done {
			if c.remaining > 0 {
				c.err = fmt.Errorf("aws-chunked body ended %d bytes short of its declared x-amz-decoded-content-length", c.remaining)
				return 0, c.err
			}
			return 0, io.EOF
		}
		line, err := c.br.ReadString('\n')
		if err != nil {
			c.err = fmt.Errorf("reading an aws-chunked size line: %w", err)
			return 0, c.err
		}
		size, _, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ";")
		if size == "" {
			continue // the CRLF that closes the previous chunk
		}
		n, perr := strconv.ParseInt(size, 16, 64)
		if perr != nil {
			c.err = fmt.Errorf("aws-chunked size %q is not hexadecimal: %w", size, perr)
			return 0, c.err
		}
		if n == 0 {
			c.done = true
			continue
		}
		c.chunk = n
	}
	if int64(len(p)) > c.chunk {
		p = p[:c.chunk]
	}
	n, err := c.br.Read(p)
	c.chunk -= int64(n)
	if c.remaining > 0 {
		c.remaining -= int64(n)
	}
	if err != nil && err != io.EOF {
		c.err = err
	}
	return n, c.err
}

// --- responses ----------------------------------------------------------------------------

func (s *Server) fail(w http.ResponseWriter, r *http.Request, id string, err error) {
	var e *s3Error
	if !errors.As(err, &e) {
		e = &s3Error{Code: "InternalError", Status: 500, Message: err.Error()}
	}
	// The store says so itself, not only to the client. The bug being designed out is a write
	// failure that was invisible everywhere except in a caller that swallowed it.
	//
	// NotImplemented is excluded, and one caller is the reason: the TypeScript object store calls
	// PutBucketCors best-effort on every construction and swallows the result, so logging that
	// refusal would print a 501 on every orchestrator boot for something working as designed. The
	// caller still receives the full message; what is being kept out of the log is noise that
	// would teach an operator to ignore it.
	if (e.Status >= 500 || e.Status == http.StatusInsufficientStorage) && e.Code != "NotImplemented" {
		s.logf("%s %s → %d %s: %s", r.Method, r.URL.Path, e.Status, e.Code, e.Message)
	}
	writeXML(w, e.Status, errorBody(e, r.URL.Path))
}

func errorBody(e *s3Error, resource string) errorResult {
	return errorResult{XMLNS: s3XMLNS, Code: e.Code, Message: e.Message, Resource: resource}
}

func writeXML(w http.ResponseWriter, status int, body any) {
	out, err := xml.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(out)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_, _ = w.Write(out)
}

func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// contentType is derived from the key's extension, never stored. Nothing in kontra reads an
// object's content type back — the SDK stores hand raw bytes to the codec, and DuckDB reads
// parquet by range — so a per-object sidecar would be state kept for no reader.
func contentType(key string) string {
	if ct := mime.TypeByExtension(filepath.Ext(key)); ct != "" {
		return ct
	}
	return "binary/octet-stream"
}

// --- the XML shapes -------------------------------------------------------------------------

type errorResult struct {
	XMLName  xml.Name `xml:"Error"`
	XMLNS    string   `xml:"xmlns,attr"`
	Code     string   `xml:"Code"`
	Message  string   `xml:"Message"`
	Resource string   `xml:"Resource,omitempty"`
}

type locationConstraint struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:",chardata"`
}

type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	Owner   struct {
		ID          string `xml:"ID"`
		DisplayName string `xml:"DisplayName"`
	} `xml:"Owner"`
	Buckets struct {
		Bucket []bucketXML `xml:"Bucket"`
	} `xml:"Buckets"`
}

type contentsXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefixXML struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResult struct {
	XMLName               xml.Name          `xml:"ListBucketResult"`
	XMLNS                 string            `xml:"xmlns,attr"`
	Name                  string            `xml:"Name"`
	Prefix                string            `xml:"Prefix"`
	Delimiter             string            `xml:"Delimiter,omitempty"`
	MaxKeys               int               `xml:"MaxKeys"`
	KeyCount              int               `xml:"KeyCount"`
	IsTruncated           bool              `xml:"IsTruncated"`
	Marker                string            `xml:"Marker,omitempty"`
	NextMarker            string            `xml:"NextMarker,omitempty"`
	ContinuationToken     string            `xml:"ContinuationToken,omitempty"`
	StartAfter            string            `xml:"StartAfter,omitempty"`
	NextContinuationToken string            `xml:"NextContinuationToken,omitempty"`
	Contents              []contentsXML     `xml:"Contents"`
	CommonPrefixes        []commonPrefixXML `xml:"CommonPrefixes"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUpload struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	XMLNS        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// hostPort is net.JoinHostPort with an int, as every listening role in this tree spells it. One
// line, copied rather than shared: the package that would hold it for all of them is the parent,
// and the parent imports the roles, not the other way round.
func hostPort(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }
