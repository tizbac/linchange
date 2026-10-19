// Package tui renders the interactive setup wizard on a terminal.
//
// It is deliberately small: a handful of widgets (a line prompt, a yes/no
// question, a single- and multi-choice menu, a status line) and a palette.
// Everything degrades politely. When stdout is a pipe, or NO_COLOR is set, or
// TERM says dumb, the escapes are simply not emitted and a menu falls back to
// a numbered list read from a line. That matters because the wizard is invoked
// from a Debian postinst, where "the terminal" is sometimes tmux, sometimes
// `ssh -t`, and sometimes an automated install with no terminal at all.
package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// ErrInterrupt is returned by every widget when the operator presses Ctrl-C.
// The wizard treats it as "stop now", leaving whatever has been written so far
// untouched.
var ErrInterrupt = errors.New("interrupted")

// Choice is one entry of a menu.
type Choice struct {
	// Label is the short name shown in the list.
	Label string
	// Hint is an explanation shown under the list, pointing at the highlighted
	// entry in raw mode and printed per entry otherwise.
	Hint string
}

// InputOpts configures a line prompt.
type InputOpts struct {
	// Default is offered in brackets and used when the answer is empty.
	Default string
	// Mask hides the typed characters, for passwords.
	Mask bool
	// Optional allows an empty answer to stand instead of being re-asked.
	Optional bool
	// Validate is called with the answer; an error is printed and the question
	// asked again, so a typo costs a keystroke rather than a second run.
	Validate func(string) error
}

// ANSI styling. The palette is a 256-colour one rather than truecolor because
// it survives terminals that advertise only 8 or 16 colours.
const (
	seqReset = "\x1b[0m"
	seqBold  = "\x1b[1m"
	seqDim   = "\x1b[2m"

	fgAccent = "\x1b[38;5;39m"  // headings, rules, the highlighted entry
	fgGood   = "\x1b[38;5;114m" // confirmed values, success
	fgValue  = "\x1b[38;5;229m" // the answer echoed back
	fgWarn   = "\x1b[38;5;214m" // warnings
	fgBad    = "\x1b[38;5;203m" // errors
	fgHint   = "\x1b[38;5;245m" // hints and explanations
)

// Term is a wizard's view of one terminal.
type Term struct {
	in  *bufio.Reader
	fd  uintptr // of the input file, when it is one; 0 otherwise
	out io.Writer
	// color is decided once, from the environment and the output's type.
	color bool
	// interactive reports that stdin is a terminal, which is what decides
	// whether the arrow-key menus are possible at all.
	interactive bool
	// wide is the usable line width, used to wrap text and draw the rules.
	wide int
	// last holds the raw buffer of the most recent prompt, before the default
	// was substituted.
	last string
}

// New builds a Term writing to out and reading from in.
func New(in *os.File, out io.Writer) *Term {
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	interactive := term.IsTerminal(int(in.Fd()))
	outIsTTY := false
	if f, ok := out.(*os.File); ok {
		outIsTTY = term.IsTerminal(int(f.Fd()))
	}

	width := 80
	if w, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && w > 0 {
		width = w
	}
	if f, ok := out.(*os.File); ok {
		if w, h, err := term.GetSize(int(f.Fd())); err == nil && w > 0 && h > 0 {
			width = w
		}
	}
	switch {
	case width > 100:
		width = 100
	case width < 44:
		width = 44
	}

	return &Term{
		in:          bufio.NewReader(in),
		fd:          in.Fd(),
		out:         out,
		color:       os.Getenv("NO_COLOR") == "" && outIsTTY && !strings.EqualFold(os.Getenv("TERM"), "dumb"),
		interactive: interactive,
		wide:        width,
	}
}

// paint wraps s in a style, or returns it unchanged when colour is off.
func (t *Term) paint(style, s string) string {
	if !t.color || s == "" {
		return s
	}
	return style + s + seqReset
}

