package hydrate

import "fmt"

// THE PINS. Same shape as install.sh's Go step, which is the one of its three fetches that
// is reproducible: a version, an explicit URL built from it, and a checksum per platform,
// all bumped together (ADR 0031 finding 6). The buf step in that file — `/releases/latest/`
// with no version and no checksum — is the shape this deliberately does not copy, and
// Artifact.Validate refuses it outright.
//
// The digests are upstream's own, from https://nodejs.org/dist/v22.13.0/SHASUMS256.txt.
// Not computed from a download somebody happened to have: a checksum taken from the file
// you already fetched checks the copy, not the artifact.

// NodeVersion is the Node the carried orchestrator runs on. control/orchestrator/package.json says
// `engines.node: >=22.13.0`; the appliance ships an exact version, because ">=" is a
// constraint on somebody else's machine and this is our machine.
const NodeVersion = "22.13.0"

var nodeDigests = map[string]string{
	"linux/amd64":  "9a33e89093a0d946c54781dcb3ccab4ccf7538a7135286528ca41ca055e9b38f",
	"linux/arm64":  "e0cc088cb4fb2e945d3d5c416c601e1101a15f73e0f024c9529b964d9f6dce5b",
	"darwin/amd64": "cfaaf5edde585a15547f858f5b3b62a292cf5929a23707b6f1e36c29a32487be",
	"darwin/arm64": "bc1e374e7393e2f4b20e5bbc157d02e9b1fb2c634b2f992136b38fb8ca2023b7",
}

// Node's own naming for the four platform artifact sets ADR 0031 §6 commits to.
var nodePlatform = map[string]string{
	"linux/amd64":  "linux-x64",
	"linux/arm64":  "linux-arm64",
	"darwin/amd64": "darwin-x64",
	"darwin/arm64": "darwin-arm64",
}

// NodeRuntime is the pinned Node runtime for a GOOS/GOARCH pair. It is the appliance's
// first real artifact and the one this package is proven against end to end.
//
// StripComponents is 1 because the tarball's single top directory repeats the version, and
// a working directory named after a version is a path somebody will eventually build by
// string concatenation. Hydrated Shared: the runtime is read and exec'd, never edited, so
// the second install of the same version costs no bytes.
func NodeRuntime(goos, goarch string) (Artifact, error) {
	key := goos + "/" + goarch
	digest, ok := nodeDigests[key]
	if !ok {
		return Artifact{}, fmt.Errorf("no pinned Node %s for %s; the appliance ships linux and macOS on amd64 and arm64", NodeVersion, key)
	}
	return Artifact{
		Name:            "node-runtime",
		URL:             fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-%s.tar.gz", NodeVersion, NodeVersion, nodePlatform[key]),
		Digest:          digest,
		Kind:            KindTarGz,
		StripComponents: 1,
	}, nil
}
