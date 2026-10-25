package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/medmahmoudi26/kontra/internal/ids"
	"github.com/medmahmoudi26/kontra/internal/lifecycle"
	"github.com/medmahmoudi26/kontra/internal/ssrf"
	"github.com/medmahmoudi26/kontra/internal/store"
	"github.com/medmahmoudi26/kontra/internal/version"
)

// Delivery rules (spec §10.2, D16, D72).
const (
	// DeliveryTimeout bounds one POST, its response's status line and the little of its body that is read.
	DeliveryTimeout = 10 * time.Second
	// RetryMin and RetryMax bound the backoff between failed deliveries of one seq: 1 s doubling to 5 min.
	RetryMin = time.Second
	RetryMax = 5 * time.Minute
	// RetryBudget is how long one seq is retried from its first failure; then the config is marked failed.
	RetryBudget = 24 * time.Hour
	// RatePerHost is the gate-wide cap of deliveries a second to one destination host; each replica allows
	// RatePerHost/N, N the live replicas in gate_replicas (draining ones included, at least 1, D72).
	RatePerHost = 10
	// ReplicaRefresh is how often N is read again.
	ReplicaRefresh = 10 * time.Second
	// ReplicaSilence is how long a replica may be silent and still count as live (spec/tunnel.md §11).
	ReplicaSilence = 30 * time.Second
	// ContentType is the body's media type (A2A §4.3.3).
	ContentType = "application/a2a+json"
)

// Header names of a delivery: the config's token goes in both (a2a-go sends the first, the ACTS runner
// expects the second, spec §10.2).
const (
	HeaderToken       = "A2A-Notification-Token"
	HeaderTokenLegacy = "X-A2A-Notification-Token"
)

const (
	// beatEvery is the longest a running push_webhook goes without recording a heartbeat (its heartbeat
	// timeout is 60 s, spec §14.2; the SDK throttles what it sends).
	beatEvery = 10 * time.Second
	// readBatch and readBytes bound one read of the log; runs of artifact rows inside one read coalesce.
	readBatch = 256
	readBytes = 4 << 20
	// drainBytes is how much of a response body is read (and discarded) so the connection can be reused.
	drainBytes = 4 << 10
)

// Stopped values of lifecycle.PushResult.
const (
	StoppedTerminal = lifecycle.PushStoppedTerminal
	StoppedDeleted  = lifecycle.PushStoppedDeleted
	StoppedFailed   = lifecycle.PushStoppedFailed
)

// ErrTypeBadInput is the non-retryable push_webhook error of an input that names no gate task or config.
const ErrTypeBadInput = "PushBadInput"

// errStopping ends an activity whose worker is stopping; Temporal retries it on another poller, which
// resumes from delivered_seq and the heartbeat details.
var errStopping = errors.New("push: the kontra-push worker is stopping")

// DelivererOptions configure a Deliverer. The zero value is the spec's timing with no waker.
type DelivererOptions struct {
	// Waker wakes an idle activity when the task's log grows (pg.Listener). Without one it re-reads every
	// IdlePoll.
	Waker store.TaskWaker
	// Logger receives delivery failures and give-ups; it never receives a URL, a token or credentials.
	Logger *slog.Logger

	// Tests only: the HTTP client (default ssrf.NewClient(policy)), the timing, the clock and the sleep,
	// and the live replica count (default: gate_replicas).
	Client   *http.Client
	Timeout  time.Duration
	RetryMin time.Duration
	RetryMax time.Duration
	Budget   time.Duration
	// IdlePoll is how often an idle activity re-reads the log and checks that its config still exists:
	// 10 s with a waker, 1 s without.
	IdlePoll time.Duration
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	Replicas func(ctx context.Context) (int, error)
	// ReadBytes bounds the stored bytes of one read of the log (default 4 MiB).
	ReadBytes int
}

// Deliverer implements lifecycle.PushActivities: push_webhook on kontra-push (spec §10.2, §14.2).
type Deliverer struct {
	log    *store.Log
	policy ssrf.Policy
	opts   DelivererOptions
	client *http.Client
	logger *slog.Logger
	limit  *hostLimiter

	mu          sync.Mutex
	replicas    int
	replicasAt  time.Time
	replicasErr bool

	onBeat func() // tests: called at every heartbeat point
}

var _ lifecycle.PushActivities = (*Deliverer)(nil)

