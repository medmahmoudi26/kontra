// Command lab runs the local vulnerable reproduction on fixed ports for manual poking.
// The behaviour lives in the lab package so tests can drive it without a subprocess —
// killing a `go run` parent does not kill the binary it spawned, and a stale lab holding
// the port silently makes every subsequent mode look identical to the first.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/medmahmoudi26/kontra-actors/go/desync/lab"
)

func main() {
	mode := flag.String("mode", "vulnerable", "vulnerable | patched | erratic")
	fe := flag.String("frontend", "127.0.0.1:9080", "frontend listen address")
	be := flag.String("backend", "127.0.0.1:9081", "backend listen address")
	flag.Parse()

	l, err := lab.StartAt(*mode, *fe, *be)
	if err != nil {
		fmt.Fprintln(os.Stderr, "lab:", err)
		os.Exit(1)
	}
	defer l.Close()
	fmt.Fprintf(os.Stderr, "lab mode=%s frontend=%s\n", l.Mode, l.URL())
	select {}
}
