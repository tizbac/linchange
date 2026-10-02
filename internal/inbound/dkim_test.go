package inbound

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"

	lc "github.com/linchange/linchange/internal/dkim"
)

const testMessage = `From: alice@example.com
To: bob@example.org
Subject: Hello
Message-ID: <test-0001@example.com>
Date: Fri, 02 Oct 2026 12:00:00 +0000
MIME-Version: 1.0
Content-Type: text/plain

Hello there.
`

func signTestMessage(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	signer := lc.NewSigner("example.com", "s1", key, crypto.SHA256)
	signed, err := signer.Sign([]byte(testMessage))
	if err != nil {
		t.Fatalf("signing test message: %v", err)
	}
	return signed
}

func keyRecord(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshalling public key: %v", err)
	}
	return "v=DKIM1; p=" + base64.StdEncoding.EncodeToString(der)
}

func TestDKIMVerify(t *testing.T) {
	key, err := rsa.GenerateKey(testRNG(), 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	signed := signTestMessage(t, key)

	f := newFixtureResolver()
	f.txt["s1._domainkey.example.com"] = []string{keyRecord(t, key)}

	verifs := DKIMVerify(context.Background(), signed, f)
	if len(verifs) != 1 {
		t.Fatalf("got %d verifications, want 1", len(verifs))
	}
	if verifs[0].Err != nil {
		t.Errorf("valid signature failed verification: %v", verifs[0].Err)
	}
	if !DKIMValid(verifs) {
		t.Errorf("DKIMValid = false for a valid signature")
	}
	if got := DKIMDomains(verifs); len(got) != 1 || got[0] != "example.com" {
		t.Errorf("DKIMDomains = %v, want [example.com]", got)
	}
}

func TestDKIMVerifyTampered(t *testing.T) {
	key, err := rsa.GenerateKey(testRNG(), 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	signed := signTestMessage(t, key)
	tampered := strings.Replace(string(signed), "Hello there.", "You won.", 1)

	f := newFixtureResolver()
	f.txt["s1._domainkey.example.com"] = []string{keyRecord(t, key)}

	verifs := DKIMVerify(context.Background(), []byte(tampered), f)
	if len(verifs) != 1 {
		t.Fatalf("got %d verifications, want 1", len(verifs))
	}
	if verifs[0].Err == nil {
		t.Error("tampered body passed DKIM verification")
	}
	if DKIMValid(verifs) {
		t.Error("DKIMValid = true for a tampered message")
	}
}

func TestDKIMVerifyNoKey(t *testing.T) {
	key, err := rsa.GenerateKey(testRNG(), 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	signed := signTestMessage(t, key)

	// No TXT record published for the key.
	f := newFixtureResolver()
	verifs := DKIMVerify(context.Background(), signed, f)
	if len(verifs) != 1 {
		t.Fatalf("got %d verifications, want 1", len(verifs))
	}
	if verifs[0].Err == nil {
		t.Error("verification succeeded without a published key")
	}
}

func TestDKIMVerifyUnsigned(t *testing.T) {
	f := newFixtureResolver()
	if verifs := DKIMVerify(context.Background(), []byte(testMessage), f); len(verifs) != 0 {
		t.Errorf("unsigned message produced %d verifications, want 0", len(verifs))
	}
	if DKIMValid(nil) {
		t.Error("DKIMValid(nil) = true")
	}
}
