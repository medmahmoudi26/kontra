package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/strictjson"
)

// The revocation feed (spec/relay.md §2.3, D316): an issuer's list of the sessions it revoked recently, which the gate
// polls so that a token it verifies by itself stops working within a bound, not when it expires. Kontra Cloud serves
// it as GET /v1/auth/revocations: {"window_seconds": 1200, "sessions": ["<sha256(sid), 64 hex>", ...]}, every session
// revoked in the last window_seconds, newest first. The gate needs only that shape; members it does not know are
// ignored.
//
// What the gate then requires of a token of that issuer, and why:
//
//   - a sid (a string): the session the feed names it by, as sha256 of its UTF-8 bytes, in hex;
//   - an iat no more than Skew in the future, and a lifetime exp - iat of at most window_seconds - 2·Skew. A session
//     revoked at R is listed until R + window (the issuer's clock), the token was issued at iat <= R, and the gate
//     accepts it until exp + Skew (its own clock, within Skew of the issuer's): the lifetime bound is what keeps the
//     session listed for as long as the gate would accept one of its tokens, after a restart included (the gate keeps
//     no revocation of its own, only the last list it read);
//   - a feed read successfully less than MaxStale ago (measured from when that read began). Past it, every token of
//     the issuer is ErrUnavailable, the gate's own failure (503), never accepted: a revocation made since the last
//     read is unknown. So a revocation reaches every replica within MaxStale, and normally within Poll.

// Bounds of the feed (D316).
const (
	// MaxFeedSessions bounds the sessions one answer lists: above Cloud's own cap (200,000 in its window), so a full
	// list is read and never refused for its size.
	MaxFeedSessions = 1 << 18
	// MaxFeedBody bounds an answer's body: MaxFeedSessions entries of 67 bytes ("<64 hex>",) with room for white space.
	MaxFeedBody = 32 << 20
	// FeedTimeout bounds one read, from the dial to the last byte of the body.
	FeedTimeout = 10 * time.Second
	// MaxFeedWindow bounds window_seconds (30 days): a longer window is not a feed the gate understands.
	MaxFeedWindow = 30 * 24 * time.Hour
	// MaxSessionID bounds a token's sid in bytes.
	MaxSessionID = 255
)

// FeedConfig configures a Feed.
type FeedConfig struct {
	Issuer string // the issuer whose sessions the feed lists (logs and metrics)
	URL    string // the feed (auth.<block>.revocations.url), an operator-configured endpoint (§10.3)
	// Poll is how often the feed is read; MaxStale how old the last successful read may be before every token of the
	// issuer is refused. Both positive, MaxStale at least Poll (config validates 2·Poll).
	Poll, MaxStale time.Duration
	// Client reads the feed: §10.3's operator-configured endpoint client (D284). Nil is ssrf.NewOperatorClient with
	// the system roots and FeedTimeout.
	Client *http.Client
	Logger *slog.Logger
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
}

// Feed is an issuer's revocation feed as a replica last read it. It is safe for concurrent use; one read runs at a
// time.
type Feed struct {
	cfg FeedConfig
	log *slog.Logger

	readMu sync.Mutex // one read at a time

	mu      sync.RWMutex
	revoked map[[32]byte]struct{} // the last list read
	window  time.Duration         // its window_seconds
	readAt  time.Time             // when the last successful read began; zero before any
	read    chan struct{}         // closed by the first successful read
	once    sync.Once
	failing bool // the last read failed (to log a recovery once)
}

