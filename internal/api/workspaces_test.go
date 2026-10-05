package api_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/api"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store/storetest"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestTheWorkspaceParameterNarrowsTheListingsToAWorkspaceTheCallerBelongsTo(t *testing.T) {
	h := newHarness(t, false)
	ana := h.person(t, "ana@example.com", auth.RoleMember)
	bob := h.person(t, "bob@example.com", auth.RoleMember)
	anas := h.mailbox(t, ana, "ana@mail.example")
	h.mailbox(t, bob, "bob@mail.example")
	anaUser, err := h.users.GetByEmail(t.Context(), "ana@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bobUser, err := h.users.GetByEmail(t.Context(), "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	hers, his := authtest.Personal(t, h.store, anaUser.ID), authtest.Personal(t, h.store, bobUser.ID)

	resp := h.do(t, http.MethodGet, "/v1/accounts?workspace="+hers, ana, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("her own workspace: %d", resp.StatusCode)
	}
	var listed []struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != anas || listed[0].WorkspaceID != hers {
		t.Errorf("her workspace lists %+v", listed)
	}
	// Another person's workspace does not exist for her, on any listing.
	for _, path := range []string{
		"/v1/accounts?workspace=" + his,
		"/v1/messages?workspace=" + his,
		"/v1/events/wait?timeout=1&workspace=" + his,
	} {
		if resp := h.do(t, http.MethodGet, path, ana, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
	// And a mailbox outside the workspace named is not in it.
	if resp := h.do(t, http.MethodGet, "/v1/messages?account="+anas+"&workspace="+his, ana, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("her mailbox under bob's workspace: %d, want 404", resp.StatusCode)
	}
}

// supportTeam is Ana's team over REST: she created it with her session, Bea
// is a member who holds nothing yet, and Ana linked a mailbox into it,
// indexed with one message.
type supportTeam struct {
	ana, bea     string // session tokens
	anaID, beaID string
	id           string // the team
	shared       string // the team's mailbox
	message      int64  // its one indexed message
	own          string // Bea's own mailbox
}

func newSupportTeam(t *testing.T, h *harness) supportTeam {
	t.Helper()
	var tm supportTeam
	tm.ana = h.person(t, "ana@example.com", auth.RoleMember)
	tm.bea = h.person(t, "bea@example.com", auth.RoleMember)
	for email, id := range map[string]*string{"ana@example.com": &tm.anaID, "bea@example.com": &tm.beaID} {
		u, err := h.users.GetByEmail(t.Context(), email)
		if err != nil {
			t.Fatal(err)
		}
		*id = u.ID
	}
	resp := h.do(t, http.MethodPost, "/v1/workspaces", tm.ana, `{"name":"Support"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating the team: %d", resp.StatusCode)
	}
	var team struct {
		ID string `json:"id"`
	}
	decodeInto(t, resp, &team)
	tm.id = team.ID
	if err := h.store.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.NewRepository(h.store, nil).AddMemberTx(t.Context(), tx, tm.id, tm.beaID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	added := h.do(t, http.MethodPost, "/v1/accounts", tm.ana,
		strings.TrimSuffix(h.passwordAccount(t, "support@mail.example"), "}")+`,"workspace_id":"`+tm.id+`"}`)
	if added.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, added)
		t.Fatalf("linking into the team: %d %s %s", added.StatusCode, code, msg)
	}
	var result struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	decodeInto(t, added, &result)
	tm.shared = result.Account.ID
	if resp := h.do(t, http.MethodPost, "/v1/me/sync-consent", tm.ana,
		`{"version":"`+service.DefaultSyncConsentVersion+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("Ana agreeing to sync: %d", resp.StatusCode)
	}
	box := providertest.NewFakeMailbox(providertest.FakeOptions{Caps: providertest.GmailCaps()})
	uid := box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "refund-1@example.org", Subject: "Refund for order 4471", From: "client@example.org",
		To: []string{"support@mail.example"}, InternalDate: time.Now().Add(-time.Hour), Size: 4000,
	})
	storetest.IndexMailbox(t, h.store, tm.shared, box)
	tm.message = h.messageRow(t, tm.shared, uid)
	tm.own = h.mailbox(t, tm.bea, "bea@mail.example")
	return tm
}

// grant sets what Bea holds on the team's mailbox, as Ana.
func (tm supportTeam) grant(t *testing.T, h *harness, body string) {
	t.Helper()
	resp := h.do(t, http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana, body)
	if resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("granting %s: %d %s %s", body, resp.StatusCode, code, msg)
	}
}

