package hostengine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeEnv presents an operator's shell without mutating this process's. AssertBackend takes an Env
// for exactly this reason: the thing under test is a decision about an environment, and a test that
// had to `os.Setenv` its way there could not run beside another that set the same variable.
func fakeEnv(kv map[string]string) Env {
	return func(k string) string { return kv[k] }
}

func TestBackendIsThisInstallationsOwnStateDirectory(t *testing.T) {
	got := Backend("/home/me/.kontra")
	want := "file:///home/me/.kontra/state"
	if got != want {
		t.Fatalf("Backend = %q, want %q — get.sh:301 and control/pulumi/README.md:243 both spell it "+
			"$KONTRA_HOME/state, and a third spelling is two backends for one install", got, want)
	}
}

func TestAssertBackendAnswersTheInstallationsBackendOnACleanShell(t *testing.T) {
	root := t.TempDir()
	got, err := AssertBackend(root, fakeEnv(map[string]string{"HOME": root}))
	if err != nil {
		t.Fatalf("a shell with no Pulumi configuration at all is the SAFEST state there is, and it was "+
			"refused: %v", err)
	}
	if got != Backend(root) {
		t.Fatalf("AssertBackend = %q, want %q", got, Backend(root))
	}
}

// THE ONE SETTING THAT CANNOT BE UNDONE BY ANYTHING THIS COMMAND DOES. Measured on 3.244.0
// (get.sh:307-311): an exported PULUMI_BACKEND_URL beats a later `pulumi login` AND a set
// PULUMI_ACCESS_TOKEN. So a hosted one exported into the shell means the converge goes there, and the
// only safe answer is to stop.
func TestAssertBackendRefusesAHostedBackendURL(t *testing.T) {
	root := t.TempDir()
	_, err := AssertBackend(root, fakeEnv(map[string]string{
		"HOME":               root,
		"PULUMI_BACKEND_URL": "https://api.pulumi.com",
	}))
	if err == nil {
		t.Fatal("an exported hosted backend URL must stop the converge")
	}
	for _, want := range []string{"Pulumi Cloud", "unset PULUMI_BACKEND_URL", Backend(root)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the trap and both ways out; %q missing from:\n%v", want, err)
		}
	}
}

// An explicitly stated self-managed backend is HONOURED, not overridden. Silently ignoring a setting
// somebody exported on purpose is its own trap — and it is the escape hatch get.sh:340-342 offers for
// the token refusal below.
func TestAssertBackendHonoursAnExplicitSelfManagedBackend(t *testing.T) {
	root := t.TempDir()
	mine := "file:///srv/pulumi-state"
	got, err := AssertBackend(root, fakeEnv(map[string]string{
		"HOME":                 root,
		"PULUMI_BACKEND_URL":   mine,
		"PULUMI_ACCESS_TOKEN":  "pul-deadbeef",
		"PULUMI_CONFIG_SOMETH": "noise",
	}))
	if err != nil {
		t.Fatalf("a stated file:// backend is the documented way to say 'not the cloud': %v", err)
	}
	if got != mine {
		t.Fatalf("AssertBackend = %q, want the stated %q", got, mine)
	}
}

// ADR 0052 §1's acceptance criterion, verbatim: "With PULUMI_ACCESS_TOKEN set and no local login,
// kontra up refuses with a message naming the Pulumi Cloud trap — it does not converge."
//
// This engine states its own backend, so the token is already beaten — the refusal is still right,
// because the operator's shell is then configured so that every invocation this repository DOCUMENTS
// by hand (control/pulumi/README.md:293-299) goes to a hosted account.
func TestAssertBackendRefusesAnAmbientAccessToken(t *testing.T) {
	root := t.TempDir()
	_, err := AssertBackend(root, fakeEnv(map[string]string{
		"HOME":                root,
		"PULUMI_ACCESS_TOKEN": "pul-deadbeef",
	}))
	if err == nil {
		t.Fatal("an ambient PULUMI_ACCESS_TOKEN must refuse, not converge")
	}
	if strings.Contains(err.Error(), "pul-deadbeef") {
		t.Error("the refusal quotes the token back — an error string is printed, logged and pasted into issues")
	}
	for _, want := range []string{"Pulumi Cloud", "env -u PULUMI_ACCESS_TOKEN", "state included"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the trap and the remedy; %q missing from:\n%v", want, err)
		}
	}
}

