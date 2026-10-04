package service_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// fetches counts the calls of one method the fake saw.
func fetches(box *providertest.FakeMailbox, method providertest.Method) int {
	n := 0
	for _, c := range box.Calls() {
		if c.Method == method {
			n++
		}
	}
	return n
}

func TestDownloadsBeyondTheSpoolBudgetAreRefusedUntilOneEnds(t *testing.T) {
	// Each download is spooled whole and kept until its client has it. A
	// caller that opens downloads and never reads them must not be able to
	// fill the disk the database lives on: past the budget the answer is
	// "try again shortly", before anything is fetched.
	size := int64(len(report))
	m := newMailFixtureWith(t, fixtureOptions{downloadSpool: size + size/2})
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	held, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatalf("the first download: %v", err)
	}
	// Another caller, so only the bytes can be what refuses it.
	other := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeRead}
	_, err = m.svc.GetRaw(t.Context(), other, msgID)
	wantCode(t, "a download past the budget", err, service.CodeRateLimited)
	if service.RetryAfterOf(err) <= 0 {
		t.Error("the refusal does not say when to try again")
	}
	if n := fetches(box, providertest.MethodFetchRaw); n != 1 {
		t.Errorf("%d originals were fetched, want the refused one never fetched", n)
	}

	readAll(t, held) // closes it
	again, err := m.svc.GetRaw(t.Context(), other, msgID)
	if err != nil {
		t.Fatalf("after the first download ended: %v", err)
	}
	readAll(t, again)

	// One download larger than the whole budget still goes through alone.
	tiny := newMailFixtureWith(t, fixtureOptions{downloadSpool: 1})
	tid, tbox := tiny.fakeAccount(t, service.Principal{}, "ana@example.org")
	tuid := deliverReport(tbox, time.Unix(1_790_000_000, 0))
	tiny.index(t, tid, tbox)
	alone, err := tiny.svc.GetRaw(t.Context(), reader(), tiny.messageID(t, tid, "INBOX", tuid))
	if err != nil {
		t.Fatalf("a download larger than the budget, with nothing else held: %v", err)
	}
	readAll(t, alone)
}

func TestOneCallerCannotTakeEveryDownloadPlace(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{downloadsPerCaller: 2})
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	first, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "2")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.GetAttachment(t.Context(), reader(), msgID, "3")
	wantCode(t, "a third download in flight", err, service.CodeRateLimited)

	// Somebody else is not held up by it.
	other := service.Principal{KeyPrefix: "cccccccc", Scope: auth.ScopeRead}
	theirs, err := m.svc.GetAttachment(t.Context(), other, msgID, "3")
	if err != nil {
		t.Fatalf("another caller's download: %v", err)
	}
	readAll(t, theirs)

	readAll(t, second)
	third, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "3")
	if err != nil {
		t.Fatalf("after one of the two ended: %v", err)
	}
	readAll(t, third)
	readAll(t, first)

	// A person's places are theirs, whichever of their credentials asks.
	ana := m.person(t, "ana@example.com", auth.RoleMember)
	anas, anaBox := m.fakeAccount(t, ana, "ana@mail.example")
	anaUID := deliverReport(anaBox, time.Unix(1_790_000_000, 0))
	m.index(t, anas, anaBox)
	anaMsg := m.messageID(t, anas, "INBOX", anaUID)
	anaKey := service.Principal{KeyPrefix: "dddddddd", Scope: auth.ScopeRead, UserID: ana.UserID, UserRole: ana.UserRole}
	for range 2 {
		dl, err := m.svc.GetRaw(t.Context(), ana, anaMsg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dl.Body.Close() }()
	}
	_, err = m.svc.GetRaw(t.Context(), anaKey, anaMsg)
	wantCode(t, "the person's key, with two downloads open in their session", err, service.CodeRateLimited)
}

func TestADownloadThatFailsGivesItsPlaceBack(t *testing.T) {
	m := newMailFixtureWith(t, fixtureOptions{downloadsPerCaller: 1, downloadSpool: 1})
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)

	for range 3 {
		box.FailNext(providertest.MethodFetchRaw, fmt.Errorf("%w: try later", provider.ErrTemporary))
		if _, err := m.svc.GetRaw(t.Context(), reader(), msgID); err == nil {
			t.Fatal("the injected failure did not reach the caller")
		}
		box.FailNext(providertest.MethodFetchPart, fmt.Errorf("%w: gone", provider.ErrMessageGone))
		_, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "2")
		wantCode(t, "an attachment the server no longer has", err, service.CodeNotFound)
	}
	raw, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	if err != nil {
		t.Fatalf("after failed downloads: %v", err)
	}
	// Closing twice gives back one place, not two.
	_ = raw.Body.Close()
	_ = raw.Body.Close()
	dl, err := m.svc.GetAttachment(t.Context(), reader(), msgID, "2")
	if err != nil {
		t.Fatalf("after the original was closed: %v", err)
	}
	defer func() { _ = dl.Body.Close() }()
	_, err = m.svc.GetRaw(t.Context(), reader(), msgID)
	wantCode(t, "a second download while one is open", err, service.CodeRateLimited)
}

func TestAnOriginalLargerThanTheCapIsRefusedBeforeAnythingIsFetched(t *testing.T) {
	// The index knows the size. Fetching fifty megabytes to throw them away
	// would also hold the account's one interactive connection, and every
	// read queued behind it, for as long as that took.
	m := newMailFixture(t)
	id, box := m.fakeAccount(t, service.Principal{}, "ana@example.org")
	uid := deliverReport(box, time.Unix(1_790_000_000, 0))
	m.index(t, id, box)
	msgID := m.messageID(t, id, "INBOX", uid)
	m.exec(t, `UPDATE messages SET size = ? WHERE id = ?`, service.MaxRawBytes+1, msgID)

	_, err := m.svc.GetRaw(t.Context(), reader(), msgID)
	wantCode(t, "an original over the cap", err, service.CodeBadRequest)
	if n := fetches(box, providertest.MethodFetchRaw); n != 0 {
		t.Errorf("the original was fetched %d times before being refused", n)
	}
	if n := box.Opens(provider.RoleInteractive); n != 0 {
		t.Errorf("%d interactive connections were opened to refuse it", n)
	}
	m.engine.mu.Lock()
	lent := len(m.engine.lent)
	m.engine.mu.Unlock()
	if lent != 0 {
		t.Errorf("the engine lent its connection %d times", lent)
	}
}
