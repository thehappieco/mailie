package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

func newKeys(t *testing.T) (*auth.Keys, *store.Store, *time.Time) {
	t.Helper()
	s := storetest.New(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	clock := &now
	return auth.NewKeysWithClock(s, func() time.Time { return *clock }), s, clock
}

func issue(t *testing.T, k *auth.Keys, req auth.NewKeyRequest) (string, auth.Key) {
	t.Helper()
	if req.Name == "" {
		req.Name = "test"
	}
	secret, key, err := k.Issue(context.Background(), req)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return secret, key
}

func TestAnIssuedKeyAuthenticates(t *testing.T) {
	k, _, _ := newKeys(t)
	secret, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})

	p, err := k.Authenticate(context.Background(), secret, nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.KeyPrefix != key.Prefix {
		t.Errorf("prefix = %q, want %q", p.KeyPrefix, key.Prefix)
	}
	if p.Scope != auth.ScopeRead {
		t.Errorf("scope = %q, want read", p.Scope)
	}
}

func TestThePlaintextKeyIsNeverStored(t *testing.T) {
	k, s, _ := newKeys(t)
	secret, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeAdmin})
	_, secretPart, _ := auth.SplitKey(secret)

	var hash string
	if err := s.Reader().QueryRowContext(context.Background(),
		`SELECT hash FROM api_keys WHERE prefix = ?`, key.Prefix).Scan(&hash); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if strings.Contains(hash, secretPart) {
		t.Fatal("the stored hash contains the secret")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("want an Argon2id PHC string, got %q", hash)
	}
}

func TestAWrongSecretIsRejected(t *testing.T) {
	k, _, _ := newKeys(t)
	_, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})

	_, err := k.Authenticate(context.Background(), key.Prefix+".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil)
	if !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey, got %v", err)
	}
}

func TestEveryFailureLooksTheSame(t *testing.T) {
	// Telling a caller which way they failed hands them a way to enumerate
	// which prefixes exist and which keys are still live.
	k, _, clock := newKeys(t)
	live, liveKey := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	revoked, revokedKey := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	expiring, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead, TTL: time.Hour})

	if err := k.Revoke(context.Background(), revokedKey.Prefix); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_ = live
	*clock = clock.Add(2 * time.Hour)

	for name, presented := range map[string]string{
		"unknown prefix": "00000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"wrong secret":   liveKey.Prefix + ".BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"revoked":        revoked,
		"expired":        expiring,
		"malformed":      "not-a-key",
		"empty":          "",
	} {
		_, err := k.Authenticate(context.Background(), presented, nil)
		if !errors.Is(err, auth.ErrInvalidKey) {
			t.Errorf("%s: want ErrInvalidKey, got %v", name, err)
		}
	}
}

func TestAnUnknownPrefixStillPaysForAHash(t *testing.T) {
	// Returning early on a missing row would make a miss measurably faster
	// than a hit, which is enough to enumerate valid prefixes.
	k, _, _ := newKeys(t)
	valid, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})

	hit := timeAuth(t, k, valid)
	miss := timeAuth(t, k, "00000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	wrongSecret := timeAuth(t, k, key.Prefix+".CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")

	// Generous bounds: this asserts that the work happens at all, not that
	// the timings match to the microsecond on a loaded CI machine.
	if miss < hit/8 {
		t.Errorf("an unknown prefix returned far too quickly: hit %v, miss %v", hit, miss)
	}
	if wrongSecret < hit/8 {
		t.Errorf("a wrong secret returned far too quickly: hit %v, wrong %v", hit, wrongSecret)
	}
}

func timeAuth(t *testing.T, k *auth.Keys, presented string) time.Duration {
	t.Helper()
	best := time.Hour
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, _ = k.Authenticate(context.Background(), presented, nil)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best
}

func TestARevokedKeyStaysListed(t *testing.T) {
	// It is part of the answer to "what could have read this mailbox".
	k, _, _ := newKeys(t)
	_, key := issue(t, k, auth.NewKeyRequest{Name: "laptop", Scope: auth.ScopeRead})
	if err := k.Revoke(context.Background(), key.Prefix); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	keys, err := k.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 key listed, got %d", len(keys))
	}
	if !keys[0].Revoked() {
		t.Error("the listed key is not marked revoked")
	}
}

