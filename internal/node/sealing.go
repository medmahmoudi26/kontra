package node

import (
	"errors"
	"log/slog"
	"net/url"
	"path/filepath"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/nodekey"
	"kontra.sh/kontra/internal/pins"
	"kontra.sh/kontra/internal/sealing"
	"kontra.sh/kontra/internal/tunnel"
)

// sweepSealingEvery is how often the node drops the job state and the per-job keys whose time has passed (a hosted
// job 7 days after its last exchange, a hired job's key after its kontra.keyRetainUntil).
const sweepSealingEvery = time.Hour

// setupSealing makes the node's sealing (spec/ext-sealed.md §5, §7; M3 WP-08): the hosting side, whose durable job
// state is under ~/.kontra/hosted/<gate host>/ and which opens with every key of the identity file (a rotated key
// still opens during its overlap, §12), and the hiring side, whose per-job keys are under ~/.kontra/jobs/<gate host>/
// and whose pins are ~/.kontra/pins.
func (n *Node) setupSealing(gate *url.URL, origin string, log *slog.Logger) error {
	home := n.cfg.Home
	if home == "" {
		home = filepath.Dir(filepath.Dir(n.cfg.CredentialsPath)) // ~/.kontra/credentials
	}
	if home == "" || home == "." {
		return errors.New("node: no home directory for ~/.kontra")
	}
	keyPath := n.cfg.KeyPath
	identities := func() []age.Identity {
		id, err := nodekey.Load(keyPath)
		if err != nil {
			log.Warn("sealing: the identity file could not be read", "err", err)
			return nil
		}
		return id.Identities()
	}
	keyIDs := func() []string {
		var out []string
		for _, i := range identities() {
			if x, ok := i.(*age.X25519Identity); ok {
				out = append(out, tunnel.KeyID(x.Recipient().String()))
			}
		}
		return out
	}
	hosting, err := sealing.NewHosting(sealing.HostingConfig{
		Dir:        filepath.Join(home, nodekey.DirName, sealing.HostedDirName, gate.Host),
		Identities: identities, KeyIDs: keyIDs, MaxAge: n.cfg.SealedMaxAge, Required: n.cfg.SealedRequired,
		Shared: upstreamGroups(n.cfg.Serves), // services of one upstream share their sealed state (D236)
	}, log)
	if err != nil {
		return err
	}
	n.hosting = hosting
	n.proxy.seal = hosting
	jobs, err := nodekey.OpenJobKeys(home, gate.Host)
	if err != nil {
		return err
	}
	ps, err := pins.Open(filepath.Join(home, nodekey.DirName, pins.FileName))
	if err != nil {
		return err
	}
	hs, err := sealing.NewHiring(sealing.HiringConfig{
		Jobs: jobs, Pins: ps, Opts: pins.Options{PinRecipients: n.cfg.PinRecipients, Accept: n.cfg.AcceptChange},
		Gate: origin, Card: n.hire.Card, Out: n.cfg.Err,
	}, log)
	if err != nil {
		return err
	}
	n.hiringSeal = hs
	n.hire.SetSealing(hs)
	return nil
}

// sweepSealing drops what sealing keeps past its time (sweepSealingEvery).
func (n *Node) sweepSealing(now time.Time) {
	if n.hosting != nil {
		if err := n.hosting.Sweep(now); err != nil {
			n.logger.Warn("sealing: the hosted job state was not swept", "err", err)
		}
	}
	if n.hiringSeal != nil {
		n.hiringSeal.Sweep()
	}
}
