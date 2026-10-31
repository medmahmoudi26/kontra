package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// CredentialsFile is the file under ~/.kontra (§2.4).
const CredentialsFile = "credentials"

// maxCredentialsFile bounds what LoadCredentials reads; the file holds a few keys and token hashes.
const maxCredentialsFile = 1 << 20

// Credentials is ~/.kontra/credentials (0600, §2.4): API keys (--api-key; `kontra login`, internal/login) and
// enrollment tokens (--enroll) per gate origin, sent only to the origin they were issued for: a node never
// sends a credential issued by one gate to another. A flag's credential is issued by the gate of the same
// command line's --gate, and is kept under that gate's origin.
//
// An enrollment token is single use (§5): once a welcome confirms the enrollment, the token is dropped and
// its sha256 kept in Consumed, so that it is never sent again, not even when a restart passes the same
// --enroll flag (a compose sidecar or a systemd unit restarts with its flags). A token sent in a hello
// whose outcome the node did not learn (the connection ended after the proof, ErrNoOutcome) is listed in
// Unconfirmed until a later handshake settles it (M2 plan audit, gap 1; see Node.Run).
//
// A token waiting for a welcome belongs to the key of the `kontra connect` it was given to (Keys, by key
// id): several nodes may serve one service, each with its own key (§11), and two `kontra connect` with
// different --key under one HOME share this file, so a token given to one is never sent in the other's
// hello, nor confirmed by the other's probe. The API key and Consumed are the origin's.
//
// Several `kontra connect` processes (one per gate, or one per key at a gate) share the file: Update
// changes one origin's entry under an exclusive flock(2), applying the change to what the file holds at
// that moment, so that no process's change is lost.
type Credentials struct {
	path     string
	byOrigin map[string]Credential
}

// Credential is what the node holds for one gate origin.
type Credential struct {
	APIKey string `json:"api_key,omitempty"`
	// Consumed are the sha256 (hex) of tokens a welcome confirmed at this origin: never sent again, by any
	// key.
	Consumed []string `json:"consumed,omitempty"`
	// Keys are the tokens waiting for a welcome, by the key id (§2.2) of the node they were given to.
	Keys map[string]*KeyCredential `json:"keys,omitempty"`
	// OAuthClient is the client kontra login registered at this gate's authorization server (§6.3, RFC 7591), kept so
	// that the next login registers no other. It is no credential: it never goes into a hello.
	OAuthClient *OAuthClient `json:"oauth_client,omitempty"`
}

// OAuthClient is a public client registered by kontra login: the authorization server's issuer and the client_id it
// issued there (spec/tunnel.md §6.3).
type OAuthClient struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// KeyCredential is one key's enrollment tokens at one origin that no welcome has confirmed yet.
type KeyCredential struct {
	Enroll []string `json:"enroll,omitempty"` // sent in this key's next hello
	// Unconfirmed are the sha256 (hex) of tokens this key sent in a hello whose outcome is unknown: its next
	// handshake is the lost-welcome probe (no tokens), whose welcome may confirm them.
	Unconfirmed []string `json:"unconfirmed,omitempty"`
}

func (k *KeyCredential) empty() bool {
	return k == nil || len(k.Enroll) == 0 && len(k.Unconfirmed) == 0
}

func (k *KeyCredential) clone() *KeyCredential {
	if k == nil {
		return nil
	}
	return &KeyCredential{Enroll: slices.Clone(k.Enroll), Unconfirmed: slices.Clone(k.Unconfirmed)}
}

// empty reports whether c holds nothing worth a line in the file.
func (c Credential) empty() bool {
	if c.APIKey != "" || len(c.Consumed) > 0 || c.OAuthClient != nil {
		return false
	}
	for _, k := range c.Keys {
		if !k.empty() {
			return false
		}
	}
	return true
}

func (c Credential) clone() Credential {
	out := Credential{APIKey: c.APIKey, Consumed: slices.Clone(c.Consumed)}
	if c.OAuthClient != nil {
		oc := *c.OAuthClient
		out.OAuthClient = &oc
	}
	if c.Keys != nil {
		out.Keys = make(map[string]*KeyCredential, len(c.Keys))
		for kid, k := range c.Keys {
			out.Keys[kid] = k.clone()
		}
	}
	return out
}

