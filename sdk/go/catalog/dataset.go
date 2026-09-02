package catalog

import (
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// Above this many units per page, the page is oversized; above the hard cap it is refused.
// Measured, not guessed — the same thresholds `kontra dispatch --batch-size` has guarded since a
// cascading unit took a whole node with it and the run still reported `completed`. A page IS a
// batch, so it inherits them; the safe working range is ~20-40 units per node.
const (
	SafePageMax = 200
	HardPageMax = 1000
)

// DatasetHandle is a named Dataset. Cheap to page — nothing is read until you page it.
//
// It carries ONE piece of write-side state: the author's `tag` and a `tagged` guard, so a handle
// used as a publish destination (`catalog.Into(catalog.Dataset("lame").WithTag(…))`) tags its Run's
// Dataset once rather than per chunk (ADR 0029 §4). A read-only handle leaves both untouched.
type DatasetHandle struct {
	Name    string
	Version string
	Dt      string

	sql     string
	orderBy string
	force   bool
	tag     string
	tagged  bool
}

// Dataset returns a handle on a named Dataset, by the bare name `kontra dataset list` shows.
func Dataset(name string) *DatasetHandle { return &DatasetHandle{Name: name} }

// WithTag declares AUTHORED POLICY on this Dataset (ADR 0029 §4) — "this kind of run always
// matters", e.g. a scheduled sweep that keeps its own output. Naming a publish destination with a
// tag makes the first publish into it write the Run's Dataset record and mirror to KontraTag. It is
// the author's peer of the operator's `kontra dataset tag`; both write the same set-valued record,
// which is the authority. Returns a COPY so a shared read handle is not mutated into a write policy.
func (d *DatasetHandle) WithTag(tag string) *DatasetHandle {
	cp := *d
	cp.tag = tag
	cp.tagged = false
	return &cp
}

// PageOption narrows or tunes a paging loop.
type PageOption func(*DatasetHandle)

// OrderBy is REQUIRED. A materialized Dataset stamps no row id, so LIMIT/OFFSET over it has no
// defined row order: two pages may overlap or skip units, and nothing would raise.
func OrderBy(cols string) PageOption { return func(d *DatasetHandle) { d.orderBy = cols } }

// Where filters the Dataset — shorthand for the common case of Query.
func Where(pred string) PageOption {
	return func(d *DatasetHandle) {
		d.sql = fmt.Sprintf(`SELECT * FROM %s WHERE %s`, quoteIdent(d.Name), pred)
	}
}

// Query is full SQL over the Dataset by its bare name. The rows it returns ARE the units.
func Query(sql string) PageOption { return func(d *DatasetHandle) { d.sql = sql } }

// Version prunes an OUTPUT dataset to one actor version. Meaningless on a standalone list.
func Version(v string) PageOption { return func(d *DatasetHandle) { d.Version = v } }

// Dt prunes an OUTPUT dataset to one dispatch time (a date or hour prefix).
func Dt(v string) PageOption { return func(d *DatasetHandle) { d.Dt = v } }

// ForceSize allows a page size above the safe maximum.
func ForceSize() PageOption { return func(d *DatasetHandle) { d.force = true } }

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Write-side activity names, served beside the pager on DatasetQueue.
const (
	PublishBatchActivity = "publishBatch"
	CloseDatasetActivity = "closeDataset"
	DatasetStateActivity = "datasetState"
	// OpenTempDatasetActivity records a temporary Dataset's owning Run at open (temp-datasets
	// slice 01). The one temp-specific write; publish and close go through the durable path.
	OpenTempDatasetActivity = "openTempDataset"
	// PromoteDatasetActivity promotes rows out of one Dataset into a durable one (temp-datasets
	// slice 02). Distinct from publishBatch so promoted rows keep the PRODUCING Run's provenance.
	PromoteDatasetActivity = "promoteDataset"
	// TagDatasetActivity writes the author's tag onto this Run's Dataset RECORD (ADR 0029 §4) — the
	// durable authority the retention sweeper reads. An in-workflow `Dataset(name).WithTag(…)` runs
	// this FIRST, then mirrors the tag to the KontraTag search attribute (see applyTag).
	TagDatasetActivity = "tagDataset"
)

// KontraTagAttribute is the Temporal search attribute an in-workflow tag MIRRORS to (ADR 0029 §4),
// registered by the orchestrator (backend/src/visibility.ts). It is a PROJECTION over the live
// window, never read as truth — the Dataset record is the authority.
const KontraTagAttribute = "KontraTag"

// PromotionSource is a Dataset a promotion reads FROM — a temporary one (the *DatasetWriter that
// TempDataset returns), normally, but any named *DatasetHandle works too. The method is unexported
// so the set is sealed to those two: a *Batch has no name and cannot be one, which keeps a chain
// from being mistaken for a promotion source.
type PromotionSource interface {
	promotionName() string
}

func (w *DatasetWriter) promotionName() string { return w.Name }
func (d *DatasetHandle) promotionName() string { return d.Name }

// DatasetWriter is an open Dataset you append Batches to — the write half of ADR 0023 §1.
//
//	w := catalog.Dataset("crawled").Writer()
//	defer w.Close(ctx, nil)
//
// `defer` IS the lifecycle, and Python's `async with` is its exact peer. While the Dataset is
// open it reads `open`; Close marks it `sealed`, or `abandoned` when handed an error. A
// crash reaches neither, so the Dataset stays `open` — which is the point: a reader can tell
// "the producer died" from "there was nothing to find", and a short-but-finished Dataset cannot
// express that.
type DatasetWriter struct {
	Name    string
	Version string
	// Rows is what this writer has appended so far.
	Rows int

	// tag is the author's tag (ADR 0029 §4), applied once on the first Write; tagged guards it so a
	// streaming, per-chunk publish pays for the record write and the KontraTag mirror only once.
	tag    string
	tagged bool
}

// Writer opens this Dataset for writing. The handle's tag (if any) rides onto the writer, so the
// writer's first Write applies it (ADR 0029 §4).
func (d *DatasetHandle) Writer() *DatasetWriter {
	return &DatasetWriter{Name: d.Name, Version: d.Version, tag: d.tag}
}

// TempDataset opens a temporary Dataset owned by the current Run — the Go peer of Python's
// `catalog.dataset.temp()` (temp-datasets slice 01, on ADR 0028).
//
//	w, err := catalog.TempDataset(ctx)
//	if err != nil { return err }
//	defer w.Close(ctx, nil)
//	if _, err := s.Call(ctx, "ask", chunk, catalog.Into(w)); err != nil { return err }
//
// Three things are true of it, and each is the point:
//
//   - IT MATERIALIZES AS THE RUN PROGRESSES. The returned *DatasetWriter is an ordinary writer
//     against a differently-named Dataset, so the same per-chunk publish a durable Dataset uses
//     puts rows in the lake mid-Run — a temp is not a new write path.
//   - IT IS OWNED BY ITS RUN. The owning workflow's id is recorded on the Dataset at open, before
//     any Batch lands, which is what makes "whose is this, can it go" answerable downstream.
//   - ITS NAME IS THE FRAMEWORK'S, NOT THE CALLER'S. Derived from the owning Run here, so the
//     Actor never learns one: ADR 0028's invariant is that a Method's dataset parameter reads the
//     same whether the destination is temporary or durable, and publishing is caller-side.
//
// The `open`/`sealed`/`abandoned` lifecycle is a durable Dataset's, unchanged: Close seals (or
// abandons on a non-nil error), and a crash reaches neither so a half-filled temp stays `open`.
// tempSlugMax bounds the Run id inside a temp's storage name. A real id is `<type>-<unixseconds>`;
// this bounds the pathological one an explicit `--id` can mint, so no temp can produce an object
// key a store refuses. Uniqueness never rode on the Run part — the suffix carries it.
const tempSlugMax = 64

// tempSlug renders a Run id as ONE path segment and ONE SQL identifier. Doing it here is not
// redundancy: without it the name a caller reads in `kontra dataset ls` and the name the lake
// stores would be two different strings for one Dataset.
//
// A THREE-WRITER DERIVATION, PINNED BY shared/conformance/slug.json. This, Python's `_slug`, and the
// orchestrator's `backend/src/data/parquet.ts:safeName`. This comment used to say all three apply
// "the same rule"; they do not — safeName never truncates, its empty fallback is `unnamed`, and it
// prefixes `a` to a leading digit. The corpus records those and the reason none of them is live
// (the orchestrator only ever sees `tmp_<slug>_<hex8>`), which is a thing that had to be measured
// rather than a thing anybody could have read off this file.
func tempSlug(runID string) string {
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= tempSlugMax {
			break
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()[:min(b.Len(), tempSlugMax)]
}

func TempDataset(ctx workflow.Context) (*DatasetWriter, error) {
	info := workflow.GetInfo(ctx)
	// THE NAME SAYS WHOSE IT IS — the peer of Python's `tmp_<run>_<suffix>` (lib/catalog.py). It
	// used to be `tmp_` + the EXECUTION's RunID, which named nothing an operator could follow and
	// was not even the Run this temp is attributed to: the owner recorded below is the WORKFLOW id
	// (ADR 0023 §12), which survives continue-as-new, while a RunID does not. Two spellings of
	// "which run" on two adjacent lines is exactly the drift a run-derived name exists to end.
	//
	// The SUFFIX stays what it was — replay-stable through SideEffect, never uuid.New(), the same
	// recipe a Session id uses (workflows.go Open) — because a Run may open several temps and the
	// Run part cannot tell them apart. Python spells its suffix with workflow.uuid4(); each host
	// uses its own SDK's replay-stable source, as they already do for Session ids.
	//
	// tempSlug keeps a caller's `--id` out of a path: this name becomes a DuckLake table and an
	// object-store key segment.
	var name string
	if err := workflow.SideEffect(ctx, func(workflow.Context) any {
		return "tmp_" + tempSlug(info.WorkflowExecution.ID) + "_" +
			info.WorkflowExecution.RunID[:8] +
			strconv.Itoa(int(workflow.Now(ctx).UnixNano()%1e6))
	}).Get(&name); err != nil {
		return nil, err
	}
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 30 * time.Minute,
	})
	// The owning Run is the workflow id (ADR 0023 §12), which survives continue-as-new so a temp
	// is attributed to one logical Run.
	if err := workflow.ExecuteActivity(ao, OpenTempDatasetActivity, map[string]any{
		"dataset": name, "owner": info.WorkflowExecution.ID,
	}).Get(ctx, nil); err != nil {
		return nil, err
	}
	return &DatasetWriter{Name: name}, nil
}

