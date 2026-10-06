package main

// REPORT TEMPLATE LINT — the refusal that happens before a two-hour run, not after it.
//
// ── WHY THIS IS IN THE CLI AND THE RENDERER IS NOT ──────────────────────────────────────────────
//
// The orchestrator is the only thing that renders a report (ADR 0055). This is a PRE-FLIGHT: it reads
// `report.md` off the disk the author is editing and refuses `kontra workflow serve` on a template that
// cannot work, so the failure arrives while they are still looking at the file. `workflowServeArgs`'s
// own doc comment calls itself the home for "every refusal that can be decided from the filesystem",
// which is exactly what this is.
//
// ── TWO PARSERS, AND THE ONE HERE IS NOT THE AUTHORITY ──────────────────────────────────────────
//
// `github.com/osteele/liquid` parses; LiquidJS 10.30.0 renders. They are different implementations of
// one language and they do not agree about everything — measured, before this was written:
//
//	{{ x | nosuchfilter }}   osteele: parses.  LiquidJS with strictFilters: ParseError.
//	{% code "http", v %}     osteele: an undefined tag, unless registered — hence `registerTags`.
//	{{ result.summary }      both: literal text. A missing brace is not catchable here.
//
// So this lint is deliberately NARROW: a syntax error and an unknown context root, both of which the
// two agree on. It does not try to be the renderer, because a lint that refuses a template the
// orchestrator would have rendered is worse than no lint — it blocks an author with no way round.
// `KONTRA_REPORT_LINT=off` is that way round if the two ever disagree anyway, and its existence is the
// admission that they might.
//
// LINE, NOT LINE AND COLUMN. §6.1 asks for both; `liquid.SourceError` carries `LineNumber()` and no
// column, and inventing one would be worse than saying so.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/osteele/liquid"
	"github.com/osteele/liquid/render"
)

// reportFile is the template beside `workflow.py`, read the same way `description.md` is.
const reportFile = "report.md"

// contextRoots is §2.4's contract: the whole of what a template can see. A root outside this set is an
// author's typo — `{{ results.x }}` for `{{ result.x }}` is the one the spec names — and it would be an
// `UndefinedVariableError` at render under `strictVariables`, which is to say after the run.
var contextRoots = map[string]bool{
	"run":      true,
	"workflow": true,
	"input":    true,
	"result":   true,
	"report":   true,
}

// liquidKeywords are the words inside `{% %}` that are syntax rather than data.
var liquidKeywords = map[string]bool{
	"if": true, "elsif": true, "else": true, "endif": true, "unless": true, "endunless": true,
	"for": true, "endfor": true, "in": true, "break": true, "continue": true, "limit": true,
	"offset": true, "reversed": true, "case": true, "when": true, "endcase": true,
	"assign": true, "capture": true, "endcapture": true, "increment": true, "decrement": true,
	"cycle": true, "tablerow": true, "endtablerow": true, "raw": true, "endraw": true,
	"comment": true, "endcomment": true, "code": true, "image": true, "echo": true,
	"and": true, "or": true, "not": true, "contains": true, "empty": true, "blank": true,
	"nil": true, "null": true, "true": true, "false": true, "with": true, "as": true,
	"show": true, "redact": true,
}

// expressionRe finds every Liquid expression — output and tag — with its body.
var expressionRe = regexp.MustCompile(`(?s)\{\{(.*?)\}\}|\{%(.*?)%\}`)

// identRe finds a dotted identifier chain. `[\w]` and not `\S`, so a filter argument's quoted string
// and a number are not mistaken for a name.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)

// bindingRe finds the names a template introduces for itself: a loop variable, an assignment, a
// capture. These are NOT context roots and must not be reported as unknown ones — the check would
// otherwise reject every template with a `{% for %}` in it.
var bindingRe = regexp.MustCompile(`\{%-?\s*(?:for\s+([A-Za-z_][A-Za-z0-9_]*)\s+in|assign\s+([A-Za-z_][A-Za-z0-9_]*)\s*=|capture\s+([A-Za-z_][A-Za-z0-9_]*))`)

// filterRe finds `| name`, whose name is a filter and not a variable.
var filterRe = regexp.MustCompile(`\|\s*([A-Za-z_][A-Za-z0-9_]*)`)

// reportLintError is a refusal an author can act on: the file, the line, and what is wrong.
type reportLintError struct {
	Path string
	Line int
	Msg  string
}

func (e *reportLintError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Msg)
}

// registerTags teaches the linter the tags the orchestrator's engine adds, so a template using them is
// not rejected for using them. `{% image %}` is reserved and errors at RENDER (ADR 0055 §1); it parses
// here because a parse error would be the wrong message for "not supported yet".
func registerTags(e *liquid.Engine) {
	noop := func(render.Context) (string, error) { return "", nil }
	e.RegisterTag("code", noop)
	e.RegisterTag("image", noop)
}

