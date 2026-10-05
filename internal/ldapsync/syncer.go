// Package ldapsync reconciles the local principals that are backed by a
// directory with what that directory currently says.
//
// The login path binds a person's password against the directory; the reconcile
// keeps the local rows in step so there is something to log in to. It is
// deliberately conservative. It provisions an entry the first time it sees it,
// refreshes the fields the directory owns, disables an entry that has gone
// missing for long enough, and never deletes mail or reclassifies a principal:
// an operator's decision outranks the directory's opinion.
package ldapsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	gldap "github.com/go-ldap/ldap/v3"
	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
	"github.com/linchange/linchange/internal/ldapdial"
	"github.com/linchange/linchange/internal/store"
)

// Syncer runs the periodic reconcile. The zero value is not usable; call New.
type Syncer struct {
	ldap config.LDAP
	db   *sqlx.DB
	log  *slog.Logger
	// now is the clock, overridable so a test can decide when an entry has
	// been missing long enough.
	now func() time.Time

	links   *store.LDAPLinks
	state   *store.LDAPSyncState
	users   *store.Users
	domains *store.Domains

	// mu guards cron, which Start writes and Stop reads.
	mu   sync.Mutex
	cron *cron.Cron
	// busy admits one reconcile at a time. A run that outlasts its interval
	// must not overlap the next one, and skipping is better than queueing.
	busy    sync.Mutex
	running sync.WaitGroup

	stopOnce sync.Once
}

// New returns a Syncer. now may be nil, which means the system clock.
func New(cfg config.LDAP, db *sqlx.DB, log *slog.Logger, now func() time.Time) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Syncer{
		ldap:    cfg,
		db:      db,
		log:     log,
		now:     now,
		links:   store.NewLDAPLinks(),
		state:   store.NewLDAPSyncState(),
		users:   store.NewUsers(),
		domains: store.NewDomains(),
	}
}

// Start begins reconciling on the configured interval and runs once
// immediately, so a freshly started server does not wait a period to have its
// directory users provisioned. It is a no-op when the sync is disabled or no
// provider is configured, which is what a deployment without a directory gets.
func (s *Syncer) Start(ctx context.Context) {
	if !s.ldap.Sync.Enabled || len(s.ldap.Providers) == 0 {
		return
	}
	c := cron.New()
	if iv := s.ldap.Sync.Interval; iv > 0 {
		c.Schedule(interval{d: iv}, job(func() { s.run(ctx) }))
	}
	s.mu.Lock()
	s.cron = c
	s.mu.Unlock()
	c.Start()

	s.log.Info("directory reconcile started",
		"providers", len(s.ldap.Providers),
		"interval", s.ldap.Sync.Interval)
	s.run(ctx)
}

// Stop ends the schedule and waits for a run in flight. It is safe to call
// more than once and safe to call when Start never did.
func (s *Syncer) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		c := s.cron
		s.mu.Unlock()
		if c == nil {
			return
		}
		// Stop returns a context that closes once the running jobs finish.
		<-c.Stop().Done()
		s.running.Wait()
	})
}

// interval is the cron schedule for a fixed period. robfig/cron v3.0.1 takes a
// Schedule interface, and a period is all this needs.
type interval struct{ d time.Duration }

func (i interval) Next(t time.Time) time.Time { return t.Add(i.d) }

// job adapts a function to the Job interface the scheduler runs.
type job func()

func (j job) Run() { j() }

// run reconciles every provider, off the caller's goroutine so that neither
// Start nor the scheduler blocks on a directory that is slow to answer.
func (s *Syncer) run(ctx context.Context) {
	if !s.busy.TryLock() {
		s.log.Warn("directory reconcile still running; skipping this one")
		return
	}
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		defer s.busy.Unlock()
		s.RunAll(ctx)
	}()
}