func (t *Term) bold(s string) string  { return t.paint(seqBold, s) }
func (t *Term) dim(s string) string   { return t.paint(fgHint+seqDim, s) }
func (t *Term) good(s string) string  { return t.paint(fgGood, s) }
func (t *Term) warn(s string) string  { return t.paint(fgWarn, s) }
func (t *Term) bad(s string) string   { return t.paint(fgBad, s) }
func (t *Term) value(s string) string { return t.paint(fgValue, s) }

// emphasis renders the wizard's **bold** markup. Hints are written as ordinary
// prose with the few words worth noticing wrapped in **stars**, so a hint reads
// as a sentence and still points at what matters.
func (t *Term) emphasis(s string) string {
	parts := strings.Split(s, "**")
	if !t.color {
		return strings.Join(parts, "")
	}
	for i, p := range parts {
		if i%2 == 1 {
			parts[i] = t.bold(p)
		}
	}
	return strings.Join(parts, "")
}

// Section opens a wizard step. The blurb says, in one sentence, what the step
// is for; the hint under it says what a good answer looks like.
func (t *Term) Section(step, total int, title, blurb string) {
	heading := title
	if step > 0 {
		heading = fmt.Sprintf("Step %d of %d · %s", step, total, title)
	}
	fmt.Fprintf(t.out, "\n%s\n", t.paint(fgAccent+seqBold, heading))
	rule := strings.Repeat("─", t.wide-2)
	if !t.color {
		rule = strings.Repeat("-", t.wide-2)
	}
	fmt.Fprintln(t.out, t.paint(fgAccent, rule))
	if blurb != "" {
		fmt.Fprintln(t.out)
		// Emphasis applies here too: a blurb is prose like any other, and
		// leaving the stars visible in it would look like a bug.
		t.wrap(t.emphasis(blurb), "")
	}
	fmt.Fprintln(t.out)
}

// Hint prints an explanatory paragraph in grey, honouring **bold** markup.
func (t *Term) Hint(text string) {
	t.wrap(t.emphasis(text), "  ")
	fmt.Fprintln(t.out)
}

// wrap prints text at the terminal width with an indent, breaking on spaces.
func (t *Term) wrap(text, indent string) {
	width := t.wide - len(indent)
	if width < 20 {
		width = 20
	}
	for _, para := range strings.Split(text, "\n") {
		line := indent
		for _, word := range strings.Fields(para) {
			if len(line)+len(word) > width && line != indent {
				fmt.Fprintln(t.out, line)
				line = indent
			}
			if line == indent {
				line += word
			} else {
				line += " " + word
			}
		}
		if line != indent {
			fmt.Fprintln(t.out, line)
		}
	}
}

// Info, Detail, Success, Warn and Error are the wizard's status lines. Each is
// prefixed with a symbol, so the meaning survives without colour.
func (t *Term) Info(format string, args ...any) {
	fmt.Fprintf(t.out, "  %s %s\n", t.paint(fgAccent, "i"), fmt.Sprintf(format, args...))
}

func (t *Term) Detail(format string, args ...any) {
	fmt.Fprintf(t.out, "    %s\n", t.dim(fmt.Sprintf(format, args...)))
}

func (t *Term) Success(format string, args ...any) {
	fmt.Fprintf(t.out, "  %s %s\n", t.good("+"), t.good(fmt.Sprintf(format, args...)))
}

func (t *Term) Warn(format string, args ...any) {
	fmt.Fprintf(t.out, "  %s %s\n", t.warn("!"), t.warn(fmt.Sprintf(format, args...)))
}

func (t *Term) Error(format string, args ...any) {
	fmt.Fprintf(t.out, "  %s %s\n", t.bad("x"), t.bad(fmt.Sprintf(format, args...)))
}

// KeyValue prints one line of a summary table, aligning the values.
func (t *Term) KeyValue(key, value string) {
	fmt.Fprintf(t.out, "    %s %s\n", t.dim(fmt.Sprintf("%-24s", key)), t.value(value))
}

