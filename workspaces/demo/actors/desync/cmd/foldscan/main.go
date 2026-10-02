// Command foldscan detects CR/LF injection reachable through lossy Unicode narrowing.
//
// It follows the http-terminator pipeline, minus the Burp dependency, so each stage maps
// 1:1 onto a kontra actor later:
//
//	seeker        (not implemented here — the corpus analysis WAS the seeker run)
//	   |
//	flamer        generate the vector space          stdin: -            stdout: vector/v1
//	   |
//	validator     probe a target, record signals     stdin: target/v1    stdout: observation/v1
//	   |
//	investigator  confirm a hit with a canary        stdin: observation  stdout: confirmation/v1
//
// Every stage is a line-oriented JSON filter, so they compose with a pipe today and
// become four actors over a dataset tomorrow without changing the contracts.
//
// SAFETY. The validator and investigator send DETECTION probes only: a path that folds
// to a newline, and at most one injected header. Neither completes a smuggled request,
// so neither can poison a response queue or capture another user's traffic. Turning a
// confirmed finding into that proof is a manual, per-target act — not something a
// scanner should ever do across a scope. Default pacing is one request per second per
// host, per the paper's own guidance.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/medmahmoudi26/kontra-actors/go/desync/detect"
	"github.com/medmahmoudi26/kontra-actors/go/desync/inject"
	"github.com/medmahmoudi26/kontra-actors/go/desync/probe"
	"github.com/medmahmoudi26/kontra-actors/go/desync/unit"
	"github.com/medmahmoudi26/kontra-actors/go/desync/vectors"
)

