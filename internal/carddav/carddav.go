// Package carddav exposes the address books over CardDAV (RFC 6352) and
// WebDAV (RFC 4918).
//
// The protocol is handled by emersion/go-webdav; this package is the backend:
// it turns the authenticated principal and a URL into one of this server's
// address books, and a card in the request into a row. The paths are the shape
// the library expects:
//
//	/carddav/                                  the service root
//	/carddav/alice@example.com                 the principal
//	/carddav/alice@example.com/                the address book home
//	/carddav/alice@example.com/personal/       a book alice owns
//	/carddav/alice@example.com/~book-7/        a book shared with alice
//	/carddav/alice@example.com/personal/x.vcf  a card
//
// A book somebody else owns is addressed by a stable alias rather than by the
// owner's path, because every member must see it under their own home: a client
// given one principal's path to another principal's book would send writes to a
// collection the server never offered it.
package carddav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	webdavcarddav "github.com/emersion/go-webdav/carddav"
	"github.com/jmoiron/sqlx"

	"github.com/linchange/linchange/internal/auth/mailauth"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/store"
)

// maxResourceSize is advertised to clients so one can tell before sending a
// card whether the server will accept it.
const maxResourceSize = 64 * 1024

type principalKey struct{}

// Server is the CardDAV service.
type Server struct {
	prefix   string
	contacts *contacts.Service
	db       *sqlx.DB
	auth     *mailauth.Verifier
}

