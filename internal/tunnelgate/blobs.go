package tunnelgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"kontra.sh/kontra/internal/blob"
	"kontra.sh/kontra/internal/obs"
)

// blobReadTimeout bounds the authorization read of a blob stream.
const blobReadTimeout = 5 * time.Second

// blobIdle bounds a blob stream's wait for the bucket: opening the object, and each read of it, must bring bytes
// within it, or the stream is ended (its context canceled, the object closed) and its in-flight slot freed. The
// stream's context is the tunnel session's, and minio's transport bounds only the time to response headers: a bucket
// that stalls mid-object would otherwise hold the stream, and its slot of the session's cap, until the session ends.
var blobIdle = 30 * time.Second

// Blobs is the gate's BlobServer (spec/tunnel.md §8.3, spec/relay.md §10.4, D167, D181): a ref is resolved only
// through the blobs table, and served only to a session that serves the row's task (an attempt of it is on the
// session) or hires it (the task's buyer is the session's hiring principal's buyer, re-validated now: a revoked
// hiring API key hires nothing). It answers one JSON line, {"size": n} and exactly n bytes, or {"error":
// "not_found" | "forbidden" | "too_large"} (an object over blob.MaxObject, by its row or by the store), and the
// stream is closed after it.
type Blobs struct {
	DB     *pgxpool.Pool
	Store  blob.Store // nil: no bucket (blobs: unset), every ref not_found
	Logger *slog.Logger
	// Lookup replaces the database read (tests): the row of ref and whether an attempt of its task is on the
	// session; ok false for no row.
	Lookup func(ctx context.Context, ref blob.Ref, session uuid.UUID) (row BlobRow, ok bool, err error)
}

// BlobRow is what a blob stream's check reads: the blobs row and its task's buyer.
type BlobRow struct {
	blob.Row
	Buyer  uuid.UUID
	Serves bool // an attempt of the task is on the asking session
}

var _ BlobServer = (*Blobs)(nil)

// ServeBlob implements BlobServer.
func (b *Blobs) ServeBlob(ctx context.Context, sess *Session, ref string, w io.Writer) error {
	log := b.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With(obs.KeyTunnelSession, sess.ID, "key_id", sess.KeyID)
	r := blob.Ref(ref)
	if !r.Valid() || b.Store == nil {
		return blob.WriteRefusal(w, blob.ReasonNotFound)
	}
	rctx, cancel := context.WithTimeout(ctx, blobReadTimeout)
	row, ok, err := b.lookup(rctx, r, sess.ID)
	cancel()
	if err != nil {
		return fmt.Errorf("tunnelgate: blob stream %s: %w", r, err) // the stream closes unanswered: the node fails the fetch
	}
	if !ok {
		log.Info("blob stream refused: no such ref", "ref", r)
		return blob.WriteRefusal(w, blob.ReasonNotFound)
	}
	if !row.Serves {
		p, err := sess.Hiring(ctx)
		if err != nil || p.BuyerID != row.Buyer {
			log.Warn("blob stream refused: the ref's task is neither served on nor hired by this session", "ref", r,
				obs.KeyTaskID, row.Task)
			return blob.WriteRefusal(w, blob.ReasonForbidden)
		}
	}
	if row.Size > blob.MaxObject {
		return blob.WriteRefusal(w, blob.ReasonTooLarge)
	}
	// The stream's own context, ended when the bucket stalls for blobIdle (the watchdog) or the stream ends.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := false
	var closer io.Closer
	var mu sync.Mutex
	dog := time.AfterFunc(blobIdle, func() {
		mu.Lock()
		defer mu.Unlock()
		stalled = true
		cancel()
		if closer != nil {
			_ = closer.Close()
		}
	})
	defer dog.Stop()
	rc, size, err := b.Store.Open(sctx, row.Key())
	mu.Lock()
	if err == nil {
		closer = rc
		if stalled {
			_ = rc.Close()
		}
	}
	opened := !stalled
	mu.Unlock()
	if !opened {
		log.Warn("blob stream ended: the bucket did not answer", "ref", r, "idle", blobIdle)
		return fmt.Errorf("tunnelgate: blob stream %s: the bucket did not answer within %v", r, blobIdle)
	}
	if errors.Is(err, blob.ErrNotFound) {
		log.Error("blob stream: the ref's object is not in the bucket", "ref", r, obs.KeyTaskID, row.Task)
		return blob.WriteRefusal(w, blob.ReasonNotFound)
	}
	if err != nil {
		return fmt.Errorf("tunnelgate: blob stream %s: %w", r, err)
	}
	defer rc.Close()
	if size > blob.MaxObject {
		return blob.WriteRefusal(w, blob.ReasonTooLarge)
	}
	if err := blob.WriteSize(w, size); err != nil {
		return err
	}
	if !dog.Reset(blobIdle) {
		return fmt.Errorf("tunnelgate: blob stream %s: the bucket did not answer within %v", r, blobIdle)
	}
	if _, err := io.CopyN(w, &idleReader{r: rc, dog: dog}, size); err != nil {
		mu.Lock()
		s := stalled
		mu.Unlock()
		if s {
			log.Warn("blob stream ended: the bucket stalled mid-object", "ref", r, "idle", blobIdle)
		}
		return fmt.Errorf("tunnelgate: blob stream %s: %w", r, err)
	}
	log.Debug("blob stream served", "ref", r, "size", size, obs.KeyTaskID, row.Task)
	return nil
}

// idleReader resets the stream's watchdog on every read that brings bytes.
type idleReader struct {
	r   io.Reader
	dog *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.dog.Reset(blobIdle)
	}
	return n, err
}

func (b *Blobs) lookup(ctx context.Context, ref blob.Ref, session uuid.UUID) (BlobRow, bool, error) {
	if b.Lookup != nil {
		return b.Lookup(ctx, ref, session)
	}
	if b.DB == nil {
		return BlobRow{}, false, nil
	}
	var row BlobRow
	var refText string
	err := b.DB.QueryRow(ctx, `
		select b.ref, b.tenant_id, b.task_id, b.size, t.buyer_id,
		       exists (select 1 from kontra.attempts a where a.task_id = b.task_id and a.tunnel_session = $2)
		  from kontra.blobs b join kontra.tasks t on t.id = b.task_id
		 where b.ref = $1`, string(ref), session).Scan(&refText, &row.Tenant, &row.Task, &row.Size, &row.Buyer, &row.Serves)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlobRow{}, false, nil
	}
	if err != nil {
		return BlobRow{}, false, err
	}
	row.Ref = blob.Ref(refText)
	return row, true, nil
}