// Write appends one Batch and returns the rows it added. The Batch's ref IS the manifest, so a
// Method's output needs no reshaping to become queryable rows and nothing passes through this
// workflow.
//
// THE BATCH'S PROVENANCE IS FORWARDED, NOT THIS WRITER'S — peer of Python's publish(). A row's
// Machine and Actor version are facts about the Method call that produced it, and this writer
// knows neither: it used to send `"0"` for an unset version and no Machine at all, which is how
// a four-Machine run landed every row carrying one node and one version between them. An
// explicitly-versioned writer still wins, because that partition was asked for on purpose; what
// is gone is substituting a placeholder for a vacancy.
func (w *DatasetWriter) Write(ctx workflow.Context, b *Batch) (int, error) {
	// The author's tag, if any, is applied ONCE (ADR 0029 §4): the durable Dataset record first,
	// then the KontraTag mirror. A record-write failure fails the publish — the authoritative half
	// must not be silently lost — so it comes before the rows.
	if err := applyTag(ctx, &w.tagged, w.tag); err != nil {
		return 0, err
	}
	n, err := publishBatch(ctx, w.Name, w.Version, b)
	w.Rows += n
	return n, err
}

// applyTag writes the author's tag onto the CURRENT Run's Dataset record, then MIRRORS it to the
// KontraTag search attribute — the in-workflow half of ADR 0029 §4, run at most once per destination.
//
// THE RECORD IS WRITTEN BEFORE TEMPORAL IS TOUCHED, and the order is the whole design (§4). The
// tagDataset activity lands the durable, authoritative record FIRST; only after it succeeds does the
// tag reach KontraTag. Reversed, a mirror that failed would leave the record — which is what the
// retention sweeper reads (§5) — saying untagged, and a Dataset the author asked to keep would be
// collected. So `tagged` flips only after the record write, and a record-write error propagates.
//
// THE MIRROR IS ONE-WAY AND IT FREEZES AT EXECUTION CLOSE. UpsertTypedSearchAttributes is a
// workflow-internal command; there is NO API to write a search attribute on a closed execution
// (`temporal workflow update-options` is versioning-only, `temporal batch` is
// cancel/terminate/signal/reset — ADR 0029 finding 6). A tag the operator applies after this Run
// closes never reaches KontraTag, so the two disagree permanently. That is a FROZEN INDEX, not
// drift: do NOT write a reconciliation job — there is no window to reconcile into. The mirror is
// best-effort (the same posture the handler's own upsert takes); the record is the truth.
func applyTag(ctx workflow.Context, tagged *bool, tag string) error {
	if tag == "" || *tagged {
		return nil
	}
	info := workflow.GetInfo(ctx)
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 5 * time.Minute,
	})
	// RECORD FIRST — the authoritative Dataset record (issue 02's store), keyed by this Run.
	if err := workflow.ExecuteActivity(ao, TagDatasetActivity, map[string]any{
		"runId": info.WorkflowExecution.ID, "tag": tag,
	}).Get(ctx, nil); err != nil {
		return err
	}
	*tagged = true
	// MIRROR SECOND — one-way, freezes at close, never read as truth. Best-effort.
	if err := workflow.UpsertTypedSearchAttributes(ctx,
		temporal.NewSearchAttributeKeyKeyword(KontraTagAttribute).ValueSet(tag)); err != nil {
		workflow.GetLogger(ctx).Warn(
			"KontraTag mirror failed; the Dataset record is written and authoritative",
			"tag", tag, "error", err)
	}
	return nil
}

