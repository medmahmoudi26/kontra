package a2avalid

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// A JSON document as a tree that keeps an object's members in order, duplicates included (encoding/json would keep
// the last), so a repeated member is an issue, as protojson makes it an error. One pass over the bytes: the console
// validates every frame of a stream it shows (BenchmarkFrame*).

type kind int

const (
	kNull kind = iota
	kBool
	kNumber
	kString
	kArray
	kObject
)

type member struct {
	key string
	val *node
}

type node struct {
	kind    kind
	str     string // a string's value, or a number's text
	members []member
	items   []*node
}

// maxDepth bounds nesting, as the gate's strict JSON decoding does.
const maxDepth = 256

type parser struct {
	b []byte
	i int
}

func parse(raw []byte) (*node, error) {
	p := &parser{b: raw}
	p.space()
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.b) {
		return nil, p.errf("data after the JSON value")
	}
	return n, nil
}

func (p *parser) errf(format string, a ...any) error {
	return fmt.Errorf("offset %d: %s", p.i, fmt.Sprintf(format, a...))
}

func (p *parser) space() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (*node, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	if p.i >= len(p.b) {
		return nil, p.errf("unexpected end")
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.string()
		if err != nil {
			return nil, err
		}
		return &node{kind: kString, str: s}, nil
	case c == 't':
		return p.literal("true", &node{kind: kBool, str: "true"})
	case c == 'f':
		return p.literal("false", &node{kind: kBool, str: "false"})
	case c == 'n':
		return p.literal("null", &node{kind: kNull})
	case c == '-' || c >= '0' && c <= '9':
		return p.number()
	}
	return nil, p.errf("unexpected %q", p.b[p.i])
}

func (p *parser) literal(word string, n *node) (*node, error) {
	if len(p.b)-p.i < len(word) || string(p.b[p.i:p.i+len(word)]) != word {
		return nil, p.errf("invalid literal")
	}
	p.i += len(word)
	return n, nil
}

func (p *parser) number() (*node, error) {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		s := p.i
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
		return p.i - s
	}
	if n := digits(); n == 0 || n > 1 && p.b[p.i-n] == '0' {
		return nil, p.errf("invalid number")
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, p.errf("invalid number")
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, p.errf("invalid number")
		}
	}
	return &node{kind: kNumber, str: string(p.b[start:p.i])}, nil
}

func (p *parser) string() (string, error) {
	start := p.i
	p.i++ // the opening quote
	plain := true
	for p.i < len(p.b) {
		c := p.b[p.i]
		switch {
		case c == '"':
			p.i++
			if plain {
				s := string(p.b[start+1 : p.i-1])
				if !utf8.ValidString(s) {
					return "", p.errf("invalid UTF-8 in a string")
				}
				return s, nil
			}
			var s string
			if err := json.Unmarshal(p.b[start:p.i], &s); err != nil {
				return "", p.errf("invalid string: %v", err)
			}
			return s, nil
		case c == '\\':
			plain = false
			p.i += 2
		case c < 0x20:
			return "", p.errf("control character in a string")
		default:
			p.i++
		}
	}
	return "", p.errf("unterminated string")
}

func (p *parser) array(depth int) (*node, error) {
	p.i++
	n := &node{kind: kArray}
	p.space()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return n, nil
	}
	for {
		p.space()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, v)
		p.space()
		if p.i >= len(p.b) {
			return nil, p.errf("unterminated array")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return n, nil
		default:
			return nil, p.errf("expected , or ]")
		}
	}
}

func (p *parser) object(depth int) (*node, error) {
	p.i++
	n := &node{kind: kObject}
	p.space()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return n, nil
	}
	for {
		p.space()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, p.errf("expected a member name")
		}
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, p.errf("expected :")
		}
		p.i++
		p.space()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		n.members = append(n.members, member{key: key, val: v})
		p.space()
		if p.i >= len(p.b) {
			return nil, p.errf("unterminated object")
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return n, nil
		default:
			return nil, p.errf("expected , or }")
		}
	}
}