// NewServer builds the service. The verifier is the same one the mail and
// calendar listeners hold, so a client cannot come to disagree with them about
// who an account is.
func NewServer(prefix string, svc *contacts.Service, db *sqlx.DB, auth *mailauth.Verifier) *Server {
	if prefix == "" {
		prefix = "/carddav"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return &Server{
		prefix:   strings.TrimSuffix(prefix, "/"),
		contacts: svc,
		db:       db,
		auth:     auth,
	}
}

// Handler returns the authenticated CardDAV handler, mounted at the prefix.
func (s *Server) Handler() http.Handler {
	inner := &webdavcarddav.Handler{
		Backend: &backend{prefix: s.prefix, contacts: s.contacts},
		Prefix:  s.prefix,
	}
	return s.authenticate(inner)
}

// WellKnownHandler answers the RFC 6764 discovery path.
//
// It redirects to the service root rather than to a principal, because the
// principal is what the root PROPFIND reports for the credentials the client
// then presents. Redirecting per user before authentication would mean either
// requiring a password on the discovery request or guessing a user from an
// unauthenticated request.
func (s *Server) WellKnownHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, s.prefix+"/", http.StatusFound)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth == nil {
			s.challenge(w)
			return
		}
		login, password, ok := r.BasicAuth()
		if !ok {
			s.challenge(w)
			return
		}
		who, err := s.auth.Authenticate(r.Context(), login, password, policy.ActionCardDAV)
		if err != nil {
			s.challenge(w)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, who)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="linchange CardDAV", charset="UTF-8"`)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
}

// backend implements the go-webdav CardDAV backend over the contact store.
type backend struct {
	prefix   string
	contacts *contacts.Service
}

func (b *backend) principal(ctx context.Context) (string, error) {
	who, ok := ctx.Value(principalKey{}).(policy.Principal)
	if !ok || who.Address == "" {
		return "", webdav.NewHTTPError(http.StatusUnauthorized, errors.New("authentication required"))
	}
	return strings.ToLower(strings.TrimSpace(who.Address)), nil
}

// CurrentUserPrincipal is the principal URL for the signed-in address.
func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	address, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.prefix + "/" + address + "/", nil
}

// AddressBookHomeSetPath is the collection holding the user's books.
func (b *backend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	address, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.home(address), nil
}

func (b *backend) home(address string) string {
	// The home is a collection below the principal rather than the principal
	// itself: RFC 6352's discovery gives the client two URLs, and a library
	// that has to tell the principal from its home reads the path depth.
	return b.prefix + "/" + address + "/contacts/"
}

// bookPath is the URL a book has under the signed-in user's home. An owned book
// uses its slug; a shared or organization book gets a stable alias.
func (b *backend) bookPath(address string, book store.AddressBookAccess) string {
	if strings.EqualFold(book.Owner, address) {
		return b.home(address) + book.Slug + "/"
	}
	return b.home(address) + "~book-" + strconv.FormatInt(book.ID, 10) + "/"
}

func (b *backend) ListAddressBooks(ctx context.Context) ([]webdavcarddav.AddressBook, error) {
	address, err := b.principal(ctx)
	if err != nil {
		return nil, err
	}
	books, err := b.contacts.Books(ctx, address)
	if err != nil {
		return nil, b.httpError(err)
	}
	out := make([]webdavcarddav.AddressBook, 0, len(books))
	for _, book := range books {
		out = append(out, webdavcarddav.AddressBook{
			Path:            b.bookPath(address, book),
			Name:            book.Name,
			Description:     book.Description,
			MaxResourceSize: maxResourceSize,
		})
	}
	return out, nil
}

func (b *backend) GetAddressBook(ctx context.Context, reqPath string) (*webdavcarddav.AddressBook, error) {
	book, remainder, err := b.resolveBook(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	if remainder != "" {
		return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	address, _ := b.principal(ctx)
	return &webdavcarddav.AddressBook{
		Path:            b.bookPath(address, book),
		Name:            book.Name,
		Description:     book.Description,
		MaxResourceSize: maxResourceSize,
	}, nil
}

func (b *backend) CreateAddressBook(ctx context.Context, ab *webdavcarddav.AddressBook) error {
	address, err := b.principal(ctx)
	if err != nil {
		return err
	}
	home := b.home(address)
	clean := strings.TrimSuffix(path.Clean(ab.Path), "/") + "/"
	if !strings.HasPrefix(clean, home) {
		return webdav.NewHTTPError(http.StatusForbidden, errors.New("a book must be created under your own home"))
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(clean, home), "/")
	if slug == "" || strings.Contains(slug, "/") {
		return webdav.NewHTTPError(http.StatusBadRequest, errors.New("invalid address book path"))
	}
	name := strings.TrimSpace(ab.Name)
	if name == "" {
		name = slug
	}
	if _, err := b.contacts.CreateBookAt(ctx, address, name, slug, ab.Description); err != nil {
		return b.httpError(err)
	}
	return nil
}

func (b *backend) DeleteAddressBook(ctx context.Context, reqPath string) error {
	book, remainder, err := b.resolveBook(ctx, reqPath)
	if err != nil {
		return err
	}
	if remainder != "" {
		return webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	address, _ := b.principal(ctx)
	if err := b.contacts.DeleteBook(ctx, address, book.ID); err != nil {
		return b.httpError(err)
	}
	return nil
}

func (b *backend) GetAddressObject(ctx context.Context, reqPath string, _ *webdavcarddav.AddressDataRequest) (*webdavcarddav.AddressObject, error) {
	book, uid, err := b.resolveObject(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	address, _ := b.principal(ctx)
	row, err := b.contacts.GetCardByUID(ctx, address, book.ID, uid)
	if err != nil {
		return nil, b.httpError(err)
	}
	return b.object(ctx, book, row)
}

func (b *backend) ListAddressObjects(ctx context.Context, reqPath string, _ *webdavcarddav.AddressDataRequest) ([]webdavcarddav.AddressObject, error) {
	book, remainder, err := b.resolveBook(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	if remainder != "" {
		return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	address, _ := b.principal(ctx)
	rows, err := b.contacts.CardRows(ctx, address, book.ID)
	if err != nil {
		return nil, b.httpError(err)
	}
	out := make([]webdavcarddav.AddressObject, 0, len(rows))
	for _, row := range rows {
		object, err := b.object(ctx, book, row)
		if err != nil {
			continue
		}
		out = append(out, *object)
	}
	return out, nil
}

func (b *backend) QueryAddressObjects(ctx context.Context, reqPath string, query *webdavcarddav.AddressBookQuery) ([]webdavcarddav.AddressObject, error) {
	objects, err := b.ListAddressObjects(ctx, reqPath, nil)
	if err != nil {
		return nil, err
	}
	if query == nil {
		return objects, nil
	}
	out := make([]webdavcarddav.AddressObject, 0, len(objects))
	for _, object := range objects {
		if cardMatches(object.Card, query) {
			out = append(out, object)
			if query.Limit > 0 && len(out) >= query.Limit {
				break
			}
		}
	}
	return out, nil
}

func (b *backend) PutAddressObject(ctx context.Context, reqPath string, card vcard.Card, _ *webdavcarddav.PutAddressObjectOptions) (*webdavcarddav.AddressObject, error) {
	book, uid, err := b.resolveObject(ctx, reqPath)
	if err != nil {
		return nil, err
	}
	address, _ := b.principal(ctx)
	entry, err := b.contacts.PutCard(ctx, address, book.ID, uid, card)
	if err != nil {
		return nil, b.httpError(err)
	}
	decoded, err := decode(entry.VCard)
	if err != nil {
		return nil, err
	}
	return &webdavcarddav.AddressObject{
		Path:          b.bookPath(address, book) + entry.UID + ".vcf",
		Card:          decoded,
		ModTime:       entry.UpdatedAt,
		ContentLength: int64(len(entry.VCard)),
		ETag:          etag(entry.UpdatedAt),
	}, nil
}

func (b *backend) DeleteAddressObject(ctx context.Context, reqPath string) error {
	book, uid, err := b.resolveObject(ctx, reqPath)
	if err != nil {
		return err
	}
	address, _ := b.principal(ctx)
	if err := b.contacts.DeleteCardByUID(ctx, address, book.ID, uid); err != nil {
		return b.httpError(err)
	}
	return nil
}

// resolveBook turns a URL into a book the signed-in user may use, plus the part
// of the path below it.
func (b *backend) resolveBook(ctx context.Context, reqPath string) (store.AddressBookAccess, string, error) {
	address, err := b.principal(ctx)
	if err != nil {
		return store.AddressBookAccess{}, "", err
	}
	home := b.home(address)
	clean := path.Clean(reqPath)
	if clean == strings.TrimSuffix(home, "/") || !strings.HasPrefix(clean+"/", home) {
		return store.AddressBookAccess{}, "", webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	rest := strings.TrimPrefix(clean+"/", home)
	parts := strings.SplitN(rest, "/", 2)
	key := parts[0]
	remainder := ""
	if len(parts) == 2 {
		remainder = parts[1]
	}
	books, err := b.contacts.Books(ctx, address)
	if err != nil {
		return store.AddressBookAccess{}, "", b.httpError(err)
	}
	for _, book := range books {
		if b.bookPath(address, book) == home+key+"/" {
			return book, remainder, nil
		}
	}
	return store.AddressBookAccess{}, "", webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
}

// resolveObject turns an object URL into its book and vCard UID.
func (b *backend) resolveObject(ctx context.Context, reqPath string) (store.AddressBookAccess, string, error) {
	book, remainder, err := b.resolveBook(ctx, reqPath)
	if err != nil {
		return store.AddressBookAccess{}, "", err
	}
	uid := strings.TrimSuffix(remainder, "/")
	if strings.HasSuffix(uid, ".vcf") {
		uid = strings.TrimSuffix(uid, ".vcf")
	}
	if uid == "" || strings.Contains(uid, "/") {
		return store.AddressBookAccess{}, "", webdav.NewHTTPError(http.StatusNotFound, errors.New("no such card"))
	}
	return book, uid, nil
}

func (b *backend) object(ctx context.Context, book store.AddressBookAccess, row store.AddressContact) (*webdavcarddav.AddressObject, error) {
	card, err := decode(row.VCard)
	if err != nil {
		return nil, err
	}
	address, _ := b.principal(ctx)
	return &webdavcarddav.AddressObject{
		Path:          b.bookPath(address, book) + row.UID + ".vcf",
		Card:          card,
		ModTime:       row.UpdatedAt,
		ContentLength: int64(len(row.VCard)),
		ETag:          etag(row.UpdatedAt),
	}, nil
}

func (b *backend) httpError(err error) error {
	switch {
	case errors.Is(err, contacts.ErrNotFound):
		return webdav.NewHTTPError(http.StatusNotFound, err)
	case errors.Is(err, contacts.ErrDenied):
		return webdav.NewHTTPError(http.StatusForbidden, errors.New("this address book does not grant that"))
	case errors.Is(err, contacts.ErrInvalid):
		return webdav.NewHTTPError(http.StatusBadRequest, err)
	default:
		return webdav.NewHTTPError(http.StatusInternalServerError, err)
	}
}

func decode(raw string) (vcard.Card, error) {
	card, err := vcard.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil || card == nil {
		return nil, webdav.NewHTTPError(http.StatusInternalServerError,
			fmt.Errorf("stored card is not a vCard: %w", err))
	}
	return card, nil
}

func etag(updatedAt interface{ UnixNano() int64 }) string {
	return fmt.Sprintf("\"%d\"", updatedAt.UnixNano())
}

// cardMatches applies the parts of an addressbook-query that clients actually
// send: property presence and a text match on a property, any or all of them.
func cardMatches(card vcard.Card, query *webdavcarddav.AddressBookQuery) bool {
	if len(query.PropFilters) == 0 {
		return true
	}
	matches := make([]bool, 0, len(query.PropFilters))
	for _, filter := range query.PropFilters {
		matches = append(matches, propMatches(card, filter))
	}
	if query.FilterTest == webdavcarddav.FilterAllOf {
		for _, ok := range matches {
			if !ok {
				return false
			}
		}
		return true
	}
	for _, ok := range matches {
		if ok {
			return true
		}
	}
	return false
}

func propMatches(card vcard.Card, filter webdavcarddav.PropFilter) bool {
	fields := card[strings.ToUpper(filter.Name)]
	if filter.IsNotDefined {
		return len(fields) == 0
	}
	if len(fields) == 0 {
		return false
	}
	if len(filter.TextMatches) == 0 {
		return true
	}
	any := filter.Test != webdavcarddav.FilterAllOf
	for _, match := range filter.TextMatches {
		hit := false
		for _, field := range fields {
			if textMatches(field.Value, match) {
				hit = true
				break
			}
		}
		if match.NegateCondition {
			hit = !hit
		}
		if any && hit {
			return true
		}
		if !any && !hit {
			return false
		}
	}
	return !any
}

func textMatches(value string, match webdavcarddav.TextMatch) bool {
	left := strings.ToLower(value)
	right := strings.ToLower(match.Text)
	switch match.MatchType {
	case webdavcarddav.MatchEquals:
		return left == right
	case webdavcarddav.MatchStartsWith:
		return strings.HasPrefix(left, right)
	case webdavcarddav.MatchEndsWith:
		return strings.HasSuffix(left, right)
	default: // MatchContains
		return strings.Contains(left, right)
	}
}
