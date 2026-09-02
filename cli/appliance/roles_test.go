// roles_test.go — the three roles a Temporal UI test has to have running behind it.
//
// THESE ARE THE HELPERS THE SPLIT COST, and they are the honest price of it. temporalui_test.go
// asserts things ABOUT THE WIRING — that the browser is handed the address the codec's listener
// bound, that the origin the codec admits is the URL the UI is served from — so it needs a real
// Temporal, a real object store and a real codec, and it can no longer reach the `startTestS3`
// its own package used to hold. Each helper below is the sibling package's own three lines,
// re-typed against that package's exported API; none of them reaches past it.
package appliance

import (
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/medmahmoudi26/kontra-local/cli/appliance/codec"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/objstore"
	"github.com/medmahmoudi26/kontra-local/cli/appliance/temporalsrv"
)

// startForTest boots an embedded Temporal and returns it with an idempotent stop. The `once` is
// not decoration: a test that stops the server mid-run would otherwise have cleanup stop it a
// second time, and a Temporal server shut down twice panics on its own closed channels.
func startForTest(t *testing.T, opts temporalsrv.Options) (*temporalsrv.Server, func()) {
	t.Helper()
	srv, err := temporalsrv.Start(opts)
	if err != nil {
		t.Fatalf("starting the embedded server: %v", err)
	}
	var once sync.Once
	stop := func() { once.Do(srv.Stop) }
	t.Cleanup(stop)
	return srv, stop
}

// freeTestPort picks a port a role can then bind. The listener is closed before we return it,
// which is what the roles' own pre-flight checks expect to find.
func freeTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	return port
}

// startTestS3 brings up the object store the codec reads. Never the default port: a developer box
// running the compose stack has SeaweedFS on 8333.
func startTestS3(t *testing.T) *objstore.Server {
	t.Helper()
	srv, err := objstore.Start(objstore.Options{
		DataDir: t.TempDir(),
		Port:    freeTestPort(t),
		Logf:    func(format string, args ...any) { t.Logf("store: "+format, args...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

// startTestCodec brings a codec up on a free port over the given store. Never the default port:
// the compose `codec-server` service published 18234 and a developer box may still have it.
func startTestCodec(t *testing.T, store *objstore.Server, opts ...func(*codec.Options)) *codec.Server {
	t.Helper()
	claims, err := store.Backing(objstore.DefaultS3Bucket)
	if err != nil {
		t.Fatal(err)
	}
	o := codec.Options{
		Store: claims,
		Port:  freeTestPort(t),
		Logf:  func(format string, args ...any) { t.Logf("codec: "+format, args...) },
	}
	for _, f := range opts {
		f(&o)
	}
	srv, err := codec.Start(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}
