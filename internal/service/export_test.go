package service

import "time"

// SetSendRetryForTest sets how long a send waits before each new attempt
// when the server says to try later: tests cannot wait the real 55 s.
func SetSendRetryForTest(s *Service, waits ...time.Duration) { s.sendRetry = waits }
