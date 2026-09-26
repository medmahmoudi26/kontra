// stateexport.go — the copy taken before every converge, and why this one volume gets one.
//
// ── A PROTECTED RESOURCE IS SAFE FROM PULUMI AND IS NOT SAFE FROM `rm` ──────────────────────────
//
// ADR 0052 §7 grades the eleven volumes by what losing one costs, and `pulumi-state` is the only one
// whose loss is not local. Every other volume costs its own contents. That one is the record of every
// cloud Machine the control plane owns, so losing it does not lose data — it loses THE ABILITY TO
// DESTROY MACHINES THAT ARE STILL BILLING, in an account this program cannot see, on droplets nothing
// can any longer find. `infra/paths.ts` already names it: a teardown that took it "would orphan every
// machine we own".
//
// ── WHAT THIS EXPORT IS, AND — SAYING IT PLAINLY — WHAT IT IS NOT ───────────────────────────────
//
// `pulumi stack export` writes THIS engine's checkpoint: the 40 resources of `kontra-control`,
// including all eleven volume resources with their docker names, their `protect` flags and which
// container mounts each. That is what re-adopts an installation whose `~/.kontra/state` was lost —
// without it, a `state/` that goes missing turns eleven existing docker volumes into eleven volumes
// nothing owns, and the next converge builds empty ones beside them.
//
// IT IS NOT A COPY OF THE BYTES INSIDE `kontra_pulumi-state`. The Machines' own checkpoints are the
// INFRA engine's, written by `orchestrator-infra` to `file:///data/pulumi` inside that volume
// (`workspace.ts:56`), and a host `pulumi` cannot read another engine's backend out of a container
// volume. Copying them would be a third shell-out to `docker` for a backup this command has no way to
// verify. So the division of labour is stated rather than blurred:
//
//   - the volume not being deleted is `protect: true` in the program (measured: it refuses in
//     `preview`);
//   - the volume still being THAT volume, attached to THAT service, is volumes.go's identity gate;
//   - the volume's contents surviving an `rm` are a person's backup, and the copy is
//     `docker run --rm -v kontra_pulumi-state:/s -w /s alpine tar cf - .`;
//   - this file is what survives the loss of the host's own state directory.
package hostengine

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ExportDirName is where the copies go, under the engine's own directory.
const ExportDirName = "exports"

// SaveExport writes one `pulumi stack export` to a timestamped file and returns its path.
//
// UTC AND SECOND-RESOLUTION, SORTABLE, NO COLONS. `state-20260926T231421Z.json` sorts
// chronologically in `ls`, survives being copied to a filesystem that will not take a colon in a
// name, and does not need a second field to disambiguate two converges in one day.
//
// 0600 IN A 0700 DIRECTORY, like the rest of this engine's state: the checkpoint carries the
// ENCRYPTED form of every stack secret, and `backend.go`'s rule for the directory that holds
// ciphertext applies to a copy of it that is going to sit around for months.
//
// NOTHING IS PRUNED, AND THAT IS THE DECISION. A rotation this command performed would be a program
// deleting the backup it just told an operator to keep; the files are ~100 KB of JSON and the
// directory is printed on every run, so the operator can see them and decide. If that ever needs a
// bound, the bound belongs to a `kontra` verb somebody types, not to a side effect of an upgrade.
func SaveExport(dir string, raw []byte, now time.Time) (string, error) {
	out := filepath.Join(dir, ExportDirName)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", out, err)
	}
	path := filepath.Join(out, "state-"+now.UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("writing the state export %s: %w", path, err)
	}
	return path, nil
}
