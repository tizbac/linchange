// Package mapihttp serves Outlook's native protocol (MS-OXCMAPIHTTP): the
// HTTP framing both MAPI/HTTP endpoints share, the ROP operations a mailbox
// client sends on /mapi/emsmdb (MS-OXCROPS), and the address book operations
// it browses on /mapi/nspi (MS-OXNSPI).
//
// The split is the protocol's own. One framing carries two endpoints, each
// with its own operations and its own Session Context: a client holds a
// context with each and drops them independently, which is why the two are
// separate routes, separate session tables and separate requests rather than
// one service with two verbs.
//
// Everything below the framing lives in the rop and nspi packages; the
// framing itself lives in the transport package. This file is the entry point
// those are mounted through, and it exists so that building a MAPI/HTTP
// server is one struct of options rather than a wiring job at every call site.
package mapihttp

import (
	"log/slog"
	"net/http"

	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/imapd"
	"github.com/linchange/linchange/internal/mapihttp/transport"
	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// Options are the inputs to New.
//
// Mail, Messages, Mailboxes and Users feed the ROP layer, so an Outlook
// client reads and writes the same rows EWS, EAS, IMAP and the web client
// do; Users and Hostname additionally feed the address book, which is a
// projection of the same user directory. Auth is the shared verifier every
// other mail protocol holds, called with ActionMAPIHTTP so that "may this
// account speak MAPI/HTTP" is answered by one rule alongside the others.
type Options struct {
	DB        *sqlx.DB
	Mail      *webmail.Service
	Messages  *imapd.MessageRepo
	Mailboxes *imapd.MailboxRepo
	Users     *store.Users
	// Contacts is the address-book store. The GAL adds titles and phone
	// numbers from its organization directory, and the mailbox store exposes
	// the account's books as Outlook contact folders. Left nil, the GAL is
	// the user list alone and there are no contact folders.
	Contacts *contacts.Service
	Hostname string
	Auth     transport.Authenticator
	Logger   *slog.Logger
	// ServerVersion, when set, is what the X-ServerApplication header
	// reports. Left empty it is the reference's default.
	ServerVersion string
	// MaxBodyBytes, when positive, bounds a request body. Left unset it
	// is ten megabytes.
	MaxBodyBytes int64
}

// Server is the MAPI/HTTP server: two endpoints behind one framing.
type Server struct {
	transport *transport.Transport
}

// New builds the server from its options.
func New(opts Options) *Server {
	return &Server{
		transport: transport.New(transport.Options{
			DB:            opts.DB,
			Mail:          opts.Mail,
			Messages:      opts.Messages,
			Mailboxes:     opts.Mailboxes,
			Users:         opts.Users,
			Contacts:      opts.Contacts,
			Hostname:      opts.Hostname,
			Auth:          opts.Auth,
			Logger:        opts.Logger,
			ServerVersion: opts.ServerVersion,
			MaxBodyBytes:  opts.MaxBodyBytes,
		}),
	}
}

// Handler returns the HTTP handler for both MAPI/HTTP endpoints.
//
// The routes are relative to the mount point: the server mounts this handler
// at /mapi, so "/emsmdb" here answers at "/mapi/emsmdb" and "/nspi" at
// /mapi/nspi. Each is registered with and without a trailing slash because
// that is how the two are written — a client that drops the trailing slash
// has not sent a different request, only written it differently.
func (s *Server) Handler() http.Handler {
	return s.transport.Handler()
}
