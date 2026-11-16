package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// A sealed part through the bucket (D167, D171, D181; ext-sealed.md §3; spec/tunnel.md §8.3): a 2 MiB file part
// sealed by the calling node (replica B) goes to sealedecho hosted by a node on replica A and comes back as an
// artifact. The bucket holds age ciphertext only, every object is a blobs row of the task, the hosting node fetched
// the part over a blob stream (the gate logs serving it to that session) and dialed nothing but the gate and its
// service, and the local client gets the original bytes (the same SHA-256), as the service did.
func TestSealedBlob(t *testing.T) {
	h := start(t, Options{Tunnel: map[string]TunnelService{"vault": {Sealed: "true"}}, Blobs: true, Peer: true})
	ctx := ctxFor(t, 5*time.Minute)
	service, err := h.StartEchoService()
	if err != nil {
		t.Fatal(err)
	}
	host, hostSession := startNode(t, ctx, h, "host", map[string]string{"vault": service.URL})
	waitSealedCard(t, ctx, h, "vault")
	h.Front.Route(h.Gates[1])
	callingNode, err := h.StartNode(ctx, NodeOptions{Name: "caller", APIKey: h.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	local, err := callingNode.Local(30 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	file := make([]byte, 2<<20)
	if _, err := rand.Read(file); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(file)
	c := localA2A(t, ctx, local, "vault")
	task := sendTask(t, ctx, c, a2a.NewMessage(a2a.MessageRoleUser,
		&a2a.Part{Content: a2a.Raw(file), MediaType: "application/octet-stream", Filename: "two.bin"}))
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the task: %+v", task.Status)
	}
	if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 || sha256.Sum256(task.Artifacts[0].Parts[0].Raw()) != sum {
		t.Fatal("the local client did not get the original 2 MiB back")
	}
	turns := service.Turns()
	if len(turns) != 1 || sha256.Sum256(turns[0][0].Raw()) != sum || turns[0][0].Filename != "two.bin" {
		t.Fatal("the service did not get the original 2 MiB")
	}
	refs := texts(t, h, `select ref from kontra.blobs where task_id = $1`, string(task.ID))
	if len(refs) < 2 {
		t.Fatalf("%d blobs rows for the task, want the part and the artifact", len(refs))
	}
	objs, err := h.Bucket.Objects()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != len(refs) {
		t.Fatalf("%d objects in the bucket, %d blobs rows", len(objs), len(refs))
	}
	for k, b := range objs {
		ref := k[strings.LastIndexByte(k, '/')+1:]
		if !strings.Contains(k, "/"+string(task.ID)+"/") || !containsName(refs, ref) {
			t.Fatalf("bucket object %s is not a blobs row of the task", k)
		}
		if !bytes.HasPrefix(b, []byte("age-encryption.org/v1\n")) || len(b) < 2<<20 || bytes.Contains(b, file[:64]) {
			t.Fatalf("bucket object %s is not age ciphertext of the part", k)
		}
	}
	// The blob streams: the hosting node fetched a ref of the task over its own session.
	served := 0
	for _, ref := range refs {
		served += gateLogLines(h, "blob stream served", ref, hostSession)
	}
	if served == 0 {
		t.Fatal("the gate served no blob stream of the task to the hosting node")
	}
	// The hosting node dialed nothing but the gate (through the Front) and its service.
	serviceAddr := strings.TrimPrefix(service.URL, "http://")
	for _, s := range checkNoListener(t, host.Pid(), "the hosting node") {
		if s.State == tcpListen {
			continue
		}
		_, localPort, _ := net.SplitHostPort(s.Local)
		local, _ := callingNodeListenPort(host)
		if s.Remote != h.Front.Addr && s.Remote != serviceAddr && localPort != local {
			t.Fatalf("the hosting node has a connection to %s (local %s): it dials only the gate and its service", s.Remote, s.Local)
		}
	}
}

// callingNodeListenPort is the port of a node's own calling endpoint, from its stdout.
func callingNodeListenPort(n *Node) (string, bool) {
	m := callingAtRE.FindAllStringSubmatch(readFile(n.OutPath), -1)
	if len(m) == 0 {
		return "", false
	}
	return m[len(m)-1][1], true
}
