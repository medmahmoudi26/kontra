// Package push stores push notification configs and delivers them (spec/relay.md §10.1–§10.2, D16, D55).
//
// Configs live in push_configs per gate task; the gate, not a2a-go's executor, validates a config's URL
// when it is set: a new task's inline config and CreateTaskPushNotificationConfig with DNS (CheckURL), a
// follow-up's inline config with DNS only when its URL is new for the task (CheckSet). A config set again
// with the URL it already has is not resolved again, so neither a follow-up that re-sends its config nor
// a2a-go's per-turn re-save is refused for a DNS change. Delivery is the lifecycle's, never the relaying
// replica's: one push_webhook activity per live config on kontra-push tails the log from its last
// delivered seq and POSTs the task snapshot at each seq, in order, at least once. a2a-go's inline push is
// disabled with NoopSender.
//
// push_configs.config holds the caller's webhook credentials (the URL, the token, the authentication). It
// is never logged, never in an operator read (spec §5.3, §13.5), and never crosses Temporal history: the
// workflow and its activities name a config by its lifecycle.ConfigRef only (a config id may be the
// caller's), and every error this package returns or logs names ids, seqs, states, HTTP status codes and
// the destination host, never a URL, a token or credentials; a refusal of a URL names the rule only, never
// what its host resolves to.
package push

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a2aproject/a2a-go/v2/a2a"
	a2apush "github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/http/httpguts"

	"kontra.sh/kontra/internal/auth"
	"kontra.sh/kontra/internal/ids"
	"kontra.sh/kontra/internal/lifecycle"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/store"
)

// Limits on configs (spec §10.1); beyond them, InvalidParams.
const (
	MaxPerTask  = 10
	MaxPerBuyer = 256 // delivering configs
)

// Limits on one config, checked when it is set. They are not the spec's: a config is sent on every delivery
// (its token and authentication as HTTP header values), so a value no HTTP header can carry, or a config
// larger than any header block, would only make every delivery fail.
const (
	// MaxConfigBytes bounds a config as stored (its JSON).
	MaxConfigBytes = 16 << 10
	// MaxIDChars is push_configs.id's CHECK (1..128 characters).
	MaxIDChars = 128
)

// signalTimeout bounds kontra.push after a Save or a Delete committed: it is best effort, because the
// workflow lists the task's configs at start and after each dispatch (spec §10.2).
const signalTimeout = 5 * time.Second

// Signaler announces a config change to the task's workflow (kontra.push); lifecycle.Client implements it.
type Signaler interface {
	SignalPush(ctx context.Context, taskID a2a.TaskID, s lifecycle.PushSignal) error
}

// ConfigStore implements a2a-go's push.ConfigStore over push_configs. Save is idempotent: a config without
// an id is keyed by (task, URL, token), so a2a-go's re-save on every turn neither duplicates a row nor
// starts a second delivery. After a Save that inserted a row, or a Delete, commits, it sends kontra.push.
// config holds the caller's webhook credentials: it is never logged and never in an operator read.
//
// Authorization is a2a-go's: its handler reads the task through the caller-scoped task store before every
// push config call, so a config call on another buyer's task is TaskNotFound before this store runs. The
// store adds what the task store cannot see: the anonymous principal never sets a config (spec §2.3), and
// the per-task and per-buyer limits.
type ConfigStore struct {
	db     *pgxpool.Pool
	signal Signaler
	policy ssrf.Policy
	logger *slog.Logger
}

var (
	_ a2apush.ConfigStore        = (*ConfigStore)(nil)
	_ lifecycle.PushConfigLister = (*ConfigStore)(nil)
	_ lifecycle.InlinePushSaver  = (*ConfigStore)(nil)
)

// NewConfigStore returns the store; policy is push.allow_private (with the resolver, in tests). signal may
// be nil (no kontra.push is sent; tests), and logger nil logs to slog.Default().
func NewConfigStore(db *pgxpool.Pool, signal Signaler, policy ssrf.Policy, logger *slog.Logger) *ConfigStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ConfigStore{db: db, signal: signal, policy: policy, logger: logger}
}

