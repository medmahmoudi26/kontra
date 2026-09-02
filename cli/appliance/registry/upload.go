// upload.go — the blob-upload session, which is a state machine and not a handler.
//
// A PUSH IS FIVE REQUESTS AND A DOCKER DAEMON DECIDES WHICH FIVE. POST opens a session and gets a
// `Location`; zero or more PATCHes append, each answering with the range accepted so far; PUT
// closes it with the digest the client claims; DELETE abandons it. The daemon may also skip
// straight from POST to PUT with the whole body (`?digest=` on the POST, the monolithic form), may
// re-PATCH a range it already sent, and may leave a session open forever. That set of legal
// sequences is the module — it is not "the part of the HTTP surface that happens to be about
// blobs", which is why it left server.go.
//
// TWO INVARIANTS HOLD IT TOGETHER, and both are here rather than spread across the routes:
//
//	the offset the client is told is the offset the file is at — appendBody is the only writer,
//	and it refuses an out-of-order PATCH by NAMING both numbers (outOfOrder), because a push
//	that silently accepts a gap produces a layer whose digest will not match and a `docker pull`
//	that fails on a different machine;
//
//	nothing enters the CAS until the whole body has hashed to the digest the client declared —
//	the session file is a temp file, and commit is the only thing that promotes it.
//
// IT IS PROCESS STATE, DELIBERATELY. The spec allows a registry to forget a session, and the
// alternative — a durable session index — buys resumability across a restart of the control plane
// for a push the client will simply retry. What it would cost is a second thing that can disagree
// with the directory, which is exactly what index.go is careful not to be.
package registry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/medmahmoudi26/kontra/handler/casstore"
)

// uploadSession is one in-flight blob upload: a file being appended to, and the mutex that keeps
// two PATCHes on one session from interleaving.
//
// IT IS PROCESS STATE, DELIBERATELY. The spec allows a registry to forget a session, and the
// alternative — a durable session index — buys resumability across a restart of the control plane
// for a push that the client will simply retry. What it would cost is a second thing that can
// disagree with the directory.
type uploadSession struct {
	id   string
	path string
	mu   sync.Mutex
	size int64
}

func (r *Server) serveUpload(w http.ResponseWriter, req *http.Request, name, ref string) {
	if ref == "" {
		if req.Method != http.MethodPost {
			r.fail(w, req, errMethod(req.Method, "/v2/"+name+"/blobs/uploads/"))
			return
		}
		r.startUpload(w, req, name)
		return
	}
	if !uploadIDRe.MatchString(ref) {
		r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "BLOB_UPLOAD_UNKNOWN",
			Message: fmt.Sprintf("upload %q is not an upload id issued by this registry", ref)})
		return
	}
	v, ok := r.uploads.Load(ref)
	if !ok {
		r.fail(w, req, &regError{Status: http.StatusNotFound, Code: "BLOB_UPLOAD_UNKNOWN",
			Message: fmt.Sprintf("upload %s is not in progress (a registry restart discards sessions; start the push again)", ref)})
		return
	}
	s := v.(*uploadSession)

	switch req.Method {
	case http.MethodPatch:
		r.appendUpload(w, req, name, s)
	case http.MethodPut:
		r.finishUpload(w, req, name, s)
	case http.MethodGet, http.MethodHead:
		s.mu.Lock()
		size := s.size
		s.mu.Unlock()
		uploadHeaders(w, name, s.id, size)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		r.cancelUpload(s)
		w.WriteHeader(http.StatusNoContent)
	default:
		r.fail(w, req, errMethod(req.Method, "/v2/"+name+"/blobs/uploads/"+ref))
	}
}

func (r *Server) startUpload(w http.ResponseWriter, req *http.Request, name string) {
	q := req.URL.Query()

	// CROSS-REPOSITORY MOUNT. The daemon asks for it when it believes the layer is already here
	// under another name, and with one shared CAS it always is if we hold the digest at all — so
	// this is where a re-tag of an existing image uploads nothing. `from` is not consulted, for
	// the reason the file header gives: blobs are not scoped.
	if mount := q.Get("mount"); mount != "" {
		if sum, err := parseDigest(mount); err == nil {
			if has, herr := r.cas.Has(sum); herr == nil && has {
				w.Header().Set("Location", "/v2/"+name+"/blobs/"+mount)
				w.Header().Set("Docker-Content-Digest", mount)
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		// A mount that cannot be satisfied is NOT an error: the spec says fall through to a normal
		// session, and the client then uploads the bytes it was hoping to skip.
	}

	s, err := r.newUpload()
	if err != nil {
		r.fail(w, req, r.internal("create upload session", err))
		return
	}

	// MONOLITHIC: POST ...?digest=<d> with the whole blob as the body. Some clients take this
	// path for small layers; skipping it would make them fail on the config blob, which is the
	// smallest thing in every image.
	if d := q.Get("digest"); d != "" {
		if err := r.appendBody(s, req.Body, -1); err != nil {
			r.cancelUpload(s)
			r.fail(w, req, r.uploadError(err))
			return
		}
		r.commit(w, req, name, s, d)
		return
	}

	uploadHeaders(w, name, s.id, 0)
	w.WriteHeader(http.StatusAccepted)
}

func (r *Server) appendUpload(w http.ResponseWriter, req *http.Request, name string, s *uploadSession) {
	// A Content-Range on a PATCH declares where the client thinks it is. Checking it is what turns
	// a lost chunk into a 416 the client can act on, instead of a blob that hashes to nothing at
	// the end of a long upload.
	start := int64(-1)
	if cr := req.Header.Get("Content-Range"); cr != "" {
		var err error
		if start, err = rangeStart(cr); err != nil {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "BLOB_UPLOAD_INVALID", Message: err.Error()})
			return
		}
	}
	if err := r.appendBody(s, req.Body, start); err != nil {
		var out *outOfOrder
		if errors.As(err, &out) {
			uploadHeaders(w, name, s.id, out.have)
			r.fail(w, req, &regError{Status: http.StatusRequestedRangeNotSatisfiable, Code: "BLOB_UPLOAD_INVALID",
				Message: fmt.Sprintf("chunk starts at %d but the upload holds %d bytes", out.want, out.have)})
			return
		}
		r.cancelUpload(s)
		r.fail(w, req, r.uploadError(err))
		return
	}
	s.mu.Lock()
	size := s.size
	s.mu.Unlock()
	uploadHeaders(w, name, s.id, size)
	w.WriteHeader(http.StatusAccepted)
}

