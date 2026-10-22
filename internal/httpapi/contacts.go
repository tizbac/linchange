package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/contacts"
	"github.com/linchange/linchange/internal/store"
)

// mountContacts adds the web client's address book routes.
//
// CardDAV speaks the same store through internal/carddav; these routes exist so
// the web app can draw the same books, and so a person who shares one gets a
// place to answer the offer.
func (s *Server) mountContacts(r chi.Router) {
	r.Route("/address-books", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Get("/", s.handleListAddressBooks)
		r.Post("/", s.handleCreateAddressBook)
		// Static routes before the id parameter, so "offers" is not mistaken
		// for the id of a book.
		r.Get("/offers", s.handleListAddressBookOffers)
		r.Post("/offers/{shareID}/accept", s.handleDecideAddressBookOffer("accepted"))
		r.Post("/offers/{shareID}/decline", s.handleDecideAddressBookOffer("declined"))

		r.Get("/{id}", s.handleGetAddressBook)
		r.Patch("/{id}", s.handleUpdateAddressBook)
		r.Delete("/{id}", s.handleDeleteAddressBook)

		r.Get("/{id}/contacts", s.handleListContacts)
		r.Post("/{id}/contacts", s.handleCreateContact)
		r.Get("/{id}/contacts/{contactID}", s.handleGetContact)
		r.Patch("/{id}/contacts/{contactID}", s.handleUpdateContact)
		r.Delete("/{id}/contacts/{contactID}", s.handleDeleteContact)

		r.Get("/{id}/shares", s.handleListAddressBookShares)
		r.Post("/{id}/shares", s.handleShareAddressBook)
		r.Delete("/{id}/shares/{shareID}", s.handleUnshareAddressBook)
	})

	r.Route("/contacts", func(r chi.Router) {
		r.Use(s.requireAuth)
		// The composer and the meeting form both call this: one search over
		// every book the caller can read, with the source named.
		r.Get("/search", s.handleContactSearch)
	})
}

func (s *Server) handleListAddressBooks(w http.ResponseWriter, r *http.Request) {
	books, err := s.contacts.Books(r.Context(), Principal(r.Context()).Address)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"books": books})
}

func (s *Server) handleCreateAddressBook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	book, err := s.contacts.CreateBook(r.Context(), Principal(r.Context()).Address, in.Name, in.Description)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, book)
}

func (s *Server) handleGetAddressBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, book)
}

func (s *Server) handleUpdateAddressBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	updated, err := s.contacts.UpdateBook(r.Context(), Principal(r.Context()).Address, book.ID, in.Name, in.Description)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteAddressBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	if err := s.contacts.DeleteBook(r.Context(), Principal(r.Context()).Address, book.ID); err != nil {
		s.writeContactError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListContacts(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	entries, err := s.contacts.Cards(r.Context(), Principal(r.Context()).Address, book.ID)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	// The list does not need each card's wire form; the editor fetches the
	// single card when it opens.
	for i := range entries {
		entries[i].VCard = ""
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"contacts": entries})
}

func (s *Server) handleCreateContact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	var in contacts.Input
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	entry, err := s.contacts.CreateCard(r.Context(), Principal(r.Context()).Address, book.ID, in)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleGetContact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "contactID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	entry, err := s.contacts.GetCard(r.Context(), Principal(r.Context()).Address, book.ID, id)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleUpdateContact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "contactID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	var in contacts.Input
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	entry, err := s.contacts.UpdateCard(r.Context(), Principal(r.Context()).Address, book.ID, id, in)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "contactID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid contact id")
		return
	}
	if err := s.contacts.DeleteCard(r.Context(), Principal(r.Context()).Address, book.ID, id); err != nil {
		s.writeContactError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContactSearch(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	suggestions, err := s.contacts.Search(r.Context(), Principal(r.Context()).Address,
		r.URL.Query().Get("q"), limit)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"suggestions": suggestions})
}

// --- sharing -------------------------------------------------------------

type addressBookShareView struct {
	store.AddressBookShare
	GroupName string `json:"groupName,omitempty"`
}

func (s *Server) handleListAddressBookShares(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	shares, err := s.contacts.Shares(r.Context(), Principal(r.Context()).Address, book.ID)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"shares": s.viewAddressBookShares(r, shares)})
}

