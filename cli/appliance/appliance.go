// appliance.go — the six roles `kontra up` runs, and the record of what they bound.
//
// ADR 0031 collapsed the compose services into one process. Five arrived as libraries and one did
// not, and each of the six now has a package of its own — a seam the compiler holds, in place of
// one flat `package appliance` in which nothing stopped the bundle builder reaching into the
// key-value store:
//
//	appliance/temporalsrv   Temporal, embedded          was the `temporal` service
//	appliance/objstore      the object store, S3+SigV4  was `seaweed`
//	appliance/kv            the state store, RESP       was `redis`
//	appliance/codec         the payload codec           was `codec-server`
//	appliance/registry      the OCI registry            was `registry`
//	appliance/bundle        the sixth, CARRIED          was `orchestrator-api`
//
// THE SIXTH IS THE ONE THAT IS NOT A LIBRARY, and that is why `bundle` is a build/hydrate/scan
// pipeline rather than a server: the orchestrator is ~60 TypeScript files that ADR 0031 CARRIES
// rather than rewrites, so it ships as a content-addressed bundle and runs as a supervised child
// (child.go). The other five are linked into this binary.
//
// WHAT STAYS HERE IS WHAT BELONGS TO NO SINGLE ROLE: the supervision the sixth needs (child.go),
// Temporal's opt-in Web UI, which is wired FROM three of them and owns none of them
// (temporalui.go), and the record below of what every listener actually bound. `cli/up.go` is
// still the sequence — this package is what it wires, not a second copy of the order.
//
// THE IMPORT RULE, and it is one line: this package may import the six; none of the six may
// import this package or each other, with one deliberate exception — `codec` reads `objstore`,
// because a claim-check ref names a digest and not a location, so the codec has to read the store
// the writers wrote to. That is the only edge in the graph, and it is named in `codec`'s own
// header.
//
// --- THE RECORD -------------------------------------------------------------------------------
//
// The addresses a running appliance bound, published for the commands that are not it (ADR 0031
// §1, issue 18).
//
// THIS EXISTS BECAUSE THE REGISTRY ADDRESS WAS ONLY THE FIRST OF FIVE. `cli/deploy.go` already
// reads `<data-dir>/registry/address` rather than assuming `localhost:5000`, and the comment there
// records why: push and pull resolving one string separately is how an image the daemon does not
// have becomes `no such image` three commands later. Every other address the appliance binds has
// the same shape of customer — `kontra serve --mode docker` has to tell a worker container where
// Temporal, the object store and the state store are — and until this, those three had exactly one
// answer: the compose DNS names `temporal:7233`, `seaweed:8333` and `redis:6379`, none of which
// resolves any more (docker-compose.yml says so in its own header). So a worker started against
// the appliance came up healthy, polled a name that does not exist, and reported as a live
// replica. That is the failure class this whole slice is aimed at.
//
// ONE WRITER, ONE READER, ONE FILE. `kontra up` writes it once every service is bound — so the
// values are what was ACTUALLY bound, never what was asked for — and removes it on the way out.
//
// A KILLED APPLIANCE LEAVES IT BEHIND, deliberately, for registry.ReadAddress's reason: the next
// command resolves the dead address and fails NAMING it, which is a better failure than silently
// falling back to a default and starting workers that poll nothing.
package appliance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// endpointsFileName is the published record, in the data directory's root beside `cas/`,
// `registry/` and the Temporal database — one directory that IS the installation.
const endpointsFileName = "endpoints.json"

// Endpoints is what one running appliance bound. Every field is a dialable address as a client
// would spell it: `host:port` for the two gRPC/RESP surfaces, a URL for the two HTTP ones.
//
// THE ZERO VALUE IS NOT A DEFAULT. A field left empty means "this appliance is not serving that",
// and a caller must treat it as absent rather than substituting a conventional value — the whole
// point is that no second party guesses.
type Endpoints struct {
	// Bind is the IP every listener below was given. Kept as its own field because the
	// REACHABILITY question is asked of it and not of the addresses: a worker container cannot
	// reach a loopback bind, whatever port follows it. See Reachable.
	Bind string `json:"bind"`

	Temporal string `json:"temporal"` // host:port, gRPC
	S3       string `json:"s3"`       // http://host:port
	KV       string `json:"kv"`       // host:port, RESP
	Codec    string `json:"codec"`    // http://host:port
	Registry string `json:"registry"` // host:port, OCI
	API      string `json:"api"`      // http://host:port — the orchestrator child, when one runs

	// PID is the `kontra up` that wrote this. Not used for control; it is what lets an operator
	// tell a live record from one a killed appliance left behind.
	PID int `json:"pid"`
}

