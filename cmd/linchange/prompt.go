package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// isTerminal reports whether f is attached to an interactive terminal.
//
// Every interactive prompt in this binary asks this first, because a Debian
// package configuration run is not always one: an automated install pipes
// answers in, and a wizard that blocked on a hidden prompt would hang the
// whole dpkg run.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// readSecret reads a secret from stdin without echoing it back.
//
// On a terminal the typed characters are suppressed; otherwise the line is
// read plainly, so `printf '%s\n' "$PW" | linchange create-admin --password -`
// and CI-driven installs both work. A single trailing newline is stripped, and
// an empty line is a valid answer: the caller decides whether to re-ask.
func readSecret(in io.Reader, prompt string) (string, error) {
	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if line == "" && err != nil {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Fprint(os.Stderr, prompt)
	raw, err := term.ReadPassword(int(f.Fd()))
	// The cursor is left wherever ReadPassword stopped, so the newline has to
	// be written explicitly or the next message lands on the prompt line.
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// readLine reads one line from in, trimming the line terminator. The reader is
// wrapped for every call rather than hoisted, because a wizard that prompts
// many times from stdin would otherwise prefetch past the answers: bufio reads
// ahead, and a buffered remainder would be lost when the slice goes away.
func readLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if line == "" && err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
