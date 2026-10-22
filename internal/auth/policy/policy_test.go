package policy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/linchange/linchange/internal/auth/policy"
)

func person() policy.Principal {
	return policy.Principal{
		Address:        "alice@example.com",
		Source:         policy.SourceLocal,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

func admin() policy.Principal {
	return policy.Principal{
		Address:        "admin@example.com",
		Source:         policy.SourceDomainAdmin,
		Enabled:        true,
		MailboxEnabled: false,
		Authenticated:  true,
	}
}

func shared() policy.Principal {
	return policy.Principal{
		Address:        "support@example.com",
		Source:         policy.SourceShared,
		Enabled:        true,
		MailboxEnabled: true,
		Authenticated:  true,
	}
}

// The user's central requirement: a domain admin administers mail but never
// receives it. This has to hold on every path, not just in the web UI.
func TestDomainAdminMayNotReceiveMail(t *testing.T) {
	a := admin()

	for _, action := range []policy.Action{
		policy.ActionIMAP, policy.ActionPOP3, policy.ActionSMTP,
		policy.ActionEWS, policy.ActionEAS, policy.ActionCalDAV, policy.ActionCardDAV,
		policy.ActionSendMail, policy.ActionReadMailbox, policy.ActionManageMailbox,
	} {
		if err := a.Can(action); err == nil {
			t.Errorf("a domain admin was allowed to %s", action)
		} else if !policy.IsDenied(err, policy.CodeNoMailbox) {
			t.Errorf("%s was refused for %q rather than %q: %v",
				action, errCode(err), policy.CodeNoMailbox, err)
		}
	}

	// But it does administer, and it does sign in to the web UI.
	for _, action := range []policy.Action{policy.ActionWebLogin, policy.ActionAdminister, policy.ActionAuthenticate} {
		if err := a.Can(action); err != nil {
			t.Errorf("a domain admin was refused %s: %v", action, err)
		}
	}
}

// The source is checked as well as the flag, so a row whose mailbox_enabled was
// mis-set still cannot receive mail.
func TestDomainAdminIsRefusedEvenIfMailboxEnabledIsSet(t *testing.T) {
	a := admin()
	a.MailboxEnabled = true // a bad row, e.g. written by hand

	if err := a.Can(policy.ActionIMAP); !policy.IsDenied(err, policy.CodeNoMailbox) {
		t.Errorf("a domain admin with mailbox_enabled=true was allowed to use IMAP: %v", err)
	}
}

func TestOrdinaryUserCanUseEveryMailSurface(t *testing.T) {
	p := person()
	for _, action := range []policy.Action{
		policy.ActionIMAP, policy.ActionPOP3, policy.ActionSMTP,
		policy.ActionEWS, policy.ActionEAS, policy.ActionCalDAV, policy.ActionCardDAV, policy.ActionSendMail,
		policy.ActionReadMailbox, policy.ActionManageMailbox, policy.ActionWebLogin,
	} {
		if err := p.Can(action); err != nil {
			t.Errorf("an ordinary user was refused %s: %v", action, err)
		}
	}
}

func TestOnlyADomainAdminMayAdminister(t *testing.T) {
	if err := person().Can(policy.ActionAdminister); !policy.IsDenied(err, policy.CodeNotDomainAdmin) {
		t.Errorf("an ordinary user was allowed to administer: %v", err)
	}
	if err := shared().Can(policy.ActionAdminister); !policy.IsDenied(err, policy.CodeNotDomainAdmin) {
		t.Errorf("a shared mailbox was allowed to administer: %v", err)
	}
}

// A mailbox holder who also administers is the ordinary administrator of a
// small deployment, and the demo user. It has to be able to administer, and it
// has to keep receiving its own mail: the grant says nothing about the mailbox.
func TestAMailboxHolderMayBeGrantedAdministration(t *testing.T) {
	p := person()
	p.IsDomainAdmin = true

	if err := p.Can(policy.ActionAdminister); err != nil {
		t.Errorf("a granted mailbox holder was refused administration: %v", err)
	}
	for _, action := range []policy.Action{
		policy.ActionIMAP, policy.ActionEWS, policy.ActionEAS, policy.ActionCalDAV, policy.ActionCardDAV,
		policy.ActionReadMailbox, policy.ActionSendMail,
	} {
		if err := p.Can(action); err != nil {
			t.Errorf("being an administrator cost %s the ability to %s: %v", p.Address, action, err)
		}
	}
	if !p.DomainAdmin() {
		t.Error("DomainAdmin() said no for a principal holding the grant")
	}
}

// A shared mailbox is not a login identity, so a grant on one buys nothing and
// the policy refuses it outright rather than leaving the caller to wonder which
// of the two answers is right. The schema refuses to store the combination.
func TestAGrantedSharedMailboxStillCannotAdminister(t *testing.T) {
	s := shared()
	s.IsDomainAdmin = true

	if err := s.Can(policy.ActionAdminister); !policy.IsDenied(err, policy.CodeNotMember) {
		t.Errorf("a granted shared mailbox was allowed to administer: %v", err)
	}
	if err := s.Can(policy.ActionIMAP); !policy.IsDenied(err, policy.CodeNotMember) {
		t.Errorf("a granted shared mailbox was allowed to use IMAP: %v", err)
	}
}

// The domain_admin source is an administration grant on its own, so a principal
// rebuilt from a claim that predates the column is still an administrator.
func TestTheDomainAdminSourceIsEnough(t *testing.T) {
	a := admin()
	a.IsDomainAdmin = false // a value read before the grant existed

	if !a.DomainAdmin() {
		t.Error("DomainAdmin() said no for a domain_admin principal with no grant set")
	}
	if err := a.Can(policy.ActionAdminister); err != nil {
		t.Errorf("a domain_admin principal was refused administration: %v", err)
	}
}

func TestDisabledPrincipalIsRefusedEverywhere(t *testing.T) {
	p := person()
	p.Enabled = false

	for _, action := range []policy.Action{
		policy.ActionAuthenticate, policy.ActionWebLogin, policy.ActionIMAP,
		policy.ActionSMTP, policy.ActionAdminister,
	} {
		if err := p.Can(action); !policy.IsDenied(err, policy.CodeDisabled) {
			t.Errorf("a disabled principal was allowed to %s: %v", action, err)
		}
	}
}

func TestUnauthenticatedPrincipalCannotAct(t *testing.T) {
	p := person()
	p.Authenticated = false

	// Credential checking is the one thing an unauthenticated caller may ask.
	if err := p.Can(policy.ActionAuthenticate); err != nil {
		t.Errorf("an unauthenticated principal could not attempt authentication: %v", err)
	}
	for _, action := range []policy.Action{
		policy.ActionWebLogin, policy.ActionIMAP, policy.ActionSMTP, policy.ActionReadMailbox,
	} {
		if err := p.Can(action); !policy.IsDenied(err, policy.CodeNotAuthenticated) {
			t.Errorf("an unauthenticated principal was allowed to %s: %v", action, err)
		}
	}
}

func TestUnknownPrincipalIsRefused(t *testing.T) {
	var empty policy.Principal
	if err := empty.Can(policy.ActionWebLogin); !policy.IsDenied(err, policy.CodeUnknownPrincipal) {
		t.Errorf("an empty principal was allowed in: %v", err)
	}

	bad := person()
	bad.Source = "wizard"
	if err := bad.Can(policy.ActionWebLogin); !policy.IsDenied(err, policy.CodeUnknownPrincipal) {
		t.Errorf("an unknown source was allowed in: %v", err)
	}
}

// A forced password change must block the mail protocols but leave the web UI
// reachable, since that is where the change happens.
func TestForcedPasswordChangeBlocksMailButNotTheWebUI(t *testing.T) {
	p := person()
	p.MustChangePassword = true

	if err := p.Can(policy.ActionWebLogin); err != nil {
		t.Errorf("a user who must change their password could not reach the web UI: %v", err)
	}
	for _, action := range []policy.Action{
		policy.ActionIMAP, policy.ActionPOP3, policy.ActionSMTP,
		policy.ActionEWS, policy.ActionEAS, policy.ActionCalDAV, policy.ActionCardDAV,
	} {
		if err := p.Can(action); !policy.IsDenied(err, policy.CodeMustChangePass) {
			t.Errorf("a user who must change their password was allowed to %s: %v", action, err)
		}
	}
}

// A shared mailbox is reached through grants, never with a password, so it must
// not be usable as a login identity.
func TestSharedMailboxIsNotALoginIdentity(t *testing.T) {
	s := shared()

	if err := s.Can(policy.ActionIMAP); !policy.IsDenied(err, policy.CodeNotMember) {
		t.Errorf("a shared mailbox was allowed to open an IMAP session: %v", err)
	}
	if err := s.Can(policy.ActionWebLogin); !policy.IsDenied(err, policy.CodeNotMember) {
		t.Errorf("a shared mailbox was allowed to sign in to the web UI: %v", err)
	}
	// Acting on a mailbox it owns is the point of it, so those stay allowed.
	for _, action := range []policy.Action{policy.ActionReadMailbox, policy.ActionManageMailbox} {
		if err := s.Can(action); err != nil {
			t.Errorf("a shared mailbox was refused %s on its own mail: %v", action, err)
		}
	}
}

func TestSecondFactorBlocksMailProtocols(t *testing.T) {
	p := person()
	p.TOTPRequired = true
	p.TOTPRegistered = true

	for _, action := range []policy.Action{policy.ActionIMAP, policy.ActionSMTP, policy.ActionReadMailbox} {
		if err := p.Can(action); !policy.IsDenied(err, policy.CodeTOTPRequired) {
			t.Errorf("a user who has not presented a second factor was allowed to %s: %v", action, err)
		}
	}
	// Signing in is how the second factor gets presented.
	if err := p.Can(policy.ActionWebLogin); err != nil {
		t.Errorf("a user could not reach the web UI to present a second factor: %v", err)
	}
}

// A user who must enrol in a second factor has not got one yet, and must not
// be locked out of the enrolment screen by the very rule requiring it.
func TestSecondFactorEnrolmentIsNotBlockedByTheSecondFactorRule(t *testing.T) {
	p := person()
	p.TOTPRequired = true
	p.TOTPRegistered = false

	if err := p.Can(policy.ActionWebLogin); err != nil {
		t.Errorf("a user who must enrol in a second factor could not reach the web UI: %v", err)
	}
}

func TestSourceValid(t *testing.T) {
	for _, s := range []policy.Source{
		policy.SourceLocal, policy.SourceLDAP, policy.SourceDomainAdmin, policy.SourceShared,
	} {
		if !s.Valid() {
			t.Errorf("%q reported as invalid", s)
		}
	}
	for _, s := range []policy.Source{"", "LOCAL", "guest", "wizard"} {
		if s.Valid() {
			t.Errorf("%q reported as valid", s)
		}
	}
}

func TestSharedAccessLevels(t *testing.T) {
	tests := []struct {
		level  policy.GrantLevel
		want   policy.GrantLevel
		allows bool
	}{
		{policy.GrantRead, policy.GrantRead, true},
		{policy.GrantRead, policy.GrantContribute, false},
		{policy.GrantContribute, policy.GrantRead, true},
		{policy.GrantContribute, policy.GrantContribute, true},
		{policy.GrantContribute, policy.GrantEditor, false},
		{policy.GrantEditor, policy.GrantContribute, true},
		{policy.GrantEditor, policy.GrantEditor, true},
		{policy.GrantEditor, policy.GrantOwner, false},
		{policy.GrantOwner, policy.GrantOwner, true},
		{policy.GrantOwner, policy.GrantEditor, true},
		{"", policy.GrantRead, false},
	}

	for _, tc := range tests {
		access := policy.SharedAccess{Level: tc.level}
		if got := access.AtLeast(tc.want); got != tc.allows {
			t.Errorf("%q at least %q = %v, want %v", tc.level, tc.want, got, tc.allows)
		}
	}
}

// An open shared mailbox is readable by the domain but never writable, or it
// would be a way to sidestep the grant list.
func TestOpenSharedMailboxIsReadOnly(t *testing.T) {
	open := policy.SharedAccess{Open: true}

	if !open.AtLeast(policy.GrantRead) {
		t.Error("an open shared mailbox was not readable")
	}
	for _, want := range []policy.GrantLevel{policy.GrantContribute, policy.GrantEditor, policy.GrantOwner} {
		if open.AtLeast(want) {
			t.Errorf("an open shared mailbox was granted %q", want)
		}
	}
}

func TestCanSharedMailbox(t *testing.T) {
	read := policy.SharedAccess{Level: policy.GrantRead}
	editor := policy.SharedAccess{Level: policy.GrantEditor}
	owner := policy.SharedAccess{Level: policy.GrantOwner}
	none := policy.SharedAccess{}

	// Reading needs read.
	if err := policy.CanSharedMailbox(policy.ActionReadMailbox, read); err != nil {
		t.Errorf("a read member could not read: %v", err)
	}
	if err := policy.CanSharedMailbox(policy.ActionReadMailbox, none); !policy.IsDenied(err, policy.CodeNoGrant) {
		t.Errorf("a non-member could read: %v", err)
	}

	// Managing folders needs editor, or a read member could escalate through
	// folder visibility.
	if err := policy.CanSharedMailbox(policy.ActionManageMailbox, read); !policy.IsDenied(err, policy.CodeNoGrant) {
		t.Errorf("a read member could manage folders: %v", err)
	}
	if err := policy.CanSharedMailbox(policy.ActionManageMailbox, editor); err != nil {
		t.Errorf("an editor could not manage folders: %v", err)
	}

	// Sending as the mailbox changes the From a third party sees, so it is
	// owner-only.
	if err := policy.CanSharedMailbox(policy.ActionSendMail, editor); !policy.IsDenied(err, policy.CodeNoGrant) {
		t.Errorf("an editor could send as a shared mailbox: %v", err)
	}
	if err := policy.CanSharedMailbox(policy.ActionSendMail, owner); err != nil {
		t.Errorf("an owner could not send as a shared mailbox: %v", err)
	}
}

func TestCanImpersonate(t *testing.T) {
	on := policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireReason: true}}
	off := policy.Policy{Impersonation: policy.Impersonation{Allowed: false, RequireReason: true}}
	noReason := policy.Policy{Impersonation: policy.Impersonation{Allowed: true, RequireReason: false}}
	a := admin()

	// A reason is required and recorded.
	if err := on.CanImpersonate(a, "alice@example.com", ""); !policy.IsDenied(err, policy.CodeReasonRequired) {
		t.Errorf("impersonation without a reason was allowed: %v", err)
	}
	if err := on.CanImpersonate(a, "alice@example.com", "   "); !policy.IsDenied(err, policy.CodeReasonRequired) {
		t.Errorf("a whitespace-only reason was accepted: %v", err)
	}
	if err := on.CanImpersonate(a, "alice@example.com", "support request 4711"); err != nil {
		t.Errorf("impersonation with a reason was refused: %v", err)
	}

	// The master switch wins.
	if err := off.CanImpersonate(a, "alice@example.com", "a reason"); !policy.IsDenied(err, policy.CodeImpersonationOff) {
		t.Errorf("impersonation succeeded while disabled: %v", err)
	}

	// Optional reason.
	if err := noReason.CanImpersonate(a, "alice@example.com", ""); err != nil {
		t.Errorf("impersonation was refused although no reason is required: %v", err)
	}

	// Acting as oneself is not impersonation.
	if err := on.CanImpersonate(a, "admin@example.com", ""); err != nil {
		t.Errorf("a domain admin was blocked from their own account: %v", err)
	}
	if err := on.CanImpersonate(a, "ADMIN@example.com", ""); err != nil {
		t.Errorf("case-differing self impersonation was refused: %v", err)
	}

	// Only a domain admin may impersonate.
	if err := on.CanImpersonate(person(), "bob@example.com", "a reason"); err == nil {
		t.Error("an ordinary user was allowed to impersonate")
	}
}

