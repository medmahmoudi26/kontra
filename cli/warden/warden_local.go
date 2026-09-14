package warden

// warden_local.go — documented local-only bootstrap for dockerFleet (ADR 0047).
//
// A Compose Warden already holds the host Docker socket. Enrolment against the Fleet CA is the
// production path; `--local` mints a self-signed identity with the same URI SAN grammar so
// `loadIdentity` and Temporal namespace selection keep working without a CA round-trip.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func bootstrapLocalIdentity(state, namespace, controller string) (*wardenIdentity, error) {
	if id, err := loadIdentity(state); err == nil {
		return id, nil
	}
	if namespace == "" {
		namespace = os.Getenv("KONTRA_NAMESPACE")
	}
	if namespace == "" {
		namespace = "default"
	}
	if controller == "" {
		controller = os.Getenv("KONTRA_ORCHESTRATOR_URL")
	}
	if controller == "" {
		controller = "http://orchestrator-api:8088"
	}
	if err := validWardenNamespace(namespace); err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	pin := sha256.Sum256(spki)
	id := wardenIDPrefix + hex.EncodeToString(pin[:])[:16]
	scope := wardenScope{Namespace: namespace, WardenID: id}
	uri, err := scope.uri()
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		IsCA:         true,
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := wardenIdentityDir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, wardenKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, wardenCertFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, wardenCAFile), certPEM, 0o644); err != nil {
		return nil, err
	}

	rec := wardenRecord{
		WardenID:      id,
		Controller:    controller,
		CAFingerprint: hex.EncodeToString(pin[:]),
		EnrolledAt:    now,
		NotAfter:      tmpl.NotAfter,
		Temporal:      os.Getenv("KONTRA_ADDRESS"),
		Namespace:     namespace,
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(state, wardenRecordFile), body, 0o644); err != nil {
		return nil, err
	}
	idLoaded, err := loadIdentity(state)
	if err != nil {
		return nil, fmt.Errorf("local bootstrap wrote an identity loadIdentity refused: %w", err)
	}
	return idLoaded, nil
}
