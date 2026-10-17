package delivery_test

import (
	"bytes"
	"net/mail"
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
			LastError: "RCPT TO: 550 5.1.1 no such mailbox",
		},
		{
			Address:   "slow@timeout.test",
			State:     delivery.RecipientFailed,
			LastError: "connection timeout",
		},
	}

	original, err := mail.ReadMessage(bytes.NewReader(
		[]byte("From: user@example.com\r\nSubject: Quarterly report\r\n\r\nbody\r\n")))
	if err != nil {
		t.Fatalf("reading the original message: %v", err)
	}

	data, err := delivery.GenerateDSN(cfg, msg, failedRecipients, original)
	if err != nil {
		t.Fatalf("GenerateDSN: %v", err)
	}

	body := string(data)

	// Check required headers.
	for _, want := range []string{
		"From: <postmaster@example.com>",
		"To: user@example.com",
		"Subject: Delivery Status Notification",
		"Auto-Submitted: auto-replied",
		"Date: ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing header %q", want)
		}
	}

	// Every failed recipient is named, with the reason the destination gave.
	for _, want := range []string{
		"bad@nonexistent.test",
		"RCPT TO: 550 5.1.1 no such mailbox",
		"slow@timeout.test",
		"connection timeout",
		"action: failed",
		"Quarterly report",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("report does not mention %q; body was:\n%s", want, body)
		}
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

	data, err := delivery.GenerateDSN(cfg, msg, nil, nil)
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

	_, err := delivery.GenerateDSN(cfg, msg, nil, nil)
	if err == nil {
		t.Error("expected error when from_address is empty")
	}
}