// WriteEndpoints publishes the record. Called once by `kontra up`, after every listener is bound.
func WriteEndpoints(dataDir string, e Endpoints) error {
	if dataDir == "" {
		return errors.New("appliance: WriteEndpoints needs a data directory")
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dataDir, endpointsFileName), append(b, '\n'))
}

// RemoveEndpoints withdraws the record on an orderly stop. A missing file is not an error: two
// stops of one appliance, or a stop after a crash that already lost it, are both fine.
func RemoveEndpoints(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, endpointsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ReadEndpoints returns the record an appliance running against dataDir published, if there is
// one. The bool is "a record exists", never "the appliance is alive" — see the file header on why
// a stale record is the failure worth having.
func ReadEndpoints(dataDir string) (Endpoints, bool) {
	if dataDir == "" {
		return Endpoints{}, false
	}
	b, err := os.ReadFile(filepath.Join(dataDir, endpointsFileName))
	if err != nil {
		return Endpoints{}, false
	}
	var e Endpoints
	if err := json.Unmarshal(b, &e); err != nil {
		return Endpoints{}, false
	}
	// A record with no Temporal address is not an appliance; it is a truncated or foreign file,
	// and answering `true` for it would hand a caller an empty string to dial.
	if e.Temporal == "" {
		return Endpoints{}, false
	}
	return e, true
}

// ReachableFromContainer reports whether an address this appliance bound can be dialled from a
// container on a bridge network, and says why not when it cannot.
//
// THE ANSWER IS A PROPERTY OF THE BIND AND NOTHING ELSE. ADR 0031 §3 makes `127.0.0.1` the
// default and gives the reason — there is no remote caller, so there is no credential question —
// and that default is right for every surface an operator touches. It is wrong for exactly one
// customer: an actor Worker, which ADR 0031 §2 keeps in a container on purpose, and a container's
// loopback is its OWN.
//
// This is a function rather than a check at the call site because the failure it prevents is
// silent. A worker container handed `127.0.0.1:7233` starts, retries a connection nothing will
// ever answer, and appears in `docker ps` as a running replica — the same shape as the
// `poller: NONE` this repo has already paid for twice.
func (e Endpoints) ReachableFromContainer() error {
	host := e.Bind
	if host == "" {
		var err error
		if host, _, err = net.SplitHostPort(e.Temporal); err != nil {
			return fmt.Errorf("appliance endpoints record has no bind address and %q is not host:port", e.Temporal)
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name, not an address. `host.docker.internal` and a DNS name are both reachable in
		// their own way and neither is ours to second-guess.
		return nil
	}
	if !ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("the appliance is bound to %s, which a worker CONTAINER cannot reach — its loopback is its own, not this host's.\n"+
		"  Restart it on an address the docker bridge can see, which is still not an address off this box:\n"+
		"      kontra up --bind %s   (plus the ports you are already passing)\n"+
		"  Or pass the endpoints yourself if the workers are somewhere else entirely.", host, DefaultBridgeBind)
}

// DefaultBridgeBind is docker0's gateway — the address the daemon gives every container on the
// default bridge, and the one docker-compose.yml's own header already tells an operator to bind
// during the parity window.
//
// IT IS NOT AN OPENING. 172.17.0.0/16 is a host-local bridge: it is reachable from this box and
// from containers on it, and there is no route to it from anywhere else. What it costs relative
// to loopback is precisely "any container on this machine can reach the control plane", which is
// the premise of running actor Workers as containers at all.
const DefaultBridgeBind = "172.17.0.1"

// writeFileAtomic is temp-then-rename, and the record needs it for the reason `kontra up` names
// at its second call: the API line is added once the child answers, and a reader must see the
// five addresses or the six, never a half-written six.
//
// A COPY OF registry/index.go's, deliberately. The two are 25 lines of temp-then-rename with
// different reasons to exist — a tag flip that must be all or nothing there, a record that gains
// a field here — and neither package may import the other under this file's import rule. The
// dependency that would remove the copy costs more than the copy.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
