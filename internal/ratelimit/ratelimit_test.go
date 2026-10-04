package ratelimit_test

import (
	"fmt"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/ratelimit"
)

func TestABurstIsAllowedAndThenTheRateApplies(t *testing.T) {
	now := time.Now()
	l := ratelimit.New(60, 3).WithClock(func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("attempt %d was refused inside the burst", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("the fourth attempt should exhaust a burst of three")
	}
	if retry <= 0 {
		t.Fatal("a refusal must say how long to wait, for Retry-After")
	}

	// One token a second at 60 per minute.
	now = now.Add(time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("a token should have refilled after a second")
	}
}

func TestKeysAreLimitedIndependently(t *testing.T) {
	l := ratelimit.New(60, 1)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("first key refused")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("one caller exhausting its bucket must not affect another")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("the first key should now be out of tokens")
	}
}

func TestBothTheAddressAndTheKeyPrefixMustHaveATokenLeft(t *testing.T) {
	// One limit alone is not enough: per-address only lets a script spread its
	// guesses across addresses, per-subject only lets it work through a list
	// of prefixes from one address.
	a := &ratelimit.Auth{PerIP: ratelimit.New(60, 10), PerSubject: ratelimit.New(60, 1)}
	r := requestFrom(t, "203.0.113.5:1234", "")

	if ok, _ := a.Allow(r, "deadbeef"); !ok {
		t.Fatal("first attempt refused")
	}
	if ok, _ := a.Allow(r, "deadbeef"); ok {
		t.Fatal("the per-subject limit should have stopped a second attempt on the same key")
	}
	// A different key prefix has its own subject bucket, so hammering one key
	// does not lock out an unrelated caller sharing the address.
	if ok, _ := a.Allow(r, "cafebabe"); !ok {
		t.Fatal("a different key prefix should have its own subject bucket")
	}

	// And the address limit still binds once it runs out, whichever keys the
	// attempts claimed to be.
	narrow := &ratelimit.Auth{PerIP: ratelimit.New(60, 1), PerSubject: ratelimit.New(600, 100)}
	if ok, _ := narrow.Allow(r, "aaaaaaaa"); !ok {
		t.Fatal("first attempt refused")
	}
	if ok, _ := narrow.Allow(r, "bbbbbbbb"); ok {
		t.Fatal("changing the key prefix must not escape the per-address limit")
	}
}

func TestARefusedAttemptStillCostsAnAddressToken(t *testing.T) {
	// An attempt that the subject limiter rejected was still an attempt: not
	// charging the address for it would let a script hold one address open
	// indefinitely by cycling through key prefixes it knows are rate-limited.
	a := &ratelimit.Auth{PerIP: ratelimit.New(60, 2), PerSubject: ratelimit.New(60, 1)}
	r := requestFrom(t, "203.0.113.5:1234", "")

	if ok, _ := a.Allow(r, "deadbeef"); !ok {
		t.Fatal("first attempt refused")
	}
	if ok, _ := a.Allow(r, "deadbeef"); ok {
		t.Fatal("the subject limit should have stopped this one")
	}
	if ok, _ := a.Allow(r, "cafebabe"); ok {
		t.Fatal("the address budget should have been spent by the refused attempt")
	}
}

func TestANilAuthAllowsEverything(t *testing.T) {
	var a *ratelimit.Auth
	if ok, _ := a.Allow(requestFrom(t, "203.0.113.5:1234", ""), "x"); !ok {
		t.Fatal("a nil limiter should not refuse; tests and headless setups rely on it")
	}
}

func TestTheForwardedHeaderIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	// Believing it from anywhere would let a caller invent a fresh address per
	// request, and the limit would bound nothing at all.
	r := requestFrom(t, "203.0.113.5:1234", "198.51.100.9")
	if got := ratelimit.ClientIP(r, nil); got != "203.0.113.5" {
		t.Fatalf("ClientIP = %q, want the peer address", got)
	}
}

func TestTheForwardedHeaderIsReadFromTheRightFromATrustedProxy(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	// Two hops: a client, then something the client itself wrote to confuse
	// the count. The rightmost address that is not a trusted proxy is the
	// real caller; everything to its left is unverifiable.
	r := requestFrom(t, "203.0.113.5:1234", "1.2.3.4, 198.51.100.9")
	if got := ratelimit.ClientIP(r, trusted); got != "198.51.100.9" {
		t.Fatalf("ClientIP = %q, want 198.51.100.9", got)
	}
}

func TestAChainOfTrustedProxiesResolvesToTheClient(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	r := requestFrom(t, "203.0.113.5:1234", "198.51.100.9, 203.0.113.7")
	if got := ratelimit.ClientIP(r, trusted); got != "198.51.100.9" {
		t.Fatalf("ClientIP = %q, want the last hop before the trusted proxies", got)
	}
}

func TestQuietKeysAreForgotten(t *testing.T) {
	// Otherwise the map grows with every address that ever tried once.
	now := time.Now()
	l := ratelimit.New(60, 1).WithClock(func() time.Time { return now })
	if ok, _ := l.Allow("one-shot"); !ok {
		t.Fatal("first attempt refused")
	}
	now = now.Add(10 * time.Minute)
	if ok, _ := l.Allow("another"); !ok {
		t.Fatal("second key refused")
	}
	// The forgotten key starts fresh, which is the observable consequence.
	if ok, _ := l.Allow("one-shot"); !ok {
		t.Fatal("a key quiet long enough to refill should be allowed again")
	}
}

func requestFrom(t *testing.T, remoteAddr, forwarded string) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = remoteAddr
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	return r
}