// Key is a copy of one key's waiting tokens (empty when it has none).
func (c Credential) Key(kid string) KeyCredential {
	if k := c.Keys[kid]; k != nil {
		return *k.clone()
	}
	return KeyCredential{}
}

// key is kid's entry, made when missing (Update drops an entry left empty).
func (c *Credential) key(kid string) *KeyCredential {
	if c.Keys == nil {
		c.Keys = map[string]*KeyCredential{}
	}
	k := c.Keys[kid]
	if k == nil {
		k = &KeyCredential{}
		c.Keys[kid] = k
	}
	return k
}

// consumed reports whether a welcome at this origin confirmed the token.
func (c Credential) consumed(token string) bool { return slices.Contains(c.Consumed, TokenHash(token)) }

// tidy drops the key entries left empty.
func (c *Credential) tidy() {
	for kid, k := range c.Keys {
		if k.empty() {
			delete(c.Keys, kid)
		}
	}
	if len(c.Keys) == 0 {
		c.Keys = nil
	}
}

// credentialsDoc is the file's JSON.
type credentialsDoc struct {
	V     int                   `json:"v"`
	Gates map[string]Credential `json:"gates"`
}

// TokenHash is how the credentials file names a token it must not send again: lowercase hex of its
// sha256 (the gate stores the same digest, enrollment_tokens.token_hash).
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// LoadCredentials reads the file (absent or empty is no credential), refusing one readable or writable
// by group or others, as the identity key is (§2.1, §2.4).
func LoadCredentials(path string) (*Credentials, error) {
	c := &Credentials{path: path, byOrigin: map[string]Credential{}}
	fh, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("node: credentials: %w", err)
	}
	defer fh.Close()
	m, err := readCredentials(path, fh)
	if err != nil {
		return nil, err
	}
	c.byOrigin = m
	return c, nil
}

// Path is the file's path.
func (c *Credentials) Path() string { return c.path }

// For returns the credential of a gate origin as last read or written (a copy).
func (c *Credentials) For(origin string) Credential {
	return c.byOrigin[origin].clone()
}

// Update changes the credential of a gate origin and writes the file atomically, 0600: under the file's
// lock it re-reads the file, applies change to that origin's entry as the file holds it now (another
// process's changes included), drops what is left empty (the whole entry, when nothing is), writes a
// temporary file in the same directory and renames it over the file. A missing directory is created 0700.
// It returns the origin's credential as written.
//
// change must express a change, never restore an earlier copy: it adds and removes the tokens and hashes
// of its own key, adds to Consumed, sets the API key. That is what keeps two processes at one origin (two
// keys under one HOME, §11) from erasing each other's records (M2 WP-06 review).
func (c *Credentials) Update(origin string, change func(*Credential)) (Credential, error) {
	if err := ensureDir(filepath.Dir(c.path)); err != nil {
		return Credential{}, err
	}
	target, fh, err := lockCredentials(c.path)
	if err != nil {
		return Credential{}, err
	}
	defer fh.Close() // releases the lock
	m, err := readCredentials(c.path, fh)
	if err != nil {
		return Credential{}, err
	}
	cred := m[origin].clone()
	change(&cred)
	cred.tidy()
	if cred.empty() {
		delete(m, origin)
	} else {
		m[origin] = cred
	}
	b, err := json.MarshalIndent(credentialsDoc{V: 1, Gates: m}, "", "  ")
	if err != nil {
		return Credential{}, fmt.Errorf("node: credentials: %w", err)
	}
	if err := replaceFile(target, append(b, '\n')); err != nil {
		return Credential{}, err
	}
	c.byOrigin = m
	return cred.clone(), nil
}