func invalid(msg string) error     { return a2a.NewError(a2a.ErrInvalidParams, msg) }
func unsupported(msg string) error { return a2a.NewError(a2a.ErrUnsupportedOperation, msg) }

// errConfigNotFound is a config the task does not have. A2A answers TaskNotFoundError (spec §3.1.8); it
// also matches a2a-go's push.ErrPushConfigNotFound.
var errConfigNotFound = a2a.NewError(errors.Join(a2a.ErrTaskNotFound, a2apush.ErrPushConfigNotFound), "push notification config not found")

func taskNotFound() error { return a2a.NewError(a2a.ErrTaskNotFound, "task not found") }

// anonymousRefused is the anonymous principal setting a config (spec §2.3); the interceptor answers the
// same before the store runs.
func anonymousRefused() error {
	return unsupported("push notifications are not available to anonymous callers")
}

// CheckURL is the set-time check of a push config URL (spec §10.1, §10.3): the URL parses and
// ssrf.CheckURL accepts it under policy: https only, or http:// too under push.allow_private (D67), no
// userinfo, and every address the host resolves to globally reachable unless allow_private. It never
// connects. A refusal is InvalidParams with a fixed text per rule (refusal); a lookup that failed for a
// reason of the gate's own (a timeout, SERVFAIL) is an internal error, not the caller's. A redirecting URL
// is accepted: its redirect is never followed (D60). The interceptors call it for a new task's inline
// config and for CreateTaskPushNotificationConfig, and CheckSet for a follow-up's inline config.
func CheckURL(ctx context.Context, rawURL string, policy ssrf.Policy) error {
	return checkURL(ctx, rawURL, policy, slog.Default())
}

func checkURL(ctx context.Context, rawURL string, policy ssrf.Policy, logger *slog.Logger) error {
	u, err := parseURL(rawURL)
	if err != nil {
		return err
	}
	if err := ssrf.CheckURL(ctx, u, policy); err != nil {
		return refusal(ctx, logger, u, err)
	}
	return nil
}

// refusal is what the caller is told of a URL ssrf refused: one fixed text per rule, never the URL, the
// addresses its host resolves to, or the resolver's answer, so a refusal maps nothing of the gate's network
// (a hire-scoped key could otherwise learn what internal names resolve to). A name that does not resolve
// and one that resolves to a refused address get the same text. The detail is logged at Debug, naming the
// host, never the URL (its path or query may carry a secret).
func refusal(ctx context.Context, logger *slog.Logger, u *url.URL, err error) error {
	logger.DebugContext(ctx, "push: a notification URL was refused", "host", u.Hostname(), "error", err.Error())
	switch {
	case errors.Is(err, ssrf.ErrLookup):
		return a2a.NewError(a2a.ErrInternalError, "push notification URL cannot be checked now: its host could not be looked up; try again")
	case errors.Is(err, ssrf.ErrScheme):
		return invalid("push notification URL refused: scheme not allowed")
	case errors.Is(err, ssrf.ErrURL):
		return invalid("push notification URL refused: URL not allowed")
	}
	return invalid("push notification URL refused: its host is not globally reachable or does not resolve")
}

// CheckSet is the set-time check of a config set on a task that already exists (spec §10.1): an inline
// config on a follow-up message. When the config Save would update (found by its id, or by its URL and
// token) already has this URL, nothing is checked again: it was checked when it was set, and every
// delivery checks every address when it dials. So a follow-up that sends its unchanged config again is
// never refused for a DNS change; any other URL gets CheckURL.
func CheckSet(ctx context.Context, db *pgxpool.Pool, taskID uuid.UUID, cfg *a2a.PushConfig, policy ssrf.Policy) error {
	if cfg == nil {
		return nil
	}
	if cfg.URL != "" {
		prior, err := find(ctx, db, taskID, cfg)
		if err != nil {
			return err
		}
		if prior != nil && prior.cfg.URL == cfg.URL {
			return nil
		}
	}
	return CheckURL(ctx, cfg.URL, policy)
}