// RunAll reconciles every configured provider once and returns the first
// failure. A provider that fails does not stop the others: one unreachable
// directory should not freeze the rest.
func (s *Syncer) RunAll(ctx context.Context) error {
	var firstErr error
	for _, p := range s.ldap.Providers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.Reconcile(ctx, p); err != nil {
			s.log.Error("directory reconcile failed", "provider", p.Name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// syncStats is what one pass did to the local principals.
type syncStats struct {
	seen     int64
	added    int64
	updated  int64
	disabled int64
}

// entryOutcome is what happened to one directory entry. The zero value means
// nothing changed, which is the common case and is not worth a counter.
type entryOutcome int

const (
	outcomeNone entryOutcome = iota
	outcomeAdded
	outcomeUpdated
	outcomeDisabled
)

func (o entryOutcome) add(s *syncStats) {
	switch o {
	case outcomeAdded:
		s.added++
	case outcomeUpdated:
		s.updated++
	case outcomeDisabled:
		s.disabled++
	}
}

// Reconcile brings one provider's local principals in line with the directory
// and records the outcome. The returned error is what the sync state shows the
// operator, so it is wrapped with the provider that produced it.
func (s *Syncer) Reconcile(ctx context.Context, p config.LDAPProvider) (err error) {
	now := s.now()
	stats := &syncStats{}
	// Registered before the connection's Close so the state is written after
	// the handle is gone; the write needs no directory, only the database.
	defer func() { s.record(ctx, p, err == nil, errText(err), stats) }()

	if p.BindDN == "" {
		// A provider that authenticates people by binding as themselves cannot
		// also be enumerated: there is no account to search with.
		return fmt.Errorf("provider %s: no bind DN to enumerate users with", p.Name)
	}
	conn, err := ldapdial.Dial(&p)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := conn.Bind(p.BindDN, p.BindPassword); err != nil {
		return fmt.Errorf("provider %s: binding as %s: %w", p.Name, p.BindDN, err)
	}

	// The admin group, read before the users so that a member of it is known
	// by the time their entry is.
	groupMembers, err := adminGroupMembers(conn, p)
	if err != nil {
		return fmt.Errorf("provider %s: reading the domain admins group: %w", p.Name, err)
	}

	hosted, err := s.hostedDomains(ctx)
	if err != nil {
		// Classifying users against a partial set of domains would disable
		// the mailboxes of a domain that was merely missed by the listing, so
		// the whole pass is abandoned instead.
		return err
	}

	filter, err := StripUsernameAssertion(p.UserFilter)
	if err != nil {
		return fmt.Errorf("provider %s: user filter: %w", p.Name, err)
	}
	attrs := wantedAttributes(p)

	linked, err := s.links.ListForProvider(ctx, s.db, p.Name)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(linked))
	hadLink := make(map[string]bool, len(linked))
	for _, l := range linked {
		key := strings.ToLower(l.Address)
		hadLink[key] = true
	}

	search := &gldap.SearchRequest{
		BaseDN:     p.BaseDN,
		Scope:      gldap.ScopeWholeSubtree,
		Filter:     filter,
		Attributes: attrs,
	}

	// Paged search only when a page size is configured: a directory that does
	// not implement the control answers a paged request with the whole result
	// set, and the sync is correct either way.
	var entries []*gldap.Entry
	if size := s.ldap.Sync.PageSize; size > 0 {
		res, err := conn.SearchWithPaging(search, size)
		if err != nil {
			return fmt.Errorf("provider %s: searching users: %w", p.Name, err)
		}
		entries = res.Entries
	} else {
		res, err := conn.Search(search)
		if err != nil {
			return fmt.Errorf("provider %s: searching users: %w", p.Name, err)
		}
		entries = res.Entries
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.seen++
		attrs := lowerAttributes(e)
		address, ok := mailAddress(attrs, p.Attributes.Mail, p.DefaultDomain, e.DN)
		if !ok {
			// An entry with no address cannot be a local principal. It is not
			// an error: directories hold service accounts and groups too.
			s.log.Warn("directory entry has no mail address; skipping",
				"provider", p.Name, "dn", e.DN)
			continue
		}
		seen[strings.ToLower(address)] = true

		memberOf := attrValues(attrs, p.Attributes.MemberOf)
		isAdmin := groupMembers[strings.ToLower(e.DN)] ||
			containsFold(memberOf, p.DomainAdminsGroupDN)
		out, err := s.applyEntry(ctx, p, address, firstAttr(attrs, p.Attributes.DisplayName),
			e.DN, memberOf, hosted[domainOf(address)], isAdmin, hadLink[strings.ToLower(address)])
		if err != nil {
			// One bad entry should not abandon the pass; the rest still need
			// reconciling and the counters stay honest.
			s.log.Error("applying directory entry failed",
				"provider", p.Name, "dn", e.DN, "address", address, "error", err)
			continue
		}
		out.add(stats)
	}

	s.handleMissing(ctx, p, linked, seen, now)
	return nil
}

// handleMissing disables the principals a provider linked that its latest pass
// did not see, once they have been gone for the configured grace. The grace is
// what makes a directory that is briefly incomplete, or a filter an operator
// just narrowed, unable to lock anyone out on its own.
func (s *Syncer) handleMissing(
	ctx context.Context, p config.LDAPProvider, linked []store.LDAPLink, seen map[string]bool, now time.Time,
) {
	grace := s.ldap.Sync.DisableMissingAfter
	for _, l := range linked {
		if seen[strings.ToLower(l.Address)] {
			continue
		}
		if now.Sub(l.LastSeenAt) < grace {
			s.log.Debug("directory entry missing but within the grace period",
				"provider", p.Name, "address", l.Address)
			continue
		}
		if err := s.users.SetEnabled(ctx, s.db, l.Address, false); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The principal is gone; so is its link, by cascade. Clearing
				// it keeps the next pass from trying again.
				if derr := s.links.Delete(ctx, s.db, l.Address); derr != nil &&
					!errors.Is(derr, store.ErrNotFound) {
					s.log.Error("dropping the link of a vanished principal failed",
						"provider", p.Name, "address", l.Address, "error", derr)
				}
				continue
			}
			s.log.Error("disabling a principal that left the directory failed",
				"provider", p.Name, "address", l.Address, "error", err)
			continue
		}
		// The link stays. It records why the principal is disabled, and its
		// stale LastSeenAt is what keeps the next pass looking for the entry
		// that may come back.
	}
}