// MailProtocol guards against a caller passing an action that is not a logon.
func TestCanMailProtocolRejectsNonLogonActions(t *testing.T) {
	p := person()
	if err := p.CanMailProtocol(policy.ActionWebLogin); err == nil {
		t.Error("CanMailProtocol accepted an action that is not a mail logon")
	}
	if err := p.CanMailProtocol(policy.ActionIMAP); err != nil {
		t.Errorf("CanMailProtocol refused a real logon action: %v", err)
	}
}

func TestDeniedErrorCarriesCode(t *testing.T) {
	err := policy.DeniedError(policy.CodeDisabled)
	if !policy.IsDenied(err, policy.CodeDisabled) {
		t.Error("IsDenied did not match the code it was given")
	}
	if policy.IsDenied(err, policy.CodeNoMailbox) {
		t.Error("IsDenied matched the wrong code")
	}
	if !strings.Contains(err.Error(), policy.CodeDisabled) {
		t.Errorf("the error message does not include the code: %v", err)
	}

	// A wrapped denial is still recognisable, which matters because these
	// travel up through handler layers.
	wrapped := errors.Join(errors.New("checking permissions"), err)
	if !policy.IsDenied(wrapped, policy.CodeDisabled) {
		t.Error("a wrapped denial was not recognised")
	}
}

func TestSourceStringsMatchTheDatabaseEnum(t *testing.T) {
	// The policy package deliberately does not import the database layer, so
	// the two enumerations can drift. The migration's own test checks the SQL
	// side; this checks the Go side against the same list, and the shared
	// literal is the contract.
	want := []string{"local", "ldap", "domain_admin", "shared"}
	got := []string{
		string(policy.SourceLocal),
		string(policy.SourceLDAP),
		string(policy.SourceDomainAdmin),
		string(policy.SourceShared),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("source %d is %q in policy but %q in the database enum", i, got[i], want[i])
		}
	}
}

func errCode(err error) string {
	var d *policy.Denied
	if errors.As(err, &d) {
		return d.Code
	}
	return "<not a policy denial>"
}
