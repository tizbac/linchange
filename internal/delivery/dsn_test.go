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