// Progress starts a labelled operation and returns the function that records
// its outcome. The label is printed immediately, because a database that takes
// ten seconds to answer must not look like a hang.
func (t *Term) Progress(label string) func(error) {
	fmt.Fprintf(t.out, "    %s %s ...\n", t.dim("*"), t.dim(label))
	return func(err error) {
		if err != nil {
			fmt.Fprintf(t.out, "    %s %s: %s\n", t.bad("x"), t.bad(label), err.Error())
			return
		}
		fmt.Fprintf(t.out, "    %s %s\n", t.good("+"), t.good(label))
	}
}

// Input asks for one line of text and loops until the answer validates.
func (t *Term) Input(q string, o InputOpts) (string, error) {
	for {
		if err := t.ask(q, o); err != nil {
			return "", err
		}
		answer := t.last
		if answer == "" && o.Default != "" {
			answer = o.Default
		}
		if answer == "" && !o.Optional {
			t.Warn("an answer is needed here")
			continue
		}
		if o.Validate != nil {
			if err := o.Validate(answer); err != nil {
				t.Warn("%s", err)
				continue
			}
		}
		// Echo the accepted answer so the transcript of the run reads back as
		// the configuration that will be written. A masked prompt is the one
		// exception: repeating the real password in the scrollback would put
		// it one `less` search away on a shared terminal, so the mask is what
		// gets echoed there, exactly as it was drawn while typing.
		if o.Mask {
			fmt.Fprintf(t.out, "    %s\n", t.value(strings.Repeat("*", len(answer))))
		} else {
			fmt.Fprintf(t.out, "    %s\n", t.value(answer))
		}
		return answer, nil
	}
}

// ask renders one prompt and collects the raw line.
func (t *Term) ask(q string, o InputOpts) error {
	prompt := "  " + q
	if o.Default != "" {
		prompt += " " + t.dim("["+o.Default+"]")
	}
	prompt += ": "

	if !t.interactive {
		fmt.Fprint(t.out, prompt)
		line, err := readLine(t.in)
		if err != nil {
			return err
		}
		t.last = line
		return nil
	}

	restore, err := t.makeRaw()
	if err != nil {
		return err
	}
	defer restore()

	fmt.Fprint(t.out, prompt)
	buf := []rune{}
	redraw := func() {
		show := string(buf)
		if o.Mask {
			show = strings.Repeat("*", len(buf))
		}
		fmt.Fprintf(t.out, "\r\x1b[2K%s%s", prompt, show)
	}
	for {
		r, err := t.readRune()
		if err != nil {
			fmt.Fprintln(t.out)
			return err
		}
		switch r {
		case '\r', '\n':
			fmt.Fprintln(t.out)
			t.last = string(buf)
			return nil
		case 3: // Ctrl-C
			fmt.Fprintln(t.out)
			return ErrInterrupt
		case 4: // Ctrl-D
			if len(buf) == 0 {
				fmt.Fprintln(t.out)
				return io.EOF
			}
		case 8, 127: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				redraw()
			}
		default:
			if r >= 32 {
				buf = append(buf, r)
				redraw()
			}
		}
	}
}

// Confirm asks a yes/no question. The default is what pressing enter gives,
// which is how the wizard recommends a choice without forcing it.
//
// The answer is only committed on Enter, even though y and n select it. A
// widget that returned the moment y was pressed would leave the Enter the
// person typed right after it sitting in the terminal, and the next menu would
// read that stray Enter and accept its own default - skipping a question the
// operator never saw. Committing on Enter is also what every other prompt here
// does, so the behaviour is the same everywhere.
func (t *Term) Confirm(q string, def bool, hint string) (bool, error) {
	if hint != "" {
		t.Hint(hint)
	}
	prompt := fmt.Sprintf("  %s %s ", q, t.dim(t.yesNoHint(def)))

	if !t.interactive {
		fmt.Fprint(t.out, prompt)
		line, err := readLine(t.in)
		if err != nil {
			return false, err
		}
		answer, err := interpretYesNo(line, def)
		if err != nil {
			t.Warn("%s", err)
			return t.Confirm(q, def, "")
		}
		fmt.Fprintf(t.out, "    %s\n", t.value(yesNo(answer)))
		return answer, nil
	}

	restore, err := t.makeRaw()
	if err != nil {
		return false, err
	}
	defer restore()

	chosen := def
	redraw := func() {
		fmt.Fprintf(t.out, "\r\x1b[2K%s%s", prompt, t.dim(yesNo(chosen)))
	}
	fmt.Fprint(t.out, prompt)
	redraw()
	for {
		r, err := t.readRune()
		if err != nil {
			fmt.Fprintln(t.out)
			return false, err
		}
		switch r {
		case '\r', '\n':
			fmt.Fprintf(t.out, "\r\x1b[2K%s%s\n", prompt, t.value(yesNo(chosen)))
			return chosen, nil
		case 'y', 'Y':
			chosen = true
			redraw()
		case 'n', 'N':
			chosen = false
			redraw()
		}
	}
}

