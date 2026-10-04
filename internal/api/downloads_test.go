package api_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// bigOriginal is a plain message of about 16 MiB: far more than a loopback
// connection buffers, so a client that stops reading holds the server's
// write.
var bigOriginal = "From: Bea <bea@example.org>\r\nTo: Ana <ana@example.org>\r\nSubject: Scans\r\n" +
	"Message-ID: <scans-1@example.org>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n" +
	strings.Repeat(strings.Repeat("x", 76)+"\r\n", (16<<20)/78)

// downloadFixture is a harness with one mailbox holding bigOriginal, and a
// read key.
type downloadFixture struct {
	*harness
	key string
	raw string // the original's path
}

func newDownloadFixture(t *testing.T, adjust func(*api.Handler), perCaller int) *downloadFixture {
	t.Helper()
	e := newLendingEngine()
	h := newHarnessWith(t, adjust, serviceOptions{sync: e, downloadsPerCaller: perCaller})
	const id = "acc_00000000000000d1"
	box := h.fakeMailbox(t, e, id, "", "ana@example.org")
	uid := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "scans-1@example.org", Subject: "Scans", From: "Bea <bea@example.org>",
		To: []string{"Ana <ana@example.org>"}, Raw: []byte(bigOriginal),
	})
	storetest.IndexMailbox(t, h.store, id, box)
	return &downloadFixture{
		harness: h, key: h.key(t, auth.ScopeRead),
		raw: fmt.Sprintf("/v1/messages/%d/raw", h.messageRow(t, id, uid)),
	}
}

// stalled starts a download whose client reads the status line and then
// stops reading, and returns its connection.
func (f *downloadFixture) stalled(t *testing.T) net.Conn {
	t.Helper()
	u, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: mailie.test\r\nAuthorization: Bearer %s\r\n\r\n",
		f.raw, f.key); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	status, err := bufio.NewReaderSize(conn, 64).ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("the stalled download began %q, %v", status, err)
	}
	return conn
}

// download asks for the original and reads all of it, returning the status
// and, for a refusal, its code and Retry-After.
func (f *downloadFixture) download(t *testing.T) (status int, code, retryAfter string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+f.raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil || n != int64(len(bigOriginal)) {
			t.Fatalf("the download sent %d of %d bytes: %v", n, len(bigOriginal), err)
		}
		return resp.StatusCode, "", ""
	}
	var body struct{ Code, Message string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Message, "downloads") {
		t.Fatalf("refused by something other than the download bound: %d %+v", resp.StatusCode, body)
	}
	return resp.StatusCode, body.Code, resp.Header.Get("Retry-After")
}

// eventually polls download until it is answered with want.
func (f *downloadFixture) eventually(t *testing.T, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		status, _, _ := f.download(t)
		if status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d after %s, want %d", status, within, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDownloadsBeyondTheCallersShareAreRefusedUntilOneEnds(t *testing.T) {
	// Each download is spooled whole on the daemon's disk and kept until its
	// client has it. A caller that opens downloads and never reads them is
	// refused more, with a time to come back, rather than filling the disk
	// the database lives on.
	f := newDownloadFixture(t, nil, 2)
	first := f.stalled(t)
	f.stalled(t)

	status, code, retryAfter := f.download(t)
	if status != http.StatusTooManyRequests || code != "rate_limited" || retryAfter == "" {
		t.Fatalf("a third download = %d %q Retry-After %q, want 429 rate_limited with a Retry-After",
			status, code, retryAfter)
	}

	// The client of one goes away: its file is removed and its place freed.
	_ = first.Close()
	f.eventually(t, http.StatusOK, 15*time.Second)
}

func TestADownloadWhoseClientStopsReadingIsAbandoned(t *testing.T) {
	// A client that stops reading must not hold a spooled file, and a place
	// among the downloads, for the route's ten minutes: each write has to go
	// through within the stall allowance of the last.
	f := newDownloadFixture(t, func(h *api.Handler) { h.DownloadStall = 2 * time.Second }, 1)
	f.stalled(t) // and never closed until the test ends

	if status, _, _ := f.download(t); status != http.StatusTooManyRequests {
		t.Fatalf("a second download while the first is held = %d, want 429", status)
	}
	f.eventually(t, http.StatusOK, 15*time.Second)
}
