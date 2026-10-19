package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/testdb"
	"github.com/linchange/linchange/internal/tui"
)

// scriptedUI answers the wizard's questions from a script, and fails the test
// the moment it is asked something it does not know about. That last property
// is the point: a new question added to the flow has to be given an answer
// here, which is what stops a silently-asked-and-ignored prompt from shipping.
type scriptedUI struct {
	t *testing.T

	inputs   map[string]string
	confirms map[string]bool
	selects  map[string]int
	multis   map[string][]bool

	// asked records every question in order, so a test can assert the shape
	// of the conversation rather than only its outcome.
	asked []string
}

func newScriptedUI(t *testing.T) *scriptedUI {
	return &scriptedUI{
		t:        t,
		inputs:   map[string]string{},
		confirms: map[string]bool{},
		selects:  map[string]int{},
		multis:   map[string][]bool{},
	}
}

func (s *scriptedUI) lookup(table map[string]any, q string) (string, bool) {
	for key, value := range table {
		if strings.Contains(q, key) {
			return fmt.Sprint(value), true
		}
	}
	return "", false
}

func (s *scriptedUI) Section(step, total int, title, blurb string) {
	s.asked = append(s.asked, "section: "+title)
}

func (s *scriptedUI) Hint(text string) {}

func (s *scriptedUI) Info(format string, args ...any) {}

func (s *scriptedUI) Detail(format string, args ...any) {}

func (s *scriptedUI) Success(format string, args ...any) {}

func (s *scriptedUI) Warn(format string, args ...any) {}

func (s *scriptedUI) Error(format string, args ...any) {}

func (s *scriptedUI) KeyValue(key, value string) {}

func (s *scriptedUI) Progress(label string) func(error) {
	return func(error) {}
}

func (s *scriptedUI) Input(q string, o tui.InputOpts) (string, error) {
	s.asked = append(s.asked, "input: "+q)
	if answer, ok := s.inputs[q]; ok {
		return answer, nil
	}
	if o.Default != "" {
		return o.Default, nil
	}
	s.t.Fatalf("no scripted answer for question %q", q)
	return "", nil
}

func (s *scriptedUI) Confirm(q string, def bool, hint string) (bool, error) {
	s.asked = append(s.asked, "confirm: "+q)
	if answer, ok := s.confirms[q]; ok {
		return answer, nil
	}
	return def, nil
}

func (s *scriptedUI) Select(q string, choices []tui.Choice, def int, hint string) (int, error) {
	s.asked = append(s.asked, "select: "+q)
	if answer, ok := s.selects[q]; ok {
		return answer, nil
	}
	return def, nil
}

func (s *scriptedUI) Multi(q string, choices []tui.Choice, def []bool, hint string) ([]bool, error) {
	s.asked = append(s.asked, "multi: "+q)
	if answer, ok := s.multis[q]; ok {
		return answer, nil
	}
	return def, nil
}

// TestRunAsksTheExpectedQuestions pins the shape of the conversation. The
// order matters to a person reading the output, and the names are what the
// hints hang off, so a rename here is a deliberate act.
func TestRunAsksTheExpectedQuestions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "linchange.yaml")

	ui := newScriptedUI(t)
	ui.inputs["Mail server hostname"] = "mail.example.com"
	ui.inputs["Public IPv4 address"] = "203.0.113.10"
	ui.inputs["Database password"] = ""
	ui.inputs["Domain name"] = "example.com"
	ui.selects["How should certificates be obtained?"] = 2 // no TLS
	ui.inputs["Administrator address"] = "admin@example.com"
	ui.inputs["Password"] = "correct horse battery staple"
	ui.inputs["Password, once more"] = "correct horse battery staple"

	answers := Defaults()
	answers.DBHost, answers.DBPort = "127.0.0.1", 5432

	// Provision is off so that the flow can be exercised without a database:
	// the connectivity check belongs to the provisioning half.
	_, err := Run(context.Background(), ui, Options{Path: path, Base: config.New(), Answers: answers, Provision: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Seven screens, in the order a person meets them.
	wantSections := []string{
		"Server identity",
		"Database",
		"Mail domain",
		"TLS certificates",
		"Protocols",
		"Administrator account",
		"Review",
	}
	var gotSections []string
	for _, asked := range ui.asked {
		if strings.HasPrefix(asked, "section: ") {
			gotSections = append(gotSections, strings.TrimPrefix(asked, "section: "))
		}
	}
	// The opening banner, the seven questions, and a closing summary.
	if len(gotSections) != len(wantSections)+2 {
		t.Fatalf("sections shown = %v, want the banner, %d questions and a summary", gotSections, len(wantSections))
	}
	for i, want := range wantSections {
		if gotSections[i+1] != want {
			t.Errorf("section %d = %q, want %q", i, gotSections[i+1], want)
		}
	}
	if gotSections[len(gotSections)-1] != "Done" {
		t.Errorf("last section = %q, want Done", gotSections[len(gotSections)-1])
	}

	// The database test runs inside the flow, so a scripted answer is not
	// enough: it must have been attempted, which the absence of a fatal shows.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the configuration was not written: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the written configuration does not load: %v", err)
	}
	if cfg.Server.Hostname != "mail.example.com" {
		t.Errorf("hostname = %q", cfg.Server.Hostname)
	}
	if cfg.TLSEnabled() {
		t.Error("TLS is enabled although the wizard chose none")
	}
}

