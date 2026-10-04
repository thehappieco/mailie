package service

import (
	"io"
	"sync"
	"time"
)

// Downloads in flight.
//
// An original or an attachment is fetched whole into the provider's spool
// before its first byte is sent, and the spooled file lives until the
// response ends — which is up to the client. Unbounded, a caller that opens
// many downloads and never reads them parks each one on the disk that also
// holds the database and its write-ahead log, until writes fail for everyone
// and sync stops. So a download reserves its size against a budget for the
// whole daemon, and one of a few places per caller, before anything is
// fetched; closing what it returned gives both back. REST and MCP share it
// because both come through here.
const (
	// DefaultDownloadSpoolBytes is the daemon-wide budget when none is
	// configured (MAIL_DOWNLOAD_SPOOL_MAX_BYTES).
	DefaultDownloadSpoolBytes = 1 << 30
	// DefaultDownloadsPerCaller is how many downloads one person — every
	// session and key they hold — or one instance key may have in flight.
	DefaultDownloadsPerCaller = 4
	// downloadRetryAfter is how long a refused download is told to wait: a
	// download that is being read ends in seconds.
	downloadRetryAfter = 5 * time.Second
)

// downloadBudget is the bound above. The zero value is not usable; see
// newDownloadBudget.
type downloadBudget struct {
	maxBytes  int64
	perCaller int
	// busyCaller and busyServer are what a refusal says: the caller has too
	// many in flight, or everyone together has too much.
	busyCaller, busyServer string

	mu      sync.Mutex
	held    int64
	callers map[string]int
}

func newDownloadBudget(maxBytes int64, perCaller int) *downloadBudget {
	if maxBytes <= 0 {
		maxBytes = DefaultDownloadSpoolBytes
	}
	if perCaller <= 0 {
		perCaller = DefaultDownloadsPerCaller
	}
	return &downloadBudget{
		maxBytes: maxBytes, perCaller: perCaller, callers: map[string]int{},
		busyCaller: "too many downloads are in progress for this caller; try again when one finishes",
		busyServer: "the server is busy with other downloads; try again shortly",
	}
}

// newSendBudget bounds what sends in flight hold in the spool: their
// attachments, uploaded and forwarded, from the moment they arrive, and the
// message as the adapter writes it out for the wire, until the send ends.
func newSendBudget(maxBytes int64) *downloadBudget {
	b := newDownloadBudget(maxBytes, sendsPerCaller)
	if maxBytes <= 0 {
		b.maxBytes = DefaultSendSpoolBytes
	}
	b.busyCaller = "too many sends are in progress for this caller; try again when one finishes"
	b.busyServer = "the server is busy with other sends; try again shortly"
	return b
}

// reserve holds size bytes and one of the caller's places, or refuses with
// CodeRateLimited and a Retry-After. release gives both back and may be
// called any number of times.
//
// A download larger than the whole budget is admitted when nothing else is
// held, so a budget set below the largest download still serves it, one at
// a time.
func (b *downloadBudget) reserve(caller string, size int64) (release func(), err error) {
	h, err := b.take(caller, size)
	if err != nil {
		return nil, err
	}
	return h.release, nil
}

// take is reserve, for a caller that learns later how much it really needs
// and resizes what it holds.
func (b *downloadBudget) take(caller string, size int64) (*budgetHold, error) {
	size = max(size, 0)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.callers[caller] >= b.perCaller {
		return nil, Retryable(b.busyCaller, downloadRetryAfter, nil)
	}
	if b.held > 0 && b.held+size > b.maxBytes {
		return nil, Retryable(b.busyServer, downloadRetryAfter, nil)
	}
	b.held += size
	b.callers[caller]++
	return &budgetHold{b: b, caller: caller, size: size}, nil
}

// budgetHold is one reservation.
type budgetHold struct {
	b      *downloadBudget
	caller string
	// size is guarded by b.mu.
	size     int64
	released bool
}

// resize makes the reservation hold size instead: less always, more only
// when the budget has room for it, as a new reservation would.
func (h *budgetHold) resize(size int64) error {
	size = max(size, 0)
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if h.released {
		return nil
	}
	others := h.b.held - h.size
	if size > h.size && others > 0 && others+size > h.b.maxBytes {
		return Retryable(h.b.busyServer, downloadRetryAfter, nil)
	}
	h.b.held = others + size
	h.size = size
	return nil
}

// release gives the reservation back. Calling it again does nothing.
func (h *budgetHold) release() {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	h.b.held -= h.size
	if h.b.callers[h.caller]--; h.b.callers[h.caller] <= 0 {
		delete(h.b.callers, h.caller)
	}
}

// downloadCaller is whose places a download takes: the person's, whichever
// of their sessions or keys asks, or the instance key's own.
func downloadCaller(p Principal) string {
	if p.UserID != "" {
		return "user:" + p.UserID
	}
	return "key:" + p.KeyPrefix
}

// releasing is a download's body that gives its reservation back once it is
// closed.
type releasing struct {
	io.ReadCloser
	release func()
}

func (r *releasing) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}