// publishBatch appends one Batch's rows to a named Dataset — the one write to the lake, shared by
// DatasetWriter.Write and DatasetHandle.Publish so the open-writer and bare-handle destinations a
// Method call accepts (ADR 0028 §2) forward provenance identically. The Batch's Machine and version
// are the row's, never this writer's: an explicit version wins (that partition was asked for on
// purpose) but an absent one stays absent rather than becoming the "0" that read back as a version
// nobody deployed — which, with the Machine, is why a multi-Machine run names every Machine.
func publishBatch(ctx workflow.Context, name, version string, b *Batch) (int, error) {
	if b == nil || b.N == 0 {
		return 0, nil
	}
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 30 * time.Minute,
	})
	info := workflow.GetInfo(ctx)
	if version == "" {
		version = b.version
	}
	var out struct {
		Rows int `json:"rows"`
	}
	err := workflow.ExecuteActivity(ao, PublishBatchActivity, map[string]any{
		"dataset":      name,
		"sha256":       b.Ref.Sha256,
		"runId":        info.WorkflowExecution.ID,
		"runStartedAt": info.WorkflowStartTime.UnixMilli(),
		"version":      version,
		"machine":      b.Machine,
	}).Get(ctx, &out)
	if err != nil {
		return 0, err
	}
	return out.Rows, nil
}

