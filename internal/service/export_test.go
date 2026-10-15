package service

import (
	"context"
	"time"
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
