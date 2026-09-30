// Package policy answers "may this principal do this" in one place.
//
// The plan calls for a single decision point shared by all five protocol
// servers and the HTTP API. That is not tidiness for its own sake: a rule
// implemented once in the web UI and once in the IMAP server is two rules, and
// they drift. The requirement that a domain admin administers mail but never
// receives it is exactly the kind of rule that has to hold on every path,
// including the ones nobody remembers to audit.
package policy

import (
	"errors"
	"fmt"
	"strings"
)

// Source mirrors the user_source enum. It is duplicated here rather than
// imported so the policy package does not depend on the database layer, and
// with it the migration set.
type Source string

const (
	// SourceLocal is a user whose password is stored and verified here.
	SourceLocal Source = "local"
	// SourceLDAP is a directory user. The password is checked by binding to
	// the directory, never stored here.
	SourceLDAP Source = "ldap"
	// SourceDomainAdmin administers mail but must not receive it. This is the
	// user's requirement, not an Exchange convention, and it is why Source
	// exists at all.
	SourceDomainAdmin Source = "domain_admin"
	// SourceShared is a shared mailbox: it holds mail of its own and is
	// reached through grants rather than a password.
	SourceShared Source = "shared"
)

// Valid reports whether s is one of the known sources.
func (s Source) Valid() bool {
	switch s {
	case SourceLocal, SourceLDAP, SourceDomainAdmin, SourceShared:
		return true
	}
	return false
}

// Principal is the identity a decision is made about.
//
// It is a value rather than a database row so that the same struct can be
// built from a session cookie, an IMAP session, an SMTP AUTH exchange or an
// ActiveSync device without any of them consulting the database first.
type Principal struct {
	// Address is the canonical, lowercased primary address.
	Address string
	Source  Source
	// Enabled is false for a disabled or deprovisioned account. A disabled
	// principal authenticates nowhere and receives nothing.
	Enabled bool
	// MailboxEnabled is false for a domain admin. The database enforces this
	// pairing for the domain_admin source; the field is carried so that
	// decisions do not have to re-derive it.
	MailboxEnabled bool
	// MustChangePassword forces a password change before anything else. It
	// does not block the web UI, which is where the change happens, but it
	// does block every mail protocol.
	MustChangePassword bool
	// TOTPRequired is true when a second factor must be presented before the
	// login completes. It is separate from MustChangePassword because a user
	// can be required to enrol without being required to change a password.
	TOTPRequired bool
	// TOTPRegistered reports whether a second factor already exists. A user
	// who must enrol has TOTPRequired and no registered factor yet.
	TOTPRegistered bool
	// Authenticated is false for a principal reconstructed from an unverified
	// input, such as a pre-authentication login name. Such a principal may be
	// checked for what it would be allowed to do, but never granted access.
	Authenticated bool
}

// Denied is a policy refusal. It carries a stable Code so callers can map it to
// an HTTP status or an IMAP response without matching on the message, and it
// deliberately never says which rule was the problem for an unauthenticated
// caller.
type Denied struct {
	// Code is a stable machine-readable reason.
	Code string
	// Detail is for the operator's logs, not for the client.
	Detail string
}

func (d *Denied) Error() string {
	if d.Detail == "" {
		return "policy: denied: " + d.Code
	}
	return "policy: denied: " + d.Code + ": " + d.Detail
}

// Denial codes. These are part of the API contract, so they are constants
// rather than inline strings.
const (
	CodeUnknownPrincipal = "unknown_principal"
	CodeNotAuthenticated = "not_authenticated"
	CodeDisabled         = "principal_disabled"
	CodeNoMailbox        = "principal_cannot_hold_mail"
	CodeMustChangePass   = "password_change_required"
	CodeTOTPRequired     = "second_factor_required"
	CodeNotDomainAdmin   = "not_a_domain_admin"
	CodeNotMember        = "not_a_shared_mailbox_member"
	CodeNoGrant          = "no_shared_mailbox_grant"
	CodeImpersonationOff = "impersonation_disabled"
	CodeReasonRequired   = "impersonation_reason_required"
)

// DeniedError is a convenience for tests and for callers that want to assert on
// the code without importing the type.
func DeniedError(code string) error { return &Denied{Code: code} }

// IsDenied reports whether err is a policy refusal with the given code.
func IsDenied(err error, code string) bool {
	var d *Denied
	if errors.As(err, &d) {
		return d.Code == code
	}
	return false
}