func TestAMemberWithoutAGrantCannotSeeTheMailbox(t *testing.T) {
	// Over REST, the long poll and storage; the event stream has a test of
	// its own below, and MCP one in its package.
	h := newHarness(t, false)
	tm := newSupportTeam(t, h)
	routes := []string{
		"/v1/accounts/" + tm.shared,
		"/v1/accounts/" + tm.shared + "/sync",
		"/v1/accounts/" + tm.shared + "/folders",
		"/v1/messages?account=" + tm.shared,
		fmt.Sprintf("/v1/messages/%d", tm.message),
		fmt.Sprintf("/v1/messages/%d/raw", tm.message),
		"/v1/events/wait?timeout=1&account=" + tm.shared,
	}
	for _, path := range routes {
		if resp := h.do(t, http.MethodGet, path, tm.bea, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, resp.StatusCode)
		}
	}
	sees := func() (listed, searched, stored bool) {
		t.Helper()
		var accounts []struct {
			ID string `json:"id"`
		}
		decodeInto(t, h.do(t, http.MethodGet, "/v1/accounts", tm.bea, ""), &accounts)
		for _, a := range accounts {
			listed = listed || a.ID == tm.shared
		}
		var page struct {
			Messages []struct {
				AccountID string `json:"account_id"`
			} `json:"messages"`
		}
		decodeInto(t, h.do(t, http.MethodGet, "/v1/messages?q=refund", tm.bea, ""), &page)
		searched = len(page.Messages) > 0
		var storage struct {
			Mailboxes []struct {
				AccountID string `json:"account_id"`
			} `json:"mailboxes"`
		}
		decodeInto(t, h.do(t, http.MethodGet, "/v1/me/storage", tm.bea, ""), &storage)
		for _, m := range storage.Mailboxes {
			stored = stored || m.AccountID == tm.shared
		}
		return listed, searched, stored
	}
	if listed, searched, stored := sees(); listed || searched || stored {
		t.Errorf("without a grant Bea lists it %v, finds its mail %v, sees its storage %v", listed, searched, stored)
	}

	// Send alone shows the card and opens nothing of the index.
	tm.grant(t, h, `{"read":false,"act":false,"send":true,"manage":false}`)
	if resp := h.do(t, http.MethodGet, "/v1/accounts/"+tm.shared, tm.bea, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("her card of a mailbox she may send from: %d", resp.StatusCode)
	}
	for _, path := range routes[2:] {
		want := http.StatusForbidden
		if strings.HasPrefix(path, "/v1/messages/") {
			// A message is reached by its id alone; without read the
			// mailbox's messages do not exist for her.
			want = http.StatusNotFound
		}
		if resp := h.do(t, http.MethodGet, path, tm.bea, ""); resp.StatusCode != want {
			t.Errorf("with send alone, %s: %d, want %d", path, resp.StatusCode, want)
		}
	}
	if listed, searched, stored := sees(); !listed || searched || stored {
		t.Errorf("with send alone Bea lists it %v, finds its mail %v, sees its storage %v", listed, searched, stored)
	}

	// Read opens it all, which is what proves the refusals above were
	// about the grant.
	tm.grant(t, h, `{"read":true,"act":false,"send":true,"manage":false}`)
	for _, path := range routes {
		resp := h.do(t, http.MethodGet, path, tm.bea, "")
		switch {
		case strings.HasSuffix(path, "/folders") || strings.HasPrefix(path, "/v1/messages/"):
			// These reach the mail server, which this harness has none of:
			// past the authorization, whatever they answer then.
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
				t.Errorf("with read, %s: %d", path, resp.StatusCode)
			}
		case resp.StatusCode != http.StatusOK:
			t.Errorf("with read, %s: %d", path, resp.StatusCode)
		}
	}
	if listed, searched, stored := sees(); !listed || !searched || !stored {
		t.Errorf("with read Bea lists it %v, finds its mail %v, sees its storage %v", listed, searched, stored)
	}
}

