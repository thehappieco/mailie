//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestQuitOrHangupAtAHiddenEntryPutsEchoBackInsteadOfKillingTheProcess(t *testing.T) {
	// Unheard, either would end this test binary at once, past every
	// deferred call, as it would end the command with echo off.
	for _, sig := range []syscall.Signal{syscall.SIGQUIT, syscall.SIGHUP} {
		f := newFakeTerminal(t)
		go func() {
			<-f.hidden // the entry listens before it hides anything
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Error(err)
			}
		}()
		if _, err := readHiddenFrom(t.Context(), f); !errors.Is(err, errEntryStopped) || !f.echoing() {
			t.Errorf("%s at a hidden entry: %v; echoing %v", sig, err, f.echoing())
		}
	}
}
