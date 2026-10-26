// Package node is `kontra connect` (spec/tunnel.md; note 2 §1–§2): one outbound WebSocket to a gate the
// node chose, over which the gate sends HTTP requests to services on the node's localhost (hosting) and the
// node sends local clients' requests to the gate (hiring).
//
//	kontra connect [--gate URL] [--serve a2a:<name>=<url> …] [--key PATH] [--enroll TOKEN|@file|-]…
//	               [--api-key KEY|@file|-] [--name NAME] [--rotate] [--require-channel-binding]
//	               [--max-inflight 64] [--listen 127.0.0.1:7777] [--local-token FILE] [--allow-origin ORIGIN]…
//	               [--log json]
//
// Hosting (§8): the node is a fixed-target reverse proxy, not a SOCKS proxy (§8.2, sidecar_not_a_proxy): a
// stream's preamble names a hosted service, and its one request goes to that service's --serve target and
// nowhere else. The kontra-blob: parts of an A2A request are fetched on blob streams inside the node's own
// tunnel and inlined (§8.3); a URL taken from a part is never dialed (D56).
//
// Hiring (§9, internal/hiring): the node serves --listen (default 127.0.0.1:7777) to local clients and forwards
// each request on a stream it opens on its current session, never on a draining one (§13). A node with no
// --serve only hires; its key is admitted on its --api-key (hire scope) alone.
//
// MCP hosting, `mcp:<name>=<url>` and `mcp:<name>=stdio:<cmd>` (§10), is M3 WP-05; sealing and per-job keys
// (§2.3) M3 WP-08; anonymous sessions and `--public` (§6) M4. There is no local console (D164).
//
// The node holds no task state: after any close but `revoked` it redials with the same key, backing off from 1 s
// to 60 s with full jitter, reset after a session that lived 60 s (§13).
//
// Environment. KONTRA_KEY (the key file, as --key, §2.1) is the only variable kontra connect reads, with
// HOME for ~/.kontra. The PRD's KONTRA_GATE, KONTRA_AGENT, KONTRA_TO and KONTRA_ENROLL (PRD §6) mirrored
// the flags of `kontra tunnel`, which note 1 replaced with kontra connect; tunnel.md, the authority, names
// none of them, so they are not honoured (M2 plan audit, the PRD's environment mirrors): a container passes
// flags, as note 2's compose sidecar does, and a secret as --enroll @/run/secrets/<file>.
//
// Work packages: M2 WP-03 (dial.go: the dial and the node's half of the handshake), M2 WP-06 (the rest), M3
// WP-03 (hiring, blob streams).
package node

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hashicorp/yamux"

	"kontra.sh/kontra/internal/hiring"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/tunnel"
)

// Config is a parsed `kontra connect` command line.
type Config struct {
	Gate                  *url.URL // --gate, default tunnel.DefaultGate; https:// except localhost (tunnel.CheckGateURL)
	Serves                []Serve  // --serve, repeatable
	KeyPath               string   // --key, $KONTRA_KEY, ~/.kontra/identity.key
	Enroll                []string // --enroll tokens, each a value, @file or - (stdin), already read
	APIKey                string   // --api-key: a value, @file or -, already read
	Name                  string   // --name, default the hostname; 1–64 characters
	Rotate                bool     // --rotate (§12)
	RequireChannelBinding bool     // --require-channel-binding: refuse cb none (§4.1)
	MaxInflight           int      // --max-inflight, default tunnel.NodeMaxInflight
	Listen                string   // --listen, default hiring.DefaultListen (§9)
	LocalToken            string   // --local-token <file>, read: required when Listen is not loopback
	AllowOrigins          []string // --allow-origin, repeatable
	LogJSON               bool     // --log json; human-readable on stderr otherwise
	CredentialsPath       string   // ~/.kontra/credentials (§2.4)
	Version               string   // for hello.agent and the User-Agent
	// Out receives the welcome's service URLs (the CLI's stdout); nil discards them.
	Out io.Writer
}