func TestAStreamWhoseKeyLostAMailboxEndsWithAConflictAndReconnects(t *testing.T) {
	// Bea's key was made for the team's mailbox and her own. Losing read on
	// the team's takes it out of the key, and the stream opened with the key
	// as it was ends: not with unauthorized, which would tell her tool its
	// key is dead, but with a conflict, after which the same key opens a
	// stream of what it still names.
	h := newHarnessWith(t, func(h *api.Handler) { h.EventPing = 30 * time.Millisecond }, serviceOptions{})
	tm := newSupportTeam(t, h)
	tm.grant(t, h, `{"read":true,"act":false,"send":false,"manage":false}`)
	resp := h.do(t, http.MethodPost, "/v1/me/apikeys", tm.bea, fmt.Sprintf(
		`{"name":"assistant","scope":"read","account_ids":[%q,%q],"terms_version":%q}`,
		tm.shared, tm.own, service.DefaultKeyTermsVersion))
	if resp.StatusCode != http.StatusCreated {
		code, msg := decodeError(t, resp)
		t.Fatalf("creating the key: %d %s %s", resp.StatusCode, code, msg)
	}
	var created struct {
		Key string `json:"key"`
	}
	decodeInto(t, resp, &created)
	held := h.openStream(t, "/v1/events", created.Key, "")
	h.publish(t, mailEvent(t, tm.shared, "Refund"))
	if f, ok := held.next(t); !ok || f.event != "message.new" {
		t.Fatalf("before: %+v", f)
	}

	if resp := h.do(t, http.MethodDelete, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking: %d", resp.StatusCode)
	}
	f, ok := held.next(t)
	if !ok || f.event != "error" || !strings.Contains(f.data, `"code":"conflict"`) {
		t.Fatalf("after the key lost the mailbox the stream sent %+v, want its end with a conflict", f)
	}
	if f, ok := held.next(t); ok {
		t.Errorf("the stream went on: %+v", f)
	}

	again := h.openStream(t, "/v1/events", created.Key, "")
	if again.resp.StatusCode != http.StatusOK {
		t.Fatalf("reconnecting with the same key: %d", again.resp.StatusCode)
	}
	h.publish(t, mailEvent(t, tm.shared, "Second refund"), mailEvent(t, tm.own, "Her own"))
	if f, ok := again.next(t); !ok || f.event != "message.new" || subjectOf(t, f) != "Her own" {
		t.Errorf("the new stream: %+v", f)
	}
}

