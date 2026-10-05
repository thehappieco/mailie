package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// file is an attachment part of a send request.
type file struct {
	name, contentType string
	body              []byte
}

// sendBody is a send request's multipart body: the compose part, then each
// file as an attachment part.
func sendBody(t *testing.T, compose string, files ...file) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	part, err := w.CreateFormField("compose")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(compose)); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="attachment"; filename=%q`, f.name))
		h.Set("Content-Type", f.contentType)
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body, w.FormDataContentType()
}

// send posts a send request with an Idempotency-Key, when key is not empty.
func (h *harness) send(t *testing.T, token, key, compose string, files ...file) *http.Response {
	t.Helper()
	body, contentType := sendBody(t, compose, files...)
	return h.post(t, token, key, contentType, body)
}

func (h *harness) post(t *testing.T, token, key, contentType string, body *bytes.Buffer) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages/send", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// sendingMailbox is fakeMailbox whose submission goes to smtp, with a
// password stored for it.
func (h *harness) sendingMailbox(t *testing.T, e *lendingEngine, id, ownerID, email string,
	smtp *providertest.SMTPServer,
) *providertest.FakeMailbox {
	t.Helper()
	box := h.fakeMailbox(t, e, id, ownerID, email)
	h.submitTo(t, id, smtp)
	return box
}

// submitTo points an account's submission at smtp and stores a password for
// it.
func (h *harness) submitTo(t *testing.T, id string, smtp *providertest.SMTPServer) {
	t.Helper()
	if _, err := h.store.Writer().ExecContext(t.Context(),
		`UPDATE accounts SET smtp_host = ?, smtp_port = ?, smtp_tls = 'starttls' WHERE id = ?`,
		smtp.Host, smtp.Port, id); err != nil {
		t.Fatal(err)
	}
	if err := account.NewRepository(h.store, testKeyring(t)).SavePassword(t.Context(), id, "hunter2"); err != nil {
		t.Fatal(err)
	}
}

func signIn(t *testing.T, h *harness, email string) string {
	t.Helper()
	resp := h.do(t, http.MethodPost, "/v1/auth/login", "",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, authtest.Password))
	var session struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil || session.Token == "" {
		t.Fatalf("sign in: %d %v", resp.StatusCode, err)
	}
	return session.Token
}

func TestSendWithoutConfirmIsRefused(t *testing.T) {
	// The request is the person's confirmation that this message goes out.
	// Without it, nothing is read, reserved or dialed — and the refusal is
	// the same for everyone allowed to send.
	engine := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: engine})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	token := signIn(t, h, "ana@example.com")
	smtp := providertest.NewSMTPServer(t)
	const id = "acc_00000000000000f1"
	h.sendingMailbox(t, engine, id, ana.ID, "ana@mail.example", smtp)
	if resp := h.do(t, http.MethodPost, "/v1/me/send-consent", token,
		`{"version":"`+service.DefaultSendConsentVersion+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("consent: %d", resp.StatusCode)
	}

	for _, compose := range []string{
		`{"account_id":"` + id + `","to":[{"email":"bea@example.org"}],"subject":"Hi","text":"Hello"}`,
		`{"account_id":"` + id + `","to":[{"email":"bea@example.org"}],"subject":"Hi","text":"Hello","confirm":false}`,
	} {
		resp := h.send(t, token, "b6e3c1a2-5d0f-4b8e-9c1a-2f3e4d5c6b7a", compose,
			file{"notes.txt", "text/plain", []byte("an attachment")})
		code, message := decodeError(t, resp)
		if resp.StatusCode != http.StatusBadRequest || code != "bad_request" || !strings.Contains(message, "confirm=true") {
			t.Errorf("a send without confirm answered %d %s %q", resp.StatusCode, code, message)
		}
	}
	if n := smtp.DataCommands(); n != 0 || len(smtp.Auths()) != 0 {
		t.Fatalf("the submission server saw %d DATA and AUTH %v", n, smtp.Auths())
	}
	var sends int
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT count(*) FROM sends`).Scan(&sends); err != nil || sends != 0 {
		t.Fatalf("%d send records (%v); a refused send reserves nothing", sends, err)
	}

	// With it, the same request sends.
	resp := h.send(t, token, "b6e3c1a2-5d0f-4b8e-9c1a-2f3e4d5c6b7a",
		`{"account_id":"`+id+`","to":[{"email":"bea@example.org"}],"subject":"Hi","text":"Hello","confirm":true}`)
	var res service.SendResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil || resp.StatusCode != http.StatusOK || res.State != "sent" {
		t.Fatalf("with confirm: %d %+v %v", resp.StatusCode, res, err)
	}
	if n := len(smtp.Messages()); n != 1 {
		t.Fatalf("the server took %d messages, want 1", n)
	}
}

func TestTheSendRoutesReadMultipartAndAnswerOnlyWhoMaySend(t *testing.T) {
	engine := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{sync: engine})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleMember)
	authtest.NewUser(t, h.store, "bob@example.com", auth.RoleMember)
	token, bobs := signIn(t, h, "ana@example.com"), signIn(t, h, "bob@example.com")
	smtp := providertest.NewSMTPServer(t)
	const id = "acc_00000000000000f2"
	h.sendingMailbox(t, engine, id, ana.ID, "ana@mail.example", smtp)
	compose := `{"account_id":"` + id + `","to":[{"email":"bea@example.org"}],"subject":"Hi","text":"Hello","confirm":true}`

	// The consent: read by the person, given and withdrawn only signed in.
	instance := h.key(t, auth.ScopeSend)
	for _, c := range []struct {
		method, token, body string
		want                int
	}{
		{http.MethodGet, instance, "", http.StatusForbidden},
		{http.MethodPost, instance, `{"version":"` + service.DefaultSendConsentVersion + `"}`, http.StatusForbidden},
		{http.MethodPost, token, `{"version":"2026-01-sending"}`, http.StatusBadRequest},
		{http.MethodPost, token, `{"version":"` + service.DefaultSendConsentVersion + `","extra":1}`, http.StatusBadRequest},
		{http.MethodPost, token, `{"version":"` + service.DefaultSendConsentVersion + `"}`, http.StatusOK},
	} {
		if resp := h.do(t, c.method, "/v1/me/send-consent", c.token, c.body); resp.StatusCode != c.want {
			t.Errorf("%s /v1/me/send-consent %s: %d, want %d", c.method, c.body, resp.StatusCode, c.want)
		}
	}

	// The body: multipart, compose first, one object, known fields.
	for name, c := range map[string]struct {
		contentType string
		body        string
	}{
		"json":             {"application/json", compose},
		"attachment first": multipartOf(t, [2]string{"attachment", compose}),
		"unknown field":    multipartOf(t, [2]string{"compose", strings.Replace(compose, `"confirm"`, `"confirmed":true,"confirm"`, 1)}),
		"two objects":      multipartOf(t, [2]string{"compose", compose + compose}),
		"another part":     multipartOf(t, [2]string{"compose", compose}, [2]string{"notes", "x"}),
		"compose too big": multipartOf(t, [2]string{"compose", strings.Replace(compose, `"Hello"`,
			`"`+strings.Repeat("a", service.MaxTextBytes+300<<10)+`"`, 1)}),
	} {
		resp := h.post(t, token, "k-"+strings.ReplaceAll(name, " ", "-"), c.contentType, bytes.NewBufferString(c.body))
		if code, _ := decodeError(t, resp); resp.StatusCode != http.StatusBadRequest || code != "bad_request" {
			t.Errorf("%s: %d %s, want 400", name, resp.StatusCode, code)
		}
	}
	body, contentType := sendBody(t, compose)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages/send", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Add("Idempotency-Key", "one")
	req.Header.Add("Idempotency-Key", "two")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("two Idempotency-Key headers: %d, want 400", resp.StatusCode)
	}
	if n := smtp.DataCommands(); n != 0 {
		t.Fatalf("a refused request reached DATA %d times", n)
	}

	// One that sends, and its record.
	const key = "a0b1c2d3-e4f5-4a6b-8c7d-9e0f1a2b3c4d"
	if resp := h.send(t, token, key, compose); resp.StatusCode != http.StatusOK {
		t.Fatalf("send: %d", resp.StatusCode)
	}
	read := key + "?account=" + id
	for _, c := range []struct {
		name, path, token string
		want              int
	}{
		{"ana", read, token, http.StatusOK},
		{"bob", read, bobs, http.StatusNotFound},
		// An instance key reaches the operator workspace's mailboxes only.
		{"an instance send key", read, instance, http.StatusNotFound},
		{"a read key", read, h.key(t, auth.ScopeRead), http.StatusForbidden},
		{"no account", key, token, http.StatusBadRequest},
		{"an unknown key", "another?account=" + id, token, http.StatusNotFound},
	} {
		if resp := h.do(t, http.MethodGet, "/v1/sends/"+c.path, c.token, ""); resp.StatusCode != c.want {
			t.Errorf("%s reading the send: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}

	// Withdrawn, nothing sends; and nothing was deleted.
	if resp := h.do(t, http.MethodDelete, "/v1/me/send-consent", token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("withdraw: %d", resp.StatusCode)
	}
	resp = h.send(t, token, "after", compose)
	if code, _ := decodeError(t, resp); resp.StatusCode != http.StatusConflict || code != "conflict" {
		t.Errorf("a send after withdrawing: %d %s, want 409", resp.StatusCode, code)
	}
	if resp := h.do(t, http.MethodGet, "/v1/sends/"+read, token, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the record after withdrawing: %d", resp.StatusCode)
	}
}

// multipartOf is a multipart/form-data body of the named fields, in order.
func multipartOf(t *testing.T, fields ...[2]string) struct {
	contentType string
	body        string
} {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range fields {
		part, err := w.CreateFormField(f[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(f[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return struct {
		contentType string
		body        string
	}{w.FormDataContentType(), buf.String()}
}