// NewDeliverer returns the push_webhook implementation; policy is push.allow_private.
func NewDeliverer(log *store.Log, policy ssrf.Policy, opts DelivererOptions) *Deliverer {
	d := &Deliverer{log: log, policy: policy, opts: opts, client: opts.Client, logger: opts.Logger, limit: newHostLimiter()}
	if d.client == nil {
		// The SSRF dialer: every address checked at dial time, no redirects (a 3xx is a failed delivery and
		// its target gets no connection), no proxy from the environment (spec §10.3, D17).
		d.client = ssrf.NewClient(policy)
	}
	if d.logger == nil {
		d.logger = slog.Default()
	}
	if d.opts.Timeout <= 0 {
		d.opts.Timeout = DeliveryTimeout
	}
	if d.opts.RetryMin <= 0 {
		d.opts.RetryMin = RetryMin
	}
	if d.opts.RetryMax <= 0 {
		d.opts.RetryMax = RetryMax
	}
	if d.opts.Budget <= 0 {
		d.opts.Budget = RetryBudget
	}
	if d.opts.IdlePoll <= 0 {
		d.opts.IdlePoll = time.Second
		if d.opts.Waker != nil {
			d.opts.IdlePoll = beatEvery
		}
	}
	if d.opts.Now == nil {
		d.opts.Now = time.Now
	}
	if d.opts.Sleep == nil {
		d.opts.Sleep = sleep
	}
	if d.opts.Replicas == nil {
		d.opts.Replicas = d.liveReplicas
	}
	if d.opts.ReadBytes <= 0 {
		d.opts.ReadBytes = readBytes
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// progress is push_webhook's heartbeat details: the last delivered seq and, while a seq is failing, when it
// first failed and the backoff to wait next, so a restarted activity keeps the seq's 24 h budget. Numbers
// and times only: heartbeat details are Temporal history.
type progress struct {
	Seq          int64         `json:"seq"`
	FailingSeq   int64         `json:"failingSeq,omitempty"`
	FirstFailure time.Time     `json:"firstFailure,omitzero"`
	Wait         time.Duration `json:"wait,omitempty"`
}

// failing reports whether a delivery after the last delivered seq has failed.
func (p progress) failing() bool { return p.FailingSeq > p.Seq && !p.FirstFailure.IsZero() }

// run is one push_webhook execution.
type run struct {
	d        *Deliverer
	taskID   uuid.UUID
	configID string // the config's id in push_configs; history names it by its ConfigRef only
	prog     progress
	stop     <-chan struct{} // the worker's stop channel; nil outside an activity
}

func badInput(msg string) error {
	return temporal.NewNonRetryableApplicationError(msg, ErrTypeBadInput, nil)
}

// PushWebhook delivers, for each event after the larger of push_configs.delivered_seq and the heartbeat
// details, the task snapshot at that seq as {"task": …}, in seq order (spec §10.2, D16): one POST, 10 s
// timeout, Content-Type application/a2a+json, the config's authentication as Authorization and its token
// as both A2A-Notification-Token and X-A2A-Notification-Token. Any 2xx is delivered; a 3xx is a failed
// delivery whose target receives no connection; anything else is retried 1 s doubling to 5 min for at most
// 24 h from the first failure of that seq, then the config is marked failed and the activity ends (the task
// is unaffected). Runs of consecutive artifact rows read together are coalesced into the snapshot at the
// last seq of the run; every other row, so every status change and the terminal event, is delivered.
// Deliveries to one destination host are capped at 10 a second across the gate (10/N per replica, D72).
// delivered_seq is written after every delivery and only moves forward. The activity ends after the
// terminal snapshot or when its config is deleted, also while it waits out a backoff. Its errors and logs
// name ids, seqs and status codes, never the config's URL, token or credentials; its errors (which cross
// Temporal history) never name the config's id either, which may be the caller's: in.ConfigID is the
// config's lifecycle.ConfigRef, resolved here among the task's configs.
func (d *Deliverer) PushWebhook(ctx context.Context, in lifecycle.PushInput) (lifecycle.PushResult, error) {
	taskID, err := ids.ParseTaskID(in.TaskID)
	if err != nil {
		return lifecycle.PushResult{}, badInput("push_webhook: not a gate task id")
	}
	if in.ConfigID == "" {
		return lifecycle.PushResult{}, badInput("push_webhook: no config id")
	}
	configID, err := d.configByRef(ctx, taskID, in.ConfigID)
	if err != nil {
		return lifecycle.PushResult{}, err
	}
	if configID == "" {
		return lifecycle.PushResult{Stopped: StoppedDeleted}, nil
	}
	r := &run{d: d, taskID: taskID, configID: configID}
	if activity.IsActivity(ctx) {
		r.stop = activity.GetWorkerStopChannel(ctx)
	}
	return r.run(ctx)
}

// configByRef finds the config whose lifecycle.ConfigRef is ref among the task's (at most MaxPerTask); ""
// when there is none: it was deleted, or its task with it.
func (d *Deliverer) configByRef(ctx context.Context, taskID uuid.UUID, ref string) (string, error) {
	rows, err := d.log.DB().Query(ctx, `select id from kontra.push_configs where task_id = $1`, taskID)
	if err != nil {
		return "", fmt.Errorf("push_webhook: read the configs of task %s: %w", taskID, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("push_webhook: read the configs of task %s: %w", taskID, err)
	}
	for _, id := range ids {
		if lifecycle.ConfigRef(taskID, id) == ref {
			return id, nil
		}
	}
	return "", nil
}

func (r *run) result(stopped string) lifecycle.PushResult {
	return lifecycle.PushResult{DeliveredSeq: r.prog.Seq, Stopped: stopped}
}

func (r *run) beat(ctx context.Context) {
	if r.d.onBeat != nil {
		r.d.onBeat()
	}
	if activity.IsActivity(ctx) {
		activity.RecordHeartbeat(ctx, r.prog)
	}
}

func (r *run) run(ctx context.Context) (lifecycle.PushResult, error) {
	cfg, err := r.d.loadRow(ctx, r.taskID, r.configID)
	if err != nil {
		return lifecycle.PushResult{}, err
	}
	if cfg == nil {
		return r.result(StoppedDeleted), nil
	}
	r.prog.Seq = cfg.delivered
	if cfg.failed {
		return r.result(StoppedFailed), nil
	}
	if activity.IsActivity(ctx) && activity.HasHeartbeatDetails(ctx) {
		var hb progress
		if err := activity.GetHeartbeatDetails(ctx, &hb); err == nil {
			// Resume from the larger of delivered_seq and the heartbeat details (spec §10.2), keeping the budget
			// of a seq that was failing.
			r.prog.Seq = max(r.prog.Seq, hb.Seq)
			if hb.failing() && hb.FailingSeq > r.prog.Seq {
				r.prog.FailingSeq, r.prog.FirstFailure, r.prog.Wait = hb.FailingSeq, hb.FirstFailure, hb.Wait
			}
		}
	}

	var wake <-chan struct{}
	if r.d.opts.Waker != nil {
		w, release := r.d.opts.Waker.WaitTask(ctx, r.taskID)
		defer release()
		wake = w
	}

	var f store.Folder
	if err := r.foldTo(ctx, &f, r.prog.Seq); err != nil {
		if errors.Is(err, errFold) {
			r.d.logger.Error("push: the task's log cannot be folded; the config is marked failed",
				"task", r.taskID.String(), "config", r.configID, "error", err)
			return r.giveUp(ctx)
		}
		return lifecycle.PushResult{}, err
	}
	r.prog.Seq = f.Seq()
	if f.Terminal() {
		// The terminal snapshot was delivered before a restart.
		return r.result(StoppedTerminal), nil
	}
	r.beat(ctx)
	for {
		evs, _, err := r.d.log.EventsWithin(ctx, r.taskID, f.Seq(), readBatch, r.d.opts.ReadBytes)
		if err != nil {
			return lifecycle.PushResult{}, r.ctxErr(ctx, fmt.Errorf("push_webhook: read the log of task %s: %w", r.taskID, err))
		}
		if len(evs) == 0 {
			// Idle: the config may have been deleted (or the task, with it) meanwhile.
			cfg, err := r.d.loadRow(ctx, r.taskID, r.configID)
			if err != nil {
				return lifecycle.PushResult{}, r.ctxErr(ctx, err)
			}
			if cfg == nil {
				return r.result(StoppedDeleted), nil
			}
			if cfg.failed {
				return r.result(StoppedFailed), nil
			}
			if err := r.idle(ctx, wake); err != nil {
				return lifecycle.PushResult{}, err
			}
			continue
		}
		for i, ev := range evs {
			if err := f.Apply(ev); err != nil {
				// A row that cannot be folded (a bucket-only part, a corrupt log) would fail every retry: the
				// config is given up, as at the end of the budget.
				r.d.logger.Error("push: the task's log cannot be folded; the config is marked failed",
					"task", r.taskID.String(), "config", r.configID, "seq", ev.Seq, "error", err)
				return r.giveUp(ctx)
			}
			if ev.Kind == store.KindArtifact && i+1 < len(evs) && evs[i+1].Kind == store.KindArtifact {
				continue // coalesced into the snapshot at the last seq of the run
			}
			// The body is the folded task itself, encoded once: the Folder is this run's own, so no copy
			// of a task of up to limits.task_bytes is made per seq.
			body, err := json.Marshal(a2a.StreamResponse{Event: f.Task()})
			if err != nil {
				return lifecycle.PushResult{}, fmt.Errorf("push_webhook: encode the snapshot of task %s at %d: %w", r.taskID, ev.Seq, err)
			}
			stopped, err := r.deliver(ctx, ev.Seq, body)
			if err != nil {
				return lifecycle.PushResult{}, err
			}
			if stopped != "" {
				return r.result(stopped), nil
			}
			if f.Terminal() {
				return r.result(StoppedTerminal), nil
			}
			select {
			case <-r.stop:
				// The worker is stopping (a drain): hand the activity over at a delivery boundary; the retry on
				// another poller resumes after this seq.
				return lifecycle.PushResult{}, errStopping
			default:
			}
		}
	}
}

// errFold is a log that cannot be folded (a bucket-only row, a corrupt row): every retry would meet it
// again, so the config is given up.
var errFold = errors.New("push_webhook: the log cannot be folded")

// foldTo folds the task's rows 1..seq into f (fewer when the log is shorter: a heartbeat of another
// generation of the config cannot move the position past the log). It reads the log as the delivery loop
// does, at most ReadBytes of stored rows at a time, so a task of up to limits.task_bytes is never read at
// once, and it heartbeats between reads, so folding a long log never outlasts the heartbeat timeout.
func (r *run) foldTo(ctx context.Context, f *store.Folder, seq int64) error {
	for f.Seq() < seq {
		evs, _, err := r.d.log.EventsWithin(ctx, r.taskID, f.Seq(), int(min(seq-f.Seq(), readBatch)), r.d.opts.ReadBytes)
		if err != nil {
			return r.ctxErr(ctx, fmt.Errorf("push_webhook: read the log of task %s: %w", r.taskID, err))
		}
		if len(evs) == 0 {
			return nil
		}
		for _, ev := range evs {
			if err := f.Apply(ev); err != nil {
				return fmt.Errorf("%w: task %s: %w", errFold, r.taskID, err)
			}
		}
		r.beat(ctx)
	}
	return nil
}

// ctxErr prefers the context's end (the activity was canceled, or its workflow closed) to the error it
// caused.
func (r *run) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// idle waits for the log to grow: a wake, or IdlePoll; it heartbeats on the way.
func (r *run) idle(ctx context.Context, wake <-chan struct{}) error {
	r.beat(ctx)
	t := time.NewTimer(r.d.opts.IdlePoll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.stop:
		return errStopping
	case <-wake:
	case <-t.C:
	}
	return nil
}

// wait sleeps d through the deliverer's sleep, in steps short enough to keep heartbeating, and returns
// early when the worker stops, or with StoppedDeleted or StoppedFailed when the config was deleted or
// marked failed meanwhile: the activity ends when its config is deleted (spec §10.2), not after a backoff
// of up to RetryMax, which would hold the workflow open that long (D78).
func (r *run) wait(ctx context.Context, d time.Duration) (string, error) {
	for d > 0 {
		step := min(d, beatEvery)
		if err := r.d.opts.Sleep(ctx, step); err != nil {
			return "", err
		}
		d -= step
		r.beat(ctx)
		select {
		case <-r.stop:
			return "", errStopping
		default:
		}
		stopped, err := r.d.rowState(ctx, r.taskID, r.configID)
		if err != nil {
			return "", r.ctxErr(ctx, err)
		}
		if stopped != "" {
			return stopped, nil
		}
	}
	return "", nil
}

// deliver POSTs body, the snapshot at seq, until it is delivered, the config is deleted, or the seq's budget
// runs out. It returns "" when delivered (delivered_seq written), else StoppedDeleted or StoppedFailed.
func (r *run) deliver(ctx context.Context, seq int64, body []byte) (string, error) {
	wait := r.d.opts.RetryMin
	if r.prog.failing() && r.prog.FailingSeq <= seq && r.prog.Wait > 0 {
		wait = r.prog.Wait
	}
	for attempt := 1; ; attempt++ {
		// The config is read again before every attempt: it may have been deleted, or set again with new
		// credentials (Save updates it in place).
		cfg, err := r.d.loadRow(ctx, r.taskID, r.configID)
		if err != nil {
			return "", r.ctxErr(ctx, err)
		}
		if cfg == nil {
			return StoppedDeleted, nil
		}
		if cfg.failed {
			return StoppedFailed, nil
		}
		host := destination(cfg.cfg.URL)
		if stopped, err := r.rate(ctx, host); err != nil || stopped != "" {
			return stopped, err
		}
		status, perr := r.d.post(ctx, cfg.cfg, body)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if perr == nil && status >= 200 && status < 300 {
			r.prog = progress{Seq: seq}
			r.beat(ctx)
			if err := r.d.markDelivered(ctx, r.taskID, r.configID, seq); err != nil {
				return "", r.ctxErr(ctx, err)
			}
			return "", nil
		}
		now := r.d.opts.Now()
		if !r.prog.failing() {
			r.prog.FailingSeq, r.prog.FirstFailure = seq, now
		}
		elapsed := now.Sub(r.prog.FirstFailure)
		attrs := []any{"task", r.taskID.String(), "config", r.configID, "seq", seq, "host", host, "attempt", attempt,
			"failing_for", elapsed.Truncate(time.Second).String()}
		if perr != nil {
			attrs = append(attrs, "error", perr.Error())
		} else {
			attrs = append(attrs, "status", status)
		}
		if elapsed >= r.d.opts.Budget {
			r.d.logger.Warn("push: delivery failed for the whole retry budget; the config is marked failed", attrs...)
			res, err := r.giveUp(ctx)
			return res.Stopped, err
		}
		r.d.logger.Info("push: delivery failed; retrying", append(attrs, "retry_in", wait.String())...)
		next := min(2*wait, r.d.opts.RetryMax)
		r.prog.Wait = next
		r.beat(ctx)
		if stopped, err := r.wait(ctx, min(wait, r.d.opts.Budget-elapsed)); err != nil || stopped != "" {
			return stopped, err
		}
		wait = next
	}
}

// giveUp marks the config failed: delivery stops for it, and the task is unaffected (spec §10.2).
func (r *run) giveUp(ctx context.Context) (lifecycle.PushResult, error) {
	if err := r.d.markFailed(ctx, r.taskID, r.configID); err != nil {
		return lifecycle.PushResult{}, r.ctxErr(ctx, err)
	}
	return r.result(StoppedFailed), nil
}

// rate waits for the next delivery slot to host: 10/N a second on this replica (D72); like wait, it ends
// early when the config is deleted or marked failed.
func (r *run) rate(ctx context.Context, host string) (string, error) {
	n := r.d.liveReplicaCount(ctx)
	interval := time.Second * time.Duration(n) / RatePerHost
	now := r.d.opts.Now()
	at := r.d.limit.reserve(host, now, interval)
	return r.wait(ctx, at.Sub(now))
}

// destination is the host a delivery's rate is counted by: the URL's host name, lowercased, without the
// port.
func destination(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// post sends one delivery and returns the response's status. Its error names no URL: http.Client wraps
// every failure in a *url.Error that quotes the URL, which may carry a secret in its path or query.
func (d *Deliverer) post(ctx context.Context, cfg *a2a.PushConfig, body []byte) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, d.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("the config's URL cannot be requested")
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("User-Agent", "kontra-gate/"+version.Get())
	if a := cfg.Auth; a != nil && a.Credentials != "" {
		v := a.Credentials
		if a.Scheme != "" {
			v = a.Scheme + " " + a.Credentials
		}
		req.Header.Set("Authorization", v)
	}
	if cfg.Token != "" {
		req.Header[HeaderToken] = []string{cfg.Token}
		req.Header[HeaderTokenLegacy] = []string{cfg.Token}
	}
	resp, err := d.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fmt.Errorf("no answer within %s", d.opts.Timeout)
		}
		return 0, err
	}
	_, _ = io.CopyN(io.Discard, resp.Body, drainBytes)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// loadRow reads the config as a delivery needs it; nil when it no longer exists (deleted, or its task
// deleted with it). A stored config that does not decode would fail every retry: it is marked failed here
// and returned as failed.
func (d *Deliverer) loadRow(ctx context.Context, taskID uuid.UUID, configID string) (*row, error) {
	var (
		r   = row{id: configID}
		raw []byte
	)
	err := d.log.DB().QueryRow(ctx, `select config, delivered_seq, failed_at is not null from kontra.push_configs
		where task_id = $1 and id = $2`, taskID, configID).Scan(&raw, &r.delivered, &r.failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("push_webhook: read a config of task %s: %w", taskID, err)
	}
	if r.cfg, err = decodeConfig(raw); err != nil || r.cfg.URL == "" {
		// The error names the JSON's position, never its content.
		d.logger.Error("push: a stored config does not decode; it is marked failed", "task", taskID.String(), "config", configID)
		if err := d.markFailed(ctx, taskID, configID); err != nil {
			return nil, err
		}
		r.failed = true
	}
	return &r, nil
}

// rowState reports whether the config ended meanwhile: StoppedDeleted when it no longer exists,
// StoppedFailed when it is marked failed, "" while it is live.
func (d *Deliverer) rowState(ctx context.Context, taskID uuid.UUID, configID string) (string, error) {
	var failed bool
	err := d.log.DB().QueryRow(ctx, `select failed_at is not null from kontra.push_configs
		where task_id = $1 and id = $2`, taskID, configID).Scan(&failed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return StoppedDeleted, nil
	case err != nil:
		return "", fmt.Errorf("push_webhook: read a config of task %s: %w", taskID, err)
	case failed:
		return StoppedFailed, nil
	}
	return "", nil
}

// markDelivered writes delivered_seq after a delivery; it only ever moves forward (spec §10.2).
func (d *Deliverer) markDelivered(ctx context.Context, taskID uuid.UUID, configID string, seq int64) error {
	if _, err := d.log.DB().Exec(ctx, `update kontra.push_configs set delivered_seq = greatest(delivered_seq, $3)
		where task_id = $1 and id = $2`, taskID, configID, seq); err != nil {
		return fmt.Errorf("push_webhook: record delivery %d of a config of task %s: %w", seq, taskID, err)
	}
	return nil
}

// markFailed marks the config failed; delivery stops for it.
func (d *Deliverer) markFailed(ctx context.Context, taskID uuid.UUID, configID string) error {
	if _, err := d.log.DB().Exec(ctx, `update kontra.push_configs set failed_at = now()
		where task_id = $1 and id = $2 and failed_at is null`, taskID, configID); err != nil {
		return fmt.Errorf("push_webhook: mark a config of task %s failed: %w", taskID, err)
	}
	return nil
}

// liveReplicas counts the replicas in gate_replicas seen within ReplicaSilence, draining ones included.
func (d *Deliverer) liveReplicas(ctx context.Context) (int, error) {
	var n int
	err := d.log.DB().QueryRow(ctx, `select count(*) from kontra.gate_replicas
		where last_seen_at > now() - $1::bigint * interval '1 microsecond'`, ReplicaSilence.Microseconds()).Scan(&n)
	return n, err
}

// liveReplicaCount is N for the rate cap, read again every ReplicaRefresh, at least 1 (D72). A failed read
// keeps the last N (1 before the first) and is retried at the next refresh.
func (d *Deliverer) liveReplicaCount(ctx context.Context) int {
	now := d.opts.Now()
	d.mu.Lock()
	if !d.replicasAt.IsZero() && now.Sub(d.replicasAt) < ReplicaRefresh {
		n := d.replicas
		d.mu.Unlock()
		return max(n, 1)
	}
	d.replicasAt = now // one reader refreshes; the others keep the last N meanwhile
	last := d.replicas
	d.mu.Unlock()

	n, err := d.opts.Replicas(ctx)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if !d.replicasErr {
			d.logger.Warn("push: cannot count the live gate replicas; the rate cap keeps the last count", "error", err)
		}
		d.replicasErr = true
		return max(last, 1)
	}
	d.replicasErr = false
	d.replicas = n
	return max(n, 1)
}

// hostLimiter spaces deliveries to one host: each reservation takes the next free slot, interval after the
// previous one. Slots are kept per host name; stale ones are pruned as the map grows.
type hostLimiter struct {
	mu   sync.Mutex
	next map[string]time.Time
}

func newHostLimiter() *hostLimiter { return &hostLimiter{next: map[string]time.Time{}} }

const limiterPrune = 4096

// reserve returns when the caller may send to host: now, or the host's next free slot.
func (l *hostLimiter) reserve(host string, now time.Time, interval time.Duration) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.next[host]
	if at.Before(now) {
		at = now
	}
	l.next[host] = at.Add(interval)
	if len(l.next) > limiterPrune {
		for h, t := range l.next {
			if !t.After(now) {
				delete(l.next, h)
			}
		}
	}
	return at
}
