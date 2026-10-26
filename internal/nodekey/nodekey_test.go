package nodekey

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"kontra.sh/kontra/internal/tunnel"
)

var t0 = time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)

func keyPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "home", DirName, FileName)
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func encryptTo(t *testing.T, recipient string, msg []byte) []byte {
	t.Helper()
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(msg)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// decryptWithFile opens ct with the identities age itself reads from the file (`age -d -i`).
func decryptWithFile(path string, ct []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(ct), ids...)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// §2.1: the first run creates a 0600 file in a 0700 directory, in age-keygen's format with the key id
// as a third comment line; the next run loads the same key.
func TestLoadOrCreate(t *testing.T) {
	old := syscall.Umask(0) // the modes must not depend on the umask
	defer syscall.Umask(old)
	path := keyPath(t)
	id, created, err := LoadOrCreate(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !created || id.Path != path || !id.Created.Equal(t0) || len(id.Older) != 0 {
		t.Fatalf("created %v, %+v", created, id)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %04o", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %04o", di.Mode().Perm())
	}
	got := lines(t, path)
	want := []string{
		"# created: 2026-10-25T10:00:00Z",
		"# public key: " + id.Recipient(),
		"# kontra key id: " + id.KeyID(),
		id.Active.String(),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("file:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !regexp.MustCompile(`^k1_[a-z2-7]{26}$`).MatchString(id.KeyID()) || id.KeyID() != tunnel.KeyID(id.Recipient()) {
		t.Fatalf("key id %q", id.KeyID())
	}
	if !strings.HasPrefix(id.Active.String(), "AGE-SECRET-KEY-1") || !strings.HasPrefix(id.Recipient(), "age1") {
		t.Fatalf("key %s", id.Recipient())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}

	again, created, err := LoadOrCreate(path, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created || again.Recipient() != id.Recipient() || !again.Created.Equal(t0) {
		t.Fatalf("second run: created %v, %s", created, again.Recipient())
	}
}

// Concurrent first runs agree on one key: exactly one creates it, and none reads half a file.
func TestLoadOrCreateConcurrent(t *testing.T) {
	path := keyPath(t)
	const n = 8
	var wg sync.WaitGroup
	recips := make([]string, n)
	createdBy := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, created, err := LoadOrCreate(path, t0)
			errs[i], createdBy[i] = err, created
			if err == nil {
				recips[i] = id.Recipient()
			}
		}()
	}
	wg.Wait()
	var creators int
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if recips[i] != recips[0] {
			t.Fatalf("two keys: %s and %s", recips[0], recips[i])
		}
		if createdBy[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d runs created the key", creators)
	}
}

// §2.1: the CLI refuses a key file readable or writable by group or others.
func TestLoadRefusesOpenModes(t *testing.T) {
	path := keyPath(t)
	id, _, err := LoadOrCreate(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o620, 0o602, 0o644, 0o660, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load accepted mode %04o", mode)
		}
		if err := CheckMode(path); err == nil {
			t.Fatalf("CheckMode accepted mode %04o", mode)
		}
		if _, _, err := LoadOrCreate(path, t0); err == nil {
			t.Fatalf("LoadOrCreate accepted mode %04o", mode)
		}
		if _, err := Rotate(path, t0); err == nil {
			t.Fatalf("Rotate accepted mode %04o", mode)
		}
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		os.Chmod(path, mode)
		got, err := Load(path)
		if err != nil {
			t.Fatalf("mode %04o: %v", mode, err)
		}
		if got.Recipient() != id.Recipient() {
			t.Fatal("another key")
		}
	}
	if err := CheckMode(filepath.Dir(path)); err == nil {
		t.Fatal("CheckMode accepted a directory")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("Load of an absent file")
	}
	// A mounted secret is a symbolic link: its target's mode counts.
	link := filepath.Join(t.TempDir(), "secret")
	os.Chmod(path, 0o600)
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err != nil {
		t.Fatalf("a link to a 0600 key: %v", err)
	}
	os.Chmod(path, 0o644)
	if _, err := Load(link); err == nil {
		t.Fatal("a link to a 0644 key")
	}
}