func parseURL(rawURL string) (*url.URL, error) {
	if rawURL == "" {
		return nil, invalid("push notification URL is required")
	}
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() {
		// url.Parse's error quotes the URL; the caller's URL may carry a secret in its path.
		return nil, invalid("push notification URL is not an absolute URL")
	}
	return u, nil
}

// errNoLookup stands in for DNS in checkURLOffline.
var errNoLookup = errors.New("push: no lookup at this point")

type noLookup struct{}

func (noLookup) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errNoLookup
}

// checkURLOffline is CheckURL without DNS: the scheme, userinfo, host and port rules, and the address rule
// for an IP literal. It is what SaveInline applies inside the work queue's transaction, where a lookup would
// hold the transaction open; the inline config's URL was checked with DNS by the interceptor a moment
// before (CheckURL, or CheckSet for a follow-up), and every delivery checks every address again when it
// dials.
func checkURLOffline(rawURL string, policy ssrf.Policy, logger *slog.Logger) error {
	u, err := parseURL(rawURL)
	if err != nil {
		return err
	}
	p := ssrf.Policy{AllowPrivate: policy.AllowPrivate, Resolver: noLookup{}}
	if err := ssrf.CheckURL(context.Background(), u, p); err != nil && !errors.Is(err, errNoLookup) {
		return refusal(context.Background(), logger, u, err)
	}
	return nil
}

// stored is push_configs.config: the caller's config as a2a-go's JSON (url, token, authentication), with
// the row's id and the gate task id.
func stored(taskID uuid.UUID, id string, cfg *a2a.PushConfig) *a2a.PushConfig {
	out := &a2a.PushConfig{TaskID: a2a.TaskID(taskID.String()), ID: id, URL: cfg.URL, Token: cfg.Token}
	if cfg.Auth != nil {
		a := *cfg.Auth
		out.Auth = &a
	}
	return out
}

