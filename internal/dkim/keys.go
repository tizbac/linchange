// Package dkim manages DKIM signing keys and signs outbound messages.
//
// Keys are stored in the data directory under dkim/<domain>.pem. Each key is
// an RSA private key in PKCS#8 format. The corresponding public key is
// published as a DNS TXT record at <selector>.dkim.<domain>.
package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Keys manages DKIM key material per domain.
type Keys struct {
	dataDir string
	mu      sync.Mutex
	cache   map[string]*keyPair
}

type keyPair struct {
	privateKey *rsa.PrivateKey
	publicKey  string
}

// New creates a key manager rooted in dataDir.
func New(dataDir string) *Keys {
	return &Keys{
		dataDir: dataDir,
		cache:   make(map[string]*keyPair),
	}
}

// KeyDir returns the directory holding DKIM keys.
func (k *Keys) KeyDir() string {
	return k.dataDir
}

// GetOrCreate returns the RSA key pair for domain, generating one if none
// exists on disk. The public key is returned in DNS TXT format for publishing.
func (k *Keys) GetOrCreate(domain string) (*rsa.PrivateKey, string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	// Check cache first.
	if kp, ok := k.cache[domain]; ok {
		return kp.privateKey, kp.publicKey, nil
	}

	kp, err := k.loadOrCreate(domain)
	if err != nil {
		return nil, "", err
	}

	k.cache[domain] = kp
	return kp.privateKey, kp.publicKey, nil
}

// Get returns a cached key pair for domain, or nil if not cached.
func (k *Keys) Get(domain string) (*rsa.PrivateKey, string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	kp, ok := k.cache[domain]
	if !ok {
		return nil, "", false
	}
	return kp.privateKey, kp.publicKey, true
}

// loadOrCreate loads the key from disk or generates a new one.
func (k *Keys) loadOrCreate(domain string) (*keyPair, error) {
	keyFile := k.keyFile(domain)

	// Try to load existing key.
	if kp, err := k.load(keyFile); err == nil {
		return kp, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("dkim: load key: %w", err)
	}

	// Generate new key.
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("dkim: generate key: %w", err)
	}

	pubKey := rsaPublicKeyToPEM(&privKey.PublicKey)

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, fmt.Errorf("dkim: create dir: %w", err)
	}

	// Write private key.
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privKey),
	})
	if err := os.WriteFile(keyFile, privPEM, 0o600); err != nil {
		return nil, fmt.Errorf("dkim: write key: %w", err)
	}

	return &keyPair{
		privateKey: privKey,
		publicKey:  pubKey,
	}, nil
}

// load reads a PEM-encoded RSA private key from keyFile.
func (k *Keys) load(keyFile string) (*keyPair, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("dkim: no PEM data")
	}

	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("dkim: parse key: %w", err)
	}

	// Build public key string for DNS.
	pubKey := rsaPublicKeyToPEM(&privKey.PublicKey)

	return &keyPair{
		privateKey: privKey,
		publicKey:  pubKey,
	}, nil
}

// keyFile returns the path to the private key file for domain.
func (k *Keys) keyFile(domain string) string {
	return filepath.Join(k.dataDir, domain+".pem")
}

// rsaPublicKeyToPEM encodes an RSA public key in PEM format.
func rsaPublicKeyToPEM(pub *rsa.PublicKey) string {
	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubBytes,
	})
	return string(pubPEM)
}