// applyEntry writes one directory entry through a transaction, so the principal
// and the link that explains it appear together or not at all.
func (s *Syncer) applyEntry(
	ctx context.Context, p config.LDAPProvider,
	address, display, dn string, memberOf []string, hosted, isAdmin, hadLink bool,
) (entryOutcome, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return outcomeNone, fmt.Errorf("beginning the entry transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	out, err := s.applyEntryTx(ctx, tx, p, address, display, dn, memberOf, hosted, isAdmin, hadLink)
	if err != nil {
		return outcomeNone, err
	}
	if err := tx.Commit(); err != nil {
		return outcomeNone, fmt.Errorf("committing the entry transaction: %w", err)
	}
	return out, nil
}

// applyEntryTx does the work of applyEntry inside its transaction.
func (s *Syncer) applyEntryTx(
	ctx context.Context, tx *sqlx.Tx, p config.LDAPProvider,
	address, display, dn string, memberOf []string, hosted, isAdmin, hadLink bool,
) (entryOutcome, error) {
	// The link goes in last: it references the principal, so writing it first
	// would fail on the foreign key for an entry being created.
	link := func() error {
		return s.links.Upsert(ctx, tx, store.LDAPLink{
			Address:     address,
			Provider:    p.Name,
			DN:          dn,
			DisplayName: display,
			MemberOf:    memberOf,
		}, s.now())
	}

	existing, err := s.users.Get(ctx, tx, address)
	switch {
	case errors.Is(err, store.ErrNotFound):
		existing = store.User{}
	case err != nil:
		return outcomeNone, fmt.Errorf("reading principal %s: %w", address, err)
	default:
	}

	// A domain that hosts the address decides whether the person receives
	// mail. Everyone else needs the admin group to get an account at all.
	if hosted {
		if errors.Is(err, store.ErrNotFound) {
			err := s.users.Create(ctx, tx, store.CreateUserRequest{
				Address:     address,
				Source:      policy.SourceLDAP,
				DisplayName: display,
				QuotaBytes:  quotaPtr(p.MailboxQuotaBytes),
			})
			if err != nil {
				return outcomeNone, fmt.Errorf("provisioning %s: %w", address, err)
			}
			if err := link(); err != nil {
				return outcomeNone, err
			}
			return outcomeAdded, nil
		}
		err := s.users.Update(ctx, tx, address, store.UpdateUserRequest{
			DisplayName:    pickDisplay(display, existing.DisplayName),
			Enabled:        true,
			MailboxEnabled: true,
			// The update replaces the whole row, so anything this path does not
			// pass is cleared. Administration is not a directory fact: it is
			// granted here, and a sync that dropped it would silently demote an
			// administrator every time the directory ran.
			IsDomainAdmin:      existing.IsDomainAdmin,
			QuotaBytes:         quotaPtr(p.MailboxQuotaBytes),
			DailySendLimit:     existing.DailySendLimit,
			Timezone:           existing.Timezone,
			Language:           existing.Language,
			MustChangePassword: existing.MustChangePassword,
		})
		if err != nil {
			return outcomeNone, fmt.Errorf("updating %s: %w", address, err)
		}
		if err := link(); err != nil {
			return outcomeNone, err
		}
		return outcomeUpdated, nil
	}

	// Off-domain. Members of the admin group get an account that administers
	// mail and never receives it.
	if isAdmin {
		if errors.Is(err, store.ErrNotFound) {
			err := s.users.Create(ctx, tx, store.CreateUserRequest{
				Address:     address,
				Source:      policy.SourceDomainAdmin,
				DisplayName: display,
			})
			if err != nil {
				return outcomeNone, fmt.Errorf("provisioning %s: %w", address, err)
			}
			if err := link(); err != nil {
				return outcomeNone, err
			}
			return outcomeAdded, nil
		}
		out := outcomeNone
		if !existing.Enabled {
			if err := s.users.SetEnabled(ctx, tx, address, true); err != nil {
				return outcomeNone, fmt.Errorf("enabling %s: %w", address, err)
			}
			out = outcomeUpdated
		}
		if err := link(); err != nil {
			return outcomeNone, err
		}
		return out, nil
	}

	// Off-domain and not an admin: refused. A principal that was already
	// linked gets disabled and unlinked; one that was never linked is left
	// alone, because creating nothing is the whole point.
	if !hadLink {
		return outcomeNone, nil
	}
	if err := s.users.SetEnabled(ctx, tx, address, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return outcomeNone, nil
		}
		return outcomeNone, fmt.Errorf("disabling %s: %w", address, err)
	}
	// The link goes, so the login path stops treating this person as
	// directory-backed: an off-domain stranger must not be able to try a
	// password against the directory at all.
	if err := s.links.Delete(ctx, tx, address); err != nil && !errors.Is(err, store.ErrNotFound) {
		return outcomeNone, err
	}
	return outcomeDisabled, nil
}