// validate checks what can be checked without the database or DNS: the id, the header values, the size.
func validate(cfg *a2a.PushConfig) error {
	if cfg == nil {
		return invalid("push notification config is required")
	}
	if cfg.URL == "" {
		return invalid("push notification URL is required")
	}
	if cfg.ID != "" {
		n := utf8.RuneCountInString(cfg.ID)
		if !utf8.ValidString(cfg.ID) || n > MaxIDChars || strings.ContainsFunc(cfg.ID, isControl) {
			return invalid(fmt.Sprintf("push notification config id must be 1 to %d printable characters", MaxIDChars))
		}
	}
	if cfg.Token != "" && !headerValue(cfg.Token) {
		return invalid("push notification token cannot be sent as an HTTP header value")
	}
	if a := cfg.Auth; a != nil {
		if a.Scheme != "" && !token(a.Scheme) {
			return invalid("push notification authentication scheme must be an HTTP token")
		}
		if a.Credentials != "" && !headerValue(a.Credentials) {
			return invalid("push notification authentication credentials cannot be sent as an HTTP header value")
		}
	}
	b, err := json.Marshal(stored(uuid.Nil, cfg.ID, cfg))
	if err != nil {
		return invalid("push notification config cannot be stored")
	}
	if len(b) > MaxConfigBytes {
		return invalid(fmt.Sprintf("push notification config is larger than %d bytes", MaxConfigBytes))
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// headerValue reports whether s can be an HTTP header value: no control character but horizontal tab (a
// NUL also cannot be stored in jsonb), and no leading or trailing whitespace that a receiver would strip.
func headerValue(s string) bool {
	return httpguts.ValidHeaderFieldValue(s) && !strings.ContainsRune(s, 0) && strings.TrimSpace(s) == s
}

// token reports whether s is an HTTP token (RFC 9110 §5.6.2), what an authentication scheme is.
func token(s string) bool { return httpguts.ValidHeaderFieldName(s) }

// lockKey is the transaction-scoped advisory lock that serializes the config writes of one buyer, so the
// per-task and per-buyer counts and the idempotency lookup are read and acted on atomically.
func lockKey(buyer uuid.UUID) int64 {
	sum := sha256.Sum256(append([]byte("kontra.push\x00"), buyer[:]...))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// row is one push_configs row as Save and the delivery read it.
type row struct {
	id        string
	cfg       *a2a.PushConfig
	delivered int64
	failed    bool
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func decodeConfig(b []byte) (*a2a.PushConfig, error) {
	var c a2a.PushConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("push: decode a stored config: %w", err)
	}
	return &c, nil
}

// find returns the config Save would update instead of inserting: the row with cfg's id, or, for a config
// without an id, the oldest row with its URL and token (spec §10.1). Nil when there is none.
func find(ctx context.Context, q querier, taskID uuid.UUID, cfg *a2a.PushConfig) (*row, error) {
	var (
		r      row
		raw    []byte
		failed bool
		err    error
	)
	if cfg.ID != "" {
		err = q.QueryRow(ctx, `select id, config, delivered_seq, failed_at is not null from kontra.push_configs
			where task_id = $1 and id = $2`, taskID, cfg.ID).Scan(&r.id, &raw, &r.delivered, &failed)
	} else {
		err = q.QueryRow(ctx, `select id, config, delivered_seq, failed_at is not null from kontra.push_configs
			where task_id = $1 and config->>'url' = $2 and coalesce(config->>'token', '') = $3
			order by created_at, id limit 1`, taskID, cfg.URL, cfg.Token).Scan(&r.id, &raw, &r.delivered, &failed)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("push: read the configs of task %s: %w", taskID, err)
	}
	if r.cfg, err = decodeConfig(raw); err != nil {
		return nil, err
	}
	r.failed = failed
	return &r, nil
}

// taskOwner is what Save reads of the task: its buyer, and whether its principal is anonymous.
type taskOwner struct {
	buyer     uuid.UUID
	anonymous bool
}

func loadOwner(ctx context.Context, q querier, taskID uuid.UUID) (taskOwner, error) {
	var (
		o   taskOwner
		via string
	)
	err := q.QueryRow(ctx, `select buyer_id, via from kontra.tasks where id = $1`, taskID).Scan(&o.buyer, &via)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, taskNotFound()
	}
	if err != nil {
		return o, fmt.Errorf("push: read task %s: %w", taskID, err)
	}
	o.anonymous = via == store.ViaAnonymous
	return o, nil
}

// insertSQL inserts a new config. On a task that has not ended, delivered_seq starts just before the
// task's last task, status or artifact row, so the first delivery is the snapshot of the task as it is when
// the config is set and every change after it follows (a config saved with a new task starts at 0: its
// first delivery is the task row). The spec's "tails the event log from the last seq it delivered" says
// nothing of a config that has delivered nothing yet; replaying snapshots from before a config existed
// would only send a receiver states it never asked for.
//
// On a task that has ended, delivered_seq starts at the terminal row (the last task, status or artifact
// row, store.Log.TerminalSeq): the config is done when it is set. An ended task has no change left to
// deliver (push_webhook stops at the terminal snapshot), as with a2a-go's own push, which sends a config
// the events that follow it; and once the task's workflow has closed nothing could deliver it anyway, so a
// config that waited for a delivery would hold one of the buyer's 256 delivering configs until retention.
// The config is stored, listed and deletable like any other (A2A §3.1.7: it persists until the task
// completes).
const insertSQL = `insert into kontra.push_configs (task_id, id, config, delivered_seq)
	values ($1, $2, $3, coalesce((select case when t.terminal_at is null then greatest(coalesce(max(e.seq), 1) - 1, 0)
			else coalesce(max(e.seq), 0) end
		from kontra.tasks t left join kontra.task_events e on e.task_id = t.id and e.kind in ('task', 'status', 'artifact')
		where t.id = $1 group by t.id), 0))
	on conflict (task_id, id) do nothing`

// deliveringSQL counts a buyer's delivering configs: not failed, and not done, i.e. its task is not
// terminal or the terminal snapshot (the task's last task, status or artifact row once it is terminal,
// store.Log.TerminalSeq) is not delivered yet.
//
// It reaches the buyer's tasks through the services and an index that leads with (tenant_id, service,
// buyer_id) (tasks_by_caller, tasks_by_context), one index range per service, and each task's configs
// through push_configs' primary key (the aggregate keeps the planner from turning that into a scan of
// every config). So its cost follows the buyer's own tasks in retention, never every buyer's tasks or
// configs: no index of 0001 leads with buyer_id, and push_configs has no buyer column.
const deliveringSQL = `select coalesce(sum(d.n), 0)::bigint from kontra.services s
	join kontra.tasks t on t.tenant_id = s.tenant_id and t.service = s.name and t.buyer_id = $1
	cross join lateral (select count(*) as n from kontra.push_configs p
		where p.task_id = t.id and p.failed_at is null
		  and (t.terminal_at is null or p.delivered_seq < (select coalesce(max(e.seq), 0) from kontra.task_events e
			where e.task_id = t.id and e.kind in ('task', 'status', 'artifact')))) d`

// savedConfig is what save did: the stored config; inserted, a new row; announce, the workflow should
// start (or keep) a push_webhook for it: a row inserted on a task that has not ended, or a failed config set
// again on such a task (revived).
type savedConfig struct {
	cfg      *a2a.PushConfig
	inserted bool
	announce bool
}

// checkBuyerLimit refuses one more delivering config for buyer beyond MaxPerBuyer.
func checkBuyerLimit(ctx context.Context, tx pgx.Tx, buyer uuid.UUID) error {
	var perBuyer int
	if err := tx.QueryRow(ctx, deliveringSQL, buyer).Scan(&perBuyer); err != nil {
		return fmt.Errorf("push: count the buyer's delivering configs: %w", err)
	}
	if perBuyer >= MaxPerBuyer {
		return invalid(fmt.Sprintf("a buyer has at most %d delivering push notification configs", MaxPerBuyer))
	}
	return nil
}

// taskEnded reports whether the task has ended (its terminal event is logged).
func taskEnded(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) (bool, error) {
	var ended bool
	err := tx.QueryRow(ctx, `select terminal_at is not null from kontra.tasks where id = $1`, taskID).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, taskNotFound()
	}
	if err != nil {
		return false, fmt.Errorf("push: read task %s: %w", taskID, err)
	}
	return ended, nil
}

// save is Save's write in tx, under the buyer's lock: update the config found by its id or key, or insert
// it within the limits. urlChecked checks a URL being set (a new config's, or a changed one): Save's has the
// DNS lookup done before the transaction, SaveInline's is checkURLOffline.
func (s *ConfigStore) save(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, buyer uuid.UUID, cfg *a2a.PushConfig, urlChecked func(string) error) (savedConfig, error) {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, lockKey(buyer)); err != nil {
		return savedConfig{}, fmt.Errorf("push: lock the buyer's configs: %w", err)
	}
	ended, err := taskEnded(ctx, tx, taskID)
	if err != nil {
		return savedConfig{}, err
	}
	q := querier(tx)
	existing, err := find(ctx, q, taskID, cfg)
	if err != nil {
		return savedConfig{}, err
	}
	if existing != nil {
		// Save creates or updates (a2a-go's ConfigStore): the row keeps its id and its delivery position, and
		// no second delivery starts. A changed URL is checked as any URL being set. A config that failed and
		// is set again on a task that has not ended is delivering again, from where it stopped, within the
		// buyer's limit: its failure is cleared and kontra.push announces it (a caller that fixed its webhook
		// would otherwise be told its config is set while nothing ever delivers it). On an ended task there
		// is nothing left to deliver (insertSQL), so a failed config stays as it is.
		if existing.cfg.URL != cfg.URL {
			if err := urlChecked(cfg.URL); err != nil {
				return savedConfig{}, err
			}
		}
		next := stored(taskID, existing.id, cfg)
		revive := existing.failed && !ended
		if revive {
			if err := checkBuyerLimit(ctx, tx, buyer); err != nil {
				return savedConfig{}, err
			}
		}
		if revive || !sameConfig(existing.cfg, next) {
			b, err := json.Marshal(next)
			if err != nil {
				return savedConfig{}, err
			}
			if _, err := tx.Exec(ctx, `update kontra.push_configs set config = $3,
				failed_at = case when $4 then null else failed_at end where task_id = $1 and id = $2`,
				taskID, existing.id, b, revive); err != nil {
				return savedConfig{}, fmt.Errorf("push: update a config of task %s: %w", taskID, err)
			}
		}
		return savedConfig{cfg: next, announce: revive}, nil
	}
	if err := urlChecked(cfg.URL); err != nil {
		return savedConfig{}, err
	}
	var perTask int
	if err := tx.QueryRow(ctx, `select count(*) from kontra.push_configs where task_id = $1`, taskID).Scan(&perTask); err != nil {
		return savedConfig{}, fmt.Errorf("push: count the configs of task %s: %w", taskID, err)
	}
	if perTask >= MaxPerTask {
		return savedConfig{}, invalid(fmt.Sprintf("a task has at most %d push notification configs", MaxPerTask))
	}
	if !ended {
		// A config set on an ended task is done when it is set (insertSQL): it never delivers, so it is not
		// counted against the buyer's delivering configs.
		if err := checkBuyerLimit(ctx, tx, buyer); err != nil {
			return savedConfig{}, err
		}
	}
	id := cfg.ID
	if id == "" {
		u, err := uuid.NewV7()
		if err != nil {
			return savedConfig{}, err
		}
		id = u.String()
	}
	next := stored(taskID, id, cfg)
	b, err := json.Marshal(next)
	if err != nil {
		return savedConfig{}, err
	}
	tag, err := tx.Exec(ctx, insertSQL, taskID, id, b)
	if err != nil {
		return savedConfig{}, fmt.Errorf("push: insert a config of task %s: %w", taskID, err)
	}
	if tag.RowsAffected() != 1 {
		// Every writer of this buyer's configs holds the lock, and the lookup above found no row with this id.
		return savedConfig{}, fmt.Errorf("push: config %q of task %s appeared under the lock", id, taskID)
	}
	return savedConfig{cfg: next, inserted: true, announce: !ended}, nil
}

