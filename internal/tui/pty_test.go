package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyPair opens a real pseudo-terminal, which is the only way to exercise the
// raw-mode widgets: MakeRaw needs a terminal, and faking one would test
// nothing but the fake.
//
// Linux has no grantpt/unlockpt in the syscall package, and does not need
// them: /dev/ptmx hands out the master, TIOCSPTLCK unlocks it, and devpts
// creates the slave with the right owner already.
//
// Both ends are opened O_NONBLOCK. That is what hands them to Go's poller, so
// SetReadDeadline works; a blocking descriptor ignores the deadline and the
// test would hang instead of failing.
func ptyPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("no pseudo-terminals available: %v", err)
	}
	one := int32(0)
	if err := ioctl(mfd, unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&one))); err != nil {
		unix.Close(mfd)
		t.Fatalf("unlocking the pty: %v", err)
	}
	var n uint32
	if err := ioctl(mfd, unix.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		unix.Close(mfd)
		t.Fatalf("reading the pty number: %v", err)
	}
	sfd, err := unix.Open(fmt.Sprintf("/dev/pts/%d", n), unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		unix.Close(mfd)
		t.Fatalf("opening the slave: %v", err)
	}
	return os.NewFile(uintptr(mfd), "pty-master"), os.NewFile(uintptr(sfd), "pty-slave")
}

func ioctl(fd int, req, arg uintptr) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, arg); errno != 0 {
		return errno
	}
	return nil
}

// send writes to the master without draining, for the moment where the very
// output being drained is what the test is about to look for.
func send(t *testing.T, master *os.File, data string, settle time.Duration) {
	t.Helper()
	if _, err := master.WriteString(data); err != nil {
		t.Fatalf("writing %q: %v", data, err)
	}
	time.Sleep(settle)
}

// keys writes to the master and then drains the master, in the order a person
// produces them: type, then see the redraw.
//
// Draining matters because the pty's buffer is finite: a widget that redraws a
// menu repeatedly will block on write once it fills, and the test would hang
// waiting for a key the wizard never gets to read. That output arrives on the
// master, so the master is what has to be drained - reading the slave instead
// would swallow the keys that were just written to it, which is a bug this
// test once had.
func keys(t *testing.T, master *os.File, data string, settle time.Duration) {
	t.Helper()
	if _, err := master.WriteString(data); err != nil {
		t.Fatalf("writing %q: %v", data, err)
	}
	// Give the wizard a moment to read and redraw, so the next key is not
	// racing the previous one.
	time.Sleep(settle)
	drain(master)
}

func drain(f *os.File) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			// A short deadline keeps this from hanging the test when the
			// widget has nothing more to draw.
			_ = f.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if _, err := f.Read(buf); err != nil {
				return
			}
		}
	}()
	<-done
}

func TestSelectArrowsOverAPty(t *testing.T) {
	master, slave := ptyPair(t)
	defer master.Close()
	defer slave.Close()

	term := New(slave, slave)
	if !term.interactive {
		t.Fatal("the wizard does not see the pty as a terminal; the menu would never be drawn")
	}

	choices := []Choice{
		{Label: "Automatic, from Let's Encrypt (ACME)", Hint: "Best for a public hostname."},
		{Label: "I already have certificate files", Hint: "Your own CA."},
		{Label: "No TLS for now", Hint: "Only for a test machine."},
	}

	type outcome struct {
		index int
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		i, err := term.Select("How should certificates be obtained?", choices, 0, "A hint.")
		done <- outcome{i, err}
	}()

	// Wait for the list to be drawn before pressing anything, so the keys
	// cannot arrive before MakeRaw has run.
	waitForOutput(t, master, "No TLS for now")

	// Two downs and enter: exactly what a person does to pick the last entry.
	keys(t, master, "\x1b[B", 100*time.Millisecond)
	keys(t, master, "\x1b[B", 100*time.Millisecond)
	keys(t, master, "\r", 100*time.Millisecond)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Select: %v", got.err)
		}
		if got.index != 2 {
			t.Errorf("Select = %d, want 2 (the two arrow presses were lost)", got.index)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not return; the menu is waiting for a key it never got")
	}
}