func (r *Server) finishUpload(w http.ResponseWriter, req *http.Request, name string, s *uploadSession) {
	d := req.URL.Query().Get("digest")
	if d == "" {
		r.cancelUpload(s)
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID",
			Message: "a blob upload is closed with ?digest=sha256:… — there is no way to store content whose address was not stated"})
		return
	}
	// The final PUT may carry the last chunk in its body, and for a small blob it may carry all
	// of it.
	if req.Body != nil {
		if err := r.appendBody(s, req.Body, -1); err != nil {
			r.cancelUpload(s)
			r.fail(w, req, r.uploadError(err))
			return
		}
	}
	r.commit(w, req, name, s, d)
}

// commit moves the session's bytes into the CAS under the digest the client declared. The store
// hashes as it writes and refuses to publish anything that does not match, so a wrong blob never
// becomes a stored blob — DIGEST_INVALID here is that refusal, reported with both digests.
func (r *Server) commit(w http.ResponseWriter, req *http.Request, name string, s *uploadSession, declared string) {
	sum, err := parseDigest(declared)
	if err != nil {
		r.cancelUpload(s)
		r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID", Message: err.Error()})
		return
	}
	f, err := os.Open(s.path)
	if err != nil {
		r.cancelUpload(s)
		r.fail(w, req, r.internal("read upload session", err))
		return
	}
	size, perr := r.cas.PutExpecting(f, sum, "blob upload "+s.id)
	_ = f.Close()
	r.cancelUpload(s)
	if perr != nil {
		var mm *casstore.DigestMismatch
		if errors.As(perr, &mm) {
			r.fail(w, req, &regError{Status: http.StatusBadRequest, Code: "DIGEST_INVALID",
				Message: mm.Error(), Detail: map[string]any{"expected": "sha256:" + mm.Want, "actual": "sha256:" + mm.Got}})
			return
		}
		r.fail(w, req, r.internal("store blob", perr))
		return
	}
	r.blobsStored.Add(1)
	r.bytesIn.Add(uint64(size))

	w.Header().Set("Location", "/v2/"+name+"/blobs/"+declared)
	w.Header().Set("Docker-Content-Digest", declared)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (r *Server) newUpload() (*uploadSession, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b[:])
	path := filepath.Join(r.index.uploadsDir(), id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	s := &uploadSession{id: id, path: path}
	r.uploads.Store(id, s)
	return s, nil
}

// appendBody appends body to the session. want >= 0 asserts the client's declared start offset.
func (r *Server) appendBody(s *uploadSession, body io.Reader, want int64) error {
	if body == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if want >= 0 && want != s.size {
		return &outOfOrder{want: want, have: s.size}
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, body)
	s.size += n
	if err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r *Server) cancelUpload(s *uploadSession) {
	r.uploads.Delete(s.id)
	_ = os.Remove(s.path)
}

// outOfOrder is a chunk that does not start where the session ended.
type outOfOrder struct{ want, have int64 }

func (e *outOfOrder) Error() string {
	return fmt.Sprintf("chunk starts at %d but the upload holds %d bytes", e.want, e.have)
}

func uploadHeaders(w http.ResponseWriter, name, id string, size int64) {
	w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	// Inclusive, and `0-0` for an empty upload: that is what the clients parse, whatever the
	// arithmetic suggests.
	if size == 0 {
		w.Header().Set("Range", "0-0")
	} else {
		w.Header().Set("Range", "0-"+strconv.FormatInt(size-1, 10))
	}
	w.Header().Set("Content-Length", "0")
}

// rangeStart reads the start offset out of a `Content-Range: <start>-<end>` PATCH header. The
// upload form has no `bytes ` prefix and no `/total`, but tolerate both rather than reject a
// client that sends them.
func rangeStart(cr string) (int64, error) {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(cr), "bytes"))
	v, _, _ = strings.Cut(v, "/")
	v = strings.TrimSpace(v)
	start, _, ok := strings.Cut(v, "-")
	if !ok {
		return 0, fmt.Errorf("Content-Range %q is not <start>-<end>", cr)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(start), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("Content-Range %q has no start offset", cr)
	}
	return n, nil
}