func TestRemovingAMemberRemovesTheirGrantsAndStopsTheirEventStream(t *testing.T) {
	h := newHarnessWith(t, func(h *api.Handler) { h.EventPing = 30 * time.Millisecond }, serviceOptions{})
	tm := newSupportTeam(t, h)
	tm.grant(t, h, `{"read":true,"act":false,"send":false,"manage":false}`)
	following := h.openStream(t, "/v1/events?account="+tm.shared, tm.bea, "")
	everything := h.openStream(t, "/v1/events", tm.bea, "")
	if resp := h.do(t, http.MethodGet, "/v1/events?account="+tm.shared, tm.ana, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("Ana's stream: %d", resp.StatusCode)
	}
	h.publish(t, mailEvent(t, tm.shared, "Refund"))
	for name, s := range map[string]*sseStream{"her stream of the mailbox": following, "her stream of everything": everything} {
		if f, ok := s.next(t); !ok || f.event != "message.new" || subjectOf(t, f) != "Refund" {
			t.Fatalf("%s: %+v", name, f)
		}
	}

	if resp := h.do(t, http.MethodDelete, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, ""); resp.StatusCode != http.StatusNoContent {
		code, msg := decodeError(t, resp)
		t.Fatalf("removing Bea: %d %s %s", resp.StatusCode, code, msg)
	}
	h.publish(t, mailEvent(t, tm.shared, "Second refund"), mailEvent(t, tm.own, "Her own"))

	// The stream that followed only the team's mailbox says so, and ends.
	if f, ok := following.next(t); !ok || f.event != "access" || f.id != "" ||
		f.data != `{"account_id":"`+tm.shared+`","read":false}` {
		t.Fatalf("after the removal the stream sent %+v", f)
	}
	if f, ok := following.next(t); !ok || f.event != "error" || !strings.Contains(f.data, `"code":"not_found"`) {
		t.Fatalf("then %+v, want the stream's end", f)
	}
	if f, ok := following.next(t); ok {
		t.Errorf("the stream went on: %+v", f)
	}
	// The one that followed everything she reads loses the mailbox and goes
	// on with her own.
	var got []string
	for len(got) < 2 {
		f, ok := everything.next(t)
		if !ok {
			t.Fatalf("her stream of everything ended after %v", got)
		}
		got = append(got, f.event)
		if f.event == "message.new" && subjectOf(t, f) != "Her own" {
			t.Errorf("her stream carried %q", subjectOf(t, f))
		}
	}
	if !slices.Contains(got, "access") || !slices.Contains(got, "message.new") {
		t.Errorf("her stream of everything sent %v", got)
	}

	// Her grants went with her place in the team.
	var directory []struct {
		Grants []struct {
			UserID string `json:"user_id"`
		} `json:"grants"`
	}
	decodeInto(t, h.do(t, http.MethodGet, "/v1/workspaces/"+tm.id+"/access", tm.ana, ""), &directory)
	for _, mb := range directory {
		for _, g := range mb.Grants {
			if g.UserID == tm.beaID {
				t.Errorf("a grant of Bea's survived her removal")
			}
		}
	}
}

