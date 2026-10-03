// Package ldapdial opens connections to the configured directories.
//
// The login path and the reconcile both need the same thing: a connected,
// optionally TLS-upgraded handle to one provider, bounded by its timeout.
// Keeping that in one place keeps the two from drifting apart on the details
// that matter, like which URLs StartTLS applies to.
package ldapdial

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	gldap "github.com/go-ldap/ldap/v3"

	"github.com/linchange/linchange/config"
)

// defaultTimeout bounds a directory operation the configuration does not give
// a timeout for. Long enough to clear a loaded link, short enough to notice a
// dead one.
const defaultTimeout = 10 * time.Second

// Dial opens a connection to the provider and, when configured, upgrades it to
// TLS. The returned connection has the provider's timeout applied to every
// operation; the caller closes it.
func Dial(p *config.LDAPProvider) (*gldap.Conn, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	conn, err := gldap.DialURL(p.URL, gldap.DialWithDialer(&net.Dialer{Timeout: timeout}))
	if err != nil {
		return nil, fmt.Errorf("ldap: reaching %s: %w", p.Name, err)
	}
	conn.SetTimeout(timeout)

	// An ldaps:// address is already encrypted; StartTLS upgrades the
	// plaintext form only.
	if p.StartTLS && !strings.HasPrefix(p.URL, "ldaps://") {
		host, err := serverName(p.URL)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("ldap: %s: %w", p.Name, err)
		}
		cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
		if err := conn.StartTLS(cfg); err != nil {
			conn.Close()
			return nil, fmt.Errorf("ldap: upgrading %s to TLS: %w", p.Name, err)
		}
	}
	return conn, nil
}

// serverName extracts the hostname a TLS connection to the URL will verify.
func serverName(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if h := u.Hostname(); h != "" {
		return h, nil
	}
	return "", errors.New("no host in " + raw)
}
