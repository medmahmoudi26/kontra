package node

import (
	"kontra.sh/kontra/internal/unimpl"
)

// Credentials is ~/.kontra/credentials (0600, §2.4): API keys (--api-key; `kontra login` from M4) and
// enrollment tokens (--enroll) per gate origin, sent only to the origin they were issued for: a node never
// sends a credential issued by one gate to another. An enrollment token is kept until a welcome confirms
// the enrollment, then dropped.
type Credentials struct {
	path     string
	byOrigin map[string]Credential
}

// Credential is what the node holds for one gate origin.
type Credential struct {
	APIKey string   `json:"api_key,omitempty"`
	Enroll []string `json:"enroll,omitempty"`
}

// LoadCredentials reads the file (absent is empty), refusing one readable by group or others.
func LoadCredentials(path string) (*Credentials, error) {
	return nil, unimpl.Error("node.LoadCredentials", "WP-06")
}

// For returns the credential of a gate origin.
func (c *Credentials) For(origin string) Credential {
	panic(unimpl.Error("node.Credentials.For", "WP-06"))
}

// Save writes the credential of a gate origin atomically, 0600.
func (c *Credentials) Save(origin string, cred Credential) error {
	return unimpl.Error("node.Credentials.Save", "WP-06")
}

// ReadSecret reads a flag value given as a value, @file or - (stdin), so a secret need not appear in a
// process list (§2.4).
func ReadSecret(v string) (string, error) {
	return "", unimpl.Error("node.ReadSecret", "WP-06")
}