func (s *Server) handleShareAddressBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	var in struct {
		SubjectKind    string `json:"subject_kind"`
		SubjectAddress string `json:"subject_address"`
		SubjectGroupID int64  `json:"subject_group_id"`
		Level          string `json:"level"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.writeError(w, http.StatusBadRequest, "malformed request")
		return
	}
	if in.Level == "" {
		in.Level = store.AddressBookShareRead
	}
	if in.Level != store.AddressBookShareRead && in.Level != store.AddressBookShareWrite {
		s.writeError(w, http.StatusBadRequest, "level must be read or write")
		return
	}

	var recipients []string
	switch in.SubjectKind {
	case store.ShareSubjectUser:
		if strings.TrimSpace(in.SubjectAddress) == "" {
			s.writeError(w, http.StatusBadRequest, "subject_address is required")
			return
		}
		recipients = []string{strings.TrimSpace(in.SubjectAddress)}
	case store.ShareSubjectGroup:
		if in.SubjectGroupID == 0 {
			s.writeError(w, http.StatusBadRequest, "subject_group_id is required")
			return
		}
		group, err := store.NewGroups().Get(r.Context(), s.db, in.SubjectGroupID)
		if err != nil {
			s.writeError(w, mapErrorCode(err), err.Error())
			return
		}
		if wg, ok := s.organizationFor(r.Context(), Principal(r.Context()).Address); !ok || wg.ID != group.WorkgroupID {
			s.writeError(w, http.StatusNotFound, "no such thing")
			return
		}
		// Sharing a personal book with Everyone is a directory, not an offer;
		// an organization book is the way to make one.
		if group.Everyone {
			s.writeError(w, http.StatusBadRequest,
				"an address book cannot be shared with everyone; choose a group or a person")
			return
		}
		members, err := store.NewGroups().Members(r.Context(), s.db, in.SubjectGroupID)
		if err != nil {
			s.writeError(w, mapErrorCode(err), err.Error())
			return
		}
		recipients = members
	default:
		s.writeError(w, http.StatusBadRequest, "subject_kind must be user or group")
		return
	}

	share, err := s.contacts.Share(r.Context(), Principal(r.Context()).Address, book.ID,
		in.SubjectKind, in.SubjectAddress, in.SubjectGroupID, in.Level)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	// The offer is real only once its recipients know it exists.
	s.notifyAddressBookOffer(r, Principal(r.Context()).Address, book, recipients)
	views := s.viewAddressBookShares(r, []store.AddressBookShare{share})
	s.writeJSON(w, http.StatusCreated, views[0])
}

func (s *Server) handleUnshareAddressBook(w http.ResponseWriter, r *http.Request) {
	book, ok := s.addressBookFromPath(w, r)
	if !ok {
		return
	}
	shareID, err := strconv.ParseInt(chi.URLParam(r, "shareID"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid share id")
		return
	}
	if err := s.contacts.Unshare(r.Context(), Principal(r.Context()).Address, book.ID, shareID); err != nil {
		s.writeContactError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListAddressBookOffers(w http.ResponseWriter, r *http.Request) {
	offers, err := s.contacts.Offers(r.Context(), Principal(r.Context()).Address)
	if err != nil {
		s.writeContactError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"offers": offers})
}

func (s *Server) handleDecideAddressBookOffer(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		shareID, err := strconv.ParseInt(chi.URLParam(r, "shareID"), 10, 64)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid share id")
			return
		}
		share, err := s.contacts.Decide(r.Context(), Principal(r.Context()).Address, shareID, decision)
		if err != nil {
			s.writeContactError(w, err)
			return
		}
		s.writeJSON(w, http.StatusOK, share)
	}
}

// addressBookFromPath loads the book named by the path, writing the failure.
func (s *Server) addressBookFromPath(w http.ResponseWriter, r *http.Request) (store.AddressBook, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid address book id")
		return store.AddressBook{}, false
	}
	access, err := s.contacts.Book(r.Context(), Principal(r.Context()).Address, id)
	if err != nil {
		s.writeContactError(w, err)
		return store.AddressBook{}, false
	}
	return access.AddressBook, true
}

// notifyAddressBookOffer files a note into each recipient's inbox saying that a
// book is waiting for them to answer. It is best effort: the share is already
// written, and an offer email that fails is a share the owner can still explain
// in person.
func (s *Server) notifyAddressBookOffer(r *http.Request, owner string, book store.AddressBook, recipients []string) {
	body := fmt.Sprintf("%s has shared the address book %q with you.\n\n"+
		"Open Contacts to accept or decline the offer.\n", owner, book.Name)
	seen := make(map[string]bool, len(recipients))
	notified := make([]string, 0, len(recipients))
	for _, to := range recipients {
		address := strings.ToLower(strings.TrimSpace(to))
		if address == "" || seen[address] || address == strings.ToLower(owner) {
			continue
		}
		seen[address] = true
		notified = append(notified, address)
	}
	if len(notified) == 0 {
		return
	}
	if err := s.mail.SendInternal(r.Context(), owner, notified,
		"Address book shared with you: "+book.Name, body); err != nil {
		s.log.Warn("could not notify the recipients of an address book share",
			"book_id", book.ID, "error", err)
	}
}

func (s *Server) viewAddressBookShares(r *http.Request, shares []store.AddressBookShare) []addressBookShareView {
	out := make([]addressBookShareView, 0, len(shares))
	for _, share := range shares {
		view := addressBookShareView{AddressBookShare: share}
		if share.SubjectGroupID != nil {
			if group, err := store.NewGroups().Get(r.Context(), s.db, *share.SubjectGroupID); err == nil {
				view.GroupName = group.Name
			}
		}
		out = append(out, view)
	}
	return out
}

func (s *Server) writeContactError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, contacts.ErrInvalid):
		s.writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, contacts.ErrDenied):
		s.writeError(w, http.StatusForbidden, "this address book does not grant that")
	case errors.Is(err, contacts.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such thing")
	default:
		s.log.Error("contact request failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "the contact request failed")
	}
}