// Action is something a principal may attempt. It is a string enum so that a
// value crossing a package boundary is still readable in a log.
type Action string

const (
	// ActionAuthenticate is a credential check. It is the only action a
	// principal may be refused for the sake of an unauthenticated caller.
	ActionAuthenticate Action = "authenticate"

	// ActionWebLogin is a browser session.
	ActionWebLogin Action = "web_login"

	// ActionIMAP and friends are the mail protocol logons.
	ActionIMAP Action = "imap"
	ActionPOP3 Action = "pop3"
	ActionSMTP Action = "smtp_submit"
	ActionEWS  Action = "ews"
	ActionEAS  Action = "eas"

	// ActionSendMail may only ever be taken by a principal holding mail.
	ActionSendMail Action = "send_mail"

	// ActionReadMailbox and ActionManageMailbox are separated so that a
	// shared mailbox member with read access cannot create folders.
	ActionReadMailbox   Action = "read_mailbox"
	ActionManageMailbox Action = "manage_mailbox"

	// ActionAdminister is the domain administration surface.
	ActionAdminister Action = "administer"
)

// mailLogons are the protocols that exist to move mail to or from this
// principal, so all of them share one "may hold mail" rule.
var mailLogons = []Action{
	ActionIMAP, ActionPOP3, ActionSMTP, ActionEWS, ActionEAS,
	ActionSendMail, ActionReadMailbox, ActionManageMailbox,
}

// Can reports whether p may perform a.
//
// The order of the checks is deliberate. Authenticated and Enabled come first
// because they apply everywhere; then anything that requires holding mail.
// Ordering matters for information disclosure: a disabled principal is told it
// is disabled whether or not it could hold mail, so the reason does not leak
// the source.
func (p Principal) Can(a Action) error {
	if p.Address == "" {
		return &Denied{Code: CodeUnknownPrincipal, Detail: "no address"}
	}
	if !p.Source.Valid() {
		return &Denied{
			Code:   CodeUnknownPrincipal,
			Detail: fmt.Sprintf("unknown source %q", p.Source),
		}
	}
	if !p.Enabled {
		return &Denied{Code: CodeDisabled, Detail: p.Address}
	}

	// A domain admin administers mail and never receives it. This is checked
	// against the source rather than only mailbox_enabled, so that a row with
	// a mis-set flag still cannot receive mail.
	if p.Source == SourceDomainAdmin && holdsMailAction(a) {
		return &Denied{
			Code:   CodeNoMailbox,
			Detail: fmt.Sprintf("%s is a domain admin and may not %s", p.Address, a),
		}
	}
	if !p.MailboxEnabled && holdsMailAction(a) {
		return &Denied{
			Code:   CodeNoMailbox,
			Detail: fmt.Sprintf("%s cannot hold mail and may not %s", p.Address, a),
		}
	}

	// A forced password change blocks the mail protocols. The web UI stays
	// available, because that is where the change is made.
	if p.MustChangePassword && a != ActionWebLogin && a != ActionAuthenticate && a != ActionAdminister {
		return &Denied{
			Code:   CodeMustChangePass,
			Detail: fmt.Sprintf("%s must change their password before %s", p.Address, a),
		}
	}

	// A second factor is required before any session is granted. It is not
	// checked for ActionAuthenticate itself, because that is the step that
	// decides whether a second factor is needed.
	if p.TOTPRequired && p.TOTPRegistered && a != ActionAuthenticate && a != ActionWebLogin {
		return &Denied{
			Code:   CodeTOTPRequired,
			Detail: fmt.Sprintf("%s must present a second factor before %s", p.Address, a),
		}
	}

	// Only a domain admin may administer.
	if a == ActionAdminister && p.Source != SourceDomainAdmin {
		return &Denied{
			Code:   CodeNotDomainAdmin,
			Detail: fmt.Sprintf("%s is not a domain admin", p.Address),
		}
	}

	// A shared mailbox is reached through grants, never with its own
	// password, so it cannot log in anywhere by itself.
	if p.Source == SourceShared && a != ActionReadMailbox && a != ActionManageMailbox && a != ActionAuthenticate {
		return &Denied{
			Code:   CodeNotMember,
			Detail: fmt.Sprintf("%s is a shared mailbox, not a login identity", p.Address),
		}
	}

	if !p.Authenticated && a != ActionAuthenticate {
		return &Denied{
			Code:   CodeNotAuthenticated,
			Detail: fmt.Sprintf("%s is not authenticated", p.Address),
		}
	}

	return nil
}