func TestAnInstanceKeyReachesOnlyOperatorMailboxes(t *testing.T) {
	h := newHarness(t, true)
	tm := newSupportTeam(t, h)
	instance := h.key(t, auth.ScopeAdmin)
	operators := h.mailbox(t, instance, "ops@mail.example")

	var accounts []struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
	}
	decodeInto(t, h.do(t, http.MethodGet, "/v1/accounts", instance, ""), &accounts)
	if len(accounts) != 1 || accounts[0].ID != operators || accounts[0].WorkspaceID != workspace.OperatorID {
		t.Errorf("an instance key lists %+v, want only the operator's mailbox", accounts)
	}
	for _, path := range []string{
		"/v1/accounts/" + tm.shared, "/v1/accounts/" + tm.own,
		"/v1/messages?account=" + tm.shared, fmt.Sprintf("/v1/messages/%d", tm.message),
		"/v1/accounts?workspace=" + tm.id,
	} {
		if resp := h.do(t, http.MethodGet, path, instance, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("an instance key on %s: %d, want 404", path, resp.StatusCode)
		}
	}
	// Nor can it be restricted to a person's mailbox: it could never reach
	// it.
	resp := h.do(t, http.MethodPost, "/v1/apikeys", instance,
		`{"name":"one","scope":"read","account_ids":["`+tm.shared+`"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an instance key restricted to a person's mailbox: %d, want 400", resp.StatusCode)
	}
	// Administering a team reads none of its mail either.
	if resp := h.do(t, http.MethodGet, "/v1/workspaces/"+tm.id+"/access", instance, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the operator's access directory: %d", resp.StatusCode)
	}
}

func TestTheWorkspaceRoutesAreThinOverTheService(t *testing.T) {
	// One pass over every new route with the console's credential, and the
	// refusals that belong to the transport's input.
	h := newHarnessWith(t, nil, serviceOptions{publicURL: "http://localhost:5174"})
	tm := newSupportTeam(t, h)
	status := func(method, path, token, body string, want int) *http.Response {
		t.Helper()
		resp := h.do(t, method, path, token, body)
		if resp.StatusCode != want {
			code, msg := decodeError(t, resp)
			t.Fatalf("%s %s: %d %s %s, want %d", method, path, resp.StatusCode, code, msg, want)
		}
		return resp
	}
	status(http.MethodPatch, "/v1/workspaces/"+tm.id, tm.ana, `{"name":"Help desk"}`, http.StatusOK)
	status(http.MethodPatch, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, `{"role":"admin"}`, http.StatusOK)
	status(http.MethodPatch, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, `{}`, http.StatusBadRequest)
	status(http.MethodPatch, "/v1/workspaces/"+tm.id+"/members/"+tm.beaID, tm.ana, `{"role":"boss"}`, http.StatusBadRequest)
	var invite struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	decodeInto(t, status(http.MethodPost, "/v1/workspaces/"+tm.id+"/invites", tm.ana,
		`{"email":"carol@example.com"}`, http.StatusCreated), &invite)
	status(http.MethodDelete, "/v1/workspaces/"+tm.id+"/invites/"+invite.ID, tm.bea, "", http.StatusNoContent)
	status(http.MethodDelete, "/v1/workspaces/"+tm.id+"/invites/"+invite.ID, tm.ana, "", http.StatusNotFound)

	// An existing person accepts with their session; their key cannot.
	carol := h.person(t, "carol@example.com", auth.RoleMember)
	decodeInto(t, status(http.MethodPost, "/v1/workspaces/"+tm.id+"/invites", tm.ana,
		`{"email":"carol@example.com","role":"member"}`, http.StatusCreated), &invite)
	link, err := url.Parse(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	accept := fmt.Sprintf(`{"invite":%q}`, fragment.Get("invite"))
	status(http.MethodPost, "/v1/auth/invites/accept", authtest.NewKey(t, h.store, auth.ScopeAdmin, ""), accept,
		http.StatusForbidden)
	status(http.MethodPost, "/v1/auth/invites/accept", tm.bea, accept, http.StatusForbidden)
	status(http.MethodPost, "/v1/auth/invites/accept", carol, accept, http.StatusOK)

	// Access: every flag is required, a flag list is checked, and a
	// person's key administers nothing.
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana, `{"read":true}`, http.StatusBadRequest)
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana,
		`{"read":true,"act":true,"send":true,"manage":true}`, http.StatusOK)
	status(http.MethodDelete, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID+"?flags=send,shout", tm.ana, "",
		http.StatusBadRequest)
	status(http.MethodDelete, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID+"?flags=send", tm.ana, "",
		http.StatusNoContent)
	var created struct {
		Key string `json:"key"`
	}
	decodeInto(t, status(http.MethodPost, "/v1/me/apikeys", tm.bea, fmt.Sprintf(`{"name":"assistant","scope":"write",`+
		`"terms_version":%q}`, service.DefaultKeyTermsVersion), http.StatusCreated), &created)
	beaKey := created.Key
	status(http.MethodGet, "/v1/workspaces", beaKey, "", http.StatusOK)
	status(http.MethodGet, "/v1/workspaces/"+tm.id+"/members", beaKey, "", http.StatusForbidden)
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, beaKey,
		`{"read":true,"act":false,"send":false,"manage":true}`, http.StatusForbidden)

	// Bea, an admin holding every flag but send, cannot take the link over
	// until she holds send and agreed to sync.
	status(http.MethodPost, "/v1/accounts/"+tm.shared+"/take-over", tm.bea, "", http.StatusConflict)
	status(http.MethodPut, "/v1/accounts/"+tm.shared+"/access/"+tm.beaID, tm.ana,
		`{"read":true,"act":true,"send":true,"manage":true}`, http.StatusOK)
	status(http.MethodPost, "/v1/me/sync-consent", tm.bea, `{"version":"`+service.DefaultSyncConsentVersion+`"}`, http.StatusOK)
	var taken struct {
		LinkedBy string `json:"linked_by"`
	}
	decodeInto(t, status(http.MethodPost, "/v1/accounts/"+tm.shared+"/take-over", tm.bea, "", http.StatusOK), &taken)
	if taken.LinkedBy != tm.beaID {
		t.Errorf("linked_by = %q after the take-over", taken.LinkedBy)
	}
	status(http.MethodDelete, "/v1/workspaces/"+tm.id+"/members/"+tm.anaID, tm.ana, "", http.StatusConflict) // the last owner
}