// §2.1: `age -d -i identity.key` opens what is sealed to the recipient, and the scalar PrivateKey decodes
// is the recipient's.
func TestAgeInterop(t *testing.T) {
	path := keyPath(t)
	id, _, err := LoadOrCreate(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("a sealed part")
	pt, err := decryptWithFile(path, encryptTo(t, id.Recipient(), msg))
	if err != nil || !bytes.Equal(pt, msg) {
		t.Fatalf("age.Decrypt with the file's identities: %q, %v", pt, err)
	}
	priv, err := id.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tunnel.ParseRecipient(id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if !priv.PublicKey().Equal(pub) {
		t.Fatal("PrivateKey().PublicKey() is not ParseRecipient(Recipient())")
	}
	// The key proves possession (§4.1).
	info := tunnel.Transcript{Origin: "https://gate.example", CB: tunnel.CBNone, Hello: []byte("{}"), Session: "s",
		GateNonce: make([]byte, 32)}.Info()
	enc, want, err := tunnel.Encapsulate(pub, info, tunnel.LabelPoP)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := tunnel.Prove(enc, priv, info, tunnel.LabelPoP)
	if err != nil || !tunnel.Accept(proof, want) {
		t.Fatalf("the file's key does not prove possession: %v", err)
	}
}

// A file age-keygen wrote (two comment lines, a local offset) loads; a file whose first key is a
// post-quantum one, or that age cannot read, is refused.
func TestLoadForeignFiles(t *testing.T) {
	dir := t.TempDir()
	k, _ := age.GenerateX25519Identity()
	keygen := filepath.Join(dir, "keygen.key")
	os.WriteFile(keygen, []byte("# created: 2021-01-02T15:30:45+01:00\n# public key: "+k.Recipient().String()+"\n"+k.String()+"\n"), 0o600)
	id, err := Load(keygen)
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient() != k.Recipient().String() || !id.Created.Equal(time.Date(2021, 1, 2, 14, 30, 45, 0, time.UTC)) {
		t.Fatalf("loaded %s created %v", id.Recipient(), id.Created)
	}
	hy, _ := age.GenerateHybridIdentity()
	for name, content := range map[string]string{
		"pq first":   hy.String() + "\n" + k.String() + "\n",
		"empty":      "",
		"only notes": "# created: 2021-01-02T15:30:45Z\n",
		"garbage":    "AGE-SECRET-KEY-1NOPE\n",
		"spaces":     k.String() + " \n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		os.WriteFile(p, []byte(content), 0o600)
		if _, err := Load(p); err == nil {
			t.Fatalf("%s: loaded", name)
		}
		if _, _, err := LoadOrCreate(p, t0); err == nil {
			t.Fatalf("%s: LoadOrCreate replaced or accepted it", name)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Fatalf("%s: the file was changed", name)
		}
	}
	// A post-quantum key below the active one is kept for decryption, not offered as an X25519 key.
	mixed := filepath.Join(dir, "mixed.key")
	os.WriteFile(mixed, []byte(k.String()+"\n"+hy.String()+"\n"), 0o600)
	id, err = Load(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Older) != 0 || len(id.Identities()) != 2 {
		t.Fatalf("older %d, identities %d", len(id.Older), len(id.Identities()))
	}
}

// §12: --rotate writes a new first key; the old one stays below it, decrypt-only, with a "# rotated:"
// stamp, and still opens what was sealed to it and proves for rotate_from.
func TestRotate(t *testing.T) {
	path := keyPath(t)
	old, _, err := LoadOrCreate(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	sealedToOld := encryptTo(t, old.Recipient(), []byte("before"))
	at := t0.Add(48 * time.Hour)
	id, err := Rotate(path, at)
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient() == old.Recipient() || !id.Created.Equal(at) {
		t.Fatalf("Active is still the old key, or created %v", id.Created)
	}
	if len(id.Older) != 1 || id.Older[0].Recipient().String() != old.Recipient() {
		t.Fatalf("Older = %v", id.Older)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recipient() != id.Recipient() || len(loaded.Older) != 1 {
		t.Fatal("the file does not hold what Rotate returned")
	}
	got := lines(t, path)
	want := []string{
		"# created: 2026-10-27T10:00:00Z",
		"# public key: " + id.Recipient(),
		"# kontra key id: " + id.KeyID(),
		id.Active.String(),
		"# created: 2026-10-25T10:00:00Z",
		"# public key: " + old.Recipient(),
		"# kontra key id: " + old.KeyID(),
		"# rotated: 2026-10-27T10:00:00Z",
		old.Active.String(),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("file:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rotation %04o", fi.Mode().Perm())
	}
	if pt, err := decryptWithFile(path, sealedToOld); err != nil || string(pt) != "before" {
		t.Fatalf("the old key no longer decrypts: %q, %v", pt, err)
	}
	if pt, err := decryptWithFile(path, encryptTo(t, id.Recipient(), []byte("after"))); err != nil || string(pt) != "after" {
		t.Fatalf("the new key does not decrypt: %v", err)
	}
	ids := id.Identities()
	if len(ids) != 2 || ids[0].(*age.X25519Identity).Recipient().String() != id.Recipient() {
		t.Fatal("Identities is not active first")
	}

	priv, err := id.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	oldPriv, err := id.OldPrivateKey(old.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	oldPub, _ := tunnel.ParseRecipient(old.Recipient())
	newPub, _ := tunnel.ParseRecipient(id.Recipient())
	if !oldPriv.PublicKey().Equal(oldPub) || !priv.PublicKey().Equal(newPub) {
		t.Fatal("a scalar does not match its recipient")
	}
	info := tunnel.Transcript{Origin: "https://gate.example", CB: tunnel.CBNone, Hello: []byte("{}"), Session: "s",
		GateNonce: make([]byte, 32)}.Info()
	encOld, wantOld, err := tunnel.Encapsulate(oldPub, info, tunnel.LabelPoPOld)
	if err != nil {
		t.Fatal(err)
	}
	proofOld, err := tunnel.Prove(encOld, oldPriv, info, tunnel.LabelPoPOld)
	if err != nil || !tunnel.Accept(proofOld, wantOld) {
		t.Fatalf("proof_old with the rotated key: %v", err)
	}
	if _, err := id.OldPrivateKey(id.Recipient()); err == nil {
		t.Fatal("OldPrivateKey of the active key")
	}
	if _, err := id.OldPrivateKey("age1nope"); err == nil {
		t.Fatal("OldPrivateKey of an unknown key")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

// §12, PRD §8: the old line goes only after 24 h; the active key and an unstamped key never go.
func TestPrune(t *testing.T) {
	path := keyPath(t)
	first, _, err := LoadOrCreate(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Rotate(path, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	third, err := Rotate(path, t0.Add(10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sealedToFirst := encryptTo(t, first.Recipient(), []byte("first"))
	before, _ := os.ReadFile(path)

	changed, err := Prune(path, t0.Add(time.Hour+RotationOverlap-time.Second))
	if err != nil || changed {
		t.Fatalf("Prune before 24 h: %v, %v", changed, err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatal("Prune before 24 h changed the file")
	}
	if pt, err := decryptWithFile(path, sealedToFirst); err != nil || string(pt) != "first" {
		t.Fatalf("23:59:59 after rotation the old key no longer decrypts: %v", err)
	}

	changed, err = Prune(path, t0.Add(time.Hour+RotationOverlap+time.Second))
	if err != nil || !changed {
		t.Fatalf("Prune after 24 h: %v, %v", changed, err)
	}
	id, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.Recipient() != third.Recipient() || len(id.Older) != 1 || id.Older[0].Recipient().String() != second.Recipient() {
		t.Fatalf("after pruning the first key: active %s, older %v", id.Recipient(), id.Older)
	}
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(first.Recipient())) || bytes.Contains(b, []byte(first.Active.String())) {
		t.Fatal("the pruned key's lines are still in the file")
	}
	if _, err := decryptWithFile(path, sealedToFirst); err == nil {
		t.Fatal("the pruned key still decrypts")
	}

	changed, err = Prune(path, t0.Add(10*time.Hour+RotationOverlap))
	if err != nil || !changed {
		t.Fatalf("Prune of the second key at exactly 24 h: %v, %v", changed, err)
	}
	id, _ = Load(path)
	if id.Recipient() != third.Recipient() || len(id.Older) != 0 {
		t.Fatal("the active key was pruned, or the second one kept")
	}
	got := lines(t, path)
	if len(got) != 4 || got[3] != third.Active.String() {
		t.Fatalf("file after pruning everything old:\n%s", strings.Join(got, "\n"))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after pruning %04o", fi.Mode().Perm())
	}
	if changed, err := Prune(path, t0.Add(1000*time.Hour)); err != nil || changed {
		t.Fatalf("Prune with nothing to drop: %v, %v", changed, err)
	}

	// A decrypt-only key the operator added without a stamp is never pruned.
	extra, _ := age.GenerateX25519Identity()
	b, _ := os.ReadFile(path)
	os.WriteFile(path, append(b, []byte("# my old laptop key\n"+extra.String()+"\n# trailing note\n")...), 0o600)
	if changed, err := Prune(path, t0.Add(1000*time.Hour)); err != nil || changed {
		t.Fatalf("Prune dropped an unstamped key: %v, %v", changed, err)
	}
	// Rotating keeps it and the operator's lines.
	if _, err := Rotate(path, t0.Add(2000*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Contains(b, []byte("# my old laptop key\n"+extra.String()+"\n# trailing note\n")) {
		t.Fatalf("rotation lost the operator's lines:\n%s", b)
	}
}

// --key, then $KONTRA_KEY, then ~/.kontra/identity.key (§2.1).
func TestResolve(t *testing.T) {
	env := map[string]string{"HOME": "/home/node", EnvPath: "/run/secrets/kontra.key"}
	getenv := func(k string) string { return env[k] }
	if p, err := Resolve("/etc/kontra/key", getenv); err != nil || p != "/etc/kontra/key" {
		t.Fatalf("flag: %q, %v", p, err)
	}
	if p, err := Resolve("", getenv); err != nil || p != "/run/secrets/kontra.key" {
		t.Fatalf("env: %q, %v", p, err)
	}
	delete(env, EnvPath)
	if p, err := Resolve("", getenv); err != nil || p != "/home/node/.kontra/identity.key" {
		t.Fatalf("default: %q, %v", p, err)
	}
	t.Setenv(EnvPath, "/from/os")
	if p, err := Resolve("", nil); err != nil || p != "/from/os" {
		t.Fatalf("os.Getenv: %q, %v", p, err)
	}
}

// A decoding fault cannot give a scalar for another recipient: PrivateKey checks it.
func TestPrivateKeyIsChecked(t *testing.T) {
	for range 20 {
		k, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		priv, err := privateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := tunnel.FormatRecipient(priv.PublicKey())
		if r != k.Recipient().String() {
			t.Fatalf("scalar of %s is %s", k.Recipient(), r)
		}
	}
	if _, err := privateKey(nil); err == nil {
		t.Fatal("privateKey(nil)")
	}
	if _, err := (&Identity{}).OldPrivateKey("age1x"); err == nil {
		t.Fatal("OldPrivateKey on an empty identity")
	}
}