// UsageError is a command line ParseArgs refused; it wraps flag.ErrHelp for -h.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

func usage(format string, args ...any) error {
	return &UsageError{Err: fmt.Errorf(format, args...)}
}

// hostname is --name's default; tests replace it.
var hostname = os.Hostname

// ParseArgs parses the arguments after `connect`; getenv reads KONTRA_KEY, and HOME for ~/.kontra.
// Secrets given as @file or - are read here, so that a bad file is a usage error before anything starts;
// stdin can be read by one flag only.
func ParseArgs(args []string, getenv func(string) string) (Config, error) {
	return parseArgs(args, getenv, os.Stdin)
}

func parseArgs(args []string, getenv func(string) string, stdin io.Reader) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fs := flag.NewFlagSet("kontra connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		serves, enrolls, origins []string
		apiKey                   *string
	)
	gate := fs.String("gate", tunnel.DefaultGate, "")
	key := fs.String("key", "", "")
	name := fs.String("name", "", "")
	rotate := fs.Bool("rotate", false, "")
	requireCB := fs.Bool("require-channel-binding", false, "")
	maxInflight := fs.Int("max-inflight", tunnel.NodeMaxInflight, "")
	logFormat := fs.String("log", "text", "")
	listen := fs.String("listen", hiring.DefaultListen, "")
	localToken := fs.String("local-token", "", "")
	fs.Func("serve", "", func(v string) error { serves = append(serves, v); return nil })
	fs.Func("allow-origin", "", func(v string) error { origins = append(origins, v); return nil })
	fs.Func("enroll", "", func(v string) error { enrolls = append(enrolls, v); return nil })
	fs.Func("api-key", "", func(v string) error {
		if apiKey != nil {
			return errors.New("given twice")
		}
		apiKey = &v
		return nil
	})
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, &UsageError{Err: flag.ErrHelp}
		}
		return Config{}, &UsageError{Err: err}
	}
	if fs.NArg() > 0 {
		return Config{}, usage("unexpected argument %q", clipValue(fs.Arg(0)))
	}

	var cfg Config
	u, err := tunnel.CheckGateURL(*gate)
	if err != nil {
		return Config{}, &UsageError{Err: err}
	}
	cfg.Gate = u

	// No --serve: the node only hires (§4: a hello may declare no services; its key is admitted on its
	// --api-key alone).
	seen := map[string]bool{}
	for _, v := range serves {
		s, err := ParseServe(v)
		if err != nil {
			return Config{}, &UsageError{Err: err}
		}
		if seen[s.Name] {
			return Config{}, usage("--serve: the service %q is named twice", s.Name)
		}
		seen[s.Name] = true
		cfg.Serves = append(cfg.Serves, s)
	}

	stdinUsed := false
	secret := func(flagName, v string) (string, error) {
		if v == "-" {
			if stdinUsed {
				return "", usage("%s -: standard input is already read by another flag", flagName)
			}
			stdinUsed = true
		}
		s, err := readSecret(v, stdin)
		if err != nil {
			return "", usage("%s: %v", flagName, err)
		}
		return s, nil
	}
	for _, v := range enrolls {
		t, err := secret("--enroll", v)
		if err != nil {
			return Config{}, err
		}
		if !tokenRE.MatchString(t) {
			return Config{}, usage("--enroll: not an enrollment token (ket_ and 32 letters or digits, from `kontra-gate enroll <service>`)")
		}
		if !slices.Contains(cfg.Enroll, t) {
			cfg.Enroll = append(cfg.Enroll, t)
		}
	}
	if apiKey != nil {
		k, err := secret("--api-key", *apiKey)
		if err != nil {
			return Config{}, err
		}
		cfg.APIKey = k
	}

	if cfg.KeyPath, err = nodekey.Resolve(*key, getenv); err != nil {
		return Config{}, &UsageError{Err: err}
	}
	home := getenv("HOME")
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil || home == "" {
			return Config{}, usage("no home directory for ~/%s/%s: set HOME", nodekey.DirName, CredentialsFile)
		}
	}
	cfg.CredentialsPath = filepath.Join(home, nodekey.DirName, CredentialsFile)

	if *name == "" {
		if cfg.Name, err = defaultName(); err != nil {
			return Config{}, &UsageError{Err: err}
		}
	} else {
		cfg.Name = *name
	}
	if err := checkName(cfg.Name); err != nil {
		return Config{}, &UsageError{Err: err}
	}
	cfg.Rotate = *rotate
	cfg.RequireChannelBinding = *requireCB
	if *maxInflight < 1 {
		return Config{}, usage("--max-inflight %d: want 1 or more", *maxInflight)
	}
	cfg.MaxInflight = *maxInflight
	if *localToken != "" {
		if cfg.LocalToken, err = hiring.ReadLocalToken(*localToken); err != nil {
			return Config{}, &UsageError{Err: err}
		}
	}
	if err := hiring.CheckListen(*listen, cfg.LocalToken); err != nil {
		return Config{}, &UsageError{Err: err}
	}
	cfg.Listen = *listen
	if _, err := hiring.New(hiring.Config{Listen: cfg.Listen, LocalToken: cfg.LocalToken, AllowOrigins: origins, Gate: cfg.Gate},
		noOpener{}, nil); err != nil {
		return Config{}, &UsageError{Err: err}
	}
	cfg.AllowOrigins = origins
	switch *logFormat {
	case "json":
		cfg.LogJSON = true
	case "text":
	default:
		return Config{}, usage("--log %q: want json or text", clipValue(*logFormat))
	}
	return cfg, nil
}

