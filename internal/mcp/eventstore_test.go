package mcp_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/mcp"
)

// fakeClock drives an event store's age bound: now, and the sweep timer.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	fn func()
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, fn func()) *time.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), fn: fn})
	// A real timer that never fires: the store only keeps it to know one is
	// armed.
	return time.NewTimer(time.Hour)
}

// Advance moves the clock and fires the timers that came due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	var left []fakeTimer
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			due = append(due, t.fn)
		} else {
			left = append(left, t)
		}
	}
	c.timers = left
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

func replay(t *testing.T, s *mcp.EventStore, session, stream string, after int) ([]string, error) {
	t.Helper()
	var out []string
	for data, err := range s.After(context.Background(), session, stream, after) {
		if err != nil {
			return out, err
		}
		out = append(out, string(data))
	}
	return out, nil
}

func TestTheEventStoreKeepsNoAnswerPastItsAge(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(1_790_000_000, 0)}
	s := mcp.NewEventStore(1<<20, 5*time.Minute)
	s.SetClock(clock.Now, clock.AfterFunc)
	if err := s.Open(ctx, "ses", "1"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"search results", "a message body"} {
		if err := s.Append(ctx, "ses", "1", []byte(body)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
	}
	if got, err := replay(t, s, "ses", "1", -1); err != nil || len(got) != 2 {
		t.Fatalf("resuming within the bound replayed %q (%v)", got, err)
	}
	// Nobody touches the store again: the age bound holds all the same.
	clock.Advance(3*time.Minute + time.Second)
	if got, err := replay(t, s, "ses", "1", 0); err != nil || len(got) != 1 || got[0] != "a message body" {
		t.Fatalf("after the first answer aged out, resuming after it replayed %q (%v)", got, err)
	}
	if _, err := replay(t, s, "ses", "1", -1); !errors.Is(err, sdk.ErrEventsPurged) {
		t.Errorf("resuming from before an aged-out answer: %v, want ErrEventsPurged", err)
	}
	clock.Advance(time.Minute)
	if held := s.Held(); held != 0 {
		t.Errorf("the store holds %d bytes past the age bound", held)
	}
}

