// linkfind — what a crawled page gives away. `urls` harvests every absolute link, which is fresh
// scope for whatever crawls next; `secrets` flags credential-shaped strings. Both share the
// patterns Load compiled once, and both are 1 -> N: a page yields as many records as it contains.
package main

import (
	"regexp"

	kontra "github.com/medmahmoudi26/kontra/sdk/go"
	// The actor runtime, imported for its side effect: it registers the Temporal actor
	// host behind a.Serve(). The SDK declares the seam and never imports across it —
	// runtime/ -> sdk/ is one-way — so this line is what puts a host in the binary.
	_ "github.com/medmahmoudi26/kontra/runtime/go"
)

func main() {
	a := kontra.New()
	a.Load(load)
	a.Method("urls", urls)
	a.Method("secrets", secrets)
	a.Serve()
}

func load(s *kontra.Session) error {
	s.Set("urls", regexp.MustCompile(`https?://[\w.-]+(?::\d+)?[\w./?=&%~+#-]*`))
	s.Set("secrets", regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|token|password)\b["'\s:=]{1,4}([\w./+-]{16,})`))
	return nil
}

func urls(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	for unit := range b.All() {
		for _, m := range re(s, "urls").FindAllString(unit.Str("body"), -1) {
			ds.Push(map[string]any{"url": m, "from": unit.Str("url")})
		}
	}
	return b.Err()
}

func secrets(s *kontra.Session, b *kontra.Batch, ds *kontra.Dataset) error {
	for unit := range b.All() {
		for _, m := range re(s, "secrets").FindAllStringSubmatch(unit.Str("body"), -1) {
			ds.Push(map[string]any{"secret": m[1], "from": unit.Str("url")})
		}
	}
	return b.Err()
}

func re(s *kontra.Session, name string) *regexp.Regexp {
	v, _ := s.Get(name)
	return v.(*regexp.Regexp)
}
