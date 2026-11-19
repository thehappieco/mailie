package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/service/issuertest"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A key an issuer makes, over REST: what holds a request open — an event
// stream, a long poll, a download — ends within a second of the record or
// the stop that takes the key's access away.

const (
	keysDirectory  = "https://directory.example.org"
	keysGeneration = "generation-1"
)

// issuedHarness is a daemon whose workspaces come from a directory, whose
// issuer, the fake one, makes its keys: Ana, followed, with a mailbox of her
// personal workspace holding a large original and a large attachment, and a
// key of hers that reads it.
type issuedHarness struct {
	*harness
	svc        *service.Service
	ana        string // her session
	anaID      string
	box        string
	fake       *providertest.FakeMailbox
	raw        string // the original's path
	attachment string // the attachment's path
	message    string // the path of the message that carries the attachment
	key        string // the issued key's credential
	operator   string // an unrestricted instance admin key
	selector   string
	hash       string
	created    time.Time
	// expires is the key's expiry; zero is 90 days after its creation.
	expires time.Time
	version int64
	cursor  int64
}

// bigAttachment is a message with one attachment of about 12 MiB, base64.
func bigAttachment() string {
	data := bytes.Repeat([]byte("0123456789abcdef"), (12<<20)/16)
	encoded := base64.StdEncoding.EncodeToString(data)
	var lines strings.Builder
	for len(encoded) > 76 {
		lines.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	lines.WriteString(encoded + "\r\n")
	return "From: Bea <bea@example.org>\r\nTo: Ana <ana@mail.example>\r\nSubject: Data\r\n" +
		"Message-ID: <data-1@example.org>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nThe data.\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=data.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + lines.String() + "--b--\r\n"
}

func newIssuedHarness(t *testing.T, adjust func(*api.Handler)) *issuedHarness {
	t.Helper()
	e := newLendingEngine()
	var svc *service.Service
	h := newHarnessWith(t, func(handler *api.Handler) {
		svc = handler.Service
		if adjust != nil {
			adjust(handler)
		}
	}, serviceOptions{sync: e, source: workspace.Platform(), keyIssuer: issuertest.NewFor(keysDirectory)})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	if _, err := h.store.Writer().ExecContext(t.Context(), `INSERT INTO user_identities(issuer, subject, user_id, created_at)
		VALUES (?, 'sub-ana', ?, 100)`, keysDirectory, ana.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyDirectoryDocument(t.Context(), service.DirectoryDocument{
		Issuer: keysDirectory, Subject: "sub-ana", Generation: keysGeneration, Version: 1, Status: service.DirectoryActive,
		Workspaces: []service.DirectoryWorkspace{{ID: "personal-ana", Kind: workspace.KindPersonal, Role: workspace.RoleOwner}},
	}, service.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	token := authtest.SignIn(t, h.users, "ana@example.com")
	if resp := h.do(t, http.MethodPost, "/v1/me/sync-consent", token,
		`{"version":"`+service.DefaultSyncConsentVersion+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("sync consent: %d", resp.StatusCode)
	}
	const id = "acc_00000000000000f1"
	box := h.fakeMailbox(t, e, id, ana.ID, "ana@mail.example")
	original := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "scans-1@example.org", Subject: "Scans", From: "Bea <bea@example.org>",
		To: []string{"Ana <ana@mail.example>"}, Raw: []byte(bigOriginal),
	})
	attached := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "data-1@example.org", Subject: "Data", From: "Bea <bea@example.org>",
		To: []string{"Ana <ana@mail.example>"}, Raw: []byte(bigAttachment()),
	})
	storetest.IndexMailbox(t, h.store, id, box)
	raw, selector, hash := issuertest.Credential(t)
	ih := &issuedHarness{
		harness: h, svc: svc, ana: token, anaID: ana.ID, box: id, fake: box, key: raw, selector: selector, hash: hash,
		created:    time.Now().Add(-time.Hour),
		raw:        fmt.Sprintf("/v1/messages/%d/raw", h.messageRow(t, id, original)),
		attachment: fmt.Sprintf("/v1/messages/%d/attachments/2", h.messageRow(t, id, attached)),
		message:    fmt.Sprintf("/v1/messages/%d", h.messageRow(t, id, attached)),
		operator:   h.key(t, auth.ScopeAdmin),
	}
	ih.record(t, false, ih.reads())
	return ih
}

