package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"kontra.sh/kontra/internal/login"
	"kontra.sh/kontra/internal/node"
	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/ssrf"
	"kontra.sh/kontra/internal/tunnel"
	"kontra.sh/kontra/internal/version"
)

// Name is the command, as its output names it.
const Name = "kontra publish"

// maxAnswer bounds what the control plane answers; maxToken the access token it is sent.
const (
	maxAnswer = 256 << 10
	maxToken  = 16 << 10
)

// bearerRE is a token kontra publish sends: RFC 6750's b64token.
var bearerRE = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

// ErrUsage is a command line publish refuses (exit 2).
var ErrUsage = errors.New("usage")

// Usage is the command's help.
const Usage = `usage: kontra publish [--gate <url>] [--control-plane <url>] [--version <n> | --replace] [--dry-run] [--no-browser] <listing.yaml>

kontra publish sends a listing, how one of your services is sold, to the gate operator's control plane (Kontra Cloud):
its kind (a2a or mcp), visibility, MCP policy, sealing and price, free, per_task, per_call or per_session, in USD
(spec/schema/listing.json). It checks the file first, then signs you in at the gate's authorization server for the
control plane, in the browser as kontra login does: a publish is a person's act, never an API key's, and the sign-in
it gets is used for this publish only and not kept. A listing is never replaced unseen: without --version or
--replace, kontra publish publishes only a service's first listing, and for one that has a listing it prints that
listing's version, price and state and publishes nothing. The control plane refuses a publish if another came in
between.
  --gate URL           the gate whose operator lists the service (default https://gateway.kontra.sh); https:// except
                       on localhost
  --control-plane URL  the operator's control plane (default: the gate's authorization server, which is Cloud's)
  --version N          the listing version this one replaces, as you read it (0: a new listing); refused if it is
                       not the current one
  --replace            replace whatever version is current when the publish runs, printing what it was
  --dry-run            check the file and print the request, without signing in or sending anything
  --no-browser         print the sign-in URL instead of opening it (open it on this machine)
`

// Options are a publish's.
type Options struct {
	File            string
	Gate            *url.URL
	ControlPlane    string // --control-plane; "" for the gate's authorization server
	Version         *int64 // --version; nil reads the current one
	Replace         bool   // --replace: replace the current version, whatever it is; without it (and --version) only a first listing is published
	DryRun          bool
	NoBrowser       bool
	CredentialsPath string // ~/.kontra/credentials: the client registration kontra login keeps per gate
	Client          *http.Client
	Open            func(string) error
	Out             io.Writer
	Login           login.Options // Wait, for the tests
	ReadFile        func(string) ([]byte, error)
}

// ParseArgs parses the arguments after `publish`; flags may come before or after the file. getenv reads HOME.
func ParseArgs(args []string, getenv func(string) string) (Options, error) {
	fs := flag.NewFlagSet(Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gate := fs.String("gate", tunnel.DefaultGate, "")
	cp := fs.String("control-plane", "", "")
	ver := fs.String("version", "", "")
	replace := fs.Bool("replace", false, "")
	dry := fs.Bool("dry-run", false, "")
	noBrowser := fs.Bool("no-browser", false, "")
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return Options{}, err
			}
			return Options{}, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}
	switch len(files) {
	case 0:
		return Options{}, fmt.Errorf("%w: name the listing file", ErrUsage)
	case 1:
	default:
		return Options{}, fmt.Errorf("%w: one listing file at a time, not %q", ErrUsage, tunnel.Printable(files[1]))
	}
	o := Options{File: files[0], ControlPlane: *cp, DryRun: *dry, NoBrowser: *noBrowser, Replace: *replace}
	u, err := tunnel.CheckGateURL(*gate)
	if err != nil {
		return Options{}, fmt.Errorf("%w: %v", ErrUsage, err)
	}
	o.Gate = u
	if *ver != "" {
		n, err := strconv.ParseInt(*ver, 10, 64)
		if err != nil || n < 0 {
			return Options{}, fmt.Errorf("%w: --version %q: want 0 or a listing version", ErrUsage, tunnel.Printable(*ver))
		}
		o.Version = &n
		if o.Replace {
			return Options{}, fmt.Errorf("%w: --version and --replace: name the version you read, or replace the current one, not both", ErrUsage)
		}
	}
	if o.ControlPlane != "" {
		if _, err := controlPlaneURL(o.ControlPlane); err != nil {
			return Options{}, fmt.Errorf("%w: --control-plane: %v", ErrUsage, err)
		}
	}
	home := getenv("HOME")
	if home == "" {
		if home, err = os.UserHomeDir(); err != nil || home == "" {
			return Options{}, fmt.Errorf("%w: no home directory for ~/%s/%s: set HOME", ErrUsage, nodekey.DirName, node.CredentialsFile)
		}
	}
	o.CredentialsPath = filepath.Join(home, nodekey.DirName, node.CredentialsFile)
	return o, nil
}