func sameConfig(a, b *a2a.PushConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// Save implements push.ConfigStore (spec §10.1): a2a-go calls it for CreateTaskPushNotificationConfig, and
// for an inline config at dispatch when the work queue did not save it. The URL is checked when it is set
// (CheckURL, DNS included), before the transaction; re-saving a config with the URL it already has checks
// nothing again, so a2a-go's per-turn re-save (which runs only when no InlinePushSaver is wired) never fails
// a turn on a DNS change. A config without an id that has the URL and token of an existing config updates
// it; one with an id updates the config with that id. A new config beyond 10 for the task, or 256
// delivering for the buyer, is InvalidParams; the anonymous principal is refused (spec §2.3). After a
// commit that inserted a row on a task that has not ended, or set a failed config again (save), kontra.push
// is sent.
func (s *ConfigStore) Save(ctx context.Context, taskID a2a.TaskID, config *a2a.PushConfig) (*a2a.PushConfig, error) {
	tid, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return nil, err
	}
	if p, ok := auth.FromContext(ctx); ok && p.Kind == auth.KindAnonymous {
		return nil, anonymousRefused()
	}
	if err := validate(config); err != nil {
		return nil, err
	}
	// Check the URL before the transaction, unless a config this Save will update already has it.
	prior, err := find(ctx, s.db, tid, config)
	if err != nil {
		return nil, err
	}
	checked := ""
	if prior == nil || prior.cfg.URL != config.URL {
		if err := checkURL(ctx, config.URL, s.policy, s.logger); err != nil {
			return nil, err
		}
		checked = config.URL
	} else {
		checked = prior.cfg.URL
	}
	urlChecked := func(u string) error {
		if u == checked {
			return nil
		}
		// The row changed between the lookup and the lock: check now (rare).
		return checkURL(ctx, u, s.policy, s.logger)
	}
	var res savedConfig
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		owner, err := loadOwner(ctx, tx, tid)
		if err != nil {
			return err
		}
		if owner.anonymous {
			return anonymousRefused()
		}
		res, err = s.save(ctx, tx, tid, owner.buyer, config, urlChecked)
		return err
	})
	if err != nil {
		return nil, foreignTask(err)
	}
	if res.announce {
		s.signalPush(ctx, tid, lifecycle.PushSignal{ConfigID: res.cfg.ID})
	}
	return res.cfg, nil
}

