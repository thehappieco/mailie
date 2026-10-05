//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import (
	"errors"
	"os"
)

// entrySignals is empty: without a hidden entry there is nothing to put back.
var entrySignals []os.Signal

var errNoHiddenEntry = errors.New("a password cannot be typed hidden on this system; " +
	"pipe it on standard input instead")

// stdinTerminal stands for a terminal this system cannot hide an entry on.
type stdinTerminal struct{}

func newStdinTerminal() hiddenTerminal { return stdinTerminal{} }

func (stdinTerminal) hide() (func(), error)     { return nil, errNoHiddenEntry }
func (stdinTerminal) readLine() ([]byte, error) { return nil, errNoHiddenEntry }