// Publish appends one Batch's rows to this Dataset WITHOUT an open writer — the bare-handle
// destination a Method call accepts (ADR 0028 §2), so `catalog.Into(catalog.Dataset("lame"))` needs
// no Writer. It marks the Dataset `open` and never seals it: sealing is the caller's, because the
// Actor is one of possibly several producers. The provenance is the Batch's, exactly as Write.
func (d *DatasetHandle) Publish(ctx workflow.Context, b *Batch) (int, error) {
	// An author's tag (ADR 0029 §4) is applied once here too — record first, then the mirror.
	if err := applyTag(ctx, &d.tagged, d.tag); err != nil {
		return 0, err
	}
	return publishBatch(ctx, d.Name, d.Version, b)
}

// InsertFrom PROMOTES rows out of a temporary (or any) Dataset into this durable one — the Go peer
// of Python's insert_from (temp-datasets slice 02, on ADR 0028). Returns the rows promoted.
//
//	w, _ := catalog.TempDataset(ctx)
//	defer w.Close(ctx, nil)
//	// … stage into w via s.Call(ctx, "ask", chunk, catalog.Into(w)) …
//	n, err := catalog.Dataset("lame").InsertFrom(ctx, w, catalog.Where("NOT ok"))
//
// Producing rows and ACCEPTING them are two acts with a gap between them, and this is the accepting
// side — a separate call, never a flag on the producing one, because the gap is the feature.
//
// Where / Query decide which rows promote, reusing the exact verbs Batches takes so there is ONE
// vocabulary for filtering a Dataset: the rows the SELECT returns ARE the rows promoted. Passing
// both raises, the same as Python. A Query references the source by its framework-derived name,
// available once the temp is open (`w.Name`).
//
// PROVENANCE SURVIVES: the default and Where are `SELECT *`, so a promoted row keeps the Machine,
// Actor version and Run that PRODUCED it — promotion is `promoteDataset`, not `publishBatch`, so it
// never stamps this workflow over the producing one. NOT IDEMPOTENT by design: a filtered
// INSERT…SELECT has no content address, so a second identical promotion appends the rows again.
//
// The destination argument on a Method call (`catalog.Into(w)`, ADR 0028 §2) SURVIVES alongside
// this: that names where output STAGES, this names what gets ACCEPTED — two acts, not two spellings
// of one destination.
func (d *DatasetHandle) InsertFrom(
	ctx workflow.Context, source PromotionSource, opts ...PageOption,
) (int, error) {
	src := source.promotionName()
	if src == "" {
		return 0, fmt.Errorf(
			"cannot promote from an unopened temporary Dataset — open it before InsertFrom, so it " +
				"has a name derived from its Run")
	}
	// Where and Query both set .sql; setting it twice IS passing both, which Python raises on, so
	// this does too. Options that do not touch .sql (Version, Dt) leave it unchanged and pass.
	sh := &DatasetHandle{Name: src}
	for _, o := range opts {
		before := sh.sql
		o(sh)
		if before != "" && sh.sql != "" && sh.sql != before {
			return 0, fmt.Errorf("pass Where OR Query, not both — Where is shorthand for it")
		}
	}
	sql := sh.sql
	if sql == "" {
		sql = "SELECT * FROM " + quoteIdent(src)
	}
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 30 * time.Minute,
	})
	var out struct {
		Rows int `json:"rows"`
	}
	err := workflow.ExecuteActivity(ao, PromoteDatasetActivity, map[string]any{
		"target": d.Name, "source": src, "sql": sql,
	}).Get(ctx, &out)
	if err != nil {
		return 0, err
	}
	return out.Rows, nil
}