// yesNoHint renders the [Y/n] marker, with the capital letter on the side
// that Enter will pick.
func (t *Term) yesNoHint(def bool) string {
	if def {
		return "[Y/n]"
	}
	return "[y/N]"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// interpretYesNo maps a typed answer onto a bool. An unrecognised answer is an
// error so that a stray keystroke is not silently taken for the default.
func interpretYesNo(line string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def, nil
	case "y", "yes", "s", "si", "true", "1":
		return true, nil
	case "n", "no", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%q is not yes or no; type y or n", strings.TrimSpace(line))
	}
}

// Select shows a menu and returns the index of the chosen entry.
//
// On a terminal it is an arrow-key list; otherwise the entries are numbered
// and the answer read from a line, which is what keeps the wizard usable when
// stdin is a pipe.
func (t *Term) Select(q string, choices []Choice, def int, hint string) (int, error) {
	if len(choices) == 0 {
		return 0, errors.New("tui: no choices offered")
	}
	if def < 0 || def >= len(choices) {
		def = 0
	}
	if q != "" {
		t.wrap(q, "  ")
		fmt.Fprintln(t.out)
	}
	if hint != "" {
		t.Hint(hint)
	}
	if !t.interactive {
		return t.selectNumbered(choices, def)
	}
	return t.selectArrows(choices, def)
}

func (t *Term) selectNumbered(choices []Choice, def int) (int, error) {
	for i, c := range choices {
		marker := " "
		if i == def {
			marker = "*"
		}
		fmt.Fprintf(t.out, "  %s %2d) %s\n", t.dim(marker), i+1, c.Label)
		if c.Hint != "" {
			fmt.Fprintf(t.out, "           %s\n", t.dim(c.Hint))
		}
	}
	for {
		fmt.Fprintf(t.out, "  %s ", t.dim(fmt.Sprintf("choice [1-%d, default %d]:", len(choices), def+1)))
		line, err := readLine(t.in)
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			fmt.Fprintf(t.out, "    %s\n", t.value(choices[def].Label))
			return def, nil
		}
		n, err := strconv.Atoi(line)
		if err == nil && n >= 1 && n <= len(choices) {
			fmt.Fprintf(t.out, "    %s\n", t.value(choices[n-1].Label))
			return n - 1, nil
		}
		t.Warn("type a number between 1 and %d", len(choices))
	}
}