// readCredentials checks the mode of the open file and parses it.
func readCredentials(path string, fh *os.File) (map[string]Credential, error) {
	fi, err := fh.Stat()
	if err != nil {
		return nil, fmt.Errorf("node: credentials: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("node: credentials: %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("node: credentials: %s has mode %04o: it holds API keys and enrollment tokens and must not "+
			"be readable or writable by group or others (chmod 600 %s)", path, perm, path)
	}
	if _, err := fh.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("node: credentials: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(fh, maxCredentialsFile+1))
	if err != nil {
		return nil, fmt.Errorf("node: credentials: %w", err)
	}
	if len(b) > maxCredentialsFile {
		return nil, fmt.Errorf("node: credentials: %s is over %d bytes", path, maxCredentialsFile)
	}
	m := map[string]Credential{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	var doc credentialsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		// Never quote the file: it holds secrets.
		return nil, fmt.Errorf("node: credentials: %s is not a credentials file this version reads", path)
	}
	if doc.V != 1 {
		return nil, fmt.Errorf("node: credentials: %s has version %d, this version reads 1", path, doc.V)
	}
	for o, cr := range doc.Gates {
		cr.tidy()
		m[o] = cr
	}
	return m, nil
}

// lockCredentials opens (creating it 0600 when missing) the file at path's target and holds an exclusive
// flock on it: closing the returned file releases it. A process that waited while another renamed a new
// file over the old one retakes the lock on the new one.
func lockCredentials(path string) (target string, fh *os.File, err error) {
	target = path
	if t, err := filepath.EvalSymlinks(path); err == nil {
		target = t
	}
	for range 1000 {
		fh, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return "", nil, fmt.Errorf("node: credentials: %w", err)
		}
		if err := lockFile(fh); err != nil {
			fh.Close()
			return "", nil, fmt.Errorf("node: credentials: lock %s: %w", target, err)
		}
		held, err := fh.Stat()
		if err != nil {
			fh.Close()
			return "", nil, fmt.Errorf("node: credentials: %w", err)
		}
		if now, err := os.Lstat(target); err == nil && os.SameFile(held, now) {
			return target, fh, nil
		}
		fh.Close()
	}
	return "", nil, fmt.Errorf("node: credentials: %s keeps being replaced", target)
}

// replaceFile puts data at path atomically: a 0600 temporary file in the same directory, fsync, rename.
func replaceFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("node: credentials: %w", err)
	}
	name := tmp.Name()
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
		return fmt.Errorf("node: credentials: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ensureDir creates a missing directory 0700 (~/.kontra, §2.1). An existing one is left as it is.
func ensureDir(dir string) error {
	fi, err := os.Stat(dir)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("node: %s is not a directory", dir)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("node: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("node: %w", err)
	}
	return nil
}

// maxSecret bounds a secret read from a file or stdin.
const maxSecret = 64 << 10

// ReadSecret reads a flag value given as a value, @file or - (stdin), so a secret need not appear in a
// process list (§2.4). Surrounding white space (a file's trailing newline) is trimmed; what remains must
// be one secret: not empty, without white space or control characters. An error never quotes the secret.
func ReadSecret(v string) (string, error) { return readSecret(v, os.Stdin) }

func readSecret(v string, stdin io.Reader) (string, error) {
	var s string
	switch {
	case v == "-":
		if stdin == nil {
			return "", errors.New("no standard input to read the secret from")
		}
		b, err := io.ReadAll(io.LimitReader(stdin, maxSecret+1))
		if err != nil {
			return "", fmt.Errorf("standard input: %w", err)
		}
		if len(b) > maxSecret {
			return "", fmt.Errorf("standard input holds over %d bytes", maxSecret)
		}
		s = string(b)
	case strings.HasPrefix(v, "@"):
		name := v[1:]
		if name == "" {
			return "", errors.New("@ names no file")
		}
		f, err := os.Open(name)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxSecret+1))
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if len(b) > maxSecret {
			return "", fmt.Errorf("%s holds over %d bytes", name, maxSecret)
		}
		s = string(b)
	default:
		s = v
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("empty")
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("white space or a control character inside the value: give one secret")
	}
	return s, nil
}

// tokenRE is an enrollment token's shape (§5: `ket_` + 32 base62 characters; enroll.ValidToken), service
// and tenant tokens alike. The gate looks up no token of another shape.
var tokenRE = regexp.MustCompile(`^ket_[0-9A-Za-z]{32}$`)