// foreignTask maps the foreign key violation of a task deleted meanwhile to TaskNotFound.
func foreignTask(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return taskNotFound()
	}
	return err
}

// SaveInline implements lifecycle.InlinePushSaver: the inline configuration.taskPushNotificationConfig, saved
// in the work queue's transaction before the work item (spec §10.1), with Save's rules and limits. The URL
// gets checkURLOffline here (the interceptor checked it with DNS); the owner is the caller, the task's
// buyer. No kontra.push is sent here: the work queue sends it for a follow-up once kontra.message is
// accepted, and a new task's workflow lists its configs at start. The id returned is the config to
// announce: a row inserted, or a failed config set again (revived); empty when the save only updated a
// config that needs no new push_webhook. inserted reports a new row (the work queue deletes it when the
// follow-up is refused). A row inserted on a task that ended meanwhile is announced too: its push_webhook,
// if the workflow still runs one, finds it done and ends at once.
func (s *ConfigStore) SaveInline(ctx context.Context, tx pgx.Tx, taskID uuid.UUID, owner store.Owner, cfg *a2a.PushConfig) (string, bool, error) {
	if owner.Via == store.ViaAnonymous {
		return "", false, anonymousRefused()
	}
	if err := validate(cfg); err != nil {
		return "", false, err
	}
	res, err := s.save(ctx, tx, taskID, owner.BuyerID, cfg, func(u string) error {
		return checkURLOffline(u, s.policy, s.logger)
	})
	if err != nil {
		return "", false, foreignTask(err)
	}
	if !res.announce && !res.inserted {
		return "", false, nil
	}
	return res.cfg.ID, res.inserted, nil
}

