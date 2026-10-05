//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// entrySignals are the signals that would end the process during a hidden
// entry without running a deferred call: quit and hangup. Interrupt and
// termination already cancel the command's context.
var entrySignals = []os.Signal{unix.SIGQUIT, unix.SIGHUP}

// stdinTerminal is the terminal on standard input.
type stdinTerminal struct{ fd int }

func newStdinTerminal() hiddenTerminal { return stdinTerminal{fd: int(os.Stdin.Fd())} }

// hide switches echo off and nothing else, as x/term's ReadPassword does: the
// terminal still edits the line and ends it at Return, and Ctrl-C still
// interrupts. Unlike ReadPassword, it does so before the read, in the caller,
// so that whatever ends the entry finds echo already off and puts it back.
func (t stdinTerminal) hide() (func(), error) {
	saved, err := unix.IoctlGetTermios(t.fd, ioctlGetTermios)
	if err != nil {
		return nil, err
	}
	hidden := *saved
	hidden.Lflag &^= unix.ECHO
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(t.fd, ioctlSetTermios, &hidden); err != nil {
		return nil, err
	}
	return func() {
		//nolint:errcheck // on the way out; a terminal that is gone has no echo to restore
		_ = unix.IoctlSetTermios(t.fd, ioctlSetTermios, saved)
	}, nil
}

func (t stdinTerminal) readLine() ([]byte, error) { return readTerminalLine(os.Stdin) }