// defaultName is --name's default: the hostname, cut to its first label, or to 64 characters, when it is
// longer than hello.name allows (§4: 1–64 characters; the gate would refuse a longer one with a code the
// node retries forever, WP-01's SPEC-CONFLICT on ParseHello).
func defaultName() (string, error) {
	h, err := hostname()
	if err != nil || h == "" {
		return "", errors.New("--name: the hostname is unknown; pass --name")
	}
	if utf8.RuneCountInString(h) > tunnel.MaxNodeName {
		if label, _, ok := strings.Cut(h, "."); ok && label != "" && utf8.RuneCountInString(label) <= tunnel.MaxNodeName {
			h = label
		} else {
			h = string([]rune(h)[:tunnel.MaxNodeName])
		}
	}
	return h, nil
}

// checkName is hello.name's rule (§4): 1–64 characters; the node also refuses control characters, which
// the operator's view of nodes would show (the gate refuses a NUL with a code the node retries forever).
func checkName(name string) error {
	if !utf8.ValidString(name) {
		return errors.New("--name: not UTF-8")
	}
	if n := utf8.RuneCountInString(name); n < 1 || n > tunnel.MaxNodeName {
		return fmt.Errorf("--name of %d characters: want 1 to %d", n, tunnel.MaxNodeName)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("--name: a control character")
	}
	return nil
}

// Node is one running `kontra connect`.
type Node struct {
	cfg    Config
	logger *slog.Logger // component=node
	out    io.Writer
	origin string // the canonical origin of --gate: what credentials are kept under
	dialer Dialer
	proxy  *Proxy // the targets, whose transports the sessions' proxies share
	now    func() time.Time
	// muxConfig is the sessions' yamux configuration (tests shorten it); nil is tunnel.YamuxConfig.
	muxConfig     *yamux.Config
	maintainEvery time.Duration
	backoff       Backoff

	creds  *Credentials
	credMu sync.Mutex // serializes updateCredential

	mu        sync.Mutex
	cred      Credential // this origin's, as last read or written
	declared  []Serve    // --serve, less the services whose binding the gate revoked (§12)
	activeKey string     // the recipient of the identity's active key at the last dial
	unsure    bool       // a hello that carried tokens or rotate_from may have been admitted: probe first
	// skipRotation is the recipient of a rotated key this process no longer names in rotate_from at this
	// gate: the gate refused it (enroll_invalid or revoked), or welcomed the new key while the key file's
	// stamp could not be written (connect's SPEC-CONFLICT on §12).
	skipRotation string
	sessions     map[*session]struct{}
	// current is the session hiring streams go on (setCurrent); currentCh is closed whenever it changes.
	current    *session
	currentCh  chan struct{}
	hire       *hiring.Server
	lastPrune  time.Time
	revoked    chan struct{} // closed when the gate revoked the active key
	revokeOnce sync.Once
}