func TestRevokingAnUnknownKeyIsReportedAsNotFound(t *testing.T) {
	k, _, _ := newKeys(t)
	if err := k.Revoke(context.Background(), "deadbeef"); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRevokingTwiceIsNotAnError(t *testing.T) {
	k, _, _ := newKeys(t)
	_, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	ctx := context.Background()
	if err := k.Revoke(ctx, key.Prefix); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if err := k.Revoke(ctx, key.Prefix); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestARevokedKeyIsRejectedByRecheck(t *testing.T) {
	// A long request that started while the key was live must not publish
	// its answer after the key was revoked.
	k, _, _ := newKeys(t)
	secret, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	ctx := context.Background()

	p, err := k.Authenticate(ctx, secret, nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := k.Recheck(ctx, p); err != nil {
		t.Fatalf("Recheck while live: %v", err)
	}
	if err := k.Revoke(ctx, key.Prefix); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := k.Recheck(ctx, p); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("want ErrInvalidKey after revocation, got %v", err)
	}
}

func TestAReadKeyCannotWriteOrSend(t *testing.T) {
	read := auth.ScopeRead
	if read.Covers(auth.ScopeWrite) {
		t.Error("read must not cover write: an observer should not be able to mark mail as read")
	}
	if read.Covers(auth.ScopeSend) {
		t.Error("read must not cover send")
	}
	if !read.Covers(auth.ScopeRead) {
		t.Error("read must cover read")
	}
}

func TestScopesAreOrdered(t *testing.T) {
	for _, tc := range []struct {
		have, want auth.Scope
		ok         bool
	}{
		{auth.ScopeAdmin, auth.ScopeSend, true},
		{auth.ScopeAdmin, auth.ScopeRead, true},
		{auth.ScopeSend, auth.ScopeWrite, true},
		{auth.ScopeSend, auth.ScopeAdmin, false},
		{auth.ScopeWrite, auth.ScopeRead, true},
		{auth.ScopeWrite, auth.ScopeSend, false},
	} {
		if got := tc.have.Covers(tc.want); got != tc.ok {
			t.Errorf("%s covers %s = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestAnUnknownScopeIsRejectedRatherThanIgnored(t *testing.T) {
	if _, err := auth.ParseScope("readonly"); err == nil {
		t.Fatal("a typo must not parse")
	}
	k, _, _ := newKeys(t)
	if _, _, err := k.Issue(context.Background(), auth.NewKeyRequest{Name: "x", Scope: "root"}); err == nil {
		t.Fatal("Issue accepted an unknown scope")
	}
}

func TestAKeyMayBeRestrictedToSomeAccounts(t *testing.T) {
	k, s, _ := newKeys(t)
	ctx := context.Background()
	seedAccount(t, s, "acc_1")
	seedAccount(t, s, "acc_2")

	secret, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead, AccountIDs: []string{"acc_1"}})
	p, err := k.Authenticate(ctx, secret, nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.MayAccess("acc_1") {
		t.Error("the key should reach the account it was restricted to")
	}
	if p.MayAccess("acc_2") {
		t.Error("the key reached an account it was not restricted to")
	}
}

func TestAKeyWhoseLastMailboxIsRemovedStopsRatherThanReachingEveryMailbox(t *testing.T) {
	// No restriction rows means every account, and a removed mailbox takes
	// its rows with it: without the trigger, a key for one mailbox would
	// quietly become a key for all of them.
	k, s, _ := newKeys(t)
	ctx := context.Background()
	for _, id := range []string{"acc_1", "acc_2", "acc_3"} {
		seedAccount(t, s, id)
	}
	one, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead, AccountIDs: []string{"acc_1"}})
	two, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead, AccountIDs: []string{"acc_2", "acc_3"}})
	remove := func(id string) {
		t.Helper()
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	remove("acc_1")
	if p, err := k.Authenticate(ctx, one, nil); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("the key for the removed mailbox authenticated as %+v (%v); it reaches every mailbox now", p, err)
	}
	listed, err := k.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range listed {
		if strings.HasPrefix(one, key.Prefix+".") && !key.Revoked() {
			t.Error("the key for the removed mailbox is not listed as revoked")
		}
	}
	remove("acc_2")
	p, err := k.Authenticate(ctx, two, nil)
	if err != nil {
		t.Fatalf("a key that still has a mailbox stopped: %v", err)
	}
	if !p.MayAccess("acc_3") || p.MayAccess("acc_2") || p.MayAccess("acc_x") {
		t.Errorf("the key reaches %v, want only acc_3", p.AccountIDs)
	}
}