// selectArrows is the interactive menu. The list plus one hint line are drawn
// as a block, and redrawn in place: the cursor is moved back to the top of the
// block after each draw, so a selection change costs no scrolling.
func (t *Term) selectArrows(choices []Choice, def int) (int, error) {
	restore, err := t.makeRaw()
	if err != nil {
		return 0, err
	}
	defer restore()

	height := len(choices) + 1 // list + hint line
	cursor := def

	draw := func() {
		for i, c := range choices {
			line := "   " + c.Label
			if i == cursor {
				arrow := ">"
				if t.color {
					arrow = "❯"
				}
				line = " " + t.paint(fgAccent+seqBold, arrow+" "+c.Label)
			}
			fmt.Fprintf(t.out, "\r\x1b[2K%s\n", line)
		}
		fmt.Fprintf(t.out, "\r\x1b[2K%s", t.dim(choices[cursor].Hint))
		fmt.Fprintf(t.out, "\x1b[%dA", height)
	}
	draw()

	for {
		k, err := t.readKey()
		if err != nil {
			t.leave(height)
			return 0, err
		}
		switch k {
		case keyUp:
			if cursor > 0 {
				cursor--
				draw()
			}
		case keyDown:
			if cursor < len(choices)-1 {
				cursor++
				draw()
			}
		case keyEnter:
			t.leave(height)
			fmt.Fprintf(t.out, "    %s\n", t.value(choices[cursor].Label))
			return cursor, nil
		default:
			// A digit jumps straight to an entry, faster than holding the
			// arrow down on a long list.
			if n := int(k - '0'); k >= '1' && n >= 1 && n <= len(choices) {
				cursor = n - 1
				draw()
			}
		}
	}
}

// Multi shows a check list and returns the checked state. Space toggles the
// highlighted entry, enter accepts.
func (t *Term) Multi(q string, choices []Choice, def []bool, hint string) ([]bool, error) {
	state := make([]bool, len(choices))
	copy(state, def)
	if q != "" {
		t.wrap(q, "  ")
		fmt.Fprintln(t.out)
	}
	if hint != "" {
		t.Hint(hint)
	}
	if !t.interactive {
		return t.multiNumbered(choices, state)
	}
	return t.multiArrows(choices, state)
}

func (t *Term) multiNumbered(choices []Choice, state []bool) ([]bool, error) {
	for {
		for i, c := range choices {
			fmt.Fprintf(t.out, "  %s %2d) %s %s\n", t.dim(" "), i+1, t.box(state[i]), c.Label)
			if c.Hint != "" {
				fmt.Fprintf(t.out, "           %s\n", t.dim(c.Hint))
			}
		}
		fmt.Fprintf(t.out, "  %s ", t.dim("toggle numbers (comma separated), or press enter to accept:"))
		line, err := readLine(t.in)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(line) == "" {
			fmt.Fprintf(t.out, "    %s\n", t.value(t.checkedList(choices, state)))
			return state, nil
		}
		for _, field := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(field))
			if err != nil || n < 1 || n > len(choices) {
				t.Warn("%q is not a number between 1 and %d", field, len(choices))
				break
			}
			state[n-1] = !state[n-1]
		}
		// The list is redrawn from the top of this function. Scrolling is
		// acceptable here: this path has no terminal to redraw in place.
	}
}

func (t *Term) multiArrows(choices []Choice, state []bool) ([]bool, error) {
	restore, err := t.makeRaw()
	if err != nil {
		return nil, err
	}
	defer restore()

	height := len(choices) + 1 // list + footer line
	cursor := 0

	draw := func() {
		for i, c := range choices {
			marker := "  "
			line := "   " + marker + t.box(state[i]) + " " + c.Label
			if i == cursor {
				arrow := ">"
				if t.color {
					arrow = "❯"
				}
				line = " " + t.paint(fgAccent+seqBold, arrow) + " " + t.box(state[i]) + " " + c.Label
			}
			fmt.Fprintf(t.out, "\r\x1b[2K%s\n", line)
		}
		fmt.Fprintf(t.out, "\r\x1b[2K%s", t.dim("    space toggles · enter accepts · up down moves"))
		fmt.Fprintf(t.out, "\x1b[%dA", height)
	}
	draw()

	for {
		k, err := t.readKey()
		if err != nil {
			t.leave(height)
			return nil, err
		}
		switch k {
		case keyUp:
			if cursor > 0 {
				cursor--
				draw()
			}
		case keyDown:
			if cursor < len(choices)-1 {
				cursor++
				draw()
			}
		case ' ', keyTab:
			state[cursor] = !state[cursor]
			draw()
		case keyEnter:
			t.leave(height)
			fmt.Fprintf(t.out, "    %s\n", t.value(t.checkedList(choices, state)))
			return state, nil
		}
	}
}