func TestTheEventStoreHoldsAtMostItsBytesOldestFirst(t *testing.T) {
	ctx := context.Background()
	s := mcp.NewEventStore(100, time.Hour)
	for _, session := range []string{"a", "b"} {
		if err := s.Open(ctx, session, "1"); err != nil {
			t.Fatal(err)
		}
	}
	chunk := strings.Repeat("x", 20)
	for i := range 4 {
		if err := s.Append(ctx, "a", "1", []byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if err := s.Append(ctx, "b", "1", []byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if held := s.Held(); held > 100 {
			t.Fatalf("after %d rounds the store holds %d bytes, over its 100", i+1, held)
		}
	}
	// Eight answers of 20 bytes in 100: the three oldest are gone, oldest
	// first across sessions.
	if _, err := replay(t, s, "a", "1", -1); !errors.Is(err, sdk.ErrEventsPurged) {
		t.Errorf("session a's first answer should be gone: %v", err)
	}
	if got, err := replay(t, s, "b", "1", 1); err != nil || len(got) != 2 {
		t.Errorf("session b's newest answers: %q (%v)", got, err)
	}
	if err := s.SessionClosed(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if held := s.Held(); held != 60 {
		t.Errorf("after session a closed the store holds %d bytes, want the 60 of b's three newest", held)
	}
}

func TestAnAnswerTooLargeToKeepIsNeverHalfReplayed(t *testing.T) {
	ctx := context.Background()
	s := mcp.NewEventStore(100, time.Hour)
	if err := s.Open(ctx, "ses", "1"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"small", strings.Repeat("x", 60), "after"} {
		if err := s.Append(ctx, "ses", "1", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if held := s.Held(); held != len("small")+len("after") {
		t.Errorf("the store holds %d bytes; the large answer should be delivered, not kept", held)
	}
	// Resuming past the large answer is fine; resuming before it cannot
	// replay what follows without it.
	if got, err := replay(t, s, "ses", "1", 1); err != nil || len(got) != 1 || got[0] != "after" {
		t.Errorf("resuming after the large answer: %q (%v)", got, err)
	}
	if _, err := replay(t, s, "ses", "1", 0); !errors.Is(err, sdk.ErrEventsPurged) {
		t.Errorf("resuming before the large answer: %v, want ErrEventsPurged", err)
	}
}

func TestTheEventStoreForgetsAStreamOnceNothingOfItIsLeft(t *testing.T) {
	// A session kept busy for hours opens a stream per request. What aged
	// out has to take its bookkeeping with it, answers too large to keep
	// included, or the session grows for as long as it lives.
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(1_790_000_000, 0)}
	s := mcp.NewEventStore(1<<20, time.Minute)
	s.SetClock(clock.Now, clock.AfterFunc)
	// The standalone stream, which the transport opens with the session.
	if err := s.Open(ctx, "ses", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "ses", "", []byte("a notification")); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 1<<19)
	for i := range 1000 {
		stream := strconv.Itoa(i)
		if err := s.Open(ctx, "ses", stream); err != nil {
			t.Fatal(err)
		}
		body := "an answer"
		if i%10 == 0 {
			body = large
		}
		if err := s.Append(ctx, "ses", stream, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2 * time.Minute)
	if held := s.Held(); held != 0 {
		t.Fatalf("the store holds %d bytes past the age bound", held)
	}
	// Only the standalone stream is left, empty: the session lives on and
	// may resume it where it was.
	if streams, items := s.Tracked("ses"); streams != 1 || items != 0 {
		t.Errorf("the session still tracks %d streams and %d items", streams, items)
	}
	if got, err := replay(t, s, "ses", "", 0); err != nil || len(got) != 0 {
		t.Errorf("resuming the standalone stream where it was: %q (%v)", got, err)
	}
	if _, err := replay(t, s, "ses", "", -1); !errors.Is(err, sdk.ErrEventsPurged) {
		t.Errorf("resuming the standalone stream before what aged out: %v, want ErrEventsPurged", err)
	}
	// The byte bound forgets the same way.
	small := mcp.NewEventStore(100, time.Hour)
	for i := range 50 {
		stream := strconv.Itoa(i)
		if err := small.Open(ctx, "ses", stream); err != nil {
			t.Fatal(err)
		}
		if err := small.Append(ctx, "ses", stream, []byte(strings.Repeat("y", 20))); err != nil {
			t.Fatal(err)
		}
	}
	if streams, items := small.Tracked("ses"); streams > 5 || items > 5 {
		t.Errorf("a store of 100 bytes tracks %d streams and %d items for 20-byte answers", streams, items)
	}
}

func TestAStreamForgottenWhileItWasStillAnsweringIsNeverMisreplayed(t *testing.T) {
	// The transport numbers a stream's events itself. Once a stream is
	// forgotten, the store no longer knows where an answer written to it
	// afterwards falls, and must not guess: replaying it as the stream's
	// first would hand a resuming client a wrong or a missing answer.
	ctx := context.Background()
	s := mcp.NewEventStore(100, time.Hour)
	if err := s.Open(ctx, "ses", "wait"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "ses", "wait", []byte("progress")); err != nil {
		t.Fatal(err)
	}
	// Another session's answers push everything of the first out.
	if err := s.Open(ctx, "other", "1"); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := s.Append(ctx, "other", "1", []byte(strings.Repeat("z", 20))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Append(ctx, "ses", "wait", []byte("the answer")); err != nil {
		t.Fatal(err)
	}
	// Each resumption replays exactly what followed, or fails and the client
	// asks again. Nothing followed nothing: "progress" is gone.
	for after, want := range map[int][]string{-1: nil, 0: {"the answer"}, 1: {}} {
		got, err := replay(t, s, "ses", "wait", after)
		switch {
		case errors.Is(err, sdk.ErrEventsPurged):
		case err != nil:
			t.Errorf("resuming after %d: %v", after, err)
		case want == nil || !slices.Equal(got, want):
			t.Errorf("resuming after %d replayed %q; want %q or ErrEventsPurged", after, got, want)
		}
	}
}
