package api_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// lendingEngine is the sync engine as the read routes use it: it lends each
// account's interactive connection, here opened on a FakeMailbox the test
// holds.
type lendingEngine struct {
	*engine

	mu    sync.Mutex
	boxes map[string]*providertest.FakeMailbox
}

var _ service.InteractiveRunner = (*lendingEngine)(nil)

func newLendingEngine() *lendingEngine {
	return &lendingEngine{engine: newEngine(), boxes: map[string]*providertest.FakeMailbox{}}
}

func (e *lendingEngine) Interactive(ctx context.Context, id string, fn func(context.Context, provider.Session) error) error {
	e.mu.Lock()
	box, ok := e.boxes[id]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: no mail server for %s in this test", provider.ErrConnClosed, id)
	}
	sess, err := box.Open(ctx, provider.RoleInteractive)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	return fn(ctx, sess)
}

// fakeMailbox registers an active mailbox owned by ownerID ("" for an
// instance mailbox, switched on by the operator) on a FakeMailbox, and
// returns its id and the fake.
func (h *harness) fakeMailbox(t *testing.T, e *lendingEngine, id, ownerID, email string) *providertest.FakeMailbox {
	t.Helper()
	if _, err := account.NewRepository(h.store, nil).Create(t.Context(), account.Account{
		ID: id, Email: email, Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.example", IMAPPort: 993, SMTPHost: "smtp.mail.example", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: email, State: account.StateActive,
	}, ownerID); err != nil {
		t.Fatal(err)
	}
	if ownerID == "" {
		if _, err := h.store.SetMailboxSync(t.Context(), id, true, "cli", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	e.mu.Lock()
	e.boxes[id] = box
	e.mu.Unlock()
	return box
}

// messageRow is the local id of the row indexed for a UID.
func (h *harness) messageRow(t *testing.T, accountID string, uid imap.UID) int64 {
	t.Helper()
	var id int64
	if err := h.store.Reader().QueryRowContext(t.Context(),
		`SELECT id FROM messages WHERE account_id = ? AND uid = ?`, accountID, uint32(uid)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// attached is a message with a plain body and three attachments: a PDF
// named in UTF-8, an HTML page and an SVG.
var attached = strings.ReplaceAll(`From: Bea Lima <bea@example.org>
To: Ana <ana@example.org>
Subject: Files
Message-ID: <files-1@example.org>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain; charset=utf-8

Here they are.
--b
Content-Type: application/pdf
Content-Disposition: attachment; filename*=utf-8''Relat%C3%B3rio%20%22final%22.pdf
Content-Transfer-Encoding: base64

`+base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 files"))+`
--b
Content-Type: text/html; name="page.html"
Content-Disposition: attachment; filename="page.html"

<script>alert(document.cookie)</script>
--b
Content-Type: image/svg+xml; name="logo.svg"
Content-Disposition: inline; filename="logo.svg"

<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>
--b--
`, "\n", "\r\n")

func readingHarness(t *testing.T) (*harness, *lendingEngine, string, int64) {
	t.Helper()
	e := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: e})
	const id = "acc_00000000000000a1"
	box := h.fakeMailbox(t, e, id, "", "ana@example.org")
	uid := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "files-1@example.org", Subject: "Files", From: "Bea Lima <bea@example.org>",
		To: []string{"ana@example.org"}, Raw: []byte(attached),
	})
	storetest.IndexMailbox(t, h.store, id, box)
	return h, e, id, h.messageRow(t, id, uid)
}

func TestAnAttachmentIsSentAsAFileTheBrowserWillNotRender(t *testing.T) {
	h, _, _, msg := readingHarness(t)
	key := h.key(t, auth.ScopeRead)

	for _, c := range []struct {
		path, contentType, disposition, body string
	}{
		{"2", "application/pdf",
			`attachment; filename="Relat_rio _final_.pdf"; filename*=UTF-8''Relat%C3%B3rio%20_final_.pdf`, "%PDF-1.4 files"},
		{"3", "application/octet-stream", `attachment; filename="page.html"; filename*=UTF-8''page.html`,
			"<script>alert(document.cookie)</script>"},
		{"4", "application/octet-stream", `attachment; filename="logo.svg"; filename*=UTF-8''logo.svg`, ""},
	} {
		resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d/attachments/%s", msg, c.path), key, "")
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("part %s: %d %s", c.path, resp.StatusCode, body)
		}
		for header, want := range map[string]string{
			"Content-Type":            c.contentType,
			"Content-Disposition":     c.disposition,
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": "sandbox; default-src 'none'",
			"Cache-Control":           "no-store",
		} {
			if got := resp.Header.Get(header); got != want {
				t.Errorf("part %s: %s = %q, want %q", c.path, header, got, want)
			}
		}
		if c.body != "" && string(body) != c.body {
			t.Errorf("part %s = %q, want %q", c.path, body, c.body)
		}
	}
}

func TestTheOriginalIsServedAsAnAttachedMessage(t *testing.T) {
	h, _, _, msg := readingHarness(t)
	resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d/raw", msg), h.key(t, auth.ScopeRead), "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != attached {
		t.Fatalf("raw = %d, %d bytes", resp.StatusCode, len(body))
	}
	name := fmt.Sprintf("message-%d.eml", msg)
	for header, want := range map[string]string{
		"Content-Type":            "message/rfc822",
		"Content-Length":          strconv.Itoa(len(attached)),
		"Content-Disposition":     `attachment; filename="` + name + `"; filename*=UTF-8''` + name,
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'",
		"Cache-Control":           "no-store",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestAMessageIsReadAsJSONWithItsBody(t *testing.T) {
	h, _, _, msg := readingHarness(t)
	resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d?format=text&max_bytes=4", msg), h.key(t, auth.ScopeRead), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got service.Message
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Body.Text == nil || *got.Body.Text != "Here" || !got.Body.Truncated || got.Body.HTML != nil {
		t.Errorf("body = %+v", got.Body)
	}
	if len(got.Parts) != 4 || got.Parts[1].Filename != "Relatório _final_.pdf" {
		t.Errorf("parts = %+v", got.Parts)
	}
}

func TestReadRoutesRefuseWhatTheyDoNotUnderstand(t *testing.T) {
	h, _, id, msg := readingHarness(t)
	key := h.key(t, auth.ScopeRead)
	for _, c := range []struct {
		path string
		code int
	}{
		{"/v1/messages?color=red", http.StatusBadRequest},
		{"/v1/messages?unseen=maybe", http.StatusBadRequest},
		{"/v1/messages?limit=0", http.StatusBadRequest},
		{"/v1/messages?limit=101", http.StatusBadRequest},
		{"/v1/messages?folder=inbox", http.StatusBadRequest},
		{"/v1/messages?cursor=%21%21", http.StatusBadRequest},
		{"/v1/messages?since=last-week", http.StatusBadRequest},
		{"/v1/messages?q=" + url.QueryEscape(strings.Repeat("word ", 20)), http.StatusBadRequest},
		{"/v1/messages?q=a&q=b", http.StatusBadRequest},
		{"/v1/messages?account=acc_0000000000000000", http.StatusNotFound},
		{"/v1/messages?folder=999", http.StatusNotFound},
		{fmt.Sprintf("/v1/messages/%d?format=pdf", msg), http.StatusBadRequest},
		{fmt.Sprintf("/v1/messages/%d?max_bytes=0", msg), http.StatusBadRequest},
		{fmt.Sprintf("/v1/messages/%d?max_bytes=%d", msg, service.MaxBodyBytes+1), http.StatusBadRequest},
		{fmt.Sprintf("/v1/messages/%d/raw?x=1", msg), http.StatusBadRequest},
		{fmt.Sprintf("/v1/messages/%d/attachments/..%%2F2", msg), http.StatusBadRequest},
		{fmt.Sprintf("/v1/messages/%d/attachments/9", msg), http.StatusNotFound},
		{"/v1/messages/abc", http.StatusNotFound},
		{"/v1/messages/-1/raw", http.StatusNotFound},
		{"/v1/messages/99999", http.StatusNotFound},
	} {
		resp := h.do(t, http.MethodGet, c.path, key, "")
		if resp.StatusCode != c.code {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("GET %s = %d %s, want %d", c.path, resp.StatusCode, body, c.code)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s answered its error as %q", c.path, ct)
		}
	}
	resp := h.do(t, http.MethodGet, "/v1/messages?account="+id+"&unseen=true&has_attachments=1&q=fil", key, "")
	var page service.MessagePage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil || len(page.Messages) != 1 {
		t.Fatalf("search = %d %+v %v", resp.StatusCode, page, err)
	}
}

func TestAPersonCannotDownloadAnotherPersonsMail(t *testing.T) {
	// The refusal is the one a message that does not exist gets, on every
	// read route: nobody learns which ids are somebody's mail.
	h, _, _, msg := readingHarness(t)
	bea := h.person(t, "bea@example.com", auth.RoleMember)
	for _, path := range []string{
		fmt.Sprintf("/v1/messages/%d", msg), fmt.Sprintf("/v1/messages/%d/raw", msg),
		fmt.Sprintf("/v1/messages/%d/attachments/2", msg),
	} {
		resp := h.do(t, http.MethodGet, path, bea, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
			continue
		}
		if code, message := decodeError(t, resp); code != "not_found" || message != "no such message" {
			t.Errorf("GET %s = %s %q", path, code, message)
		}
	}
	resp := h.do(t, http.MethodGet, "/v1/messages", bea, "")
	var page service.MessagePage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil || len(page.Messages) != 0 {
		t.Errorf("bea's search = %+v, %v", page, err)
	}
}

func TestARevokedKeyGetsNoDownload(t *testing.T) {
	h, _, _, msg := readingHarness(t)
	secret, key, err := h.keys.Issue(t.Context(), auth.NewKeyRequest{Name: "t", Scope: auth.ScopeRead})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.keys.Revoke(t.Context(), key.Prefix); err != nil {
		t.Fatal(err)
	}
	resp := h.do(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d/raw", msg), secret, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked key downloaded with %d", resp.StatusCode)
	}
}