// lintReportTemplate checks one template's text. `path` is used only in messages.
//
// IT RETURNS EVERY PROBLEM IT CAN SEE, not the first: an author who fixes one unknown root and is then
// told about the next has been made to pay for the round trip twice — the reasoning `startRun`'s slot
// refusal already follows.
func lintReportTemplate(path, text string) []error {
	engine := liquid.NewEngine()
	registerTags(engine)
	if _, err := engine.ParseTemplateAndCache([]byte(text), path, 1); err != nil {
		line := 0
		var se liquid.SourceError
		if errors.As(err, &se) {
			line = se.LineNumber()
		}
		// A SYNTAX ERROR STOPS HERE. The root check below walks the raw text, and over a template that
		// does not parse it would report nonsense about the half the parser never reached.
		return []error{&reportLintError{Path: path, Line: line, Msg: liquidMessage(err)}}
	}

	bound := map[string]bool{"forloop": true, "tablerowloop": true}
	for _, m := range bindingRe.FindAllStringSubmatch(text, -1) {
		for _, name := range m[1:] {
			if name != "" {
				bound[name] = true
			}
		}
	}

	unknown := map[string]int{}
	for _, m := range expressionRe.FindAllStringSubmatchIndex(text, -1) {
		body := ""
		if m[2] >= 0 {
			body = text[m[2]:m[3]]
		} else if m[4] >= 0 {
			body = text[m[4]:m[5]]
		}
		if body == "" {
			continue
		}
		filters := map[string]bool{}
		for _, f := range filterRe.FindAllStringSubmatch(body, -1) {
			filters[f[1]] = true
		}
		// Quoted strings are data, not names: `{% code "http", v %}` must not report `http`.
		naked := stripQuoted(body)
		for _, ident := range identRe.FindAllString(naked, -1) {
			root := ident
			if i := strings.IndexByte(ident, '.'); i >= 0 {
				root = ident[:i]
			}
			if contextRoots[root] || liquidKeywords[root] || bound[root] || filters[root] {
				continue
			}
			if _, seen := unknown[root]; !seen {
				unknown[root] = lineOf(text, m[0])
			}
		}
	}

	names := make([]string, 0, len(unknown))
	for name := range unknown {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]error, 0, len(names))
	for _, name := range names {
		out = append(out, &reportLintError{
			Path: path,
			Line: unknown[name],
			Msg: fmt.Sprintf(
				"%q is not part of a report's context. A template can see run, workflow, input, result and report — and nothing else (ADR 0055). %s",
				name, didYouMean(name),
			),
		})
	}
	return out
}

// stripQuoted blanks out single- and double-quoted runs, keeping the length so offsets still line up.
func stripQuoted(body string) string {
	out := []byte(body)
	var quote byte
	for i := 0; i < len(out); i++ {
		switch {
		case quote == 0 && (out[i] == '"' || out[i] == '\''):
			quote = out[i]
			out[i] = ' '
		case quote != 0 && out[i] == quote:
			quote = 0
			out[i] = ' '
		case quote != 0:
			out[i] = ' '
		}
	}
	return string(out)
}

// didYouMean names the root an author probably meant. `results` for `result` is the typo §6.1 cites.
func didYouMean(name string) string {
	for root := range contextRoots {
		if strings.HasPrefix(name, root) || strings.HasPrefix(root, name) {
			return fmt.Sprintf("Did you mean %q?", root)
		}
	}
	return "A workflow puts things in its report by RETURNING them, which arrive as `result`."
}

// lineOf is the 1-based line containing byte offset `at`.
func lineOf(text string, at int) int {
	if at > len(text) {
		at = len(text)
	}
	return strings.Count(text[:at], "\n") + 1
}

// liquidMessage strips the library's own `Liquid error (line N): ` prefix, which would otherwise read
// twice beside this error's own `path:line:`.
func liquidMessage(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "): "); i >= 0 && strings.HasPrefix(msg, "Liquid error") {
		return msg[i+3:]
	}
	return strings.TrimPrefix(msg, "Liquid error: ")
}

// lintReportInFolder checks the `report.md` beside a workflow, if there is one.
//
// A FOLDER WITH NO TEMPLATE IS NOT AN ERROR — most have none and get the default report — so this
// returns nothing at all for that case rather than a notice nobody needs.
//
// `KONTRA_REPORT_LINT=off` skips it. That exists because this linter is a second Liquid implementation
// and the renderer is the authority: if the two ever disagree about a template that renders correctly,
// an author must be able to proceed. It prints when it is off, because a silent skip would make the
// next person wonder why their syntax error was not caught.
func lintReportInFolder(folder string) []error {
	if os.Getenv("KONTRA_REPORT_LINT") == "off" {
		fmt.Fprintln(os.Stderr, "kontra: report.md lint is OFF (KONTRA_REPORT_LINT=off)")
		return nil
	}
	path := filepath.Join(folder, reportFile)
	text, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return lintReportTemplate(path, string(text))
}

// reportLintRefusal turns the lint's findings into one error for `serve` to return, naming every one.
func reportLintRefusal(problems []error) error {
	if len(problems) == 0 {
		return nil
	}
	lines := make([]string, 0, len(problems)+2)
	lines = append(lines, fmt.Sprintf("%s has %d problem(s), and a report renders only after the run:", reportFile, len(problems)))
	for _, p := range problems {
		lines = append(lines, "  "+p.Error())
	}
	// THE TYPE CHECK §6.1 ALSO ASKS FOR IS NOT BUILT, and saying so here is better than leaving an
	// author to assume `{{ result.missing_field }}` was checked. It needs the workflow's return-type
	// schema, which means extending `runtime/python/internals/schemadump.py` to dump a workflow's
	// return annotation — that is its own change.
	lines = append(lines, "  (field names under `result` are NOT checked — that needs the workflow's return type)")
	return errors.New(strings.Join(lines, "\n"))
}
