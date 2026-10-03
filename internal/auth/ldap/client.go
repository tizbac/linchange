// Package ldap implements login.Directory for LDAP and Active Directory.
//
// A directory principal is bound to the directory entry that backs it by the
// row in ldap_links, which the periodic reconcile maintains. Authentication
// is the directory's bind operation itself: the password is never stored or
// compared here, it is presented to the directory and the directory's verdict
// is taken as the answer. A password change in the directory takes effect on
// the next sign-in, with nothing to rotate on this side.
package ldap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gldap "github.com/go-ldap/ldap/v3"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/ldapdial"
	"github.com/linchange/linchange/internal/store"
)

// Client authenticates principals against the configured directories. It is
// safe for concurrent use.
type Client struct {
	providers map[string]*config.LDAPProvider
	cacheTTL  time.Duration
	links     *store.LDAPLinks
	q         store.Querier
	now       func() time.Time

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
}

// cacheEntry is a resolved DN that is trusted until expires.
type cacheEntry struct {
	dn      string
	expires time.Time
}

// New builds a Client for the configured providers. q is where the links are
// read from, and now may be supplied by tests.
func New(lcfg config.LDAP, links *store.LDAPLinks, q store.Querier, now func() time.Time) *Client {
	providers := make(map[string]*config.LDAPProvider, len(lcfg.Providers))
	for i := range lcfg.Providers {
		p := lcfg.Providers[i]
		providers[p.Name] = &p
	}
	if now == nil {
		now = time.Now
	}
	return &Client{
		providers: providers,
		cacheTTL:  lcfg.CacheTTL,
		links:     links,
		q:         q,
		now:       now,
		cache:     make(map[string]cacheEntry),
	}
}

// Authenticate reports whether the directory accepts password for address,
// implementing login.Directory.
//
// The result is split the way the login layer splits it: (true, nil) is an
// accepted sign-in; (false, nil) is a consulted directory that refused the
// password, which is a failed attempt; an error means the directory could not
// be consulted at all, which must not count against the user.
func (c *Client) Authenticate(ctx context.Context, address, password string) (bool, error) {
	link, err := c.links.Get(ctx, c.q, address)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No link, no directory opinion: nothing can verify this address,
			// which reads as a refusal rather than an outage.
			return false, nil
		}
		return false, fmt.Errorf("ldap: reading the link for %s: %w", address, err)
	}

	prov, ok := c.providers[link.Provider]
	if !ok {
		// The link names a provider the configuration no longer has. Nothing
		// can verify against it, and the operator needs the failure to be
		// visible rather than a quiet refusal.
		return false, fmt.Errorf("ldap: provider %q linked to %s is not configured", link.Provider, address)
	}

	conn, err := ldapdial.Dial(prov)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	dn, ok, err := c.resolveDN(conn, prov, link, address)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	if err := conn.Bind(dn, password); err != nil {
		if gldap.IsErrorAnyOf(err, gldap.LDAPResultInvalidCredentials) {
			return false, nil
		}
		return false, fmt.Errorf("ldap: binding as %s: %w", address, err)
	}
	return true, nil
}

// resolveDN determines which DN the password will be checked as, and whether
// the directory has an entry for the address at all.
//
// Without a service account the link carries the DN the reconcile resolved,
// and the bind goes straight to it. With one, the entry is located by search,
// so a move in the directory does not lock its users out, and the answer is
// cached for the configured TTL so a login burst does not turn into a search
// burst.
func (c *Client) resolveDN(conn *gldap.Conn, prov *config.LDAPProvider, link store.LDAPLink, address string) (string, bool, error) {
	if prov.BindDN == "" {
		if link.DN == "" {
			return "", false, nil
		}
		return link.DN, true, nil
	}
	if c.cacheTTL > 0 {
		if dn, ok := c.cachedDN(address); ok {
			return dn, true, nil
		}
	}
	if err := conn.Bind(prov.BindDN, prov.BindPassword); err != nil {
		return "", false, fmt.Errorf("ldap: binding the service account of %s: %w", prov.Name, err)
	}
	filter := strings.ReplaceAll(prov.UserFilter, "{{username}}", gldap.EscapeFilter(address))
	res, err := conn.Search(&gldap.SearchRequest{
		BaseDN:     prov.BaseDN,
		Scope:      gldap.ScopeWholeSubtree,
		Filter:     filter,
		Attributes: []string{"dn"},
	})
	if err != nil {
		return "", false, fmt.Errorf("ldap: searching %s: %w", prov.Name, err)
	}
	if len(res.Entries) == 0 {
		return "", false, nil
	}
	dn := res.Entries[0].DN
	if c.cacheTTL > 0 {
		c.storeCachedDN(address, dn)
	}
	return dn, true, nil
}

func (c *Client) cachedDN(address string) (string, bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	e, ok := c.cache[address]
	if !ok || !c.now().Before(e.expires) {
		delete(c.cache, address)
		return "", false
	}
	return e.dn, true
}

func (c *Client) storeCachedDN(address, dn string) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	c.cache[address] = cacheEntry{dn: dn, expires: c.now().Add(c.cacheTTL)}
}