// Command runs `kontra publish` with args (after the subcommand). -h prints the usage on stdout.
func Command(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := ParseArgs(args, os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, Usage)
		return nil
	}
	if err != nil {
		fmt.Fprint(stderr, Usage)
		return err
	}
	o.Out = stdout
	return Run(ctx, o)
}

// controlPlaneURL checks a control plane's base URL: https:// (http:// on a loopback host), no userinfo, query or
// fragment; its routes are under it (/v1/…). It returns the URL without a trailing slash, scheme and host lower case.
func controlPlaneURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil {
		err = ssrf.CheckOperatorURL(u)
	}
	if err != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%q is not an https:// URL without a query", tunnel.Printable(raw))
	}
	origin, err := tunnel.CanonicalOrigin(u)
	if err != nil {
		return "", err
	}
	return origin + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// Run publishes o.File (D323): the file checked; then, unless DryRun, the gate's authorization server discovered,
// the person signed in for the control plane, the current listing read (unless --version), and the listing sent: the
// version --version names, the current one with --replace, or 0 when the service has no listing. A service that has
// one, with neither flag, is refused with what it is, and nothing is sent.
func Run(ctx context.Context, o Options) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	read := o.ReadFile
	if read == nil {
		read = readFile
	}
	b, err := read(o.File)
	if err != nil {
		return err
	}
	l, err := Parse(b)
	if err != nil {
		return err
	}
	if o.DryRun {
		v := int64(0)
		if o.Version != nil {
			v = *o.Version
		}
		body, err := l.Body(v)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, body, "  ", "  ")
		at := "no listing (version 0; a service that has one is refused unless --version or --replace)"
		switch {
		case o.Version != nil:
			at = "version " + strconv.FormatInt(*o.Version, 10)
		case o.Replace:
			at = "the version current at publish (--replace)"
		}
		fmt.Fprintf(o.Out, "%s: %s is a valid listing of %s; it replaces %s with:\n  PUT /v1/services/%s\n  %s\n(dry run: nothing was sent)\n",
			Name, tunnel.Printable(o.File), l.Service, at, l.Service, pretty.String())
		return nil
	}
	if o.Gate == nil || o.CredentialsPath == "" {
		return fmt.Errorf("%w: no gate or credentials path", ErrUsage)
	}
	if o.Client == nil {
		o.Client = login.NewClient(nil)
	}
	s, err := login.Discover(ctx, o.Client, o.Gate.String())
	if errors.Is(err, login.ErrNoSignIn) {
		return errors.New("this gate has no sign-in, so no control plane to publish to: its operator lists its services " +
			"(a team gate's are in its configuration, spec/relay.md §13.5)")
	}
	if err != nil {
		return err
	}
	cp := o.ControlPlane
	if cp == "" {
		cp = s.Issuer
	}
	if cp, err = controlPlaneURL(cp); err != nil {
		return fmt.Errorf("the control plane: %w", err)
	}
	lo := o.Login // its Wait
	lo.Gate, lo.CredentialsPath, lo.Client, lo.Out, lo.Name = o.Gate, o.CredentialsPath, o.Client, o.Out, Name
	lo.Open, lo.NoBrowser = o.Open, o.NoBrowser
	token, err := login.Authenticate(ctx, s, cp, lo)
	if err != nil {
		return err
	}
	if len(token) > maxToken || !bearerRE.MatchString(token) {
		return errors.New("the authorization server's access token is not a bearer token kontra publish can send")
	}
	// The control plane's own client: no redirect is followed (the token goes to the control plane only), and one is
	// answered as a refusal rather than a transport error.
	cl := *o.Client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c := &controlPlane{base: cp, token: token, client: &cl}
	var endpoint string
	ver := int64(0)
	if o.Version != nil {
		ver = *o.Version
	} else {
		cur, err := c.current(ctx, l.Service)
		if err != nil {
			return err
		}
		ver, endpoint = cur.Version, cur.Endpoint
		// D323, kontra-cloud D-M5-63 and D-M5-111: a listing is never replaced unseen. The file may predate a price set
		// in the console or an unpublish by a teammate, which the version the CLI just read cannot tell.
		switch {
		case ver > 0 && !o.Replace:
			return fmt.Errorf("%s already has a listing, at version %d: %s. Nothing was published. Look at it, then run "+
				"kontra publish --version %d %s to replace that version (or --replace to replace whatever is current then)",
				l.Service, ver, cur.describe(), ver, tunnel.Printable(o.File))
		case ver > 0:
			fmt.Fprintf(o.Out, "%s: replacing %s's listing at version %d: %s.\n", Name, l.Service, ver, cur.describe())
		}
	}
	body, err := l.Body(ver)
	if err != nil {
		return err
	}
	view, err := c.put(ctx, l.Service, body, ver)
	if err != nil {
		return err
	}
	state := "published"
	if !view.Published {
		state = "unpublished (the gate answers it as unknown; the listing is kept)"
	}
	fmt.Fprintf(o.Out, "%s: %s is %s at version %d: %s, %s, on %s.\n", Name, tunnel.Printable(view.Service), state,
		view.Version, describe(view), tunnel.Printable(view.Visibility), cp)
	if endpoint != "" && view.Published {
		fmt.Fprintf(o.Out, "  %s\n", tunnel.Printable(endpoint))
	}
	return nil
}

func readFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("the listing file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("the listing file: %w", err)
	}
	return b, nil
}

// describe is a ListingView's price as the control plane wrote it back, for the terminal.
func describe(v *ListingView) string {
	p := v.Price
	switch p.Model {
	case ModelFree, "":
		return "free"
	case ModelPerTask:
		return fmt.Sprintf("per_task %s %s", tunnel.Printable(p.Amount), tunnel.Printable(p.Currency))
	default:
		return fmt.Sprintf("%s %s %s", tunnel.Printable(p.Model), tunnel.Printable(p.Amount), tunnel.Printable(p.Currency))
	}
}

// ListingView is what PUT /v1/services/{name} answers (Cloud's ListingView), the members kontra publish prints.
type ListingView struct {
	Service    string `json:"service"`
	Version    int64  `json:"version"`
	Kind       string `json:"kind"`
	Visibility string `json:"visibility"`
	Published  bool   `json:"published"`
	Price      struct {
		Model    string `json:"model"`
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"price"`
}

// controlPlane is the operator's API, as a person's sign-in reaches it.
type controlPlane struct {
	base   string
	token  string
	client *http.Client
}

// errorBody is the control plane's answer to a refusal (Cloud's ErrorBody).
type errorBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail"`
}

// current is a service's listing as the control plane holds it before a publish: its version (0: none), what it
// sells and whether the gate serves the service.
type current struct {
	Version   int64
	Endpoint  string
	Listing   *ListingView
	Published *bool // the gate serves it (Cloud's ServiceView.published); nil when not said
}

// describe is the current listing for the terminal: its price, visibility and whether it is published.
func (c current) describe() string {
	if c.Listing == nil {
		return "no listing"
	}
	published := c.Listing.Published
	if c.Published != nil {
		published = *c.Published
	}
	state := "published"
	if !published {
		state = "unpublished"
	}
	return fmt.Sprintf("%s, %s, %s", describe(c.Listing), tunnel.Printable(c.Listing.Visibility), state)
}

// current reads the service's listing (version 0: none) and its gate URL, GET /v1/services/{name}; a service the
// control plane does not know (404 service_not_found) has none.
func (c *controlPlane) current(ctx context.Context, service string) (current, error) {
	b, status, err := c.do(ctx, http.MethodGet, service, nil)
	if err != nil {
		return current{}, err
	}
	switch status {
	case http.StatusOK:
		var v struct {
			Endpoint  *string      `json:"endpoint"`
			Published *bool        `json:"published"`
			Listing   *ListingView `json:"listing"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return current{}, fmt.Errorf("the control plane's view of %s is not one kontra publish reads: %v", service, err)
		}
		cur := current{Listing: v.Listing, Published: v.Published}
		if v.Endpoint != nil {
			cur.Endpoint = *v.Endpoint
		}
		if v.Listing != nil {
			if v.Listing.Version < 0 {
				return current{}, fmt.Errorf("the control plane's view of %s names version %d", service, v.Listing.Version)
			}
			cur.Version = v.Listing.Version
		}
		return cur, nil
	case http.StatusNotFound:
		if e := parseError(b); e.Code == "service_not_found" {
			return current{}, nil
		}
	}
	return current{}, refusal("reading the listing of "+service, status, b)
}

// put sends the listing.
func (c *controlPlane) put(ctx context.Context, service string, body []byte, ver int64) (*ListingView, error) {
	b, status, err := c.do(ctx, http.MethodPut, service, body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusOK {
		var v ListingView
		if err := json.Unmarshal(b, &v); err != nil || v.Service == "" {
			return nil, errors.New("the control plane's answer to the publish is not a listing: it may have been published; " +
				"look at the service in the console")
		}
		return &v, nil
	}
	e := parseError(b)
	if status == http.StatusConflict && e.Code == "stale_version" {
		var d struct {
			Version *int64 `json:"version"`
		}
		_ = json.Unmarshal(e.Detail, &d)
		now := "another"
		if d.Version != nil {
			now = strconv.FormatInt(*d.Version, 10)
		}
		return nil, fmt.Errorf("the listing of %s changed since version %d was read: it is at version %s now (409 stale_version). "+
			"Nothing was published. Look at it, then run kontra publish --version %s to replace it", service, ver, now, now)
	}
	return nil, refusal("the publish", status, b)
}

// do sends one request to {base}/v1/services/{name} with the person's token, the answer bounded; a redirect is an
// error (the client follows none, so the token goes nowhere else).
func (c *controlPlane) do(ctx context.Context, method, service string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/v1/services/"+url.PathEscape(service), rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.String("kontra"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, 0, fmt.Errorf("the control plane %s: %w", tunnel.Printable(c.base), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, resp.StatusCode, fmt.Errorf("the control plane answered a redirect (%d), which %s does not follow", resp.StatusCode, Name)
	}
	b, err := ssrf.ReadBody(resp.Body, maxAnswer)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("the control plane's answer: %w", err)
	}
	return b, resp.StatusCode, nil
}

func parseError(b []byte) errorBody {
	var e errorBody
	_ = json.Unmarshal(b, &e)
	return e
}

// refusal is a control plane's refusal for the terminal: its status, code and message, printable and bounded, and
// what to do about the ones a publisher can act on.
func refusal(what string, status int, b []byte) error {
	e := parseError(b)
	msg := fmt.Sprintf("%s was refused: HTTP %d", what, status)
	if e.Code != "" {
		msg += " " + tunnel.Printable(clip(e.Code))
	}
	if e.Message != "" {
		msg += ": " + tunnel.Printable(clip(e.Message))
	}
	switch {
	case status == http.StatusUnauthorized:
		msg += " (the control plane did not take this sign-in: run kontra publish again)"
	case e.Code == "fresh_login_required" || e.Code == "confirmation_required":
		msg += " (a publish needs a person's recent sign-in: run kontra publish again and sign in)"
	case status == http.StatusUnprocessableEntity:
		msg += " (nothing was published)"
	}
	return errors.New(msg)
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
