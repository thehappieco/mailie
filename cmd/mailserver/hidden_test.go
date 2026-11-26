package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeTerminal is a terminal for readHiddenFrom: it records whether it
// echoes, and a line arrives when the test types one.
type fakeTerminal struct {
	mu     sync.Mutex
	echo   bool
	hidden chan struct{} // closed once echo is off
	typed  chan []byte
}

func newFakeTerminal(t *testing.T) *fakeTerminal {
	f := &fakeTerminal{echo: true, hidden: make(chan struct{}), typed: make(chan []byte)}
	// A read nobody waits for any more ends with the test, as it ends with
	// the process.
	t.Cleanup(func() { close(f.typed) })
	return f
}

func (f *fakeTerminal) hide() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.echo = false
	close(f.hidden)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.echo = true
	}, nil
}

func (f *fakeTerminal) readLine() ([]byte, error) {
	line, ok := <-f.typed
	if !ok {
		return nil, io.EOF
	}
	return line, nil
}

func (f *fakeTerminal) echoing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.echo
}

func TestEchoComesBackWhicheverWayAHiddenEntryEnds(t *testing.T) {
	t.Run("Return", func(t *testing.T) {
		f := newFakeTerminal(t)
		go func() { <-f.hidden; f.typed <- []byte("a secret") }()
		line, err := readHiddenFrom(t.Context(), f)
		if err != nil || string(line) != "a secret" || !f.echoing() {
			t.Errorf("read %q, %v; echoing %v", line, err, f.echoing())
		}
	})
	t.Run("an interrupt while typing", func(t *testing.T) {
		f := newFakeTerminal(t)
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-f.hidden; cancel() }()
		if _, err := readHiddenFrom(ctx, f); !errors.Is(err, context.Canceled) || !f.echoing() {
			t.Errorf("%v; echoing %v", err, f.echoing())
		}
	})
	t.Run("an interrupt before the prompt", func(t *testing.T) {
		f := newFakeTerminal(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := readHiddenFrom(ctx, f); !errors.Is(err, context.Canceled) || !f.echoing() {
			t.Errorf("%v; echoing %v", err, f.echoing())
		}
	})
}

func TestAHiddenEntryReadsOneLineAndNothingPastIt(t *testing.T) {
	r := strings.NewReader("secret\b\bxx\r\nthe next line")
	if line, err := readTerminalLine(r); err != nil || string(line) != "secrxx" || r.Len() != len("the next line") {
		t.Errorf("read %q, %v, leaving %d bytes", line, err, r.Len())
	}
	if line, err := readTerminalLine(strings.NewReader("no newline")); err != nil || string(line) != "no newline" {
		t.Errorf("a line ended by the end of input: %q, %v", line, err)
	}
	if _, err := readTerminalLine(strings.NewReader("")); !errors.Is(err, io.EOF) {
		t.Errorf("nothing typed before the end of input: %v", err)
	}
}
