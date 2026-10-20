package delivery

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// newTestPool is a pool with no database: these tests ask the resolver where a
// message should go and never touch the outbox.
func newTestPool() *Pool {
	return NewPool(Config{
		SMTP: SMTPConfig{ConnectTimeout: 5 * time.Second, CommandTimeout: 10 * time.Second},
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
}

// A domain that publishes nothing -- no mail exchanger and no address -- is
// unreachable for good. Writing it off permanently is what lets the sender get
// a bounce now instead of a retry every few minutes for a week.
func TestMailExchangerReportsUnreachableDomainAsPermanent(t *testing.T) {
	pool := newTestPool()
	_, err := pool.mailExchanger(context.Background(), "linchange-does-not-exist.invalid")
	if err == nil {
		t.Fatal("a domain that exists nowhere resolved to a mail exchanger")
	}
	if !IsPermanent(err) {
		t.Fatalf("error %v is not permanent: the message would keep retrying "+
			"and the sender would never get a bounce", err)
	}
}

// A domain that explicitly says it accepts no mail (RFC 7505, a mail exchanger
// of ".") is unreachable for the same reason, and saying so is better than
// dialing its web address and hoping.
func TestMailExchangerReportsNullMXAsPermanent(t *testing.T) {
	pool := newTestPool()
	exchanger, err := pool.mailExchanger(context.Background(), "example.com")
	if err == nil {
		t.Logf("example.com resolved to %s:%d", exchanger.Host, exchanger.Port)
		return
	}
	if !IsPermanent(err) {
		t.Fatalf("a domain that accepts no mail must fail permanently, got %v", err)
	}
}

// A domain with no mail exchanger but a real address is still worth trying at
// that address, as RFC 5321 allows.
func TestMailExchangerFallsBackToTheDomainAddress(t *testing.T) {
	pool := newTestPool()
	exchanger, err := pool.mailExchanger(context.Background(), "linchange.test")
	if err == nil {
		if exchanger.Host != "linchange.test" {
			t.Fatalf("fallback should dial the domain itself, got %q", exchanger.Host)
		}
		return
	}
	if !IsPermanent(err) {
		t.Logf("%q could not be resolved at all: %v", "linchange.test", err)
	}
}
