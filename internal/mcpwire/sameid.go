package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// SameID reports whether a response's id a answers a request whose id is b, as stock clients match them: equal
// strings; for a numeric request id, a number of the same value (1, 1.0 and 1e0 are one id) or a string whose
// JavaScript Number() is that value (the TypeScript SDK looks its handler up by Number(response.id), so "7" answers
// request 7). The gate's relay meters, ends a stream after, and opens a session on the response the client takes,
// and both nodes' sealing decide by it whether a response answers a sealed request (spec/ext-sealed.md §5 rules 4
// and 4a): an id that a client would take for the request's must be sealed, opened or refused as the request's
// answer, never relayed as another's (relay §11.4, D172).
func SameID(a, b json.RawMessage) bool {
	a, b = bytes.TrimSpace(a), bytes.TrimSpace(b)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	str := func(raw json.RawMessage) (string, bool) {
		var s string
		return s, raw[0] == '"' && json.Unmarshal(raw, &s) == nil
	}
	num := func(raw json.RawMessage) (float64, bool) {
		if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
			return 0, false
		}
		f, err := strconv.ParseFloat(string(raw), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	}
	as, aStr := str(a)
	bs, bStr := str(b)
	switch {
	case aStr && bStr:
		return as == bs
	case bStr:
		return false // a string request id is answered by that string only
	}
	bn, ok := num(b)
	if !ok {
		return false
	}
	if aStr {
		an, ok := JSNumber(as)
		return ok && an == bn
	}
	an, ok := num(a)
	return ok && an == bn
}

// AnswerKeys are the IDKeys of every request id a response whose id is id answers by SameID: its own key, and for a
// string whose JavaScript Number() is a finite number, that number's key too ("8", " 8 ", "0x8" and "8e0" answer
// request 8). A map of requests by IDKey is looked up with each, in order.
func AnswerKeys(id json.RawMessage) []string {
	k, ok := IDKey(id)
	if !ok {
		return nil
	}
	keys := []string{k}
	if strings.HasPrefix(k, "s:") {
		if f, ok := JSNumber(k[2:]); ok && !math.IsInf(f, 0) {
			keys = append(keys, "n:"+strconv.FormatFloat(f, 'g', -1, 64))
		}
	}
	return keys
}

// JSNumber is JavaScript's Number(s) for a string (ECMAScript StringToNumber): white space around it trimmed, "" is
// 0, a decimal literal, Infinity with an optional sign, or a 0x, 0o or 0b integer; ok false where Number gives NaN.
func JSNumber(s string) (float64, bool) {
	s = strings.TrimFunc(s, func(c rune) bool {
		switch c {
		case '\t', '\v', '\f', ' ', '\n', '\r', '\u00a0', '\ufeff', '\u2028', '\u2029':
			return true
		}
		return unicode.Is(unicode.Zs, c)
	})
	switch s {
	case "":
		return 0, true
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			v := 0.0
			for _, c := range s[2:] {
				d := strings.IndexRune("0123456789abcdef", unicode.ToLower(c))
				if d < 0 || d >= base {
					return 0, false
				}
				v = v*float64(base) + float64(d)
			}
			return v, true
		}
	}
	// [+-]? (digits ("." digits?)? | "." digits) ([eE] [+-]? digits)?
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := func() int {
		n := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			n++
		}
		return n
	}
	n := digits()
	if i < len(s) && s[i] == '.' {
		i++
		n += digits()
	}
	if n == 0 {
		return 0, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return 0, false
		}
	}
	if i != len(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}
