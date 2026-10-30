// Package httpapi is the chi router behind the REST API and the web interface.
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/linchange/linchange/internal/store"
)

// mountOrganization routes the reads the web client needs to know which
// organization the signed-in address belongs to: its contacts, and the
// workgroup itself for the header and the administration page.
//
// These are deliberately separate from /admin. A contact list is not a
// privileged resource — everyone in the organization is meant to be able to
// write to everyone else in it — so putting it under the admin group would
// make the address book unusable for exactly the people it is for.
func (s *Server) mountOrganization(r chi.Router) {
	r.Route("/organization", func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Use(s.requirePasswordChanged)
		r.Get("/", s.handleMyOrganization)
		r.Get("/people", s.handleOrganizationPeople)
		// The groups are here, next to the people, for the same reason the
		// people are not under /admin: choosing "invite the sales team" or
		// "share with everyone" is a thing every member does, not an
		// administrative act.
		r.Get("/groups", s.handleOrganizationGroups)
	})
}

// organizationFor resolves the organization a principal belongs to.
//
// Membership is by domain, not by a membership row: whoever has an address in
// one of the domains an organization owns is part of it. The second lookup
// covers the impersonating admin, whose session principal is the address they
// are acting as.
func (s *Server) organizationFor(ctx context.Context, address string) (store.Workgroup, bool) {
	wg, err := s.workgroups.ForAddress(ctx, s.db, address)
	if err != nil {
		return store.Workgroup{}, false
	}
	return wg, true
}

// handleMyOrganization returns the caller's organization, or an empty object
// when their domain belongs to none.
func (s *Server) handleMyOrganization(w http.ResponseWriter, r *http.Request) {
	wg, found := s.organizationFor(r.Context(), Principal(r.Context()).Address)
	if !found {
		s.writeJSON(w, http.StatusOK, map[string]any{"organization": nil})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"organization": viewWorkgroup(wg)})
}

// handleOrganizationPeople lists every enabled mailbox in the domains the
// caller's organization owns.
//
// When the caller's domain belongs to no organization, the domain is the group:
// the list is then everyone on that domain, which is still the set of people one
// would expect to be able to write to.
func (s *Server) handleOrganizationPeople(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	address := strings.ToLower(Principal(ctx).Address)
	_, domain, found := strings.Cut(address, "@")
	if !found {
		s.writeJSON(w, http.StatusOK, map[string]any{"people": []contactView{}})
		return
	}

	domains := []string{domain}
	orgID := int64(0)
	if wg, ok := s.organizationFor(ctx, address); ok {
		orgID = wg.ID
		if owned, err := s.workgroups.Domains(ctx, s.db, wg.ID); err == nil && len(owned) > 0 {
			domains = owned
		}
	}

	var people []store.Contact
	var err error
	if orgID != 0 {
		people, err = s.workgroups.People(ctx, s.db, orgID)
	} else {
		people, err = s.workgroups.PeopleInDomains(ctx, s.db, domains)
	}
	if err != nil {
		s.log.Error("listing organization contacts", "error", err, "address", address)
		s.writeError(w, http.StatusInternalServerError, "could not load contacts")
		return
	}
	out := make([]contactView, 0, len(people))
	for _, c := range people {
		out = append(out, viewContact(c))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"people": out})
}

// handleOrganizationGroups lists the groups in the caller's organization.
//
// The list is names, counts and ids rather than membership. A member picking a
// group to invite or to share a calendar with needs to know which one they
// mean; the server expands it when the invitation goes out, so the addresses
// are neither something the client has to carry nor something it can get subtly
// wrong. Somebody whose domain belongs to no organization gets an empty list,
// which is also the truth.
func (s *Server) handleOrganizationGroups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wg, found := s.organizationFor(ctx, Principal(ctx).Address)
	if !found {
		s.writeJSON(w, http.StatusOK, map[string]any{"groups": []workgroupGroupView{}})
		return
	}
	groups, err := store.NewGroups().List(ctx, s.db, wg.ID)
	if err != nil {
		s.log.Error("listing organization groups", "error", err)
		s.writeError(w, http.StatusInternalServerError, "could not load groups")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"groups": viewWorkgroupGroups(groups)})
}

// contactView is one address in the organization.
//
// It is written out by hand rather than serialised from the row struct so that
// the field names stay the ones the client expects, the convention every other
// view in this package follows.
type contactView struct {
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
}

func viewContact(c store.Contact) contactView {
	return contactView{Address: c.Address, DisplayName: c.DisplayName}
}