// OutputDest is a Method call's output destination — the peer of Python's second positional
// argument (ADR 0028 §2), accepting an open *DatasetWriter or a bare *DatasetHandle. The method is
// unexported so the set is sealed to those two, the same closed choice Python's isinstance check
// makes; a Batch cannot be one, which is what keeps a chain (the first argument) from being mistaken
// for a destination.
type OutputDest interface {
	publishInto(ctx workflow.Context, b *Batch) (int, error)
}

func (w *DatasetWriter) publishInto(ctx workflow.Context, b *Batch) (int, error) {
	return w.Write(ctx, b)
}

func (d *DatasetHandle) publishInto(ctx workflow.Context, b *Batch) (int, error) {
	return d.Publish(ctx, b)
}

// Close ends the Dataset: `sealed` normally, `abandoned` when given a non-nil error. Pass the
// error your workflow is failing with so the state says which of the two happened.
func (w *DatasetWriter) Close(ctx workflow.Context, failure error) {
	state := "sealed"
	if failure != nil {
		state = "abandoned"
	}
	// Disconnected, so the close still lands when the workflow is already failing.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	ao := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 5 * time.Minute,
	})
	_ = workflow.ExecuteActivity(ao, CloseDatasetActivity,
		map[string]any{"dataset": w.Name, "state": state}).Get(dctx, nil)
}