// hostedDomains is the set of domains this server actually accepts mail for. A
// suspended domain still holds mail, so it counts; a disabled one does not.
func (s *Syncer) hostedDomains(ctx context.Context) (map[string]bool, error) {
	list, err := s.domains.List(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, d := range list {
		if d.Status != store.DomainStatusDisabled {
			out[strings.ToLower(d.Name)] = true
		}
	}
	return out, nil
}

// adminGroupMembers reads the member list of the admin group. AD publishes it
// on the group as "member"; other directories put the groups on the user as
// "memberOf". The syncer reads both, and the entry is accepted as an admin if
// either says so.
func adminGroupMembers(conn *gldap.Conn, p config.LDAPProvider) (map[string]bool, error) {
	out := map[string]bool{}
	if p.DomainAdminsGroupDN == "" {
		return out, nil
	}
	res, err := conn.Search(&gldap.SearchRequest{
		BaseDN:     p.DomainAdminsGroupDN,
		Scope:      gldap.ScopeBaseObject,
		Filter:     "(objectClass=*)",
		Attributes: []string{"member"},
	})
	if err != nil {
		return nil, err
	}
	for _, e := range res.Entries {
		for _, dn := range e.GetAttributeValues("member") {
			out[strings.ToLower(strings.TrimSpace(dn))] = true
		}
	}
	return out, nil
}

// wantedAttributes is the attribute list a search asks for: the mail,
// display-name and group attributes this provider maps, lowercased and
// deduplicated. A directory returns nothing but what is asked for, so asking is
// how the attributes arrive at all.
func wantedAttributes(p config.LDAPProvider) []string {
	var out []string
	add := func(names []string) {
		for _, n := range names {
			n = strings.ToLower(strings.TrimSpace(n))
			if n == "" {
				continue
			}
			dup := false
			for _, have := range out {
				if have == n {
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, n)
			}
		}
	}
	add(p.Attributes.Mail)
	add(p.Attributes.DisplayName)
	add(p.Attributes.MemberOf)
	return out
}

// lowerAttributes copies an entry's attributes under lowercased names, the way
// a server spells them. Attribute names are case-insensitive and servers are
// inconsistent about the casing, so the lookup normalizes both sides. A
// directory may repeat an attribute across entries, hence the slice.
func lowerAttributes(e *gldap.Entry) map[string][]string {
	out := make(map[string][]string, len(e.Attributes))
	for _, a := range e.Attributes {
		out[strings.ToLower(a.Name)] = append(out[strings.ToLower(a.Name)], a.Values...)
	}
	return out
}

// attrValues returns the values of the first of names the entry carries.
func attrValues(attrs map[string][]string, names []string) []string {
	for _, n := range names {
		if v, ok := attrs[strings.ToLower(strings.TrimSpace(n))]; ok {
			return v
		}
	}
	return nil
}

// firstAttr is the first value of the first of names the entry carries.
func firstAttr(attrs map[string][]string, names []string) string {
	if v := attrValues(attrs, names); len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// containsFold reports whether values holds want, ignoring case and surrounding
// space. Directory DNs are compared this way because servers differ on both.
func containsFold(values []string, want string) bool {
	if strings.TrimSpace(want) == "" {
		return false
	}
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// mailAddress decides what local address an entry maps to.
//
// The mail attribute is the answer when there is one. A mail attribute may hold
// several addresses, and a prefixed "SMTP:" one names the address mail actually
// goes to, so it wins over a bare alias. With no mail attribute at all, the
// entry's DN leaf becomes the local part under the provider's default domain:
// some directories simply do not publish mail, and refusing those entries
// would leave a configured directory unable to provision anyone.
func mailAddress(attrs map[string][]string, candidates []string, defaultDomain, dn string) (string, bool) {
	var plain string
	for _, v := range attrValues(attrs, candidates) {
		for _, candidate := range strings.Split(v, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			if rest, ok := cutPrefixFold(candidate, "SMTP:"); ok {
				if rest != "" {
					return strings.ToLower(rest), true
				}
				continue
			}
			if plain == "" && strings.Contains(candidate, "@") {
				plain = candidate
			}
		}
	}
	if plain != "" {
		return strings.ToLower(plain), true
	}
	domain := strings.ToLower(strings.TrimSpace(defaultDomain))
	if domain == "" {
		return "", false
	}
	local := dnLocalPart(dn)
	if local == "" {
		return "", false
	}
	return strings.ToLower(local + "@" + domain), true
}

// cutPrefixFold removes a case-insensitive prefix.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// dnLocalPart returns the value of the first RDN of a DN, which is the
// conventional human name of the entry.
func dnLocalPart(dn string) string {
	first := dn
	if i := strings.IndexByte(first, ','); i >= 0 {
		first = first[:i]
	}
	i := strings.IndexByte(first, '=')
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(first[i+1:])
}

// domainOf returns the lowercased part of an address after the "@", or "" when
// it has none.
func domainOf(address string) string {
	if i := strings.LastIndexByte(address, '@'); i >= 0 {
		return strings.ToLower(address[i+1:])
	}
	return ""
}

// quotaPtr turns a configured quota into the pointer the store takes, with
// zero meaning "inherit" rather than "no quota".
func quotaPtr(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	v := n
	return &v
}

// pickDisplay keeps the current display name when the directory reports none,
// so an entry that carries only a login does not blank out a name an operator
// chose.
func pickDisplay(fromDirectory, current string) string {
	if fromDirectory == "" {
		return current
	}
	return fromDirectory
}

// record writes what the pass did. It is bookkeeping for the operator, so a
// failure to write it is logged and swallowed: the reconcile itself has already
// succeeded, and reporting an error here would mark a good run as failed. The
// timestamp is taken here, at the end of the pass, because the store records
// when a run finished rather than when it began.
func (s *Syncer) record(ctx context.Context, p config.LDAPProvider, ok bool, failure string, stats *syncStats) {
	ranAt := s.now()
	var successAt *time.Time
	if ok {
		t := ranAt
		successAt = &t
	}
	if err := s.state.RecordRun(ctx, s.db, p.Name, ranAt, successAt, failure,
		stats.seen, stats.added, stats.updated, stats.disabled); err != nil {
		s.log.Error("recording the directory sync state failed", "provider", p.Name, "error", err)
	}
}

// errText is the string a failed run is recorded with.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// StripUsernameAssertion removes the {{username}} assertion from a user filter,
// leaving the part that selects the kind of entry to reconcile.
//
// The login path replaces {{username}} with the name being authenticated and
// runs the same filter; the reconcile needs every entry that filter describes,
// not one of them. What remains must still be a group: a bare "(mail=*)" has
// no shape to search with once its assertion is gone, so it is refused rather
// than silently widened into something the operator did not write.
func StripUsernameAssertion(filter string) (string, error) {
	if !strings.Contains(filter, "{{username}}") {
		return filter, nil
	}
	trimmed := strings.TrimSpace(filter)
	if !strings.HasPrefix(trimmed, "(") || !strings.HasSuffix(trimmed, ")") {
		return "", errors.New("filter is not a group")
	}
	inner := trimmed[1 : len(trimmed)-1]
	if inner == "" {
		return "", errors.New("filter is not a group")
	}
	op := inner[0]
	if op != '&' && op != '|' {
		return "", errors.New("filter is not an AND or OR group")
	}

	// Collect the top-level children, tracking nesting so an assertion that
	// contains groups of its own survives intact.
	var kept []string
	depth := 0
	start := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth < 0 {
				return "", errors.New("filter has an unbalanced parenthesis")
			}
			if depth == 0 {
				child := inner[start : i+1]
				if !strings.Contains(child, "{{username}}") {
					kept = append(kept, child)
				}
			}
		}
	}
	if depth != 0 {
		return "", errors.New("filter has an unbalanced parenthesis")
	}
	if len(kept) == 0 {
		return "", errors.New("no assertion survives the removal of {{username}}")
	}
	return "(" + string(op) + strings.Join(kept, "") + ")", nil
}