// reads is the grant of read on the mailbox, by Ana.
func (h *issuedHarness) reads() service.IssuedGrant {
	return service.IssuedGrant{AccountID: h.box, Use: "read", GivenBy: "sub-ana", GivenAt: h.created}
}

// record applies the key's next record: revoked, or listing grants.
func (h *issuedHarness) record(t *testing.T, revoked bool, grants ...service.IssuedGrant) {
	t.Helper()
	if err := h.apply(t.Context(), revoked, grants...); err != nil {
		t.Fatal(err)
	}
}

// apply is record, for a goroutine that is not the test's.
func (h *issuedHarness) apply(ctx context.Context, revoked bool, grants ...service.IssuedGrant) error {
	h.version++
	h.cursor++
	expires := h.expires
	if expires.IsZero() {
		expires = h.created.Add(90 * 24 * time.Hour)
	}
	_, err := h.svc.ApplyIssuedKeys(ctx, service.IssuedKeysPage{
		Issuer: keysDirectory, Generation: keysGeneration, Cursor: h.cursor, Last: true,
		Keys: []service.IssuedKey{{
			ExternalID: "tok-1", Selector: h.selector, Hash: h.hash, WorkspaceID: "personal-ana", Creator: "sub-ana",
			Version: h.version, CreatedAt: h.created, ExpiresAt: expires,
			Terms: config.DefaultKeyTermsVersion, Revoked: revoked, Grants: grants,
		}},
	})
	return err
}

// stops are the ways the key's access is taken away, each measured.
var stops = map[string]func(t *testing.T, h *issuedHarness){
	"its revocation": func(t *testing.T, h *issuedHarness) { h.record(t, true) },
	"the operator's hold": func(t *testing.T, h *issuedHarness) {
		if resp := h.do(t, http.MethodPut, "/v1/issued-keys/"+h.selector+"/hold", h.harness.key(t, auth.ScopeAdmin), ""); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("holding: %d", resp.StatusCode)
		}
	},
	"read taken away": func(t *testing.T, h *issuedHarness) { h.record(t, false) },
}

func TestAStreamEndsWithinASecondOfItsKeysRevocation(t *testing.T) {
	for what, stop := range stops {
		t.Run(what, func(t *testing.T) {
			h := newIssuedHarness(t, nil)
			stream := h.openStream(t, "/v1/events?types=message.new&account="+h.box, h.key, "")
			stopped := time.Now()
			stop(t, h)
			// Read taken away says so first (event: access), then ends the
			// stream, which follows nothing else.
			f, ok := stream.next(t)
			if ok && f.event == "access" {
				f, ok = stream.next(t)
			}
			if !ok || f.event != "error" {
				t.Fatalf("after %s the stream sent %+v", what, f)
			}
			if took := time.Since(stopped); took > time.Second {
				t.Errorf("the stream ended %s after %s, past a second", took, what)
			}
		})
	}
}

func TestALongPollEndsWithinASecondOfItsKeysRevocation(t *testing.T) {
	h := newIssuedHarness(t, nil)
	answered := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+"/v1/events/wait?timeout=30", nil)
		req.Header.Set("Authorization", "Bearer "+h.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- nil
			return
		}
		answered <- resp
	}()
	time.Sleep(200 * time.Millisecond)
	revoked := time.Now()
	h.record(t, true)
	select {
	case resp := <-answered:
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("the long poll answered %+v", resp)
		}
		_ = resp.Body.Close()
		if took := time.Since(revoked); took > time.Second {
			t.Errorf("the long poll ended %s after the revocation", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long poll went on after its key's revocation")
	}
}

