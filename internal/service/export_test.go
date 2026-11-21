package service

import (
	"context"
	"errors"
	"time"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// SetSendRetryForTest sets how long a send waits before each new attempt
// when the server says to try later: tests cannot wait the real 55 s.
func SetSendRetryForTest(s *Service, waits ...time.Duration) { s.sendRetry = waits }

// SignInExternalMeanwhileForTest is SignInExternal with meanwhile run
// between the write that applies an existing person's document and the
// sign-in's own: what a test needs to reach that moment.
func SignInExternalMeanwhileForTest(ctx context.Context, s *Service, in ExternalSignIn, meanwhile func()) (ExternalSignedIn, error) {
	return s.signInExternal(ctx, in, meanwhile)
}

// LinkErrorForTest is what a link at the provider's start or return answers
// for err, as the registry returned it.
func LinkErrorForTest(err error) error { return fromLink(err) }

// RunForTest runs do as one act of acts' request (PersonActs.run), with the
// request its write records: what a test needs to reach the moment after a
// commit.
func RunForTest(ctx context.Context, acts *PersonActs, act, accountID string,
	do func(req *store.Request) (store.RequestOutcome, error),
) (ActOutcome, error) {
	return acts.run(ctx, act, accountID, false, do)
}

// KeySidesForTest calls, as p, every branch of a use case whose other side
// is a key's or a session's own: a sealed grant given, a link's key, a
// send's owner, a consent's flows, and the guard of what needs a session.
func KeySidesForTest(ctx context.Context, s *Service, p Principal, accountID, userID string) map[string]error {
	read := true
	_, _, grant := s.setAccess(ctx, p, accountID, userID, GrantRequest{Read: &read, Act: new(bool), Send: new(bool),
		Manage: new(bool), Grant: "sealed", PublicKey: "key"}, nil)
	_, link := s.checkAddRequest(p, AddAccountRequest{Email: "box@mail.example", Provider: "imap", Password: "secret",
		IMAPHost: "imap.mail.example", SMTPHost: "smtp.mail.example"})
	out := map[string]error{"a sealed grant": grant, "a link's key": link, "a session's guard": requireSession(p)}
	if ownsSend(p, p.UserID, p.Actor()) {
		out["a send's owner"] = errors.New("owns a send")
	}
	if flows := s.flowsFor(p, provider.KindGmail); len(flows) > 0 {
		out["a consent's flows"] = errors.New("starts a consent")
	}
	return out
}
