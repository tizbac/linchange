package delivery

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests are about how a delivery attempt starts: the server's EHLO reply
// decides whether there is a TLS session at all, and a server that says nothing
// about STARTTLS must still be delivered to. That is the whole difference
// between a message going out through such a server and every message through
// it deferring until the retry lifetime runs out.

// scriptedSMTP is an SMTP peer written for these tests rather than taken from
// a library, because what is under test is precisely what it advertises and
// what it does next: a scripted peer says exactly that and nothing else.
type scriptedSMTP struct {
	// advertiseStartTLS puts STARTTLS in the EHLO reply.
	advertiseStartTLS bool
	// upgrade is the configuration the server uses for the TLS handshake
	// after answering STARTTLS. Nil means the server answers 220 and hangs up
	// instead, leaving the client's handshake to fail against a peer that is
	// not speaking TLS.
	upgrade *tls.Config

	mu        sync.Mutex
	lines     []string
	encrypted bool
}

// start serves on 127.0.0.1 at a free port until the test ends.
func (s *scriptedSMTP) start(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	host, portText, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port %q: %v", portText, err)
	}
	return host, port
}

func (s *scriptedSMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(text string) {
		_, _ = conn.Write([]byte(text))
	}

	write("220 test.example ESMTP\r\n")
	for {
		line, err := readLine(r)
		if err != nil {
			return
		}
		s.record(line)
		verb, _, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO":
			if s.advertiseStartTLS {
				write("250-test.example\r\n250 STARTTLS\r\n")
			} else {
				write("250-test.example\r\n250 HELP\r\n")
			}
		case "HELO":
			write("250 test.example\r\n")
		case "STARTTLS":
			write("220 ready to start tls\r\n")
			if s.upgrade == nil {
				return
			}
			upgraded := tls.Server(conn, s.upgrade)
			if err := upgraded.Handshake(); err != nil {
				return
			}
			conn = upgraded
			r = bufio.NewReader(conn)
			s.setEncrypted()
		case "MAIL", "RCPT":
			write("250 ok\r\n")
		case "DATA":
			write("354 end with a line holding only a dot\r\n")
			for {
				body, err := readLine(r)
				if err != nil {
					return
				}
				if body == "." {
					break
				}
			}
			write("250 queued\r\n")
		case "QUIT":
			write("221 bye\r\n")
			return
		default:
			write("500 that is not a command I know\r\n")
		}
	}
}

func (s *scriptedSMTP) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *scriptedSMTP) setEncrypted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.encrypted = true
}

// saw reports whether the server was ever sent a line starting with prefix.
func (s *scriptedSMTP) saw(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.lines {
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(prefix)) {
			return true
		}
	}
	return false
}

// seen is every line the server was sent, for a failure message that shows
// what the conversation actually was.
func (s *scriptedSMTP) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *scriptedSMTP) wasEncrypted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encrypted
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// selfSignedTLSConfig is a certificate the server can present without a
// public authority behind it. The client in these tests skips verification —
// that is a configuration it supports and a deployment it exists for — so
// what is being shown is that the session becomes a TLS session, not that a
// chain of trust was walked.
func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func testMessage() []byte {
	return []byte("Subject: hello\r\n\r\nhello\r\n")
}

// A server with no STARTTLS in its EHLO reply is delivered to in the clear
// instead of being reported as a failure. Refusing it deferred the message
// into the same refusal on every attempt until the retry lifetime ended, so
// the mail never left and the log named no fault anybody could fix.
func TestSendFallsBackToPlaintextWhenTheServerOffersNoStartTLS(t *testing.T) {
	server := &scriptedSMTP{}
	host, port := server.start(t)

	client := &smtpClient{cfg: SMTPConfig{
		Host:           host,
		Port:           port,
		TLS:            true,
		ConnectTimeout: 5 * time.Second,
		CommandTimeout: 5 * time.Second,
	}}

	err := client.Send(context.Background(), "from@example.com",
		[]string{"to@example.com"}, testMessage())
	if err != nil {
		t.Fatalf("Send to a server without STARTTLS: %v", err)
	}
	if server.saw("STARTTLS") {
		t.Error("asked a server that does not advertise STARTTLS to start TLS")
	}
	if !server.saw("MAIL FROM") || !server.saw("DATA") {
		t.Errorf("the message was not carried through: server saw %v", server.seen())
	}
}