// slowly reads a download, a little at a time, calling stop once the first
// bytes came, and reports how much it read, whether it ended in error, and
// how long after stop it ended.
func (h *issuedHarness) slowly(t *testing.T, path string, stop func()) (int, time.Duration, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the download began %d", resp.StatusCode)
	}
	buf := make([]byte, 32<<10)
	read := 0
	var stopped time.Time
	for {
		n, err := resp.Body.Read(buf)
		read += n
		if stopped.IsZero() && read > 0 {
			stop()
			stopped = time.Now()
		}
		if err == io.EOF {
			return read, time.Since(stopped), nil
		}
		if err != nil {
			return read, time.Since(stopped), err
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestADownloadStopsWithinASecondOfItsKeysRevocation(t *testing.T) {
	for what, stop := range stops {
		for name, path := range map[string]func(*issuedHarness) string{
			"an original":   func(h *issuedHarness) string { return h.raw },
			"an attachment": func(h *issuedHarness) string { return h.attachment },
		} {
			t.Run(name+", "+what, func(t *testing.T) {
				h := newIssuedHarness(t, nil)
				read, took, err := h.slowly(t, path(h), func() { stop(t, h) })
				if err == nil {
					t.Fatalf("the download ended as whole after %s, %d bytes", what, read)
				}
				if read >= len(bigOriginal) {
					t.Errorf("the whole file was sent: %d bytes", read)
				}
				if took > time.Second+500*time.Millisecond {
					t.Errorf("the download ended %s after %s", took, what)
				}
			})
		}
	}
}

// takers are the ways the key's access is taken away from another
// goroutine than the test's, while a request of the key runs.
var takers = map[string]func(ctx context.Context, h *issuedHarness) error{
	"its revocation":  func(ctx context.Context, h *issuedHarness) error { return h.apply(ctx, true) },
	"read taken away": func(ctx context.Context, h *issuedHarness) error { return h.apply(ctx, false) },
	"the operator's hold": func(ctx context.Context, h *issuedHarness) error {
		return h.send(ctx, http.MethodPut, "/v1/issued-keys/"+h.selector+"/hold", h.operator, http.StatusNoContent)
	},
	"its mailbox removed": func(ctx context.Context, h *issuedHarness) error {
		return h.send(ctx, http.MethodDelete, "/v1/accounts/"+h.box+"?confirm="+h.box, h.ana, http.StatusOK)
	},
}

// send is a request whose answer must be want, for a goroutine that is not
// the test's.
func (h *issuedHarness) send(ctx context.Context, method, path, credential string, want int) error {
	req, err := http.NewRequestWithContext(ctx, method, h.server.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s answered %d", method, path, resp.StatusCode)
	}
	return nil
}

// whileFetched asks for path with the key, taking its access away (take)
// while the mail server is read for it, and answers what came back.
func (h *issuedHarness) whileFetched(t *testing.T, path string, take func(context.Context, *issuedHarness) error) (int, []byte) {
	t.Helper()
	var once sync.Once
	h.fake.OnCall(func(ctx context.Context, c providertest.Call) error {
		if c.Method == providertest.MethodFetchRaw || c.Method == providertest.MethodFetchPart {
			once.Do(func() {
				// The fetch's own context: it runs while this takes the
				// access away.
				if err := take(ctx, h); err != nil {
					t.Errorf("taking the key's access away: %v", err)
				}
			})
		}
		return nil
	})
	defer h.fake.OnCall(nil)
	resp := h.do(t, http.MethodGet, path, h.key, "")
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return resp.StatusCode, body
}

func TestNothingIsSentOfAMessageWhoseAccessGoesWhileTheMailServerIsRead(t *testing.T) {
	for what, take := range takers {
		for name, path := range map[string]func(*issuedHarness) string{
			"an original":      func(h *issuedHarness) string { return h.raw },
			"an attachment":    func(h *issuedHarness) string { return h.attachment },
			"a message's body": func(h *issuedHarness) string { return h.message },
		} {
			t.Run(name+", "+what, func(t *testing.T) {
				h := newIssuedHarness(t, nil)
				status, body := h.whileFetched(t, path(h), take)
				if status == http.StatusOK || len(body) > 512 {
					t.Fatalf("after %s while the mail server was read: %d, %d bytes", what, status, len(body))
				}
				for _, content := range []string{"xxxxxxxx", "0123456789abcdef", "The data."} {
					if strings.Contains(string(body), content) {
						t.Errorf("the answer carried the message: %.200s", body)
					}
				}
			})
		}
	}
}

func TestADownloadStopsAtItsKeysExpiry(t *testing.T) {
	for name, path := range map[string]func(*issuedHarness) string{
		"an original":   func(h *issuedHarness) string { return h.raw },
		"an attachment": func(h *issuedHarness) string { return h.attachment },
	} {
		t.Run(name, func(t *testing.T) {
			h := newIssuedHarness(t, nil)
			// A record brings its expiry a second or two ahead: the key still
			// works when the download begins, and ends while it streams, at
			// the moment the daemon records it (EndExpiredIssuedKeys).
			h.expires = time.Now().Add(2 * time.Second).Truncate(time.Second)
			h.record(t, false, h.reads())
			read, took, err := h.slowly(t, path(h), func() {
				time.Sleep(time.Until(h.expires) + 50*time.Millisecond)
				if _, err := h.svc.EndExpiredIssuedKeys(t.Context(), time.Now()); err != nil {
					t.Error(err)
				}
			})
			if err == nil {
				t.Fatalf("the download ended as whole after its key's expiry, %d bytes", read)
			}
			if took > time.Second+500*time.Millisecond {
				t.Errorf("the download ended %s after its key's expiry", took)
			}
		})
	}
}

func TestALongPollOfAKeyRevokedSinceItAuthenticatedEndsAtOnce(t *testing.T) {
	h := newIssuedHarness(t, nil)
	p, err := h.svc.Authenticate(t.Context(), h.key, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Revoked between the transport's authentication and the wait: no
	// commit comes after the wait began to wake it.
	h.record(t, true)
	began := time.Now()
	_, err = h.svc.WaitForNewMail(t.Context(), p, 0, 3*time.Second, service.EventFilter{})
	if service.CodeOf(err) != service.CodeUnauthorized {
		t.Fatalf("the long poll of a revoked key answered %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Errorf("the long poll ended %s after it began", took)
	}
}

// TestAStreamOrALongPollEndsWithinASecondHoweverCloseItsKeysHoldComes holds
// the key at moments around when a stream or a long poll is checked and
// begins to wait: whichever comes first, it ends within a second, never at
// its next ping or its timeout.
func TestAStreamOrALongPollEndsWithinASecondHoweverCloseItsKeysHoldComes(t *testing.T) {
	h := newIssuedHarness(t, func(handler *api.Handler) { handler.EventPing = 10 * time.Second })
	operator, err := h.svc.Authenticate(t.Context(), h.operator, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/events?types=message.new", "/v1/events/wait?timeout=10"} {
		for i := range 300 {
			ended := make(chan time.Time, 1)
			sent := make(chan struct{})
			go func() {
				defer func() { ended <- time.Now() }()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+path, nil)
				if err != nil {
					close(sent)
					return
				}
				req.Header.Set("Authorization", "Bearer "+h.key)
				close(sent)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return
				}
				// A stream ends with its error event; a long poll, with its
				// refusal: either way, its body ends.
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
			<-sent
			for at := time.Now().Add(time.Duration(i%150) * 20 * time.Microsecond); time.Now().Before(at); {
				runtime.Gosched()
			}
			if err := h.svc.HoldIssuedKey(t.Context(), operator, h.selector); err != nil {
				t.Fatal(err)
			}
			held := time.Now()
			select {
			case at := <-ended:
				if took := at.Sub(held); took > time.Second {
					t.Errorf("%s, try %d: it ended %s after the hold", path, i, took)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("%s, try %d: it went on after the hold", path, i)
			}
			if err := h.svc.ReleaseIssuedKey(t.Context(), operator, h.selector); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestADownloadOfAKeyStillReadingIsSentWhole(t *testing.T) {
	h := newIssuedHarness(t, nil)
	// A change that may have changed someone's access, which is not this
	// key's (a person resumed who was never suspended): the download is
	// checked again, and goes on.
	changed := h.svc.AccessChanged()
	read, _, err := h.slowly(t, h.raw, func() {
		if _, err := h.svc.ResumePerson(t.Context(), h.anaID); err != nil {
			t.Error(err)
		}
		select {
		case <-changed:
		default:
			t.Error("the change was not announced")
		}
	})
	if err != nil || read != len(bigOriginal) {
		t.Errorf("read %d of %d: %v", read, len(bigOriginal), err)
	}
}

func TestTheHoldRoutesAreTheOperatorsAlone(t *testing.T) {
	h := newIssuedHarness(t, nil)
	operator := h.harness.key(t, auth.ScopeAdmin)
	path := "/v1/issued-keys/" + h.selector + "/hold"
	for what, credential := range map[string]string{
		"a person signed in": h.ana, "a read instance key": h.harness.key(t, auth.ScopeRead), "the key itself": h.key,
	} {
		if resp := h.do(t, http.MethodPut, path, credential, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s held the key: %d", what, resp.StatusCode)
		}
	}
	if resp := h.do(t, http.MethodPut, path, operator, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the operator's hold: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/accounts", h.key, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a held key: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodDelete, path, operator, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the operator's release: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodGet, "/v1/accounts", h.key, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("a released key: %d", resp.StatusCode)
	}
	if resp := h.do(t, http.MethodPut, "/v1/issued-keys/ffffffffffffffff/hold", operator, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a selector not held: %d", resp.StatusCode)
	}
}