// DeleteInline implements lifecycle.InlinePushSaver: it removes the row SaveInline inserted for a follow-up
// the lifecycle refused. No kontra.push is sent; a push_webhook a listing started for it ends when it finds
// its config gone (spec §10.2).
func (s *ConfigStore) DeleteInline(ctx context.Context, taskID uuid.UUID, configID string) error {
	if _, err := s.db.Exec(ctx, `delete from kontra.push_configs where task_id = $1 and id = $2`, taskID, configID); err != nil {
		return fmt.Errorf("push: delete a config of task %s: %w", taskID, err)
	}
	return nil
}

// Get implements push.ConfigStore: the config, or TaskNotFound when the task has no config with that id
// (spec §3.1.8).
func (s *ConfigStore) Get(ctx context.Context, taskID a2a.TaskID, configID string) (*a2a.PushConfig, error) {
	tid, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return nil, err
	}
	if configID == "" || !utf8.ValidString(configID) || strings.ContainsRune(configID, 0) {
		return nil, errConfigNotFound
	}
	var raw []byte
	err = s.db.QueryRow(ctx, `select config from kontra.push_configs where task_id = $1 and id = $2`, tid, configID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errConfigNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("push: read a config of task %s: %w", tid, err)
	}
	return decodeConfig(raw)
}

