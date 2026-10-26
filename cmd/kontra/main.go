// Command kontra is the node CLI: connect, console, login, seal, publish (note 2 §1). M1 ships only
// `kontra version`; `connect` is M2 (spec/tunnel.md).
package main

import (
	"fmt"
	"os"

	"kontra.sh/kontra/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kontra version")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(version.String("kontra"))
	case "connect", "console", "login", "seal", "publish":
		fmt.Fprintf(os.Stderr, "kontra %s: not until M2 or later (PRD §14)\n", os.Args[1])
		os.Exit(1)
	default:
		fmt.Fprintln(os.Stderr, "usage: kontra version")
		os.Exit(2)
	}
}
