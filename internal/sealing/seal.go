package sealing

import (
	"context"
	"io"

	"kontra.sh/kontra/internal/unimpl"
)

// SealUsage is kontra seal's synopsis (spec/ext-sealed.md §5.1).
const SealUsage = `usage:
  kontra seal --service <service URL> [--context <id>] [--task <id>] [file | -]
  kontra seal --open <part.json>
`

// SealCommand is `kontra seal` (spec/ext-sealed.md §5.1, PRD §8): the escape hatch for scripting, not the path.
// It fetches the service's served card, verifies the gate's signature (jku = the service URL's origin +
// /.well-known/jwks.json) and the pin (§7), mints a per-job key and rk, and prints one A2A 1.0 Message JSON
// (role user, a fresh messageId, the contextId given or minted, taskId when given) whose single part is the
// sealed file, with metadata["kontra.sealed.replyTo"] set; the per-job key is kept under
// ~/.kontra/jobs/<gate host>/ so that --open can open the replies' parts. Sealing in-process without the CLI
// means reproducing §3 exactly; the example documents the age calls (Go filippo.io/age, Python pyrage), not an
// SDK.
func SealCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return unimpl.Error("sealing.SealCommand", "M3 WP-08")
}