// State returns `open` / `sealed` / `abandoned`, or "" if nothing ever wrote this Dataset.
// Read it before trusting a Dataset you did not produce: `open` means its producer never
// finished, and paging it reads a partial answer as a whole one.
func (d *DatasetHandle) State(ctx workflow.Context) (string, error) {
	ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           DatasetQueue,
		StartToCloseTimeout: 5 * time.Minute,
	})
	var out struct {
		State string `json:"state"`
	}
	if err := workflow.ExecuteActivity(ao, DatasetStateActivity,
		map[string]any{"dataset": d.Name}).Get(ctx, &out); err != nil {
		return "", err
	}
	return out.State, nil
}

// Batches pages this Dataset into Batches — the caller's loop, one page at a time.
//
//	for batch, err := range catalog.Dataset("subs").Batches(ctx, 200, catalog.OrderBy("host")) {
//		if err != nil { return err }
//		if _, err := s.Call(ctx, "crawl", batch); err != nil { return err }
//	}
//
// Each yielded Batch is a REF: no row enters your workflow, so a 40k-unit Dataset costs its
// history a handful of ~110-byte refs rather than the payload (ADR 0007).
//
// The error rides in the SECOND slot rather than being returned, because a Go iterator cannot
// return one. That is the honest peer of Python's raising `async for` — and the shape where
// ignoring the error is visible at the call site rather than silent. Iteration stops after
// yielding an error.
//
// Termination is on the first SHORT page, which is a fact rather than a guess: the pager reads
// one row past the page to decide. An EMPTY FIRST page is an ERROR — a Dataset that is gone,
// misspelled or filtered to nothing is a mistake, not an empty sweep — while an empty later page
// is simply the end.
func (d *DatasetHandle) Batches(
	ctx workflow.Context, size int, opts ...PageOption,
) iter.Seq2[*Batch, error] {
	cfg := *d
	for _, o := range opts {
		o(&cfg)
	}

	return func(yield func(*Batch, error) bool) {
		switch {
		case size <= 0:
			yield(nil, fmt.Errorf("page size must be positive, got %d", size))
			return
		case size > HardPageMax && !cfg.force:
			yield(nil, fmt.Errorf(
				"page size %d exceeds the safe maximum of %d units; one cascading unit can take "+
					"a whole node with it and the run still reports `completed` — use ~%d, or "+
					"pass ForceSize() if you have a reason", size, HardPageMax, SafePageMax/5))
			return
		case cfg.orderBy == "":
			yield(nil, fmt.Errorf(
				"paging %q needs OrderBy: a materialized dataset stamps no row id, so "+
					"LIMIT/OFFSET over it has no defined row order and two pages may overlap "+
					"or skip units", cfg.Name))
			return
		}
		if size > SafePageMax {
			workflow.GetLogger(ctx).Warn("dataset page size is above the safe range (~20-40/node)",
				"dataset", cfg.Name, "size", size)
		}

		sql := cfg.sql
		if sql == "" {
			sql = "SELECT * FROM " + quoteIdent(cfg.Name)
		}
		scope := map[string]any{"name": cfg.Name}
		if cfg.Version != "" {
			scope["version"] = cfg.Version
		}
		if cfg.Dt != "" {
			scope["dt"] = cfg.Dt
		}

		ao := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			TaskQueue:           DatasetQueue,
			StartToCloseTimeout: 5 * time.Minute,
		})

		offset, first := 0, true
		for {
			var page struct {
				Ref  BareRef `json:"ref"`
				N    int     `json:"n"`
				Done bool    `json:"done"`
			}
			err := workflow.ExecuteActivity(ao, PageDatasetActivity, map[string]any{
				"name": cfg.Name, "sql": sql, "orderBy": cfg.orderBy,
				"limit": size, "offset": offset, "scope": scope,
			}).Get(ctx, &page)
			if err != nil {
				yield(nil, err)
				return
			}
			if first && page.N == 0 {
				yield(nil, fmt.Errorf(
					"dataset %q returned no rows on its first page — check the name, the "+
						"version/dt scope and the filter before treating this as an empty sweep",
					cfg.Name))
				return
			}
			first = false
			if page.N > 0 {
				if !yield(batchFromRef(page.Ref, "", ""), nil) {
					return
				}
			}
			if page.Done {
				return
			}
			offset += page.N
		}
	}
}
