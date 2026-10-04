// Package ratelimit bounds how often one caller may try something.
//
// It exists first for authentication. Verifying a key runs Argon2id — 19 MiB
// and a few milliseconds — which is nothing for a person and free for a script
// working through a list. Unmetered, that script gets unlimited guesses and,
// run wide enough, a memory-exhaustion attack for the price of a few thousand
// requests a second.
//
// It exists second for sending. A model in a loop can call send_message far
// faster than any human would, and both providers throttle hard: Exchange
// Online allows thirty messages a minute per mailbox and answers the
// thirty-first with a rejection the recipient never sees.
//
// Token buckets, in memory, per key. One process, one set of limits; a shared
// store on the authentication path would be a dependency in the way of the
// thing it is meant to protect.
package ratelimit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Limiter allows a burst and then a steady rate, per key.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter admitting burst attempts at once and perMinute
// thereafter, per key.
func New(perMinute, burst int) *Limiter {
	if perMinute < 1 {
		perMinute = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// WithClock overrides the limiter's clock, for tests.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = now
	return l
}

// Allow spends one token for key. When it cannot, it reports how long until it
// could, which is what a Retry-After header wants.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)
	b, found := l.buckets[key]
	if !found {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait.Round(time.Second) + time.Second
}

// Refund gives back a token Allow spent for key. It is how a bucket that only
// failures should pay into is charged before an attempt whose outcome is not
// known yet: spend first, so that attempts running side by side cannot all
// see the same last token, and give it back when the attempt turns out not to
// have been a failure.
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, found := l.buckets[key]; found {
		b.tokens = min(l.burst, b.tokens+1)
	}
}

// sweep forgets keys quiet long enough to be full again, so the map does not
// grow with every address that ever tried once.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for key, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, key)
		}
	}
}

// ClientIP is the address a request came from.
//
// The connection's own address, unless the connection came from a proxy the
// deployment named as trusted: then the last hop before that proxy, read from
// X-Forwarded-For right to left. Believing that header from anywhere else
// would let a caller invent a fresh address per request, and the limit would
// bound nothing at all.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !within(peer, trusted) {
		return peer.String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		addr = addr.Unmap()
		if !within(addr, trusted) {
			return addr.String()
		}
	}
	return peer.String()
}

// addressKey is the bucket a request's address spends from: the address itself
// for IPv4, and its /64 for IPv6.
//
// A /64 is what one IPv6 subscriber is handed — a home line, a phone, the
// cheapest VPS — and it holds 2^64 addresses. Limiting each one separately
// would give a single machine an unlimited supply of fresh buckets, and every
// per-address limit here would bound nothing. Whoever shares a /64 shares one
// network, which is what an IPv4 address behind a NAT means too.
func addressKey(r *http.Request, trusted []netip.Prefix) string {
	ip := ClientIP(r, trusted)
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() {
		return ip
	}
	subnet, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return subnet.String()
}

func within(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Auth applies two limits at once: one per address, because a script should
// not get unmetered guesses, and a tighter one per subject — a key prefix, an
// email address — because spreading those guesses across many addresses should
// not help either.
//
// It is used two ways. Sign-in spends both buckets on every attempt (Allow),
// since each attempt is a guess at a password. Bearer traffic spends the
// address bucket on every request, and only failures stay charged to the
// subject (Admit, then Refund for anything that was not a miss): a console
// polling an account every few seconds, or the CLI waiting for consent, is a
// caller proving its credential over and over, and must never be throttled for
// it, while somebody guessing a key's secret is throttled after a handful of
// misses.
//
// Nothing here refuses a credential because of what another caller did, which
// is why failures are never charged to an address. Behind a NAT, or behind a
// proxy MAIL_TRUSTED_PROXIES does not name, one address is a whole office, and
// ten stale tokens there would otherwise lock every good one out with them.
// What a failure-per-address bucket would buy, bounding guesses at session
// tokens, the address bucket buys already: a session token is 256 random bits
// found with one SHA-256, and 600 guesses a minute at it are no guesses at all.
// The one check that can only fail and is expensive — a key whose prefix
// matches nothing, which still pays for an Argon2id derivation — has its own
// bucket per address (AdmitUnknownKey), which no real key ever reaches.
//
// A nil *Auth allows everything, so a test need not build one.
type Auth struct {
	PerIP      *Limiter
	PerSubject *Limiter
	// Proxies are the addresses whose X-Forwarded-For is believed.
	Proxies []netip.Prefix
}

// DefaultAuth is the production setting for bearer traffic: generous per
// address, because a NAT hides a whole office behind one, and tight on
// failures per key prefix, and on keys that match nothing per address.
func DefaultAuth(proxies []netip.Prefix) *Auth {
	return &Auth{
		PerIP:      New(600, 60),
		PerSubject: New(30, 10),
		Proxies:    proxies,
	}
}

// DefaultSignIn is the production setting for sign-in, sign-up and password
// changes, where every attempt costs a 64 MiB Argon2id derivation and is a
// guess at somebody's password: Wappie's numbers, per address and per
// account.
func DefaultSignIn(proxies []netip.Prefix) *Auth {
	return &Auth{
		PerIP:      New(60, 20),
		PerSubject: New(5, 5),
		Proxies:    proxies,
	}
}

// Allow spends a token for the request's address and, when subject is not
// empty, for the subject as well. Both must have one.
func (a *Auth) Allow(r *http.Request, subject string) (ok bool, retryAfter time.Duration) {
	if a == nil {
		return true, 0
	}
	if a.PerIP != nil {
		if ok, wait := a.PerIP.Allow("ip:" + addressKey(r, a.Proxies)); !ok {
			return false, wait
		}
	}
	if subject != "" && a.PerSubject != nil {
		if ok, wait := a.PerSubject.Allow(subjectKey(subject)); !ok {
			return false, wait
		}
	}
	return true, 0
}

// Admit spends a token for the request's address and, when subject is not
// empty, reserves one of the subject's failures: it is spent now, before the
// credential is checked, and Refund gives it back if the check was not a miss.
//
// Reserving rather than asking is what makes the bound hold under
// concurrency. Forty wrong secrets for one key sent at the same instant would
// all find the bucket full if they only looked, and all be checked; spent up
// front, the eleventh finds it empty. A good credential gives its token back as
// soon as it is proved, so it holds one only for the length of its own check.
func (a *Auth) Admit(r *http.Request, subject string) (ok bool, retryAfter time.Duration) {
	return a.Allow(r, subject)
}

// Refund gives back the failure Admit reserved for subject, for an attempt
// that turned out not to be a miss: the credential was proved, or the check
// never ran — a database error, or every hashing slot busy — which is not a
// guess either.
func (a *Auth) Refund(subject string) {
	if a == nil || a.PerSubject == nil || subject == "" {
		return
	}
	a.PerSubject.Refund(subjectKey(subject))
}

// AdmitUnknownKey spends one of the address's checks of keys that match
// nothing. It is asked only once a presented key's prefix has been looked up
// and not found, so it can never refuse a real key; what it bounds is a script
// inventing a fresh prefix for every request, which would otherwise dodge the
// per-prefix limit and still cost an Argon2id derivation each time.
func (a *Auth) AdmitUnknownKey(r *http.Request) (ok bool, retryAfter time.Duration) {
	if a == nil || a.PerSubject == nil {
		return true, 0
	}
	return a.PerSubject.Allow("unknown-key:" + addressKey(r, a.Proxies))
}

func subjectKey(subject string) string {
	return "subject:" + strings.ToLower(strings.TrimSpace(subject))
}