func TestConfirmOverAPty(t *testing.T) {
	master, slave := ptyPair(t)
	defer master.Close()
	defer slave.Close()

	term := New(slave, slave)
	done := make(chan struct {
		value bool
		err   error
	}, 1)
	go func() {
		v, err := term.Confirm("Deliver mail here?", true, "A hint.")
		done <- struct {
			value bool
			err   error
		}{v, err}
	}()

	waitForOutput(t, master, "Deliver mail here?")
	// y or n chooses, Enter commits - exactly the two keys a person presses.
	keys(t, master, "n", 100*time.Millisecond)
	keys(t, master, "\r", 100*time.Millisecond)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Confirm: %v", got.err)
		}
		if got.value {
			t.Error("Confirm = true, want false after n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Confirm did not return")
	}
}

// TestEnterAfterAConfirmDoesNotSkipTheNextMenu is a regression for a bug that
// only a person could have reported: pressing y then Enter on a yes/no
// question left the Enter unread, and the menu that followed read it and
// accepted its own default, silently skipping a question.
func TestEnterAfterAConfirmDoesNotSkipTheNextMenu(t *testing.T) {
	master, slave := ptyPair(t)
	defer master.Close()
	defer slave.Close()

	term := New(slave, slave)
	choices := []Choice{
		{Label: "Automatic, from Let's Encrypt (ACME)", Hint: "Best for a public hostname."},
		{Label: "I already have certificate files", Hint: "Your own CA."},
		{Label: "No TLS for now", Hint: "Only for a test machine."},
	}

	type result struct {
		answer bool
		index  int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		answer, err := term.Confirm("Deliver mail here?", true, "A hint.")
		if err != nil {
			done <- result{err: err}
			return
		}
		// No delay at all: this is the moment the stray Enter would be read.
		index, err := term.Select("How should certificates be obtained?", choices, 0, "A hint.")
		done <- result{answer: answer, index: index, err: err}
	}()

	waitForOutput(t, master, "Deliver mail here?")
	// The two keys, written together as a terminal would deliver them. Not
	// drained: the menu this triggers is the thing the next wait looks for.
	send(t, master, "y\r", 300*time.Millisecond)
	// The hint line under the list carries the highlighted entry's hint, and
	// is drawn last, so it marks "the menu is up and waiting for a key".
	waitForOutput(t, master, "Best for a public hostname")
	// The menu must still be waiting, so this is what picks the last entry.
	keys(t, master, "\x1b[B", 100*time.Millisecond)
	keys(t, master, "\x1b[B", 100*time.Millisecond)
	keys(t, master, "\r", 100*time.Millisecond)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("wizard: %v", got.err)
		}
		if !got.answer {
			t.Error("Confirm = false, want true after y")
		}
		if got.index != 2 {
			t.Errorf("Select = %d, want 2; the Enter after y leaked into the menu and accepted its default", got.index)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wizard is stuck; a stray Enter was read as the menu's answer")
	}
}

func TestInputOverAPtyMasksThePassword(t *testing.T) {
	master, slave := ptyPair(t)
	defer master.Close()
	defer slave.Close()

	term := New(slave, slave)
	type outcome struct {
		value string
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		v, err := term.Input("Password", InputOpts{Mask: true})
		done <- outcome{v, err}
	}()

	waitForOutput(t, master, "Password")
	// Typed one character at a time, as a person does.
	for _, ch := range "hunter2hunter2" {
		keys(t, master, string(ch), 20*time.Millisecond)
	}
	keys(t, master, "\r", 100*time.Millisecond)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Input: %v", got.err)
		}
		if got.value != "hunter2hunter2" {
			t.Errorf("Input = %q, want the typed password", got.value)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Input did not return")
	}
}

// waitForOutput polls the master until the widget has drawn what the test is
// waiting for. Without it the keys would race MakeRaw, and a test that passes
// only because of timing is worse than no test at all.
func waitForOutput(t *testing.T, master *os.File, want string) {
	t.Helper()
	buf := make([]byte, 8192)
	accumulated := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = master.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := master.Read(buf)
		if n > 0 {
			accumulated += string(buf[:n])
		}
		if strings.Contains(accumulated, want) {
			return
		}
		if err != nil && !isTimeout(err) {
			break
		}
	}
	t.Fatalf("the widget never drew %q; it drew:\n%s", want, accumulated)
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	t, ok := err.(timeout)
	return ok && t.Timeout()
}
