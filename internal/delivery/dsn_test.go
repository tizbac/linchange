package delivery_test

import (
	"strings"
	"testing"
	"time"

	"github.com/linchange/linchange/internal/delivery"
)

func TestGenerateDSN(t *testing.T) {
	cfg := delivery.DSNConfig{
		Enabled:     true,
		FromAddress: "postmaster@example.com",
		Delay:       4 * time.Hour,
	}

	msg := delivery.OutboxMessage{
		ID:           42,
		SubmittedBy:  "user@example.com",
		EnvelopeFrom: "user@example.com",
		State:        delivery.OutboxFailed,
		Attempts:     3,
		LastError:    "connection refused",
	}

	failedRecipients := []delivery.OutboxRecipient{
		{
			Address:   "bad@nonexistent.test",
			State:     delivery.RecipientFailed,
			LastError: "NXDOMAIN",
		},
		{
			Address:   "slow@timeout.test",
			State:     delivery.RecipientFailed,
			LastError: "connection timeout",
		},
	}

	data, err := delivery.GenerateDSN(cfg, msg, failedRecipients)
	if err != nil {
		t.Fatalf("GenerateDSN: %v", err)
	}

	body := string(data)

	// Check required headers.
	if !strings.Contains(body, "From: <postmaster@example.com>") {
		t.Error("missing From header")
	}
	if !strings.Contains(body, "To: user@example.com") {
		t.Error("missing To header")
	}
	if !strings.Contains(body, "Subject: Delivery Status Notification") {
		t.Error("missing Subject")
	}

	// Check failed recipients are listed.
	if !strings.Contains(body, "bad@nonexistent.test") {
		t.Error("missing first failed recipient")
	}
	if !strings.Contains(body, "slow@timeout.test") {
		t.Error("missing second failed recipient")
	}

	// Check error details.
	if !strings.Contains(body, "NXDOMAIN") {
		t.Error("missing first recipient error")
	}
	if !strings.Contains(body, "connection timeout") {
		t.Error("missing second recipient error")
	}
}

func TestGenerateDSNDisabled(t *testing.T) {
	cfg := delivery.DSNConfig{
		Enabled: false,
	}

	msg := delivery.OutboxMessage{
		ID:           1,
		SubmittedBy:  "user@example.com",
		EnvelopeFrom: "user@example.com",
	}

	data, err := delivery.GenerateDSN(cfg, msg, nil)
	if err != nil {
		t.Fatalf("GenerateDSN: %v", err)
	}
	if data != nil {
		t.Error("expected nil data when DSNs disabled")
	}
}

func TestGenerateDSNNoFromAddress(t *testing.T) {
	cfg := delivery.DSNConfig{
		Enabled: true,
		Delay:   4 * time.Hour,
	}

	msg := delivery.OutboxMessage{
		ID: 1,
	}

	_, err := delivery.GenerateDSN(cfg, msg, nil)
	if err == nil {
		t.Error("expected error when from_address is empty")
	}
}

func TestParseMessageTo(t *testing.T) {
	data := []byte(`From: sender@example.com
To: recipient@example.com
Cc: cc@example.com, cc2@example.com
Subject: Test Message

Body text here.
`)

	addrs, err := delivery.ParseMessageTo(data)
	if err != nil {
		t.Fatalf("ParseMessageTo: %v", err)
	}

	if len(addrs) != 3 {
		t.Fatalf("expected 3 addresses, got %d", len(addrs))
	}

	// Check all expected addresses are present.
	expected := map[string]bool{
		"recipient@example.com": true,
		"cc@example.com":        true,
		"cc2@example.com":       true,
	}
	for _, addr := range addrs {
		if !expected[addr] {
			t.Errorf("unexpected address: %s", addr)
		}
	}
}

func TestParseMessageToMultipleRecipients(t *testing.T) {
	data := []byte(`From: sender@example.com
To: a@example.com, b@example.com, c@example.com
Subject: Test

Body
`)

	addrs, err := delivery.ParseMessageTo(data)
	if err != nil {
		t.Fatalf("ParseMessageTo: %v", err)
	}

	if len(addrs) != 3 {
		t.Fatalf("expected 3 addresses, got %d", len(addrs))
	}
}

func TestParseMessageToInvalidAddress(t *testing.T) {
	data := []byte(`From: sender@example.com
To: not-an-email
Subject: Test

Body
`)

	// Invalid addresses should be skipped, not cause an error.
	addrs, err := delivery.ParseMessageTo(data)
	if err != nil {
		t.Fatalf("ParseMessageTo: %v", err)
	}
	if len(addrs) != 0 {
		t.Errorf("expected 0 addresses, got %d", len(addrs))
	}
}

func TestParseMessageToCrlf(t *testing.T) {
	// RFC 5322 uses \r\n — make sure ParseMessageTo handles it.
	data := []byte("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\n\r\nBody\r\n")

	addrs, err := delivery.ParseMessageTo(data)
	if err != nil {
		t.Fatalf("ParseMessageTo: %v", err)
	}

	if len(addrs) != 1 {
		t.Fatalf("expected 1 address, got %d", len(addrs))
	}
	if addrs[0] != "recipient@example.com" {
		t.Errorf("got %q, want %q", addrs[0], "recipient@example.com")
	}
}

func TestParseMessageToMixedLineEndings(t *testing.T) {
	data := []byte("From: sender@example.com\r\nTo: a@example.com\nSubject: Test\r\n\r\nBody\n")

	addrs, err := delivery.ParseMessageTo(data)
	if err != nil {
		t.Fatalf("ParseMessageTo: %v", err)
	}

	if len(addrs) != 1 {
		t.Fatalf("expected 1 address, got %d", len(addrs))
	}
}
