package stdiobridge

import (
	"errors"
	"fmt"
	"strings"
)

// shellOperators are the characters a POSIX shell would read as an operator, an expansion or a comment when
// unquoted. The command line runs without a shell, so a pipe, a redirection, a variable or a comment would reach
// the program as a literal argument, which is not what its author meant: SplitCommand refuses them unquoted.
const shellOperators = "|&;<>()$`"

// SplitCommand splits a command line with POSIX shell word rules (quotes, backslashes; no expansion but a
// leading ~/), refusing an empty one or an unterminated quote.
//
//   - Blanks (space, tab, newline) separate words; a backslash-newline joins lines.
//   - '…' keeps everything up to the next single quote as it is; "…" does too, except that a backslash before $, `,
//     ", \ or a newline escapes it; outside quotes a backslash escapes the next character. "" is an empty word.
//   - A word that starts with an unquoted ~/ starts with home instead of ~ (home is kontra connect's HOME).
//   - Nothing else is expanded, and nothing runs a shell. An unquoted |, &, ;, <, >, (, ), $ or `, or a # at the
//     start of a word, is refused rather than passed on literally: quote it, or run a script.
//
// SPEC-CONFLICT (spec/tunnel.md §10: "split with POSIX shell word rules, expands a leading ~/ and runs it without a
// shell"): §10 does not say what an unquoted operator, expansion or comment becomes when no shell runs. Under POSIX
// rules they are not word characters, and passing them on as arguments would run something other than what the
// operator wrote (a "> log" would reach the server as two arguments). The closest faithful behaviour is to refuse
// them at start (a usage error), so no command line is run with a meaning a shell would not give it.
func SplitCommand(line, home string) ([]string, error) {
	var (
		words  []string
		cur    strings.Builder
		inWord bool
	)
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			flush()
			i++
		case c == '\'':
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("the command line has an unterminated single quote")
			}
			cur.WriteString(line[i+1 : i+1+j])
			inWord = true
			i += j + 2
		case c == '"':
			inWord = true
			i++
			closed := false
			for i < len(line) {
				d := line[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(line) {
					switch n := line[i+1]; n {
					case '$', '`', '"', '\\':
						cur.WriteByte(n)
						i += 2
						continue
					case '\n':
						i += 2
						continue
					}
				}
				cur.WriteByte(d)
				i++
			}
			if !closed {
				return nil, errors.New("the command line has an unterminated double quote")
			}
		case c == '\\':
			if i+1 >= len(line) {
				return nil, errors.New("the command line ends with a backslash")
			}
			if line[i+1] != '\n' {
				cur.WriteByte(line[i+1])
				inWord = true
			}
			i += 2
		case strings.IndexByte(shellOperators, c) >= 0, c == '#' && !inWord:
			return nil, fmt.Errorf("the command line has an unquoted %q: it runs without a shell (quote it, or run a script)", c)
		case c == '~' && !inWord && strings.HasPrefix(line[i:], "~/"):
			if home == "" {
				return nil, errors.New("the command line starts a word with ~/ and there is no home directory")
			}
			cur.WriteString(strings.TrimSuffix(home, "/"))
			inWord = true
			i++ // the '/' is written next
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	flush()
	if len(words) == 0 {
		return nil, errors.New("the command line is empty")
	}
	return words, nil
}
