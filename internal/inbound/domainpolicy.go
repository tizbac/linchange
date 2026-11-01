package inbound

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	smtp "github.com/emersion/go-smtp"

	"github.com/linchange/linchange/internal/store"
	"github.com/linchange/linchange/internal/webmail"
)

// domainPolicy is what the inbound listener needs to know about one hosted
// domain: which source addresses are allowed to deliver to it, and what its
// administrator's header rules say to do with a message that matches one.
//
// A domain with no rows is not an error: mail for a mailbox in a domain nobody
// registered is delivered the ordinary way.
type domainPolicy struct {
	name     string
	networks []netip.Prefix
	rules    []store.DomainHeaderRule
}

// restricted reports whether the domain names sources at all. When it does not,
// anything may deliver, which is the behaviour every domain had before this
// existed.
func (p domainPolicy) restricted() bool { return len(p.networks) > 0 }

// trusts reports whether ip is one of the addresses the domain accepts.
func (p domainPolicy) trusts(ip netip.Addr) bool {
	for _, network := range p.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// domainPolicy reads a domain's settings. A read that fails is returned to the
// caller, because refusing a message and accepting one are both wrong answers
// to a database that is not answering, and the caller decides which to give.
func (b *Backend) domainPolicy(ctx context.Context, domain string) (domainPolicy, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))

	d, err := b.domains.Get(ctx, b.db, domain)
	if err != nil {
		if store.IsNotFound(err) {
			return domainPolicy{name: domain}, nil
		}
		return domainPolicy{}, err
	}

	policy := domainPolicy{name: d.Name}
	for _, raw := range d.AntispamNetworks {
		network, err := store.ParseAntispamNetwork(raw)
		if err != nil {
			// A network nobody can parse is dropped rather than treated as a
			// wildcard: the administrator meant to allow one specific source,
			// and answering "everything is allowed" to a typo would open the
			// domain to the very mail it was meant to block.
			b.log.Warn("ignoring unparseable allowed network",
				"domain", d.Name, "network", raw, "error", err)
			continue
		}
		policy.networks = append(policy.networks, network)
	}

	rules, err := b.headerRules.ListEnabled(ctx, b.db, d.Name)
	if err != nil {
		return domainPolicy{}, err
	}
	policy.rules = rules
	return policy, nil
}

// headerRuleMatches tests one rule against the headers of a message.
//
// Header names are matched case-insensitively because the protocol treats them
// that way; the value is matched case-insensitively unless the rule says
// otherwise, because an anti-spam service writes "Yes", "YES" and "yes"
// interchangeably.
func headerRuleMatches(rule store.DomainHeaderRule, headers map[string][]string) bool {
	values := headers[strings.ToLower(strings.TrimSpace(rule.Header))]
	if len(values) == 0 {
		return false
	}

	needle := rule.Value
	if !rule.CaseSensitive {
		needle = strings.ToLower(needle)
	}

	for _, value := range values {
		haystack := value
		if !rule.CaseSensitive {
			haystack = strings.ToLower(haystack)
		}
		switch rule.Match {
		case store.HeaderMatchContains:
			if strings.Contains(haystack, needle) {
				return true
			}
		case store.HeaderMatchIs:
			if strings.TrimSpace(haystack) == strings.TrimSpace(needle) {
				return true
			}
		case store.HeaderMatchStartsWith:
			if strings.HasPrefix(strings.TrimSpace(haystack), needle) {
				return true
			}
		case store.HeaderMatchEndsWith:
			if strings.HasSuffix(strings.TrimSpace(haystack), needle) {
				return true
			}
		case store.HeaderMatchExists:
			return true
		}
	}
	return false
}

// domainDecision is what a domain's header rules decided for one mailbox.
type domainDecision struct {
	// Junk files the message in Junk rather than INBOX.
	junk bool
	// Discard accepts the message and stores nothing for this owner.
	discard bool
	// Matched names the headers that fired, for the log line.
	matched []string
}

// applyHeaderRules runs every owner's domain rules against a message.
//
// It returns a decision per owner, and an error only for a rule whose action
// is to reject the message outright: that is the one outcome the caller must
// answer with an SMTP failure before anything is stored. A rule that cannot be
// evaluated is not a reason to lose mail, so the caller is never asked to
// refuse a message for a settings mistake.
func (s *session) applyHeaderRules(ctx context.Context, facts webmail.MatchContext) (map[string]domainDecision, error) {
	decisions := make(map[string]domainDecision, len(s.owners))
	for _, owner := range s.owners {
		policy := s.policies[owner]
		for _, rule := range policy.rules {
			if !headerRuleMatches(rule, facts.Headers) {
				continue
			}
			decision := decisions[owner]
			decision.matched = append(decision.matched, rule.Header)
			switch rule.Action {
			case store.HeaderActionJunk:
				decision.junk = true
			case store.HeaderActionDiscard:
				decision.discard = true
			case store.HeaderActionReject:
				s.backend.log.Warn("inbound message rejected by a domain header rule",
					"domain", policy.name, "header", rule.Header, "from", s.envFrom)
				return nil, &smtp.SMTPError{
					Code:         550,
					EnhancedCode: smtp.EnhancedCode{5, 7, 1},
					Message: fmt.Sprintf("message rejected: header %s matches a rule for %s",
						rule.Header, policy.name),
				}
			}
			decisions[owner] = decision
		}
	}
	return decisions, nil
}