// Offering STARTTLS is still taken: the fallback is for servers that have
// nothing to offer, not a reason to stop encrypting where encryption is there.
func TestSendTakesTLSWhenTheServerOffersIt(t *testing.T) {
	server := &scriptedSMTP{advertiseStartTLS: true, upgrade: selfSignedTLSConfig(t)}
	host, port := server.start(t)

	client := &smtpClient{cfg: SMTPConfig{
		Host:               host,
		Port:               port,
		TLS:                true,
		ConnectTimeout:     5 * time.Second,
		CommandTimeout:     5 * time.Second,
		InsecureSkipVerify: true,
	}}

	err := client.Send(context.Background(), "from@example.com",
		[]string{"to@example.com"}, testMessage())
	if err != nil {
		t.Fatalf("Send over STARTTLS: %v", err)
	}
	if !server.wasEncrypted() {
		t.Error("the session stayed in the clear although the server offered STARTTLS")
	}
}

// SendEach is the path delivery takes when no smart host is configured, so it
// needs the same answer for the same server.
func TestSendEachFallsBackToPlaintextWhenTheServerOffersNoStartTLS(t *testing.T) {
	server := &scriptedSMTP{}
	host, port := server.start(t)

	client := &smtpClient{cfg: SMTPConfig{
		Host:           host,
		Port:           port,
		TLS:            true,
		ConnectTimeout: 5 * time.Second,
		CommandTimeout: 5 * time.Second,
	}}

	out := client.SendEach(context.Background(), "from@example.com",
		[]string{"to@example.com"}, testMessage())
	if len(out.Transient) > 0 || len(out.Permanent) > 0 {
		t.Fatalf("SendEach: %v", out.Reasons)
	}
	if len(out.Delivered) != 1 {
		t.Fatalf("delivered %v, want the one recipient", out.Delivered)
	}
	if server.saw("STARTTLS") {
		t.Error("asked a server that does not advertise STARTTLS to start TLS")
	}
	if !server.saw("DATA") {
		t.Errorf("the message was not carried through: server saw %v", server.seen())
	}
}

// A server that advertises STARTTLS and then cannot complete the handshake is
// still a failure rather than a reason to send the message in the clear: it
// promised encryption it did not deliver, and quietly going on without it is
// exactly the downgrade the offer was supposed to rule out.
//
// The attempt failing at all is what is asserted rather than how it is
// classified: go-smtp starts the handshake lazily, so it surfaces as a broken
// connection at MAIL FROM, and whether a server that hangs up is retried or
// bounced is a separate question from whether its message goes out in the
// clear. Neither must happen here.
func TestSendDoesNotFallBackAfterAStartTLSHandshakeFailure(t *testing.T) {
	server := &scriptedSMTP{advertiseStartTLS: true}
	host, port := server.start(t)

	client := &smtpClient{cfg: SMTPConfig{
		Host:           host,
		Port:           port,
		TLS:            true,
		ConnectTimeout: 5 * time.Second,
		CommandTimeout: 5 * time.Second,
	}}

	err := client.Send(context.Background(), "from@example.com",
		[]string{"to@example.com"}, testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a server whose handshake never completed")
	}
	if !server.saw("STARTTLS") {
		t.Error("never asked the server that offered STARTTLS to start TLS")
	}
	if server.saw("DATA") {
		t.Error("the message was sent in the clear after the handshake failed")
	}
}
