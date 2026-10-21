package delivery

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestPool is a pool with no database: these tests ask where a message
// should go and never touch the outbox.
func newTestPool() *Pool {
	return NewPool(Config{
		SMTP: SMTPConfig{ConnectTimeout: 5 * time.Second, CommandTimeout: 10 * time.Second},
	}, slog.New(slog.NewTextHandler(os.Stderr, nil)), nil)
}

// notFound is how the resolver answers for a name that publishes nothing.
func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

// answeringDNS lets a test answer the exchanger search with records it wrote
// itself. The order the published exchangers are tried in, and what happens
// when the first one cannot be reached, are what these tests are about; left
// to whatever resolver the machine has, both would be indistinguishable from
// the network's mood on the day they run.
func answeringDNS(pool *Pool, mx []*net.MX, hosts []string) {
	pool.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		if len(mx) == 0 {
			return nil, notFound("example.test")
		}
		return mx, nil
	}
	pool.lookupHost = func(context.Context, string) ([]string, error) {
		if len(hosts) == 0 {
			return nil, notFound("example.test")
		}
		return hosts, nil
	}
}

// deadAddress is a port nothing listens on, so a dial to it is refused
// right away. That is what an exchanger this host cannot reach looks like,
// and the test can wait for it.
func deadAddress(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return splitAddress(t, addr)
}

func splitAddress(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	return host, port
}

// A domain that publishes nothing -- no mail exchanger and no address -- is
// unreachable for good. Writing it off permanently is what lets the sender get
// a bounce now instead of a retry every few minutes for a week.
func TestMailExchangerReportsUnreachableDomainAsPermanent(t *testing.T) {
	pool := newTestPool()
	answeringDNS(pool, nil, nil)

	_, err := pool.mailExchangers(context.Background(), "linchange-does-not-exist.invalid")
	if err == nil {
		t.Fatal("a domain that exists nowhere resolved to a mail exchanger")
	}
	if !IsPermanent(err) {
		t.Fatalf("error %v is not permanent: the message would keep retrying "+
			"and the sender would never get a bounce", err)
	}
}

// A domain that says it accepts no mail (RFC 7505, a mail exchanger of ".") is
// unreachable for the same reason, and saying so is better than dialing its
// web address and hoping.
func TestMailExchangerReportsNullMXAsPermanent(t *testing.T) {
	pool := newTestPool()
	answeringDNS(pool, []*net.MX{{Host: ".", Pref: 10}}, []string{"192.0.2.1"})

	_, err := pool.mailExchangers(context.Background(), "example.test")
	if err == nil {
		t.Fatal("a domain that publishes a null mail exchanger resolved to one")
	}
	if !IsPermanent(err) {
		t.Fatalf("a domain that accepts no mail must fail permanently, got %v", err)
	}
	if !strings.Contains(err.Error(), "accepts no mail") {
		t.Errorf("the bounce would not say why the domain was given up: %v", err)
	}
}

// A domain with no mail exchanger but a real address is still worth trying at
// that address, as RFC 5321 allows.
func TestMailExchangerFallsBackToTheDomainAddress(t *testing.T) {
	pool := newTestPool()
	answeringDNS(pool, nil, []string{"192.0.2.1"})

	exchangers, err := pool.mailExchangers(context.Background(), "linchange.test")
	if err != nil {
		t.Fatalf("a domain with an address but no exchanger: %v", err)
	}
	if len(exchangers) != 1 || exchangers[0].Host != "linchange.test" {
		t.Fatalf("fallback should dial the domain itself, got %v", exchangers)
	}
}

// Every published exchanger is returned, lowest preference first. Only the
// lowest-preference one used to be used, so a single exchanger that could not
// be reached deferred the whole domain, although the domain published four
// more that were answering the whole time.
func TestMailExchangersReturnsEveryPublishedHostLowestPreferenceFirst(t *testing.T) {
	pool := newTestPool()
	answeringDNS(pool, []*net.MX{
		{Host: "alt3.example.test.", Pref: 40},
		{Host: "mx.example.test.", Pref: 5},
		// Published twice under different spelling: it is one host, and
		// dialling it twice would be two attempts at the same failure.
		{Host: "MX.EXAMPLE.TEST.", Pref: 10},
		{Host: "alt1.example.test.", Pref: 20},
	}, nil)

	exchangers, err := pool.mailExchangers(context.Background(), "example.test")
	if err != nil {
		t.Fatalf("mailExchangers: %v", err)
	}
	got := make([]string, 0, len(exchangers))
	for _, exchanger := range exchangers {
		got = append(got, exchanger.Host)
	}
	want := []string{"mx.example.test", "alt1.example.test", "alt3.example.test"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("exchangers = %v, want %v: the order is the fallback order", got, want)
	}
	for _, exchanger := range exchangers {
		if exchanger.Port != 25 {
			t.Errorf("exchanger %s dials port %d, not the SMTP destination port", exchanger.Host, exchanger.Port)
		}
	}
}