// credentials.json's `current` is what a machine logged in to Pulumi Cloud reads back. Only a HOSTED
// value is refused; a DIY backend there belongs to whatever else the operator does with Pulumi.
func TestAssertBackendRefusesAHostedLoginAndIgnoresADIYOne(t *testing.T) {
	root := t.TempDir()
	pulumiHome := filepath.Join(root, ".pulumi")
	if err := os.MkdirAll(pulumiHome, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(pulumiHome, "credentials.json")
	env := fakeEnv(map[string]string{"HOME": root, "PULUMI_HOME": pulumiHome})

	write := func(body string) {
		if err := os.WriteFile(creds, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"current":"https://api.pulumi.com","accessTokens":{}}`)
	_, err := AssertBackend(root, env)
	if err == nil {
		t.Fatal("a machine logged in to Pulumi Cloud must be named, not ignored")
	}
	if !strings.Contains(err.Error(), creds) || !strings.Contains(err.Error(), "pulumi login") {
		t.Errorf("the refusal must name the file it read and the one-line fix:\n%v", err)
	}

	// THE MEASURED STATE OF THIS BOX, and it must not refuse: `~/.pulumi/credentials.json` here has
	// `"current": "file:///tmp/tmp.7BDTntBkgF"`, a scratch backend an earlier harness left behind.
	// That is a dead `file://` URL, which is somebody else's problem and not a hosted login.
	write(`{"current":"file:///tmp/tmp.7BDTntBkgF","accessTokens":{"file:///tmp/tmp.7BDTntBkgF":""}}`)
	if _, err := AssertBackend(root, env); err != nil {
		t.Fatalf("a DIY current backend is not a hosted login and must not refuse: %v", err)
	}

	// A file that is not JSON is not evidence of a hosted login either — refusing on a corrupt
	// unrelated dotfile would make `kontra up` fail for a reason it cannot explain.
	write(`not json at all`)
	if _, err := AssertBackend(root, env); err != nil {
		t.Fatalf("an unparseable credentials.json must not refuse: %v", err)
	}
}

// --- the passphrase -----------------------------------------------------------------------------

// A ROTATED PASSPHRASE MAKES AN EXISTING STACK UNREADABLE: `Pulumi.<stack>.yaml`'s first line is an
// encryptionsalt derived from it (verified on 3.244.0), and a regenerated one does not fail loudly —
// it fails at the next read of a secret, long after the file that could have decrypted it was
// overwritten. So this mints once and then only ever reads.
func TestPassphraseIsMintedOnceAndThenKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	first, err := Passphrase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 {
		t.Fatalf("a %d-character passphrase is not 32 bytes of randomness", len(first))
	}
	second, err := Passphrase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("the passphrase was regenerated on the second call, which is how a stack's secrets " +
			"become permanently unreadable")
	}
	info, err := os.Stat(filepath.Join(dir, PassphraseFileName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the passphrase file is mode %04o, want 0600", info.Mode().Perm())
	}
}

// IT IS NOT UNDER state/, AND THAT IS THE WHOLE REASON FOR A SEPARATE PATH: the passphrase decrypts
// the state directory, so a backup or an `scp` of state/ that carried the key beside the ciphertext
// would be a copy of both halves.
func TestPassphraseIsNotKeptInsideTheStateDirectory(t *testing.T) {
	root := t.TempDir()
	lay := NewLayout(root)
	if _, err := Passphrase(lay.Dir); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, StateDirName)
	if strings.HasPrefix(filepath.Join(lay.Dir, PassphraseFileName), stateDir+string(os.PathSeparator)) {
		t.Fatalf("the passphrase lives under %s — the directory it decrypts", stateDir)
	}
}

// A WIDER MODE IS REFUSED AND NOT REPAIRED, which is `config.go:733`'s rule and
// `secrets/keyring.ts`'s ("a world-readable key is a fact about the machine somebody has to see").
// Repairing it silently would hide that something else had already read it.
func TestPassphraseRefusesAFileOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not mean the same thing here, and Passphrase skips the check for that reason")
	}
	dir := filepath.Join(t.TempDir(), "engine")
	if _, err := Passphrase(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, PassphraseFileName)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Passphrase(dir)
	if err == nil {
		t.Fatal("a world-readable passphrase was used rather than refused")
	}
	for _, want := range []string{"0644", "chmod 600", StateDirName} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must say what is wrong, what it protects and how to fix it; %q missing from:\n%v", want, err)
		}
	}
}

// An EMPTY file is not a passphrase. `workspace.ts:100-104`: "a DIY backend cannot use the default
// secrets provider, and an empty passphrase silently changes how secrets encrypt."
func TestPassphraseTreatsAnEmptyFileAsAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "engine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PassphraseFileName), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Passphrase(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("an empty passphrase file was accepted as an empty passphrase")
	}
}
