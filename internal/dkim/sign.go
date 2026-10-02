package dkim

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/linchange/linchange/config"
)

// HashAlgorithm selects the digest for the signature.
type HashAlgorithm crypto.Hash

// Canonicalization controls how headers and body are normalised before signing.
type Canonicalization string

const (
	CanonicalizationSimple       Canonicalization = "simple"
	CanonicalizationRelaxed      Canonicalization = "relaxed"
)

// Sign signs the message bytes and returns the signed message.
func Sign(msg []byte, opts *SignOptions) ([]byte, error) {
	var hash crypto.Hash
	switch opts.Hash {
	case HashAlgorithm(crypto.SHA256):
		hash = crypto.SHA256
	default:
		hash = crypto.SHA256
	}

	var headerCanon, bodyCanon dkim.Canonicalization
	switch opts.HeaderCanonicalization {
	case CanonicalizationRelaxed:
		headerCanon = dkim.CanonicalizationRelaxed
	default:
		headerCanon = dkim.CanonicalizationSimple
	}
	switch opts.BodyCanonicalization {
	case CanonicalizationRelaxed:
		bodyCanon = dkim.CanonicalizationRelaxed
	default:
		bodyCanon = dkim.CanonicalizationSimple
	}

	signOpts := &dkim.SignOptions{
		Domain:                 opts.Domain,
		Selector:               opts.Selector,
		Signer:                 opts.PrivateKey,
		Hash:                   hash,
		HeaderKeys:             opts.HeaderKeys,
		HeaderCanonicalization: headerCanon,
		BodyCanonicalization:   bodyCanon,
	}

	var buf bytes.Buffer
	if err := dkim.Sign(&buf, bytes.NewReader(msg), signOpts); err != nil {
		return nil, fmt.Errorf("dkim: sign: %w", err)
	}

	return buf.Bytes(), nil
}

// SignOptions configures DKIM signing for a message.
type SignOptions struct {
	Domain                 string
	Selector               string
	PrivateKey             *rsa.PrivateKey
	Hash                   HashAlgorithm
	HeaderKeys             []string
	HeaderCanonicalization Canonicalization
	BodyCanonicalization   Canonicalization
}

// Signer wraps an RSA key and domain for repeated signing.
type Signer struct {
	domain   string
	selector string
	key      *rsa.PrivateKey
	hash     crypto.Hash
	headerCanon, bodyCanon dkim.Canonicalization
	headerKeys             []string
}

// NewSigner creates a DKIM signer for domain and selector.
func NewSigner(domain, selector string, key *rsa.PrivateKey, hash crypto.Hash) *Signer {
	return &Signer{
		domain:   domain,
		selector: selector,
		key:      key,
		hash:     hash,
	}
}

// Sign signs the message and returns the signed message bytes.
func (s *Signer) Sign(msg []byte) ([]byte, error) {
	signOpts := &dkim.SignOptions{
		Domain:                 s.domain,
		Selector:               s.selector,
		Signer:                 s.key,
		Hash:                   s.hash,
		HeaderKeys:             s.headerKeys,
		HeaderCanonicalization: s.headerCanon,
		BodyCanonicalization:   s.bodyCanon,
	}

	var buf bytes.Buffer
	if err := dkim.Sign(&buf, bytes.NewReader(msg), signOpts); err != nil {
		return nil, fmt.Errorf("dkim: sign: %w", err)
	}

	return buf.Bytes(), nil
}

// SignReader signs a message stream and writes the signed output to dst.
func (s *Signer) SignReader(src io.Reader, dst io.Writer) error {
	signOpts := &dkim.SignOptions{
		Domain:                 s.domain,
		Selector:               s.selector,
		Signer:                 s.key,
		Hash:                   s.hash,
		HeaderKeys:             s.headerKeys,
		HeaderCanonicalization: s.headerCanon,
		BodyCanonicalization:   s.bodyCanon,
	}

	return dkim.Sign(dst, src, signOpts)
}

// Domain returns the signing domain.
func (s *Signer) Domain() string {
	return s.domain
}

// Selector returns the DKIM selector.
func (s *Signer) Selector() string {
	return s.selector
}

// ConfiguredSigner creates a signer from the DKIM config and key manager.
func ConfiguredSigner(cfg config.DKIM, selectorPrefix string, domain string, key *rsa.PrivateKey) (*Signer, error) {
	if domain == "" {
		return nil, errors.New("dkim: domain is required")
	}

	selector := fmt.Sprintf("%s-%s", selectorPrefix, domain)
	if cfg.SelectorPrefix != "" {
		selector = fmt.Sprintf("%s-%s", cfg.SelectorPrefix, domain)
	}

	return &Signer{
		domain:   domain,
		selector: selector,
		key:      key,
		hash:     crypto.SHA256,
	}, nil
}