// A lookup that failed for a reason that is not "there is no such name" is
// written off as transient: DNS comes back, and a bounce that blames the
// recipient for the resolver being down would be a lie.
func TestMailExchangerReportsLookupFailureAsTransient(t *testing.T) {
	pool := newTestPool()
	pool.lookupMX = func(context.Context, string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "server misbehaving", Name: "example.test"}
	}

	_, err := pool.mailExchangers(context.Background(), "example.test")
	if err == nil {
		t.Fatal("a failed lookup resolved to a mail exchanger")
	}
	if IsPermanent(err) {
		t.Fatalf("error %v is permanent: a resolver outage would be blamed on the recipient", err)
	}
}

// An exchanger that cannot be dialled at all is the network's answer about
// that address, so the next address the domain published gets its turn.
func TestDeliveryFallsBackToTheNextPublishedExchanger(t *testing.T) {
	deadHost, deadPort := deadAddress(t)
	server := &scriptedSMTP{}
	host, port := server.start(t)
	t.Logf("dead exchanger %s:%d, answering exchanger %s:%d", deadHost, deadPort, host, port)

	pool := newTestPool()
	out := pool.deliverToExchangers(context.Background(), 1, "example.test", []SMTPConfig{
		{Host: deadHost, Port: deadPort, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
		{Host: host, Port: port, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
	}, "from@example.com", []string{"to@example.com"}, testMessage())

	if len(out.Delivered) != 1 {
		t.Fatalf("the reachable exchanger did not deliver: delivered=%v transient=%v reasons=%v",
			out.Delivered, out.Transient, out.Reasons)
	}
	if len(out.Transient) != 0 {
		t.Fatalf("the message deferred although an exchanger took it: %v", out.Reasons)
	}
	if !server.saw("DATA") {
		t.Errorf("the fallback exchanger never carried the message: %v", server.seen())
	}
}

// A message the second exchanger takes is not offered to the first one again
// after it answered and declined: the destination spoke, and its neighbours
// would only collect the same refusal.
func TestDeliveryStopsAtTheExchangerThatAnswers(t *testing.T) {
	server := &scriptedSMTP{}
	host, port := server.start(t)
	deadHost, deadPort := deadAddress(t)

	pool := newTestPool()
	out := pool.deliverToExchangers(context.Background(), 1, "example.test", []SMTPConfig{
		{Host: host, Port: port, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
		{Host: deadHost, Port: deadPort, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
	}, "from@example.com", []string{"to@example.com"}, testMessage())

	if len(out.Delivered) != 1 {
		t.Fatalf("the first exchanger did not deliver: %v", out)
	}
}

// When no exchanger answers, the message defers, and it carries what the dial
// actually said. "The mail exchanger could not be reached" on its own left
// whoever read it to guess at the address family, the port or the peer.
func TestDeliveryDefersWithTheReasonWhenNoExchangerAnswers(t *testing.T) {
	firstHost, firstPort := deadAddress(t)
	secondHost, secondPort := deadAddress(t)

	pool := newTestPool()
	out := pool.deliverToExchangers(context.Background(), 1, "example.test", []SMTPConfig{
		{Host: firstHost, Port: firstPort, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
		{Host: secondHost, Port: secondPort, ConnectTimeout: 5 * time.Second, CommandTimeout: 5 * time.Second},
	}, "from@example.com", []string{"to@example.com"}, testMessage())

	if len(out.Transient) != 1 {
		t.Fatalf("the recipient should have been deferred, got %v", out)
	}
	if out.Connected {
		t.Error("the outcome claims a session with an exchanger that was never dialled through")
	}
	reason := out.Reasons["to@example.com"]
	if !strings.Contains(reason, "connect") || !strings.Contains(reason, secondHost) {
		t.Errorf("the reason %q does not name what failed: it should say that connecting "+
			"to the last exchanger tried did not work", reason)
	}
	if got := transientReason(out); got != reason {
		t.Errorf("transientReason = %q, want %q", got, reason)
	}
}

// An attempt with nothing to try must not settle as sent: a message whose
// recipients are neither delivered nor transient is marked sent, so an empty
// attempt would quietly drop the mail while reporting success.
func TestDeliveryWithoutExchangersDefers(t *testing.T) {
	pool := newTestPool()
	out := pool.deliverToExchangers(context.Background(), 1, "example.test", nil,
		"from@example.com", []string{"to@example.com", "also@example.com"}, testMessage())

	if len(out.Delivered) != 0 {
		t.Fatalf("nothing was dialled, yet %v are reported delivered", out.Delivered)
	}
	if len(out.Transient) != 2 {
		t.Fatalf("both recipients should be deferred, got %v", out.Transient)
	}
	if reason := transientReason(out); !strings.Contains(reason, "example.test") {
		t.Errorf("the reason %q does not name the domain that published nothing", reason)
	}
}

// transientReason prefers the connection-level reason when one is set: it
// covers every recipient of the attempt, and repeating it once per address
// would only bury the one line that matters under the same line again.
func TestTransientReasonPrefersTheConnectionLevelReason(t *testing.T) {
	out := RecipientOutcome{
		Transient: []string{"a@example.test", "b@example.test"},
		Reasons: map[string]string{
			"*":              "connect 192.0.2.1:25: connection refused",
			"a@example.test": "connect 192.0.2.1:25: connection refused",
		},
	}
	if got := transientReason(out); got != out.Reasons["*"] {
		t.Fatalf("transientReason = %q, want the connection-level reason %q", got, out.Reasons["*"])
	}
}
