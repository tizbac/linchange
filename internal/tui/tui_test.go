package tui

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"
)

// pipeTerminal builds a Term whose input is a pipe and whose output is a
// buffer: exactly the shape of an automated install, where there is nobody at
// a keyboard and nothing to colour.
func pipeTerminal(t *testing.T, input string) (*Term, *bytes.Buffer) {
	t.Helper()
	in, out, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { in.Close(); out.Close() })

	if _, err := out.WriteString(input); err != nil {
		t.Fatalf("writing the scripted input: %v", err)
	}
	out.Close() // EOF after the scripted answers, as a pipe does

	var buf bytes.Buffer
	return New(in, &buf), &buf
}

func TestOutputIsPlainWhenThereIsNoTerminal(t *testing.T) {
	term, buf := pipeTerminal(t, "")
	term.Section(1, 7, "Server identity", "A **blurb** with markup.")
	term.Hint("A hint with **bold** words in it.")
	term.Info("some information")
	term.Detail("some detail")
	term.Success("all good")
	term.Warn("a warning")
	term.Error("an error")
	term.KeyValue("Hostname", "mail.example.com")

	got := buf.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("output contains an escape sequence although stdout is a buffer:\n%q", got)
	}
	// The markup is for a colour terminal; without one it must not leak the
	// stars into the text a person reads.
	if strings.Contains(got, "**") {
		t.Errorf("output contains markup stars:\n%s", got)
	}
	for _, want := range []string{"Server identity", "blurb", "bold words", "Hostname", "mail.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
}

func TestEmphasisRendersBoldOnlyWithColour(t *testing.T) {
	plain, _ := pipeTerminal(t, "")
	if got := plain.emphasis("a **b** c"); got != "a b c" {
		t.Errorf("emphasis without colour = %q, want the stars removed", got)
	}

	coloured := &Term{color: true}
	got := coloured.emphasis("a **b** c")
	if !strings.Contains(got, seqBold) || !strings.Contains(got, seqReset) {
		t.Errorf("emphasis with colour = %q, want the bold escape around b", got)
	}
	if strings.ReplaceAll(strings.ReplaceAll(got, seqBold, ""), seqReset, "") != "a b c" {
		t.Errorf("emphasis changed the text: %q", got)
	}
}

func TestInputReadsALine(t *testing.T) {
	term, buf := pipeTerminal(t, "mail.example.com\n")
	got, err := term.Input("Mail server hostname", InputOpts{})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if got != "mail.example.com" {
		t.Errorf("Input = %q", got)
	}
	if !strings.Contains(buf.String(), "mail.example.com") {
		t.Errorf("the answer is not echoed back, so the transcript does not show what was chosen:\n%s", buf.String())
	}
}

func TestInputAcceptsAnEmptyLineAsTheDefault(t *testing.T) {
	term, _ := pipeTerminal(t, "\n")
	got, err := term.Input("Data directory", InputOpts{Default: "/var/lib/linchange"})
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if got != "/var/lib/linchange" {
		t.Errorf("Input = %q, want the default", got)
	}
}

func TestInputReportsEndOfInput(t *testing.T) {
	// A pipe that closes mid-wizard is what an automated install looks like
	// when it forgot a flag; it has to be an error, not an empty answer that
	// would then be written into the configuration.
	term, _ := pipeTerminal(t, "")
	if _, err := term.Input("Domain name", InputOpts{}); err == nil {
		t.Fatal("Input = nil error, want an error at end of input")
	}
}

