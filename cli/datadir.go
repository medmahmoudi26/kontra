package main

// datadir.go — the one directory this installation keeps its own state in.
//
// `$KONTRA_HOME/data` rather than a sibling of the checkout, because KONTRA_HOME is already THE
// answer to "where does this installation keep its things": config.yaml, workflows/ and actors/ are
// there, resolved by the same rule in cliutil.KontraRoot. A second location for the same
// installation is a second thing to find, back up and get wrong.
//
// KONTRA_DATA_DIR IS THE SAME VARIABLE `control/orchestrator/src/data/dataDir.ts` READS, and that
// is what keeps the two halves of one installation pointing at one directory — docker-compose.yml
// sets it to the container path of the volume that survives a recreate. The two functions must stay
// twins; the DuckLake catalog the orchestrator writes lives under this path.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/medmahmoudi26/kontra/cli/internal/cliutil"
)

func installDataDir(override string) (string, error) {
	if override == "" {
		override = os.Getenv("KONTRA_DATA_DIR")
	}
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("--data-dir %s: %w", override, err)
		}
		return abs, nil
	}
	root, err := cliutil.KontraRoot()
	if err != nil {
		return "", errors.New("no data directory: " + err.Error() + " (or pass --data-dir)")
	}
	return filepath.Join(root, "data"), nil
}