func TestAnUnrestrictedKeyReachesEveryAccount(t *testing.T) {
	k, _, _ := newKeys(t)
	secret, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	p, err := k.Authenticate(context.Background(), secret, nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.MayAccess("acc_anything") {
		t.Error("an unrestricted key should reach every account")
	}
}

func TestAKeyCannotOutliveAYear(t *testing.T) {
	k, _, _ := newKeys(t)
	_, _, err := k.Issue(context.Background(), auth.NewKeyRequest{
		Name: "forever", Scope: auth.ScopeRead, TTL: 2 * auth.MaxLifetime,
	})
	if err == nil {
		t.Fatal("want a lifetime complaint")
	}
}

func TestLastUsedIsRecordedWithoutBlockingTheRequest(t *testing.T) {
	k, s, _ := newKeys(t)
	secret, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	if _, err := k.Authenticate(context.Background(), secret, nil); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		var lastUsed int64
		if err := s.Reader().QueryRowContext(context.Background(),
			`SELECT last_used_at FROM api_keys WHERE prefix = ?`, key.Prefix).Scan(&lastUsed); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsed != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("last_used_at was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestACancelledRequestStillRecordsTheUseThatHappened(t *testing.T) {
	k, s, _ := newKeys(t)
	secret, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := k.Authenticate(ctx, secret, nil); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	cancel() // the client hung up right after authenticating

	deadline := time.Now().Add(3 * time.Second)
	for {
		var lastUsed int64
		if err := s.Reader().QueryRowContext(context.Background(),
			`SELECT last_used_at FROM api_keys WHERE prefix = ?`, key.Prefix).Scan(&lastUsed); err != nil {
			t.Fatalf("read last_used_at: %v", err)
		}
		if lastUsed != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the bookkeeping write was lost with the request context")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func seedAccount(t *testing.T, s *store.Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO accounts(id, email, provider, auth_kind, imap_host, imap_port, smtp_host, smtp_port,
			 smtp_tls, login_user, save_sent_copy, state, state_changed_at, created_at, updated_at)
			 VALUES (?, ?, 'gmail', 'oauth2', 'imap.gmail.com', 993, 'smtp.gmail.com', 465, 'implicit', ?, 0, 'active', 0, 0, 0)`,
			id, id+"@example.com", id+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func TestAKeyThatMatchesNothingIsRefusedUnhashedWhenTheGateSaysSo(t *testing.T) {
	// A random prefix can only fail. Once a caller has sent too many, the
	// gate stops them before the 19 MiB derivation; a real key never reaches
	// the gate, so a gate that always refuses must not turn one away.
	k, _, _ := newKeys(t)
	valid, key := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})

	var derivations atomic.Int32
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		derivations.Add(1)
		return argon2.IDKey(password, salt, time, memory, threads, keyLen)
	})
	var asked int
	closed := func() (bool, time.Duration) { asked++; return false, 7 * time.Second }

	for name, presented := range map[string]string{
		"unknown prefix": "00000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"malformed":      "not.a.key",
	} {
		derivations.Store(0)
		_, err := k.Authenticate(t.Context(), presented, closed)
		var throttled *auth.ThrottledError
		if !errors.As(err, &throttled) || throttled.RetryAfter != 7*time.Second {
			t.Errorf("%s: %v, want a ThrottledError carrying the gate's wait", name, err)
		}
		if n := derivations.Load(); n != 0 {
			t.Errorf("%s: %d derivations behind a closed gate", name, n)
		}
	}

	asked = 0
	if _, err := k.Authenticate(t.Context(), valid, closed); err != nil {
		t.Errorf("a closed gate turned a real key away: %v", err)
	}
	if _, err := k.Authenticate(t.Context(), key.Prefix+".BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", closed); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("a wrong secret on a real prefix: %v, want ErrInvalidKey", err)
	}
	if asked != 0 {
		t.Errorf("the gate was asked %d times about keys whose prefix exists", asked)
	}
}

func TestKeyChecksAreBoundedAndAFullQueueIsRefusedAtOnce(t *testing.T) {
	// 19 MiB a check. Four at once bounds the memory whatever arrives, and a
	// queue that is already full refuses at once rather than holding every
	// request until its deadline.
	k, _, _ := newKeys(t)
	valid, _ := issue(t, k, auth.NewKeyRequest{Scope: auth.ScopeRead})
	_, _, running, queue := auth.HashLimitsForTest()

	var now, peak atomic.Int32
	release := make(chan struct{})
	auth.SetDeriveKeyForTest(t, func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		n := now.Add(1)
		for {
			seen := peak.Load()
			if n <= seen || peak.CompareAndSwap(seen, n) {
				break
			}
		}
		<-release
		now.Add(-1)
		return argon2.IDKey(password, salt, time, memory, threads, keyLen)
	})

	var wg sync.WaitGroup
	for range running + queue {
		wg.Go(func() {
			if _, err := k.Authenticate(context.Background(), valid, nil); err != nil {
				t.Errorf("a queued check failed: %v", err)
			}
		})
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, waiting := auth.HashWaitersForTest()
		if int(now.Load()) == running && waiting == queue {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d running and %d waiting; want %d and %d", now.Load(), waiting, running, queue)
		}
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	if _, err := k.Authenticate(t.Context(), valid, nil); !errors.Is(err, auth.ErrHashBusy) {
		t.Errorf("a check behind a full queue: %v, want ErrHashBusy", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the refusal took %v; it should not wait", waited)
	}

	close(release)
	wg.Wait()
	if got := peak.Load(); int(got) != running {
		t.Fatalf("at most %d checks ran at once, want exactly %d", got, running)
	}
}