// New loads (or creates) the identity key, refuses a group- or other-readable key file, resolves the
// credentials for the gate's origin, and checks the --serve targets.
//
// It merges the command line's credentials into ~/.kontra/credentials under the gate's origin (§2.4): an
// --api-key replaces the stored one, and an --enroll token is kept, for this node's key, until a welcome
// confirms it, unless a welcome already did (its sha256 is in Consumed), in which case it is not sent again
// (M2 plan audit, gap 1). With --rotate it writes a new first line to the key file (nodekey.Rotate); it
// refuses a rotation of a key it has just created, one while an earlier rotation is still unconfirmed by
// this gate (its rotate_from would name a key the gate never saw), and one while an earlier enrollment of
// the key at this gate is unconfirmed (the lost-welcome probe needs the key that hello was made with).
// Decrypt-only keys whose rotation every confirming gate stamped 24 h ago or more are pruned (§12).
func New(cfg Config, logger *slog.Logger) (*Node, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Gate == nil {
		return nil, errors.New("node: no --gate")
	}
	gate, err := tunnel.CheckGateURL(cfg.Gate.String())
	if err != nil {
		return nil, err
	}
	cfg.Gate = gate
	origin, err := tunnel.CanonicalOrigin(gate)
	if err != nil {
		return nil, fmt.Errorf("--gate: %w", err)
	}
	seen := map[string]bool{}
	for _, s := range cfg.Serves {
		if err := checkServe(s); err != nil {
			return nil, err
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("--serve: the service %q is named twice", s.Name)
		}
		seen[s.Name] = true
	}
	if err := checkName(cfg.Name); err != nil {
		return nil, err
	}
	for _, t := range cfg.Enroll {
		if !tokenRE.MatchString(t) {
			return nil, errors.New("--enroll: not an enrollment token")
		}
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = tunnel.NodeMaxInflight
	}
	if cfg.KeyPath == "" || cfg.CredentialsPath == "" {
		return nil, errors.New("node: no key or credentials path")
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	log := logger.With("component", "node", "gate", origin)
	n := &Node{
		cfg:           cfg,
		logger:        log,
		out:           out,
		origin:        origin,
		dialer:        Dialer{UserAgent: UserAgent(cfg.Version)},
		proxy:         NewProxy(cfg.Serves, cfg.MaxInflight, log),
		now:           time.Now,
		maintainEvery: maintainEvery,
		declared:      slices.Clone(cfg.Serves),
		sessions:      map[*session]struct{}{},
		currentCh:     make(chan struct{}),
		revoked:       make(chan struct{}),
	}
	if cfg.Listen == "" {
		cfg.Listen = hiring.DefaultListen
		n.cfg.Listen = cfg.Listen
	}
	if n.hire, err = hiring.New(hiring.Config{Listen: cfg.Listen, LocalToken: cfg.LocalToken, AllowOrigins: cfg.AllowOrigins, Gate: gate},
		n, log); err != nil {
		return nil, err
	}
	now := n.now()

	// The identity (§2.1).
	id, created, err := nodekey.LoadOrCreate(cfg.KeyPath, now)
	if err != nil {
		return nil, err
	}
	if created {
		log.Info("created the node's identity key", "path", cfg.KeyPath, "key_id", id.KeyID())
	}
	kid := id.KeyID()

	// Credentials (§2.4): the tokens are this key's.
	creds, err := LoadCredentials(cfg.CredentialsPath)
	if err != nil {
		return nil, err
	}
	n.creds = creds
	n.cred = creds.For(origin)
	_, pending := id.PendingRotation(origin)
	if cfg.Rotate {
		switch {
		case created:
			return nil, fmt.Errorf("--rotate: %s was just created; there is no enrolled key to rotate", cfg.KeyPath)
		case pending:
			return nil, fmt.Errorf("--rotate: the previous rotation of %s is not confirmed by %s yet; run kontra connect "+
				"without --rotate first, so that the gate confirms it", cfg.KeyPath, origin)
		case len(n.cred.Key(kid).Unconfirmed) > 0:
			return nil, fmt.Errorf("--rotate: an enrollment at %s is not confirmed yet; run kontra connect without --rotate first", origin)
		}
	}
	for _, t := range cfg.Enroll {
		if n.cred.consumed(t) {
			log.Info("an --enroll token this node already enrolled with at this gate is not sent again", "enroll_sha256", TokenHash(t)[:12])
		}
	}
	if err := n.updateCredential(func(c *Credential) {
		if cfg.APIKey != "" {
			c.APIKey = cfg.APIKey
		}
		k := c.key(kid)
		for _, t := range cfg.Enroll {
			if !c.consumed(t) && !slices.Contains(k.Enroll, t) {
				k.Enroll = append(k.Enroll, t)
			}
		}
	}); err != nil {
		return nil, err
	}
	if cfg.Rotate {
		if id, err = nodekey.Rotate(cfg.KeyPath, now); err != nil {
			return nil, err
		}
		// The tokens waiting under the old key move to the new one at the first dial (connect).
		log.Info("rotated the node's identity key: the next hello names the old key in rotate_from (§12)",
			"key_id", id.KeyID(), "old_key_id", kid)
	} else if pending || len(n.cred.Key(kid).Unconfirmed) > 0 {
		// A hello with rotate_from or tokens may have been admitted before this process started (§12 with
		// §4; the audit's lost-welcome protocol): the first hello is the probe.
		n.unsure = true
	}
	n.activeKey = id.Recipient()
	n.prune(now)
	return n, nil
}

// prune drops the key file's decrypt-only keys whose rotation is 24 h old at every gate that confirmed it
// (nodekey.Prune, §12).
func (n *Node) prune(now time.Time) {
	n.lastPrune = now
	changed, err := nodekey.Prune(n.cfg.KeyPath, now)
	switch {
	case err != nil:
		n.logger.Warn("the key file's rotated keys were not pruned", "err", err)
	case changed:
		n.logger.Info("pruned a rotated key whose 24 h overlap has passed (§12)", "path", n.cfg.KeyPath)
	}
}

// declaredServices is the hello's services: --serve less the services whose binding was revoked.
func (n *Node) declaredServices() []tunnel.Service {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]tunnel.Service, 0, len(n.declared))
	for _, s := range n.declared {
		out = append(out, tunnel.Service{Kind: s.Kind, Name: s.Name})
	}
	return out
}

// dropService stops declaring and serving a service whose binding the gate revoked ({"t": "revoked",
// "service"}, §12): its preambles are refused not_hosted on every session from now on, and the next hello
// leaves it out, until the operator changes --serve (a restart).
func (n *Node) dropService(name string) {
	n.mu.Lock()
	before := len(n.declared)
	n.declared = slices.DeleteFunc(n.declared, func(s Serve) bool { return s.Name == name })
	dropped := len(n.declared) != before
	sessions := make([]*session, 0, len(n.sessions))
	for s := range n.sessions {
		sessions = append(sessions, s)
	}
	n.mu.Unlock()
	for _, s := range sessions {
		s.proxy.Drop(name)
	}
	if dropped {
		n.logger.Warn("the gate revoked this node's binding to a service: it is no longer served or declared, "+
			"until --serve changes", "service", name)
	}
}