// List implements push.ConfigStore: every config of the task, failed ones included, oldest first. a2a-go's
// processor also lists them after every event a dispatch logs (and sends nothing, NoopSender).
func (s *ConfigStore) List(ctx context.Context, taskID a2a.TaskID) ([]*a2a.PushConfig, error) {
	tid, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `select config from kontra.push_configs where task_id = $1 order by created_at, id`, tid)
	if err != nil {
		return nil, fmt.Errorf("push: list the configs of task %s: %w", tid, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*a2a.PushConfig, error) {
		var raw []byte
		if err := r.Scan(&raw); err != nil {
			return nil, err
		}
		return decodeConfig(raw)
	})
	if err != nil {
		return nil, fmt.Errorf("push: list the configs of task %s: %w", tid, err)
	}
	if out == nil {
		out = []*a2a.PushConfig{}
	}
	return out, nil
}

// Delete implements push.ConfigStore; deleting a missing config is not an error (spec §3.1.10). A Delete
// that removed a row sends kontra.push {configId, deleted}; its push_webhook ends when it finds the config
// gone (spec §10.2).
func (s *ConfigStore) Delete(ctx context.Context, taskID a2a.TaskID, configID string) error {
	tid, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return err
	}
	if configID == "" || !utf8.ValidString(configID) || strings.ContainsRune(configID, 0) {
		return nil
	}
	tag, err := s.db.Exec(ctx, `delete from kontra.push_configs where task_id = $1 and id = $2`, tid, configID)
	if err != nil {
		return fmt.Errorf("push: delete a config of task %s: %w", tid, err)
	}
	if tag.RowsAffected() > 0 {
		s.signalPush(ctx, tid, lifecycle.PushSignal{ConfigID: configID, Deleted: true})
	}
	return nil
}

// DeleteAll implements push.ConfigStore: every config of the task, each announced as deleted.
func (s *ConfigStore) DeleteAll(ctx context.Context, taskID a2a.TaskID) error {
	tid, err := ids.ParseTaskID(string(taskID))
	if err != nil {
		return err
	}
	rows, err := s.db.Query(ctx, `delete from kontra.push_configs where task_id = $1 returning id`, tid)
	if err != nil {
		return fmt.Errorf("push: delete the configs of task %s: %w", tid, err)
	}
	deleted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("push: delete the configs of task %s: %w", tid, err)
	}
	for _, id := range deleted {
		s.signalPush(ctx, tid, lifecycle.PushSignal{ConfigID: id, Deleted: true})
	}
	return nil
}

// LiveConfigIDs implements lifecycle.PushConfigLister: the task's configs that are not failed (spec §10.2,
// list_push_configs). A config whose terminal snapshot was delivered is listed too; its push_webhook, if
// started again, finds it delivered and ends at once.
func (s *ConfigStore) LiveConfigIDs(ctx context.Context, taskID uuid.UUID) ([]string, error) {
	rows, err := s.db.Query(ctx, `select id from kontra.push_configs where task_id = $1 and failed_at is null order by id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("push: list the live configs of task %s: %w", taskID, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("push: list the live configs of task %s: %w", taskID, err)
	}
	return out, nil
}

// signalPush sends kontra.push once the write committed, even if the caller went away meanwhile. It is
// best effort: the workflow lists the task's configs at start and after each dispatch (spec §10.2).
func (s *ConfigStore) signalPush(ctx context.Context, taskID uuid.UUID, sig lifecycle.PushSignal) {
	if s.signal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalTimeout)
	defer cancel()
	if err := s.signal.SignalPush(ctx, a2a.TaskID(taskID.String()), sig); err != nil {
		s.logger.Warn("push: kontra.push not sent; the workflow lists the configs after its next dispatch",
			"task", taskID.String(), "config", sig.ConfigID, "deleted", sig.Deleted, "error", err)
	}
}

// NoopSender is the push.Sender a2a-go's handler is built with, so push CRUD works while delivery stays
// with the lifecycle (spec §15).
type NoopSender struct{}

var _ a2apush.Sender = NoopSender{}

// SendPush implements push.Sender: it does nothing.
func (NoopSender) SendPush(ctx context.Context, config *a2a.PushConfig, event a2a.Event) error {
	return nil
}
