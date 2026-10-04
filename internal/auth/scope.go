package auth

import "fmt"

// Scope is what a key may do, ordered: each level contains the ones before it.
//
// Four levels rather than three, because "read" has to mean it. An agent that
// is only supposed to look at a mailbox should not be able to mark things read
// — that changes what the human sees the next time they open their mail — and
// an integration that files and drafts is not automatically one that may send
// on somebody's behalf.
type Scope string

const (
	// ScopeRead observes: list, search, read bodies, fetch attachments, wait
	// for new mail. It changes nothing, including flags.
	ScopeRead Scope = "read"
	// ScopeWrite changes the mailbox: flags, moves, drafts. Nothing leaves the
	// machine.
	ScopeWrite Scope = "write"
	// ScopeSend submits mail over SMTP.
	ScopeSend Scope = "send"
	// ScopeAdmin manages accounts, keys and webhooks.
	ScopeAdmin Scope = "admin"
)

func rank(s Scope) int {
	switch s {
	case ScopeRead:
		return 1
	case ScopeWrite:
		return 2
	case ScopeSend:
		return 3
	case ScopeAdmin:
		return 4
	}
	return 0
}

// Covers reports whether s grants everything want grants.
func (s Scope) Covers(want Scope) bool {
	r := rank(s)
	return r > 0 && r >= rank(want)
}

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return rank(s) > 0 }

// ParseScope converts a string, rejecting anything unknown. A typo must not
// quietly become a scope that grants nothing — or, worse, everything.
func ParseScope(s string) (Scope, error) {
	scope := Scope(s)
	if !scope.Valid() {
		return "", fmt.Errorf("auth: unknown scope %q (want read, write, send or admin)", s)
	}
	return scope, nil
}

// Scopes lists the scopes in order, for help text.
func Scopes() []Scope { return []Scope{ScopeRead, ScopeWrite, ScopeSend, ScopeAdmin} }