// CanMailProtocol is the check every mail listener makes before it will talk
// to a client at all.
func (p Principal) CanMailProtocol(a Action) error {
	if !isMailLogon(a) {
		return &Denied{
			Code:   CodeUnknownPrincipal,
			Detail: fmt.Sprintf("%q is not a mail logon action", a),
		}
	}
	return p.Can(a)
}

// holdsMailAction reports whether a is meaningless for a principal that cannot
// receive mail.
func holdsMailAction(a Action) bool {
	if a == ActionSendMail || a == ActionReadMailbox || a == ActionManageMailbox {
		return true
	}
	return isMailLogon(a)
}

func isMailLogon(a Action) bool {
	for _, candidate := range mailLogons {
		if a == candidate {
			return true
		}
	}
	return false
}

// SharedAccess is what a member may do with a shared mailbox.
type SharedAccess struct {
	// Level is the grant level, empty when there is no grant.
	Level GrantLevel
	// Open means the shared mailbox is open to the domain, so a member is
	// allowed even without an explicit grant row.
	Open bool
}

// GrantLevel is the permission a member holds on a shared mailbox.
type GrantLevel string

const (
	GrantRead       GrantLevel = "read"
	GrantContribute GrantLevel = "contribute"
	GrantEditor     GrantLevel = "editor"
	GrantOwner      GrantLevel = "owner"
)

// rank orders the levels for comparison. read < contribute < editor < owner.
var rank = map[GrantLevel]int{
	GrantRead:       1,
	GrantContribute: 2,
	GrantEditor:     3,
	GrantOwner:      4,
}

// AtLeast reports whether the access satisfies want.
func (s SharedAccess) AtLeast(want GrantLevel) bool {
	if s.Open {
		// An open shared mailbox is readable by anyone in the domain, but
		// never writable, so it cannot be used to sidestep the grant list.
		return want == GrantRead
	}
	return rank[s.Level] >= rank[want]
}

// CanSharedMailbox reports whether the member may perform a on the shared
// mailbox.
//
// A grant of read cannot create folders; only editor and above can. A shared
// mailbox member who could manage folders could effectively escalate, since
// folder visibility is how a shared mailbox is often administered.
func CanSharedMailbox(a Action, access SharedAccess) error {
	switch a {
	case ActionReadMailbox:
		if !access.AtLeast(GrantRead) {
			return &Denied{Code: CodeNoGrant, Detail: "read access required"}
		}
	case ActionManageMailbox:
		if !access.AtLeast(GrantEditor) {
			return &Denied{Code: CodeNoGrant, Detail: "editor access required to manage a shared mailbox"}
		}
	case ActionSendMail:
		// Sending as a shared mailbox is an owner action: it changes the
		// From header a third party sees.
		if !access.AtLeast(GrantOwner) {
			return &Denied{Code: CodeNoGrant, Detail: "owner access required to send as a shared mailbox"}
		}
	default:
		return &Denied{
			Code:   CodeUnknownPrincipal,
			Detail: fmt.Sprintf("%q is not a shared mailbox action", a),
		}
	}
	return nil
}

// Impersonation decides whether a domain admin may act as another user.
type Impersonation struct {
	// Allowed is the master switch.
	Allowed bool
	// RequireReason forces a written justification, which is audited.
	RequireReason bool
}

// CanImpersonate reports whether admin may impersonate target.
//
// A domain admin impersonating themselves is not impersonation and is always
// allowed; treating it as a special case here keeps the audit log free of noise
// and stops an admin from being blocked from their own account by a policy
// meant to constrain them.
func (p Policy) CanImpersonate(admin Principal, target string, reason string) error {
	if !p.Impersonation.Allowed {
		return &Denied{Code: CodeImpersonationOff, Detail: "impersonation is disabled"}
	}
	if err := admin.Can(ActionAdminister); err != nil {
		return err
	}
	if strings.EqualFold(admin.Address, target) {
		return nil
	}
	if p.Impersonation.RequireReason && strings.TrimSpace(reason) == "" {
		return &Denied{
			Code:   CodeReasonRequired,
			Detail: fmt.Sprintf("%s must give a reason to impersonate %s", admin.Address, target),
		}
	}
	return nil
}

// Policy bundles the configuration-driven switches alongside the decisions.
// It is a struct rather than a package-level variable so that two policies
// with different configuration can coexist in one process, which the tests
// rely on and which makes the settings explicit at every call site.
type Policy struct {
	Impersonation Impersonation
}