func TestARefundedTokenCanBeSpentAgain(t *testing.T) {
	l := ratelimit.New(60, 1)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first attempt refused")
	}
	l.Refund("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("the refunded token could not be spent")
	}
	// A refund never takes a bucket past its burst.
	l.Refund("k")
	l.Refund("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("refunded token missing")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("two refunds bought a burst of two")
	}
}

func TestOnlyFailuresSpendTheTightBucket(t *testing.T) {
	// A caller proving a good key over and over is polling, not guessing,
	// and must not run out; a caller missing is guessing, and must.
	now := time.Now()
	clock := func() time.Time { return now }
	a := ratelimit.DefaultAuth(nil)
	a.PerIP.WithClock(clock)
	a.PerSubject.WithClock(clock)
	r := requestFrom(t, "203.0.113.5:1234", "")

	for i := range 300 {
		if ok, _ := a.Admit(r, "key:deadbeef"); !ok {
			t.Fatalf("poll %d with a good key was refused", i+1)
		}
		a.Refund("key:deadbeef")
		now = now.Add(2 * time.Second)
	}
	for range 10 {
		if ok, _ := a.Admit(r, "key:deadbeef"); !ok {
			t.Fatal("refused before any failure was charged")
		}
	}
	if ok, retry := a.Admit(r, "key:deadbeef"); ok || retry <= 0 {
		t.Fatalf("ten misses did not throttle the key: ok=%t retry=%v", ok, retry)
	}
	// The misses were charged to the key, not the address: a session, or
	// another key, proved from the same place is not refused for them.
	if ok, _ := a.Admit(r, ""); !ok {
		t.Fatal("ten misses at one key throttled the whole address")
	}
	if ok, _ := a.Admit(r, "key:cafebabe"); !ok {
		t.Fatal("ten misses at one key throttled another key from the same address")
	}
	if ok, _ := a.Admit(requestFrom(t, "198.51.100.9:1", ""), "key:cafebabe"); !ok {
		t.Fatal("another address and another key were throttled too")
	}
}

func TestConcurrentMissesCannotOutrunTheFailureBucket(t *testing.T) {
	// Admit reserves before the check rather than only looking: forty wrong
	// secrets arriving together would otherwise all find the same last token
	// and all be checked.
	a := ratelimit.DefaultAuth(nil)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 40 {
		wg.Go(func() {
			<-start
			r := requestFrom(t, fmt.Sprintf("198.51.100.%d:1", i+1), "")
			if ok, _ := a.Admit(r, "key:deadbeef"); ok {
				admitted.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if n := admitted.Load(); n != 10 {
		t.Fatalf("%d of 40 simultaneous misses were admitted, want the burst of 10", n)
	}
}

func TestKeysThatMatchNothingAreLimitedPerAddress(t *testing.T) {
	// A script inventing a fresh prefix per request dodges the per-prefix
	// bucket; this one follows the address instead.
	a := ratelimit.DefaultAuth(nil)
	r := requestFrom(t, "203.0.113.5:1234", "")
	for i := range 10 {
		if ok, _ := a.AdmitUnknownKey(r); !ok {
			t.Fatalf("unknown key %d refused inside the burst", i+1)
		}
	}
	if ok, retry := a.AdmitUnknownKey(r); ok || retry <= 0 {
		t.Fatalf("an eleventh unknown key: ok=%t retry=%v", ok, retry)
	}
	if ok, _ := a.AdmitUnknownKey(requestFrom(t, "198.51.100.9:1", "")); !ok {
		t.Fatal("another address was refused for the first one's unknown keys")
	}
	// And it is a separate bucket from the address's own: bearer traffic from
	// the same place still flows.
	if ok, _ := a.Admit(r, "key:cafebabe"); !ok {
		t.Fatal("the unknown-key bucket throttled other traffic from the address")
	}
}

func TestAddressesInOneIPv6SubnetShareALimit(t *testing.T) {
	// A /64 is one subscriber: limiting each of its 2^64 addresses on its
	// own would hand one machine an endless supply of fresh buckets.
	a := &ratelimit.Auth{PerIP: ratelimit.New(60, 1)}
	if ok, _ := a.Allow(requestFrom(t, "[2001:db8:1:2::1]:1234", ""), ""); !ok {
		t.Fatal("first attempt refused")
	}
	if ok, _ := a.Allow(requestFrom(t, "[2001:db8:1:2:dead:beef:0:1]:1234", ""), ""); ok {
		t.Fatal("another address in the same /64 got a bucket of its own")
	}
	if ok, _ := a.Allow(requestFrom(t, "[2001:db8:1:3::1]:1234", ""), ""); !ok {
		t.Fatal("the next /64 over shares a bucket with the first")
	}
	// The same through a trusted proxy.
	trusted := []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}
	b := &ratelimit.Auth{PerIP: ratelimit.New(60, 1), Proxies: trusted}
	if ok, _ := b.Allow(requestFrom(t, "203.0.113.5:1", "2001:db8:9:9::1"), ""); !ok {
		t.Fatal("first forwarded attempt refused")
	}
	if ok, _ := b.Allow(requestFrom(t, "203.0.113.5:1", "2001:db8:9:9::2"), ""); ok {
		t.Fatal("a forwarded address in the same /64 got a bucket of its own")
	}
	// IPv4 stays one address, one bucket.
	if ok, _ := a.Allow(requestFrom(t, "198.51.100.1:1", ""), ""); !ok {
		t.Fatal("first IPv4 attempt refused")
	}
	if ok, _ := a.Allow(requestFrom(t, "198.51.100.2:1", ""), ""); !ok {
		t.Fatal("two IPv4 addresses shared a bucket")
	}
}