func TestConfirmReadsTheUsualSpellings(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"\n", true},  // enter accepts the default
		{"y\n", true}, //
		{"yes\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"no\n", false},
		{"0\n", false},
		{"1\n", true},
	}
	for _, tc := range tests {
		term, _ := pipeTerminal(t, tc.in)
		got, err := term.Confirm("Deliver mail here?", true, "")
		if err != nil {
			t.Fatalf("Confirm(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Confirm(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestConfirmRejectsAGibberishAnswer(t *testing.T) {
	// Two bad lines then a yes: anything else would silently take the
	// default, and "did you mean yes?" is exactly the question to avoid.
	term, _ := pipeTerminal(t, "maybe\nbanana\nyes\n")
	got, err := term.Confirm("Enable SMTP?", true, "")
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !got {
		t.Error("Confirm = false, want the final yes to win")
	}
}

func TestSelectFallsBackToNumbers(t *testing.T) {
	choices := []Choice{
		{Label: "Automatic, from Let's Encrypt (ACME)"},
		{Label: "I already have certificate files"},
		{Label: "No TLS for now"},
	}
	term, buf := pipeTerminal(t, "2\n")
	got, err := term.Select("How should certificates be obtained?", choices, 0, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != 1 {
		t.Errorf("Select = %d, want index 1", got)
	}
	// Without a terminal every entry has to be visible, or the numbers are
	// meaningless.
	for _, c := range choices {
		if !strings.Contains(buf.String(), c.Label) {
			t.Errorf("the list is missing %q:\n%s", c.Label, buf.String())
		}
	}
}

func TestSelectAcceptsAnEmptyLineAsTheDefault(t *testing.T) {
	choices := []Choice{{Label: "first"}, {Label: "second"}}
	term, _ := pipeTerminal(t, "\n")
	got, err := term.Select("Pick one", choices, 1, "")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != 1 {
		t.Errorf("Select = %d, want the default 1", got)
	}
}

func TestMultiTogglesAndAccepts(t *testing.T) {
	choices := []Choice{{Label: "IMAP"}, {Label: "POP3"}}
	term, buf := pipeTerminal(t, "1\n\n") // toggle IMAP off, then accept
	got, err := term.Multi("Which protocols?", choices, []bool{true, true}, "")
	if err != nil {
		t.Fatalf("Multi: %v", err)
	}
	if got[0] || !got[1] {
		t.Errorf("Multi = %v, want IMAP toggled off and POP3 untouched", got)
	}
	if !strings.Contains(buf.String(), "POP3") {
		t.Errorf("the list is missing POP3:\n%s", buf.String())
	}
}

// byteAtATime hands over one byte per Read, which is how a terminal delivers
// an escape sequence over ssh: never all at once, and not predictably split
// either.
type byteAtATime struct{ data []byte }

func (r *byteAtATime) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, os.ErrClosed
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestArrowsSurviveASplitEscapeSequence(t *testing.T) {
	// Regression: reading the sequence in one call returned one byte when
	// only one had arrived, so the arrow keys were dropped and the menu took
	// the default. The symptom for a person was "the down key does nothing,
	// but the next question is full of [B".
	for _, tc := range []struct {
		in   string
		want key
	}{
		{"\x1b[A", keyUp},
		{"\x1b[B", keyDown},
	} {
		term := &Term{in: bufio.NewReader(&byteAtATime{data: []byte(tc.in)})}
		got, err := term.readKey()
		if err != nil {
			t.Fatalf("readKey(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("readKey(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSequencesThatAreNotArrowsAreSwallowed(t *testing.T) {
	// Home is ESC [ 1 ~: the parameter must be drained, or the next widget
	// reads it as typing.
	term := &Term{in: bufio.NewReader(&byteAtATime{data: []byte("\x1b[1~")})}
	if _, err := term.readKey(); err != nil {
		t.Fatalf("readKey: %v", err)
	}
	// The buffer must be empty now, so this blocks and reports EOF rather
	// than handing back the leftover bytes as keys.
	if _, err := term.readKey(); err == nil {
		t.Error("readKey = nil error, want EOF after the sequence was drained")
	}
}

func TestProgressReportsBothOutcomes(t *testing.T) {
	term, buf := pipeTerminal(t, "")
	finish := term.Progress("Testing the database connection")
	finish(nil)
	if !strings.Contains(buf.String(), "Testing the database connection") {
		t.Errorf("the label is not printed up front, so a slow operation looks like a hang:\n%s", buf.String())
	}

	term, buf = pipeTerminal(t, "")
	finish = term.Progress("Applying the database schema")
	finish(os.ErrPermission)
	if !strings.Contains(buf.String(), "permission denied") {
		t.Errorf("the failure is not reported:\n%s", buf.String())
	}
}

func TestSectionNumbering(t *testing.T) {
	term, buf := pipeTerminal(t, "")
	term.Section(2, 7, "Database", "Where the data lives.")
	if !strings.Contains(buf.String(), "Step 2 of 7") {
		t.Errorf("the step counter is missing:\n%s", buf.String())
	}
	term, buf = pipeTerminal(t, "")
	term.Section(0, 0, "Done", "")
	if strings.Contains(buf.String(), "Step") {
		t.Errorf("an unnumbered section is numbered:\n%s", buf.String())
	}
}