// leave moves the cursor below a drawn block and erases the transient footer.
func (t *Term) leave(height int) {
	fmt.Fprintf(t.out, "\r\x1b[2K")
	fmt.Fprintf(t.out, "\x1b[%dB", height)
}

// box renders a checkbox, coloured when it is on so a glance shows the set.
func (t *Term) box(on bool) string {
	if on {
		return t.good("[x]")
	}
	return t.dim("[ ]")
}

// checkedList names the selected labels, for the transcript line.
func (t *Term) checkedList(choices []Choice, state []bool) string {
	var on []string
	for i, c := range choices {
		if i < len(state) && state[i] {
			on = append(on, c.Label)
		}
	}
	if len(on) == 0 {
		return "none"
	}
	return strings.Join(on, ", ")
}

// --- key reading ----------------------------------------------------------

// key is one decoded keystroke. Multi-byte runes are represented by their rune
// value; the control keys and the arrows occupy the space below and above it.
type key rune

const (
	keyEnter key = 13
	keyTab   key = 9
	keyCtrlC key = 3
	keyCtrlD key = 4

	keyUp   key = 1000
	keyDown key = 1001
)

// readKey decodes one keystroke. The terminal is already in raw mode when this
// is called, so every key arrives on its own.
func (t *Term) readKey() (key, error) {
	r, _, err := t.in.ReadRune()
	if err != nil {
		return 0, err
	}
	switch r {
	case '\r', '\n':
		return keyEnter, nil
	case 3, 4:
		return 0, ErrInterrupt
	case 8, 127:
		return keyTab, nil
	case 27:
		return t.readEscape()
	}
	return key(r), nil
}

// readEscape turns an ESC-introduced sequence into a key. A lone escape, or
// one this wizard does not act on, is reported as itself and ignored.
//
// The bytes are read one at a time and each read blocks, because a terminal
// delivers an escape sequence in whatever chunks the kernel feels like: ESC on
// its own, or the whole sequence in one read. Reading "two bytes" in one call
// returns one when only one has arrived, which would silently drop every arrow
// key press on an ssh connection.
func (t *Term) readEscape() (key, error) {
	next, err := t.in.ReadByte()
	if err != nil {
		return 0, nil // the terminal went away; the widget reports it as EOF
	}
	if next != '[' && next != 'O' {
		return 0, nil
	}
	final, err := t.in.ReadByte()
	if err != nil {
		return 0, nil
	}
	switch final {
	case 'A':
		return keyUp, nil
	case 'B':
		return keyDown, nil
	case 'C', 'D':
		return 0, nil
	}
	// Sequences with parameters, such as Home (ESC [ 1 ~) or a modified
	// arrow (ESC [ 1 ; 5 A), end in a byte this wizard does not act on.
	// Draining the rest keeps the buffer clean for the next read.
	if final >= '0' && final <= '9' {
		for {
			b, err := t.in.ReadByte()
			if err != nil || b == '~' || (b >= 'A' && b <= 'Z') {
				break
			}
		}
	}
	return 0, nil
}

// readRune reads one character, decoding UTF-8 and reporting Ctrl-C.
func (t *Term) readRune() (rune, error) {
	r, _, err := t.in.ReadRune()
	if err != nil {
		return 0, err
	}
	if r == rune(keyCtrlC) {
		return 0, ErrInterrupt
	}
	return r, nil
}

// makeRaw switches the terminal to raw mode and returns the restore function.
func (t *Term) makeRaw() (func(), error) {
	state, err := term.MakeRaw(int(t.fd))
	if err != nil {
		return nil, fmt.Errorf("tui: cannot read the terminal: %w", err)
	}
	return func() { _ = term.Restore(int(t.fd), state) }, nil
}

// readLine reads one line from an interactive-less input. Nothing is echoed:
// when stdin is a pipe there is no one watching, and a password piped in must
// not be repeated on stderr.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if line == "" && err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