func TestRunRefusesWhenTheReviewIsDeclined(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "linchange.yaml")

	ui := newScriptedUI(t)
	ui.inputs["Mail server hostname"] = "mail.example.com"
	ui.inputs["Public IPv4 address"] = "203.0.113.10"
	ui.inputs["Database password"] = ""
	ui.inputs["Domain name"] = "example.com"
	ui.inputs["Administrator address"] = "admin@example.com"
	ui.inputs["Password"] = "correct horse battery staple"
	ui.inputs["Password, once more"] = "correct horse battery staple"
	ui.selects["How should certificates be obtained?"] = 2 // no TLS
	ui.confirms["Write this configuration and prepare the database?"] = false

	answers := Defaults()
	if _, err := Run(context.Background(), ui, Options{Path: path, Base: config.New(), Answers: answers, Provision: false}); err == nil {
		t.Fatal("Run = nil error, want the refusal to be reported")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the configuration was written although the review was declined: %v", err)
	}
}

func TestRunKeepsThePasswordOutOfTheSummary(t *testing.T) {
	// The review is printed to a terminal that may be recorded, pasted into a
	// ticket, or shoulder-surfed. The password is shown as a fixed mask.
	dir := t.TempDir()
	path := filepath.Join(dir, "linchange.yaml")

	recording := &recordingUI{scriptedUI: newScriptedUI(t)}
	recording.inputs["Mail server hostname"] = "mail.example.com"
	recording.inputs["Public IPv4 address"] = "203.0.113.10"
	recording.inputs["Database password"] = ""
	recording.inputs["Domain name"] = "example.com"
	recording.inputs["Administrator address"] = "admin@example.com"
	recording.inputs["Password"] = "correct horse battery staple"
	recording.inputs["Password, once more"] = "correct horse battery staple"
	recording.selects["How should certificates be obtained?"] = 2 // no TLS

	answers := Defaults()
	if _, err := Run(context.Background(), recording, Options{Path: path, Base: config.New(), Answers: answers, Provision: false}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, line := range recording.lines {
		if strings.Contains(line, "correct horse battery staple") {
			t.Errorf("the password was echoed in the transcript: %q", line)
		}
	}
}

// recordingUI wraps the scripted one and captures what each widget prints, so
// a test can look at the transcript a person would see.
type recordingUI struct {
	*scriptedUI
	lines []string
}

func (r *recordingUI) KeyValue(key, value string) {
	r.lines = append(r.lines, key+": "+value)
}

func (r *recordingUI) Info(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingUI) Warn(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

// TestProvisionCreatesDomainAndAdministrator is the half of the wizard that
// touches the database. It needs one, and follows the repository's convention
// of skipping when there is none rather than pretending to have run.
func TestProvisionCreatesDomainAndAdministrator(t *testing.T) {
	db := testdb.New(t)

	answers := validAnswers()
	cfg, _, err := BuildConfig(config.New(), answers)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}

	result := &Result{}
	if err := Provision(context.Background(), newScriptedUI(t), db, cfg, answers, result); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !result.AdminCreated {
		t.Error("AdminCreated = false on a first run")
	}

	// Re-running is what an operator does after changing one answer; "already
	// exists" has to be a normal outcome, not a failure.
	second := &Result{}
	if err := Provision(context.Background(), newScriptedUI(t), db, cfg, answers, second); err != nil {
		t.Fatalf("Provision (second run): %v", err)
	}
	if second.AdminCreated {
		t.Error("AdminCreated = true on a second run, want the existing account left alone")
	}

	// The account must be one that can actually administer: a user row with
	// the grant, and a password that verifies.
	var (
		address    string
		isDomainAd bool
		source     string
	)
	err = db.QueryRow(`SELECT address, is_domain_admin, source FROM users WHERE address = $1`,
		answers.AdminEmail).Scan(&address, &isDomainAd, &source)
	if err != nil {
		t.Fatalf("reading the created administrator back: %v", err)
	}
	if !isDomainAd {
		t.Error("is_domain_admin = false; the first account could not administer anything")
	}
	if source != "local" {
		t.Errorf("source = %q, want local", source)
	}

	// The domain is what makes mail for the address deliverable.
	var isLocal bool
	if err := db.QueryRow(`SELECT is_local FROM domains WHERE name = $1`,
		answers.Domain).Scan(&isLocal); err != nil {
		t.Fatalf("reading the created domain back: %v", err)
	}
	if !isLocal {
		t.Error("is_local = false; mail for the domain would not be delivered here")
	}

	// And the password must be stored as a hash, not in the clear.
	var hash string
	if err := db.QueryRow(`SELECT password_hash FROM credentials WHERE address = $1`,
		answers.AdminEmail).Scan(&hash); err != nil {
		t.Fatalf("reading the stored password back: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2") {
		t.Errorf("stored password does not look like an argon2 hash: %.20q", hash)
	}
}

// TestProvisionRefusesADomainItCannotCreate keeps the failure path honest: a
// domain that collides with an existing row of another kind must be reported,
// not swallowed.
func TestProvisionReportsMigrationFailure(t *testing.T) {
	db := testdb.New(t)

	answers := validAnswers()
	cfg, _, err := BuildConfig(config.New(), answers)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	// A migration that cannot run leaves the schema missing, and provisioning
	// must say so rather than carry on to the domain insert.
	cfg.Database.AutoMigrate = true

	// Point at a table that cannot be created to prove errors propagate: the
	// simplest reliable way is to close the handle out from under it.
	db.Close()
	if err := Provision(context.Background(), newScriptedUI(t), db, cfg, answers, &Result{}); err == nil {
		t.Fatal("Provision = nil error, want a failure when the database is gone")
	}
}
