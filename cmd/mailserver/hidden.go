package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"golang.org/x/term"
)

// A secret typed at a terminal: a mailbox's password, which `account add
// --password -` reads without echoing it (hidden_unix.go switches the echo
// off, hidden_other.go says it cannot).

// stdinIsTerminal and readHidden stand for the terminal, which `go test`
// never has; variables so that a test can be one. `account add --password -`
// reads a mailbox's password with them.
var (
	stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	readHidden      = readHiddenLine
)

// hiddenTerminal is a terminal a secret is typed at. hide switches its echo
// off and returns what switches it back on; readLine reads one line and never
// touches the terminal's settings.
type hiddenTerminal interface {
	hide() (show func(), err error)
	readLine() ([]byte, error)
}

// errEntryStopped is a hidden entry ended by quit or hangup.
var errEntryStopped = errors.New("stopped by a signal")

// readHiddenLine reads one line from the terminal on standard input without
// echoing it.
func readHiddenLine(ctx context.Context) ([]byte, error) {
	return readHiddenFrom(ctx, newStdinTerminal())
}

// readHiddenFrom reads one line from t without echoing it, and leaves the
// terminal echoing again whichever way it ends: Return, an interrupt or a
// termination (ctx), or quit or hangup.
//
// Echo goes off here, before anything waits, and the deferred call puts it
// back, last. The read runs in a goroutine, because it is the one step a
// signal does not end (the terminal still waits for Return), and it never
// touches the terminal's settings: a process that gives up leaves it blocked
// and exits, and nothing switches echo off again after it was put back. Quit
// (Ctrl-\, which the terminal still sends) and hangup would otherwise end the
// process at once, past every deferred call, so they are listened for while
// the entry is open.
func readHiddenFrom(ctx context.Context, t hiddenTerminal) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var listen []os.Signal
	for _, sig := range entrySignals {
		// An ignored signal ends nothing, and listening would stop
		// ignoring it (nohup's hangup).
		if !signal.Ignored(sig) {
			listen = append(listen, sig)
		}
	}
	stopped := make(chan os.Signal, 1)
	if len(listen) > 0 { // none would mean every signal
		signal.Notify(stopped, listen...)
		defer signal.Stop(stopped) // after show: echo is back before quit can kill again
	}
	show, err := t.hide()
	if err != nil {
		return nil, err
	}
	defer show()

	type read struct {
		line []byte
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := t.readLine()
		done <- read{line, err}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case sig := <-stopped:
		return nil, fmt.Errorf("%w (%s); nothing was changed", errEntryStopped, sig)
	}
}

// readTerminalLine reads one line of r a byte at a time, so that nothing past
// it is consumed, as x/term's ReadPassword does: a backspace that reaches it
// removes the byte before, a carriage return is dropped, and the end of input
// ends a line that has something in it.
func readTerminalLine(r io.Reader) ([]byte, error) {
	var b [1]byte
	var line []byte
	for {
		n, err := r.Read(b[:])
		if n > 0 {
			switch b[0] {
			case '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			case '\n':
				return line, nil
			case '\r':
			default:
				line = append(line, b[0])
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return line, nil
			}
			return nil, err
		}
	}
}
