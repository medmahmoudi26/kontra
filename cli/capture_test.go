package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/medmahmoudi26/kontra/cli/internal/cliio"
)

// captureStdout runs `body` with the package writer redirected, on top of dispatch_test.go's
// existing `swap` rather than re-implementing the save/restore around it.
//
// IT LIVED IN `fleet_tmux_test.go` UNTIL THE MONITOR WENT. That file's subject was the `--tmux`
// session converge and it is gone; this helper is not about tmux and has callers across the suite,
// so it moved to a file named for what it does rather than staying in one named for a feature that
// no longer exists.
func captureStdout(t *testing.T, body func()) string {
	t.Helper()
	var buf bytes.Buffer
	defer swap[io.Writer](&cliio.Stdout, &buf)()
	body()
	return buf.String()
}