const usage = `foldscan — CR/LF injection via lossy Unicode narrowing

USAGE
  foldscan seed         -u URL                                          > unit.jsonl
  foldscan points                                                       < units.jsonl
  foldscan flamer       [-tier 1|2|3] [-class lf,cr] [-only ID,...]
  foldscan validator    [-tier N] [-repeat N] [-rate DUR] [-only ID,...] < targets.jsonl
  foldscan investigator [-rate DUR]                                      < observations.jsonl
  foldscan summary                                                       < observations.jsonl
  foldscan schema

INPUT  (validator)   foldscan.unit/v1 — one crawled exchange:
                     {"url":"https://host/path",
                      "request": {"raw":"<wire bytes>"},
                      "response":{"raw":"<wire bytes>"}}
                     The RESPONSE is not optional: it names injection points the
                     request never mentions (Access-Control-Allow-Headers, Vary,
                     Set-Cookie, proxy-set headers, echoed values).
OUTPUT (points)      foldscan.point/v1
OUTPUT (flamer)      foldscan.vector/v1
OUTPUT (validator)   foldscan.observation/v1
OUTPUT (investigator) foldscan.confirmation/v1

EXAMPLE
  foldscan seed -u https://example.com/ \
    | tee unit.jsonl \
    | foldscan validator -tier 1 \
    | tee obs.jsonl \
    | foldscan investigator > confirmed.jsonl

  foldscan seed -u https://example.com/ | foldscan points   # what would be probed
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "flamer":
		err = cmdFlamer(os.Args[2:])
	case "validator":
		err = cmdValidator(os.Args[2:])
	case "seed":
		err = cmdSeed(os.Args[2:])
	case "points":
		err = cmdPoints(os.Args[2:])
	case "investigator":
		err = cmdInvestigator(os.Args[2:])
	case "summary":
		err = cmdSummary(os.Args[2:])
	case "schema":
		fmt.Print(schemaDoc)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------------- flamer

func cmdFlamer(args []string) error {
	fs := flag.NewFlagSet("flamer", flag.ExitOnError)
	tier := fs.Int("tier", vectors.TierQuick, "1=quick 2=+3-byte 3=+astral")
	class := fs.String("class", "lf,cr", "control chars to target")
	only := fs.String("only", "", "comma-separated vector IDs or encodings")
	_ = fs.Parse(args)

	vecs, err := selectVectors(*tier, *class, *only)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	for _, v := range vecs {
		out := struct {
			Schema string `json:"schema"`
			vectors.Vector
		}{"foldscan.vector/v1", v}
		if err := enc.Encode(out); err != nil {
			return err
		}
	}
	return nil
}

// selectVectors is the actor's selection, spelled the same way. The FOLDSCAN doc promises a
// vector debugged here is the vector the fleet sends, and these two copies had drifted on class
// parsing AND on `--only` case sensitivity. The class half is now `vectors.ParseClasses`, shared;
// the `only` half is upper-cased on both sides in both copies.
func selectVectors(tier int, class, only string) ([]vectors.Vector, error) {
	classes, err := vectors.ParseClasses(class)
	if err != nil {
		return nil, err
	}
	all := vectors.Generate(tier, classes)
	if strings.TrimSpace(only) == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, o := range strings.Split(only, ",") {
		if o = strings.ToUpper(strings.TrimSpace(o)); o != "" {
			want[o] = true
		}
	}
	var out []vectors.Vector
	for _, v := range all {
		if want[strings.ToUpper(v.ID)] || want[strings.ToUpper(v.Encoded)] {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--only %q selected no vector at tier %d, class %q — nothing would be sent", only, tier, class)
	}
	return out, nil
}

// -------------------------------------------------------------------- seed

// cmdSeed turns a bare URL into a unit by fetching it once. In production the crawler
// supplies units; this exists so the tool is usable without one, and so a unit can be
// refreshed when a target's response headers change (they name the injection points, so
// a stale unit scans the wrong places).
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	url := fs.String("u", "", "URL to seed from (otherwise reads {\"url\":...} lines on stdin)")
	timeout := fs.Duration("timeout", 10*time.Second, "request timeout")
	_ = fs.Parse(args)

	enc := json.NewEncoder(os.Stdout)
	seed := func(u unit.Exchange) error {
		po := probe.DefaultOptions()
		po.KeepRawResponses = true
		po.ReadTimeout, po.DialTimeout = *timeout, *timeout
		pt := probe.Target{Host: u.Hostname(), Port: u.Port(), Scheme: u.Scheme()}
		raw := inject.Apply(u, inject.Point{}, "")
		obs := probe.Run(context.Background(), pt, []probe.Step{{Label: "seed", Raw: raw}}, po)
		if obs.DialErr != "" {
			return fmt.Errorf("%s: dial: %s", u.URL, obs.DialErr)
		}
		if obs.TLSErr != "" {
			return fmt.Errorf("%s: tls: %s", u.URL, obs.TLSErr)
		}
		u.Request = unit.Message{Raw: string(raw)}
		u.Response = unit.Message{Raw: string(obs.Steps[0].RespRaw)}
		if err := u.Normalize(); err != nil {
			return err
		}
		// Re-emit as raw wire text on both halves: that is what a crawler hands over,
		// and round-tripping through it here proves the parser accepts its own output.
		return enc.Encode(u)
	}

	if *url != "" {
		u := unit.Exchange{URL: *url}
		if err := u.Normalize(); err != nil {
			return err
		}
		return seed(u)
	}
	return eachUnit(seed)
}

// ------------------------------------------------------------------- points

func cmdPoints(args []string) error {
	fs := flag.NewFlagSet("points", flag.ExitOnError)
	_ = fs.Parse(args)
	enc := json.NewEncoder(os.Stdout)
	return eachUnit(func(u unit.Exchange) error {
		for _, p := range inject.Enumerate(u, inject.DefaultOptions()) {
			out := struct {
				Schema string `json:"schema"`
				UnitID string `json:"unit_id,omitempty"`
				URL    string `json:"url"`
				inject.Point
			}{"foldscan.point/v1", u.ID, u.URL, p}
			if err := enc.Encode(out); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------- validator

func cmdValidator(args []string) error {
	fs := flag.NewFlagSet("validator", flag.ExitOnError)
	tier := fs.Int("tier", vectors.TierQuick, "vector tier")
	class := fs.String("class", "lf,cr", "control chars to target")
	only := fs.String("only", "", "restrict to these vector IDs/encodings")
	points := fs.String("points", "", "restrict to these point IDs (substring match)")
	maxPoints := fs.Int("max-points", 40, "cap on injection points per unit")
	screen := fs.String("screen", "raw-lf,fold-lf-u070a", "vectors used to screen each point")
	noScreen := fs.Bool("no-screen", false, "skip screening; sweep every vector at every point")
	repeat := fs.Int("repeat", 3, "repeats per sample (>=2 enables the contamination oracle)")
	screenRepeat := fs.Int("screen-repeat", 2, "repeats per screening sample")
	rate := fs.Duration("rate", time.Second, "minimum gap between requests to one host")
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	quiet := fs.Bool("quiet", false, "suppress progress on stderr")
	_ = fs.Parse(args)

	o := detect.DefaultOptions()
	o.Rate, o.Timeout = *rate, *timeout
	sc := detect.New(o)
	vecs, err := selectVectors(*tier, *class, *only)
	if err != nil {
		return err
	}
	// An empty --screen means "screen with the whole selection"; a --screen that names something
	// and matches nothing is a typo, and the old fallback turned it into a full-corpus screen.
	screenVecs := vecs
	if strings.TrimSpace(*screen) != "" {
		if screenVecs, err = selectVectors(*tier, *class, *screen); err != nil {
			return fmt.Errorf("--screen: %w", err)
		}
	}

	ctx := context.Background()
	enc := json.NewEncoder(os.Stdout)

	return eachUnit(func(u unit.Exchange) error {
		io := inject.DefaultOptions()
		pts := inject.Enumerate(u, io)
		if *points != "" {
			pts = filterPoints(pts, *points)
		}
		if len(pts) > *maxPoints {
			// Never truncate silently: a bounded sweep that reads as complete is how a
			// scan reports "clean" for ground it never covered.
			fmt.Fprintf(os.Stderr, "[cap] %s: %d points enumerated, probing first %d — %d dropped\n",
				u.URL, len(pts), *maxPoints, len(pts)-*maxPoints)
			pts = pts[:*maxPoints]
		}

		// Stability gate on the unmodified request.
		base := sc.SampleRawN(ctx, u, inject.Apply(u, inject.Point{}, ""), "baseline", *repeat)
		if !base.Stable {
			return enc.Encode(detect.Observation{
				Schema: detect.SchemaObservation, TS: now(), UnitID: u.ID, URL: u.URL,
				Erratic: true, Base: base,
				Error: "baseline not reproducible — refusing to scan (statuses " + fmt.Sprint(base.Statuses) + ")",
			})
		}
		if !*quiet {
			fmt.Fprintf(os.Stderr, "[baseline] %s -> %d  (%d points, %d vectors)\n",
				u.URL, base.Status, len(pts), len(vecs))
		}

		probeOne := func(p inject.Point, v vectors.Vector, reps int) detect.Observation {
			mut := sc.SampleRawN(ctx, u, inject.Apply(u, p, inject.Render(v, p.Render)), p.ID+"/"+v.ID, reps)
			obs := detect.Observation{
				Schema: detect.SchemaObservation, TS: now(), UnitID: u.ID, URL: u.URL,
				Point: p, Vector: v, Base: base, Mut: mut, Signals: detect.Analyze(base, mut, v),
			}
			if obs.Signals.Count > 0 && v.Control != "" {
				obs.Control = sc.SampleRawN(ctx, u, inject.Apply(u, p, inject.RenderControl(v, p.Render)), p.ID+"/control", reps)
				obs.Signals = detect.Analyze(obs.Control, mut, v)
				obs.Signals.ControlUsed = true
			}
			return obs
		}

		for _, p := range pts {
			// PHASE A — screen the point with a couple of high-prior vectors. Most
			// points are inert, and sweeping every vector at every point costs
			// points x vectors requests for nothing.
			live := *noScreen
			if !*noScreen {
				for _, v := range screenVecs {
					obs := probeOne(p, v, *screenRepeat)
					if err := enc.Encode(obs); err != nil {
						return err
					}
					if obs.Signals.Count > 0 {
						live = true
					}
				}
			}
			if !live {
				continue
			}
			if !*quiet {
				fmt.Fprintf(os.Stderr, "  [live] %-34s %-13s %s\n", p.ID, p.Kind, p.Source)
			}

			// PHASE B — a point that reacted earns the full vector sweep.
			for _, v := range vecs {
				if !*noScreen && containsVector(screenVecs, v.ID) {
					continue // already probed in phase A
				}
				obs := probeOne(p, v, *repeat)
				if err := enc.Encode(obs); err != nil {
					return err
				}
				if !*quiet && obs.Signals.Count > 0 {
					fmt.Fprintf(os.Stderr, "    [signal] %-14s %-8s ctrl=%d mut=%d %v\n",
						v.Encoded, v.Codepoint, obs.Control.Status, obs.Mut.Status, obs.Signals.ShapeAnomaly)
				}
			}
		}
		return nil
	})
}

func filterPoints(pts []inject.Point, filter string) []inject.Point {
	var out []inject.Point
	for _, p := range pts {
		for _, f := range strings.Split(filter, ",") {
			if f = strings.TrimSpace(f); f != "" && strings.Contains(p.ID, f) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func containsVector(vs []vectors.Vector, id string) bool {
	for _, v := range vs {
		if v.ID == id {
			return true
		}
	}
	return false
}

// eachUnit decodes units from stdin, tolerating either a full unit or the bare
// {"url": "..."} shorthand, and normalises before handing it on.
func eachUnit(f func(unit.Exchange) error) error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4<<20), 4<<20)
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var u unit.Exchange
		if err := json.Unmarshal([]byte(line), &u); err != nil {
			return fmt.Errorf("bad unit %q: %w", trunc(line, 80), err)
		}
		if err := u.Normalize(); err != nil {
			return fmt.Errorf("unit %s: %w", u.URL, err)
		}
		if err := f(u); err != nil {
			return err
		}
	}
	return in.Err()
}

// ------------------------------------------------------------- investigator

func cmdInvestigator(args []string) error {
	fs := flag.NewFlagSet("investigator", flag.ExitOnError)
	rate := fs.Duration("rate", time.Second, "minimum gap between requests to one host")
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	minSignals := fs.Int("min-signals", 1, "only investigate observations with at least this many signals")
	_ = fs.Parse(args)

	o := detect.DefaultOptions()
	o.Repeats, o.Rate, o.Timeout = 1, *rate, *timeout
	sc := detect.New(o)

	ctx := context.Background()
	enc := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4<<20), 4<<20)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var obs detect.Observation
		if err := json.Unmarshal([]byte(line), &obs); err != nil {
			continue
		}
		if obs.Erratic || obs.Signals.Count < *minSignals || obs.Vector.Encoded == "" {
			continue
		}
		var u unit.Exchange
		if err := json.Unmarshal([]byte(line), &struct {
			U *unit.Exchange `json:"unit"`
		}{&u}); err != nil || u.URL == "" {
			u = unit.Exchange{ID: obs.UnitID, URL: obs.URL}
		}
		if err := u.Normalize(); err != nil {
			continue
		}

		// The confirmation payload: fold, a Host header, fold again. It proves the
		// injected bytes become HEADERS in the secondary request rather than merely
		// upsetting the parser — the difference between "odd response" and "I control
		// the Host of its backend request". It stops short of a second request line,
		// so nothing is smuggled.
		canary := "c" + randHex(6) + ".probe.invalid"
		v := obs.Vector
		fold := inject.Render(v, obs.Point.Render)
		ctlFold := inject.RenderControl(v, obs.Point.Render)
		suffix := "Host:" + canary
		raw := inject.Apply(u, obs.Point, fold+suffix+fold)
		s := sc.SampleRaw(ctx, u, raw, "confirm")

		// The control arm carries the SAME canary through the SAME point with the
		// non-folding twin. Any target that echoes the request reflects the canary in
		// both arms; only a target that folded the byte into a real newline turns it
		// into a header the backend acted on. This replaces the earlier "canary must
		// follow ://" heuristic, which was tuned to one target's error format and
		// silently confirmed nothing anywhere else.
		ctl := sc.SampleRaw(ctx, u, inject.Apply(u, obs.Point, ctlFold+suffix+ctlFold), "confirm-control")

		c := detect.Confirmation{
			Schema: detect.SchemaConfirmation, TS: now(), UnitID: u.ID, URL: u.URL,
			Point: obs.Point, Vector: v, Probe: printableReq(raw), Canary: canary,
			Sample: s, ControlSample: ctl,
		}
		can := strings.ToLower(canary)
		body := strings.ToLower(s.BodyPreview)
		ctlBody := strings.ToLower(ctl.BodyPreview)
		c.EchoedAnywhere = strings.Contains(body, can)
		if c.EchoedAnywhere && !strings.Contains(ctlBody, can) {
			c.Reflected, c.ReflectedIn, c.HeaderInject = true, "body-vs-control", true
		}
		// Stronger still: the canary is the authority of a URL the SERVER built, which
		// no amount of request echoing can produce.
		if strings.Contains(body, "://"+can) {
			c.Reflected, c.ReflectedIn, c.HeaderInject = true, "authority", true
		}
		if !c.Reflected {
			for _, h := range s.HeaderNames {
				if strings.Contains(strings.ToLower(h), can) {
					c.Reflected, c.ReflectedIn, c.HeaderInject = true, "header", true
				}
			}
		}
		if !c.Reflected && s.Status == obs.Base.Status && obs.Signals.StatusChanged {
			c.HeaderInject = true
		}
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return in.Err()
}

func printableReq(raw []byte) string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return '.'
		}
		return r
	}, string(raw))
	return trunc(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " | "), "\n", " | "), 400)
}

// ------------------------------------------------------------------ summary

func cmdSummary(args []string) error {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	_ = fs.Parse(args)

	type row struct {
		target, point, enc, cp string
		from, to               int
		anomalies              string
		known                  bool
		n                      int
	}
	var rows []row
	seen, erratic := 0, 0
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		var o detect.Observation
		if json.Unmarshal(in.Bytes(), &o) != nil {
			continue
		}
		if o.Erratic {
			erratic++
			continue
		}
		seen++
		if o.Signals.Count == 0 {
			continue
		}
		ctl := o.Control.Status
		if !o.Signals.ControlUsed {
			ctl = o.Base.Status
		}
		rows = append(rows, row{o.URL, o.Point.ID, o.Vector.Encoded, o.Vector.Codepoint,
			ctl, o.Mut.Status,
			strings.Join(o.Signals.ShapeAnomaly, ","), o.Vector.Known, o.Signals.Count})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })

	fmt.Printf("%d probes, %d erratic targets, %d surviving the matched control\n\n", seen, erratic, len(rows))
	if len(rows) > 0 {
		fmt.Printf("%-34s %-14s %-9s %-13s %-6s %s\n", "INJECTION POINT", "VECTOR", "CODEPOINT", "CTRL->VECTOR", "KNOWN", "ANOMALIES")
		for _, r := range rows {
			mark := "NEW"
			if r.known {
				mark = "known"
			}
			fmt.Printf("%-34s %-14s %-9s %-13s %-6s %s\n", trunc(r.point, 34), r.enc, r.cp,
				fmt.Sprintf("%d->%d", r.from, r.to), mark, r.anomalies)
		}
	}
	return in.Err()
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const schemaDoc = `foldscan.target/v1        (validator stdin)
  url          string   required   e.g. "https://api.example.com/"
  host_header  string   optional   override Host (probe a backend IP directly)
  notes        string   optional   free-form provenance

foldscan.vector/v1        (flamer stdout)
  id, family, class, codepoint, encoded, wire_bytes, tier, known, note
  family: raw | utf8-fold | overlong | double-encoded

foldscan.observation/v1   (validator stdout, investigator stdin)
  target, vector
  erratic   bool     baseline not reproducible; nothing was scanned
  baseline  Sample   {path,n,statuses,status,stable,body_len,body_sha,
                      body_preview,header_names,declared_content_length,
                      elapsed_ms_avg,errors}
  mutated   Sample
  signals   {status_changed,status_from,status_to,body_changed,body_len_delta,
             new_headers,contamination,shape_anomaly[],signal_count}
    shape_anomaly is a port of terminator's FindingType:
      dual_response | truncated_body | cl_mismatch | leaked_headers | unclosed_html

foldscan.confirmation/v1  (investigator stdout)
  target, vector, probe, canary
  reflected, reflected_in, header_injection_confirmed, sample, error
`