// NewFeed checks cfg. It reads nothing: Run (or Refresh) does.
func NewFeed(cfg FeedConfig) (*Feed, error) {
	u, err := url.Parse(cfg.URL)
	if err == nil {
		err = ssrf.CheckOperatorURL(u)
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: revocations url: %w", err)
	}
	if cfg.Poll <= 0 || cfg.MaxStale < cfg.Poll {
		return nil, errors.New("oidc: revocations: poll must be positive and max_stale at least poll")
	}
	if cfg.Client == nil {
		cfg.Client = ssrf.NewOperatorClient(ssrf.OperatorOptions{Timeout: FeedTimeout})
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Feed{cfg: cfg, log: cfg.Logger.With("component", "oidc", "issuer", cfg.Issuer), read: make(chan struct{})}, nil
}

// URL is the feed's URL.
func (f *Feed) URL() string { return f.cfg.URL }

// Issuer is the issuer whose sessions the feed lists.
func (f *Feed) Issuer() string { return f.cfg.Issuer }

// MaxStale is how old the last successful read may be.
func (f *Feed) MaxStale() time.Duration { return f.cfg.MaxStale }

// Run reads the feed at once and then every Poll until ctx ends. A failed read is logged and tried again at the next
// tick; the tokens of the issuer fail closed once no read has succeeded for MaxStale. Run returns nil when ctx ends.
func (f *Feed) Run(ctx context.Context) error {
	t := time.NewTicker(f.cfg.Poll)
	defer t.Stop()
	for {
		if err := f.Refresh(ctx); err != nil && ctx.Err() == nil {
			f.mu.Lock()
			first := !f.failing
			f.failing = true
			f.mu.Unlock()
			level := slog.LevelDebug
			if first {
				level = slog.LevelWarn
			}
			age, ok := f.Age()
			f.log.Log(ctx, level, "the revocation feed could not be read; the issuer's tokens are refused once it is stale",
				"url", f.cfg.URL, "error", err, "age", age, "read", ok, "max_stale", f.cfg.MaxStale)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Wait waits until the feed has been read once, or ctx ends; it reports whether it has.
func (f *Feed) Wait(ctx context.Context) bool {
	select {
	case <-f.read:
		return true
	case <-ctx.Done():
		return false
	}
}

// Age is how long ago the last successful read began; ok is false before any.
func (f *Feed) Age() (age time.Duration, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.readAt.IsZero() {
		return 0, false
	}
	return f.cfg.Now().Sub(f.readAt), true
}

// Fresh reports whether the last successful read began less than MaxStale ago.
func (f *Feed) Fresh() bool {
	age, ok := f.Age()
	return ok && age < f.cfg.MaxStale
}

// Refresh reads the feed once and, when the answer is a feed, replaces the list held. A failed read keeps the list it
// had: its age still counts from the last read that succeeded.
func (f *Feed) Refresh(ctx context.Context) error {
	f.readMu.Lock()
	defer f.readMu.Unlock()
	started := f.cfg.Now()
	ctx, cancel := context.WithTimeout(ctx, FeedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.cfg.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d (want 200; redirects are not followed, relay.md §10.3)", resp.StatusCode)
	}
	b, err := ssrf.ReadBody(resp.Body, MaxFeedBody)
	if err != nil {
		return err
	}
	set, window, err := ParseFeed(b)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.revoked, f.window, f.readAt = set, window, started
	recovered := f.failing
	f.failing = false
	f.mu.Unlock()
	f.once.Do(func() { close(f.read) })
	if recovered {
		f.log.Info("the revocation feed is read again", "url", f.cfg.URL, "sessions", len(set))
	}
	if window <= 2*Skew {
		f.log.Warn("the revocation feed's window is no longer than twice the skew: every token of the issuer is refused",
			"window", window, "skew", Skew)
	}
	return nil
}

// ParseFeed reads a feed's answer: a JSON object (no repeated or case-variant key) whose window_seconds is an integer
// from 1 to 30 days and whose sessions is an array of at most MaxFeedSessions sha256 digests of 64 hex digits. Other
// members are ignored. One malformed entry makes the whole answer unusable: the gate cannot tell what it would miss.
func ParseFeed(b []byte) (map[[32]byte]struct{}, time.Duration, error) {
	m, err := strictjson.Members(b)
	if err != nil {
		return nil, 0, fmt.Errorf("not a revocation feed: %w", err)
	}
	raw, ok := m["window_seconds"]
	if !ok {
		return nil, 0, errors.New("not a revocation feed: no window_seconds")
	}
	var w float64
	if err := json.Unmarshal(raw, &w); err != nil || w != math.Trunc(w) || w < 1 || w > MaxFeedWindow.Seconds() {
		return nil, 0, fmt.Errorf("window_seconds %s: want an integer from 1 to %d", strictjson.Quote(string(raw)), int64(MaxFeedWindow.Seconds()))
	}
	raw, ok = m["sessions"]
	if !ok || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		return nil, 0, errors.New("not a revocation feed: sessions is not an array")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, 0, fmt.Errorf("sessions: %w", err)
	}
	if len(list) > MaxFeedSessions {
		return nil, 0, fmt.Errorf("sessions: %d entries, more than %d", len(list), MaxFeedSessions)
	}
	set := make(map[[32]byte]struct{}, len(list))
	for i, e := range list {
		var s string
		if err := json.Unmarshal(e, &s); err != nil || !strings.HasPrefix(string(e), `"`) || len(s) != 2*sha256.Size {
			return nil, 0, fmt.Errorf("sessions[%d]: want a sha256 digest of 64 hex digits", i)
		}
		var d [32]byte
		if _, err := hex.Decode(d[:], []byte(s)); err != nil {
			return nil, 0, fmt.Errorf("sessions[%d]: want a sha256 digest of 64 hex digits", i)
		}
		set[d] = struct{}{}
	}
	return set, time.Duration(w) * time.Second, nil
}

// SessionDigest is how the feed names a session: sha256 of its sid's UTF-8 bytes.
func SessionDigest(sid string) [32]byte { return sha256.Sum256([]byte(sid)) }

// errRevoked and errLifetime are the reasons check refuses a token (logged, never sent).
var (
	errRevoked  = errors.New("its session is on the revocation feed")
	errLifetime = errors.New("it lives longer than the revocation feed's window allows (exp - iat > window_seconds - 2·skew)")
)

// check decides a verified token of the issuer against the list last read: errRevoked or errLifetime for a token the
// gate refuses (a 401), ErrUnavailable for a feed never read or stale (the gate's own failure), nil to accept it.
func (f *Feed) check(sid string, iat, exp time.Time) error {
	d := SessionDigest(sid)
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.readAt.IsZero() {
		return fmt.Errorf("%w: the revocation feed has not been read yet", ErrUnavailable)
	}
	if _, ok := f.revoked[d]; ok {
		return errRevoked
	}
	if exp.Sub(iat) > f.window-2*Skew {
		return errLifetime
	}
	if age := f.cfg.Now().Sub(f.readAt); age >= f.cfg.MaxStale {
		return fmt.Errorf("%w: the revocation feed was last read %s ago (max_stale %s)", ErrUnavailable, age.Round(time.Second), f.cfg.MaxStale)
	}
	return nil
}
