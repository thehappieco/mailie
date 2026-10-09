package api_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/store/storetest"
)

// The console's TypeScript types are checked against these files by
// web/test/contract.spec.ts. They are written from what the real handlers
// answer, so the two sides cannot drift without one of the two test suites
// saying so:
//
//	go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update
var update = flag.Bool("update", false, "rewrite internal/api/testdata/contract from the handlers' output")

const contractDir = "testdata/contract"

func TestTheContractFixturesMatchTheHandlers(t *testing.T) {
	epoch := time.Unix(1790000000, 0).UTC()
	sync := newLendingEngine()
	h := newHarnessWith(t, nil, serviceOptions{
		publicURL: "http://localhost:5174",
		now:       func() time.Time { return epoch },
		sync:      sync,
		mcpHTTP:   true,
		registry: account.RegistryOptions{
			Google:    account.OAuthClient{ClientID: "google-installed-client"},
			Microsoft: account.OAuthClient{ClientID: "microsoft-installed-client", Tenant: "common"},
			GoogleWeb: account.OAuthClient{ClientID: "google-web-client", ClientSecret: "not-a-real-secret"},
		},
	})
	ana := authtest.NewUser(t, h.store, "ana@example.com", auth.RoleOwner)
	n := newNormalizer()

	capture := func(name string, want int, method, path, token, body string) map[string]any {
		t.Helper()
		resp := h.do(t, method, path, token, body)
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s: %s %s answered %d, want %d: %s", name, method, path, resp.StatusCode, want, raw)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n.check(t, name, value)
		obj, _ := value.(map[string]any)
		return obj
	}

	// Signing in (docs/key-scheme.md section 12): the challenge for an
	// address, which is the same with or without an account, and the
	// sign-in with the auth key derived under it.
	capture("challenge", http.StatusOK, http.MethodPost, "/v1/auth/challenge", "", `{"email":"ana@example.com"}`)
	login := capture("login", http.StatusOK, http.MethodPost, "/v1/auth/login", "",
		fmt.Sprintf(`{"email":"ana@example.com","auth_key":%q}`, authtest.AuthKey))
	token, _ := login["token"].(string)

	capture("me", http.StatusOK, http.MethodGet, "/v1/auth/me", token, "")
	capture("stepup", http.StatusOK, http.MethodPost, "/v1/auth/stepup", token,
		fmt.Sprintf(`{"auth_key":%q}`, authtest.AuthKey))
	capture("password_begin", http.StatusOK, http.MethodPost, "/v1/auth/password/begin", token,
		fmt.Sprintf(`{"current_auth_key":%q}`, authtest.AuthKey))
	capture("recover_open", http.StatusOK, http.MethodPost, "/v1/auth/recover/open", "",
		fmt.Sprintf(`{"email":"ana@example.com","recovery_proof":%q}`, authtest.RecoveryProof))
	capture("user", http.StatusOK, http.MethodPut, "/v1/auth/profile", token, `{"name":"Ana Lima"}`)
	capture("providers", http.StatusOK, http.MethodGet, "/v1/providers", token, "")
	capture("mcp", http.StatusOK, http.MethodGet, "/v1/me/mcp", token, "")
	capture("invite", http.StatusCreated, http.MethodPost, "/v1/users/invites", token, `{"email":"bea@example.com"}`)

	added := capture("add_account", http.StatusCreated, http.MethodPost, "/v1/accounts", token,
		`{"email":"ana@gmail.com","display_name":"Ana (Gmail)","flow":"loopback"}`)
	gmail, _ := added["account"].(map[string]any)["id"].(string)
	capture("auth_flow_loopback", http.StatusOK, http.MethodPost, "/v1/accounts/"+gmail+"/oauth/start", token,
		`{"flow":"loopback"}`)
	// With no flow named, a console with a web client gets the web flow.
	capture("auth_flow_web", http.StatusOK, http.MethodPost, "/v1/accounts/"+gmail+"/oauth/start", token, "")

	imap := capture("", http.StatusCreated, http.MethodPost, "/v1/accounts", token,
		h.passwordAccount(t, "ana@mail.example"))
	imapID, _ := imap["account"].(map[string]any)["id"].(string)
	capture("account", http.StatusOK, http.MethodGet, "/v1/accounts/"+imapID, token, "")

	// An iCloud account cannot be added here: its login would have to reach
	// Apple, and it may not point anywhere else. So it goes in the database
	// the way the registry stores one — generic IMAP on Apple's servers — and
	// the handler is what presents it as icloud.
	icloud, err := account.NewRepository(h.store, nil).Create(t.Context(), account.Account{
		ID: "acc_00000000000000c1", Email: "ana@icloud.com", DisplayName: "Ana (iCloud)",
		Provider: provider.KindIMAP, AuthKind: "password",
		IMAPHost: "imap.mail.me.com", IMAPPort: 993, SMTPHost: "smtp.mail.me.com", SMTPPort: 587, SMTPTLS: "starttls",
		LoginUser: "ana@icloud.com", SaveSentCopy: true, State: account.StateActive,
	}, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	capture("account_icloud", http.StatusOK, http.MethodGet, "/v1/accounts/"+icloud.ID, token, "")

	capture("error", http.StatusNotFound, http.MethodGet, "/v1/accounts/acc_0000000000000000", token, "")

	// Sync: consent, then an account the engine is running.
	capture("sync_consent", http.StatusOK, http.MethodGet, "/v1/me/sync-consent", token, "")
	capture("sync_consent_given", http.StatusOK, http.MethodPost, "/v1/me/sync-consent", token,
		`{"version":"`+service.DefaultSyncConsentVersion+`"}`)
	sync.running(imapID, service.SyncStatus{
		Running: true, State: "initial", Tier: "condstore", FoldersSynced: 3, FoldersTotal: 7,
		Messages: 1284, InitialProgress: 42, LastSyncedAt: epoch,
	})
	capture("sync_status", http.StatusOK, http.MethodGet, "/v1/accounts/"+imapID+"/sync", token, "")
	capture("sync_triggered", http.StatusAccepted, http.MethodPost, "/v1/accounts/"+imapID+"/sync", token, "")
	capture("account_syncing", http.StatusOK, http.MethodGet, "/v1/accounts/"+imapID, token, "")

	// Events, as the engine journals them: the long poll's answer, and the
	// data line of the same event on the stream.
	before := h.publish(t, mustEvent(t, events.TypeFolderChanged, imapID, epoch, store.FolderChanged{
		AccountID: imapID, FolderID: 1, Name: "INBOX", Role: "inbox", Change: store.FolderInitialDone, Count: 1283,
	}))[0].Seq
	h.publish(t, mustEvent(t, events.TypeMessageNew, imapID, epoch, store.MessageNew{
		AccountID: imapID, MessageID: 1284, FolderID: 1, FolderRole: "inbox", Subject: "Lunch on Friday?",
		From: &provider.Address{Name: "Bea Lima", Email: "bea@example.com"}, InternalDate: epoch.Unix(),
		FirstCopy: true, FirstInboxCopy: true,
	}))
	capture("events_wait", http.StatusOK, http.MethodGet,
		fmt.Sprintf("/v1/events/wait?since=%d&timeout=1", before), token, "")
	stream := h.openStream(t, "/v1/events", token, strconv.FormatInt(before, 10))
	frame, ok := stream.next(t)
	if !ok || frame.event != "message.new" {
		t.Fatalf("the stream sent %+v", frame)
	}
	var data any
	if err := json.Unmarshal([]byte(frame.data), &data); err != nil {
		t.Fatal(err)
	}
	n.check(t, "event", data)

	// Mail: one of ana's mailboxes, indexed as sync would, and a message
	// read from its (fake) server.
	const work = "acc_00000000000000d1"
	box := h.fakeMailbox(t, sync, work, ana.ID, "ana@work.example")
	box.CreateFolder("Sent", goimap.MailboxAttrSent)
	box.CreateFolder("Receipts")
	box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "menu-2@example.org", Subject: "Re: Lunch on Friday?", From: "Bea Lima <bea@example.org>",
		To: []string{"Ana Lima <ana@work.example>"}, Cc: []string{"caio@example.org"},
		InReplyTo: "menu-1@work.example", References: []string{"menu-1@work.example"},
		InternalDate: epoch.Add(-time.Hour), Raw: []byte(contractMessage),
	})
	box.Deliver("INBOX", providertest.FakeMessage{
		MessageID: "week-1@example.org", Subject: "Your week", From: "Calendar <calendar@example.org>",
		To: []string{"ana@work.example"}, InternalDate: epoch.Add(-2 * time.Hour), Flags: []goimap.Flag{goimap.FlagSeen},
	})
	storetest.IndexMailbox(t, h.store, work, box)
	capture("folders_indexed", http.StatusOK, http.MethodGet, "/v1/accounts/"+work+"/folders", token, "")
	page := capture("messages", http.StatusOK, http.MethodGet, "/v1/messages?account="+work+"&limit=1", token, "")
	first, _ := page["messages"].([]any)[0].(map[string]any)["id"].(float64)
	capture("message", http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/messages/%d", int64(first)), token, "")

	// The keys of ana's personal workspace: one for a tool that reads her
	// work mailbox, used once; one that could act on it, revoked; and one
	// that reads and sends from it, which sends below.
	var personal string
	if err := h.store.Reader().QueryRowContext(t.Context(),
		`SELECT id FROM workspaces WHERE person_id = ?`, ana.ID).Scan(&personal); err != nil {
		t.Fatal(err)
	}
	keys := "/v1/workspaces/" + personal + "/apikeys"
	created := capture("apikey_created", http.StatusCreated, http.MethodPost, keys, token,
		fmt.Sprintf(`{"name":"Claude Code","scope":"read","mailboxes":[{"account_id":%q,"read":true,"act":false,"send":false}],`+
			`"ttl_days":90,"terms_version":%q}`, work, service.DefaultKeyTermsVersion))
	readPrefix, _ := created["prefix"].(string)
	if _, err := h.store.Writer().ExecContext(t.Context(),
		`UPDATE api_keys SET last_used_at = ? WHERE prefix = ?`, epoch.Add(time.Hour).Unix(), readPrefix); err != nil {
		t.Fatal(err)
	}
	actor := capture("", http.StatusCreated, http.MethodPost, keys, token,
		fmt.Sprintf(`{"name":"Assistant","scope":"write","ttl_days":30,"terms_version":%q}`, service.DefaultKeyTermsVersion))
	actorPrefix, _ := actor["prefix"].(string)
	capture("key_access", http.StatusOK, http.MethodPut, keys+"/"+actorPrefix+"/accounts/"+work, token,
		`{"read":true,"act":true,"send":false}`)
	if resp := h.do(t, http.MethodDelete, keys+"/"+actorPrefix, token, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking a key answered %d", resp.StatusCode)
	}
	sender := capture("", http.StatusCreated, http.MethodPost, keys, token,
		fmt.Sprintf(`{"name":"Mail assistant","scope":"send","mailboxes":[{"account_id":%q,"read":true,"act":false,"send":true}],`+
			`"ttl_days":30,"terms_version":%q}`, work, service.DefaultKeyTermsVersion))
	senderKey, _ := sender["key"].(string)
	senderPrefix, _ := sender["prefix"].(string)
	capture("apikeys", http.StatusOK, http.MethodGet, keys, token, "")
	capture("my_apikeys", http.StatusOK, http.MethodGet, "/v1/me/apikeys", token, "")

	// Actions: the consent, and what an action answers. The server here has
	// MOVE and no UIDPLUS, so it reports no new UIDs: the message with a
	// Message-ID is found in its new folder and keeps its id, and the one
	// without leaves the index for the folder's next pass to find.
	capture("actions_consent", http.StatusOK, http.MethodGet, "/v1/me/actions-consent", token, "")
	capture("actions_consent_given", http.StatusOK, http.MethodPost, "/v1/me/actions-consent", token,
		`{"version":"`+service.DefaultActionsConsentVersion+`"}`)
	nameless := box.Deliver("INBOX", providertest.FakeMessage{
		Subject: "Receipt 4471", From: "Shop <orders@shop.example>", To: []string{"ana@work.example"},
		InternalDate: epoch.Add(-3 * time.Hour),
	})
	storetest.IndexMailbox(t, h.store, work, box)
	week := h.messageRow(t, work, 2)
	box.SetCaps(capsWithoutUIDPlus())
	var receipts int64
	if err := h.store.Reader().QueryRowContext(t.Context(),
		`SELECT id FROM folders WHERE account_id = ? AND name = 'Receipts'`, work).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	var lastSeq int64
	if err := h.store.Reader().QueryRowContext(t.Context(), `SELECT coalesce(max(seq), 0) FROM events`).Scan(&lastSeq); err != nil {
		t.Fatal(err)
	}
	capture("action_result", http.StatusOK, http.MethodPost, "/v1/messages/move", token,
		fmt.Sprintf(`{"ids":[%d,%d],"to":"%d"}`, week, h.messageRow(t, work, nameless), receipts))
	stream = h.openStream(t, "/v1/events", token, strconv.FormatInt(lastSeq, 10))
	for {
		frame, ok := stream.next(t)
		if !ok {
			t.Fatal("the stream ended before the move was announced")
		}
		if frame.event != "message.moved" {
			continue
		}
		if err := json.Unmarshal([]byte(frame.data), &data); err != nil {
			t.Fatal(err)
		}
		n.check(t, "event_moved", data)
		break
	}

	// Sending: the consent, a reply with an attachment that the server
	// takes (the account files its own copy in Sent), its record, and a
	// message whose recipient the server refuses.
	smtp := providertest.NewSMTPServer(t)
	h.submitTo(t, work, smtp)
	if _, err := h.store.Writer().ExecContext(t.Context(),
		`UPDATE accounts SET display_name = 'Ana Lima', save_sent_copy = 1 WHERE id = ?`, work); err != nil {
		t.Fatal(err)
	}
	capture("send_consent", http.StatusOK, http.MethodGet, "/v1/me/send-consent", token, "")
	capture("send_consent_given", http.StatusOK, http.MethodPost, "/v1/me/send-consent", token,
		`{"version":"`+service.DefaultSendConsentVersion+`"}`)
	const sendKey = "3f2b8c9e-4a1d-4e6f-9b7c-0d5e6f7a8b9c"
	sendCapture := func(name, key, compose string, files ...file) {
		t.Helper()
		resp := h.send(t, token, key, compose, files...)
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: POST /v1/messages/send answered %d: %s", name, resp.StatusCode, raw)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		n.check(t, name, value)
	}
	sendCapture("send_result", sendKey, fmt.Sprintf(`{"account_id":%q,"to":[{"name":"Bea Lima","email":"bea@example.org"}],`+
		`"cc":[],"bcc":[],"subject":"","text":"Friday at noon works.\n\nOn Fri, Bea Lima wrote:\n> Friday works.",`+
		`"in_reply_to":%d,"confirm":true}`, work, int64(first)),
		file{"agenda.pdf", "application/pdf", []byte("%PDF-1.4 agenda")})
	capture("send_status", http.StatusOK, http.MethodGet, "/v1/sends/"+sendKey+"?account="+work, token, "")
	smtp.RejectRecipient("nobody@example.org")
	sendCapture("send_result_refused", "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d",
		fmt.Sprintf(`{"account_id":%q,"to":[{"email":"bea@example.org"},{"email":"nobody@example.org"}],`+
			`"subject":"Minutes","text":"Attached.","confirm":true}`, work))
	// A key's send, as its workspace's owners and admins list them.
	if resp := h.send(t, senderKey, "5c4d3e2f-1a0b-4c9d-8e7f-6a5b4c3d2e1f", fmt.Sprintf(`{"account_id":%q,`+
		`"to":[{"email":"bea@example.org"}],"subject":"Agenda","text":"Sent by the assistant.","confirm":true}`,
		work)); resp.StatusCode != http.StatusOK {
		code, msg := decodeError(t, resp)
		t.Fatalf("a key's send: %d %s %s", resp.StatusCode, code, msg)
	}
	capture("key_sends", http.StatusOK, http.MethodGet, keys+"/"+senderPrefix+"/sends", token, "")

	// Storage, last, so its account ids take no number another fixture
	// has: ana's mailboxes, as an owner signed in sees them, with the size
	// of the whole database.
	capture("storage", http.StatusOK, http.MethodGet, "/v1/me/storage", token, "")

	// Workspaces, after everything else for the same reason: a team ana
	// creates, which she lists beside her personal workspace; a person she
	// invites who signs up with the link, one who already has an account
	// and accepts, and one still on the way; a mailbox she links into the
	// team, what she grants on it, and who holds what.
	team := capture("workspace_created", http.StatusCreated, http.MethodPost, "/v1/workspaces", token,
		`{"name":"Support"}`)
	teamID, _ := team["id"].(string)
	capture("workspaces", http.StatusOK, http.MethodGet, "/v1/workspaces", token, "")
	invited := capture("team_invite", http.StatusCreated, http.MethodPost, "/v1/workspaces/"+teamID+"/invites", token,
		`{"email":"bea@example.com","role":"member"}`)
	link, err := url.Parse(invited["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	fragment, err := url.ParseQuery(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	// Opening an invitation answers what the browser binds and derives
	// under; the sign-up names the seal id it answered.
	opened := capture("signup_open", http.StatusOK, http.MethodPost, "/v1/auth/signup/open", "",
		fmt.Sprintf(`{"invite":%q,"email":"bea@example.com"}`, fragment.Get("invite")))
	signedUp := capture("", http.StatusCreated, http.MethodPost, "/v1/auth/signup", "",
		jsonOf(t, enrolment(t, authtest.AuthKey, authtest.RecoveryProof,
			map[string]any{"invite": fragment.Get("invite"), "email": "bea@example.com", "name": "Bea Lima", "seal_id": opened["seal_id"]})))
	beaID, _ := signedUp["user"].(map[string]any)["id"].(string)
	carol := authtest.NewUser(t, h.store, "carol@example.com", auth.RoleMember)
	toCarol := capture("", http.StatusCreated, http.MethodPost, "/v1/workspaces/"+teamID+"/invites", token,
		`{"email":"carol@example.com","role":"admin"}`)
	carolLink, err := url.Parse(toCarol["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	carolCode, err := url.ParseQuery(carolLink.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	capture("invite_accepted", http.StatusOK, http.MethodPost, "/v1/auth/invites/accept",
		authtest.SignIn(t, h.users, "carol@example.com"), fmt.Sprintf(`{"invite":%q}`, carolCode.Get("invite")))
	capture("", http.StatusCreated, http.MethodPost, "/v1/workspaces/"+teamID+"/invites", token,
		`{"email":"dan@example.com","role":"admin"}`)
	capture("team_invites", http.StatusOK, http.MethodGet, "/v1/workspaces/"+teamID+"/invites", token, "")
	capture("member", http.StatusOK, http.MethodPatch, "/v1/workspaces/"+teamID+"/members/"+carol.ID, token,
		`{"role":"member"}`)
	// Two mailboxes ana links into the team: one with the team's consent to
	// sync it, given with the link, which bea reads too; one without, which
	// ana alone reads, so the listings mark her its last reader.
	shared := capture("", http.StatusCreated, http.MethodPost, "/v1/accounts", token,
		strings.TrimSuffix(h.passwordAccount(t, "support@mail.example"), "}")+
			fmt.Sprintf(`,"workspace_id":%q,"sync_consent_version":%q}`, teamID, service.DefaultSyncConsentVersion))
	sharedID, _ := shared["account"].(map[string]any)["id"].(string)
	capture("", http.StatusCreated, http.MethodPost, "/v1/accounts", token,
		strings.TrimSuffix(h.passwordAccount(t, "billing@mail.example"), "}")+fmt.Sprintf(`,"workspace_id":%q}`, teamID))
	capture("grant", http.StatusOK, http.MethodPut, "/v1/accounts/"+sharedID+"/access/"+beaID, token,
		`{"read":true,"act":false,"send":true,"manage":false}`)
	// A key of the team, which ana gives read on the mailbox she reads.
	capture("", http.StatusCreated, http.MethodPost, "/v1/workspaces/"+teamID+"/apikeys", token,
		fmt.Sprintf(`{"name":"Support bot","scope":"write","mailboxes":[{"account_id":%q,"read":true,"act":true,"send":false}],`+
			`"terms_version":%q}`, sharedID, service.DefaultKeyTermsVersion))
	capture("members", http.StatusOK, http.MethodGet, "/v1/workspaces/"+teamID+"/members", token, "")
	capture("access", http.StatusOK, http.MethodGet, "/v1/workspaces/"+teamID+"/access", token, "")

	// A person who signs in only through an identity provider, as an
	// extension of the daemon signs them in: no password to change, and a
	// session of the length the extension asked for.
	external, _, _, err := h.users.SignInExternal(t.Context(), auth.ExternalSignIn{
		Issuer: "https://accounts.example.com", Subject: "subject-of-cy", Email: "cy@example.com",
		EmailVerified: true, Name: "Cy Lima", TTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	capture("me_without_password", http.StatusOK, http.MethodGet, "/v1/auth/me", external, "")

	// An account not at its target (its address changed, say) is named the
	// target to derive under again, with the ticket that finishes it.
	dee := authtest.NewUser(t, h.store, "dee@example.com", auth.RoleMember)
	if _, err := h.store.Writer().ExecContext(t.Context(), `UPDATE users SET kdf_salt = ? WHERE id = ?`,
		bytes.Repeat([]byte{0x44}, 16), dee.ID); err != nil {
		t.Fatal(err)
	}
	capture("login_rederive", http.StatusOK, http.MethodPost, "/v1/auth/login", "",
		fmt.Sprintf(`{"email":"dee@example.com","auth_key":%q}`, authtest.AuthKey))
	// A person who signed up before the key scheme: the challenge says so,
	// in this release, and their password in clear proves a ticket to enrol
	// with, never a session.
	authtest.NewLegacyUser(t, h.store, "eve@example.com", auth.RoleMember)
	capture("challenge_upgrade", http.StatusOK, http.MethodPost, "/v1/auth/challenge", "", `{"email":"eve@example.com"}`)
	capture("upgrade_ticket", http.StatusOK, http.MethodPost, "/v1/auth/upgrade/login", "",
		fmt.Sprintf(`{"email":"eve@example.com","password":%q}`, authtest.Password))

	// A reset link answers the target the new password is derived under.
	resetCode, _, err := h.users.CreateReset(t.Context(), dee.ID, true, "cli")
	if err != nil {
		t.Fatal(err)
	}
	capture("reset_open", http.StatusOK, http.MethodPost, "/v1/auth/reset/open", "",
		fmt.Sprintf(`{"reset":%q,"email":"dee@example.com"}`, resetCode))

	// A plain session, as signing up, a reset, the upgrade's enrolment and
	// a password change answer it: here a password change, which ends
	// every session ana had, so it comes last.
	begun := capture("", http.StatusOK, http.MethodPost, "/v1/auth/password/begin", token,
		fmt.Sprintf(`{"current_auth_key":%q}`, authtest.AuthKey))
	ticket, _ := begun["ticket"].(string)
	capture("session", http.StatusOK, http.MethodPost, "/v1/auth/password/finish", token, jsonOf(t, map[string]any{
		"ticket": ticket, "auth_key": authtest.AuthKey, "kdf": defaultKDF(),
		"password_wrap": base64.RawURLEncoding.EncodeToString(authtest.Wrap(t)),
	}))
}

// capsWithoutUIDPlus is a server with MOVE and without UIDPLUS: a move
// comes back without COPYUID.
func capsWithoutUIDPlus() provider.Caps {
	caps := providertest.GmailCaps()
	caps.UIDPlus = false
	return caps
}

// contractMessage is the message the contract reads: a plain and an HTML
// body, and a PDF whose name is sent only in its RFC 2231 form.
var contractMessage = strings.ReplaceAll(`From: Bea Lima <bea@example.org>
To: Ana Lima <ana@work.example>
Cc: caio@example.org
Subject: Re: Lunch on Friday?
Message-ID: <menu-2@example.org>
In-Reply-To: <menu-1@work.example>
References: <menu-1@work.example>
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="mixed"

--mixed
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Friday works. The menu is attached =E2=80=94 see you at noon.
--alt
Content-Type: text/html; charset=utf-8

<p>Friday works. The menu is attached &mdash; see you at <b>noon</b>.</p>
--alt--
--mixed
Content-Type: application/pdf
Content-Disposition: attachment; filename*=utf-8''Card%C3%A1pio.pdf
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--mixed--
`, "\n", "\r\n")

func mustEvent(t *testing.T, typ events.Type, accountID string, at time.Time, payload any) events.Event {
	t.Helper()
	ev, err := events.New(typ, accountID, at, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// normalizer replaces what differs between runs — random ids, tokens, OAuth
// state, loopback ports — with fixed stand-ins of the same shape, and then
// writes or compares the fixture.
type normalizer struct {
	ids      map[string]string
	prefixes map[string]string
}

func newNormalizer() *normalizer {
	return &normalizer{ids: map[string]string{}, prefixes: map[string]string{}}
}

func (n *normalizer) check(t *testing.T, name string, value any) {
	t.Helper()
	if name == "" {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Readable URLs in the files; the escaping is JSON's business, not the
	// contract's.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(n.walk("", value, false)); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	path := filepath.Join(contractDir, name+".json")
	if *update {
		if err := os.MkdirAll(contractDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run with -update to write it)", path, err)
	}
	if !bytes.Equal(stored, body) {
		t.Errorf("%s no longer matches what the handler answers; if the change is intended, run\n"+
			"  go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update\n"+
			"and let the console's contract test tell you what to update.\ngot:\n%s", path, body)
	}
}

// walk rebuilds value with the random parts fixed. inFlow says the current
// object is an AuthFlow, whose "state" is the random OAuth state rather than
// an account's state.
func (n *normalizer) walk(key string, value any, inFlow bool) any {
	switch v := value.(type) {
	case map[string]any:
		_, flow := v["flow"]
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = n.walk(k, item, flow)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = n.walk(key, item, false)
		}
		return out
	case string:
		return n.normalizeString(key, v, inFlow)
	case float64:
		if key == "database_bytes" {
			// The file's size depends on SQLite's page use, not on the
			// handler; that it is a number of bytes is the contract.
			return float64(1 << 20)
		}
	}
	return value
}

func (n *normalizer) normalizeString(key, v string, inFlow bool) string {
	switch key {
	case "token":
		return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	case "ticket":
		return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x54}, 32))
	case "seal_id":
		return n.fixed("seal", v, func(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", i) })
	case "public_key":
		// A fresh key per person: stable, and still 32 bytes of base64url.
		return n.fixed("public_key", v, func(i int) string {
			return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(0x30 + i)}, 32))
		})
	case "password_wrap", "recovery_wrap":
		// 61 bytes starting with 0x02, as every wrap.
		return base64.RawURLEncoding.EncodeToString(append([]byte{0x02}, bytes.Repeat([]byte{0x57}, 60)...))
	case "state":
		if inFlow {
			return "fixed-oauth-state"
		}
	case "id", "account_id", "account_ids", "workspace_id", "linked_by", "user_id", "granted_by", "created_by",
		"enabled_by", "last_reader_of":
		return n.id(v)
	case "message_id":
		// The Message-ID a send generates: a random UUID at the sender's
		// domain. Message-IDs read from mail are fixed and left alone.
		if uuid, domain, ok := strings.Cut(v, "@"); ok && generatedID.MatchString(uuid) {
			return "00000000-0000-4000-8000-000000000001@" + domain
		}
	case "prefix":
		return n.prefix(v)
	case "key":
		// A new API key: its random prefix, the one its listing shows, and
		// a secret of the same shape.
		prefix, _, ok := strings.Cut(v, ".")
		if !ok {
			return v
		}
		return n.prefix(prefix) + "." + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x4b}, 32))
	case "auth_url":
		return fixURL(v, func(q url.Values) {
			q.Set("state", "fixed-oauth-state")
			q.Set("code_challenge", "fixed-pkce-challenge")
			// Only the loopback listener's port is random; the web flow's
			// redirect is the configured origin, port and all.
			if redirect, err := url.Parse(q.Get("redirect_uri")); err == nil && redirect.Path == "/oauth/callback" {
				redirect.Host = redirect.Hostname() + ":53682"
				q.Set("redirect_uri", redirect.String())
			}
		})
	case "url":
		// The invite code lives in the fragment.
		u, err := url.Parse(v)
		if err != nil {
			return v
		}
		fragment, err := url.ParseQuery(u.Fragment)
		if err != nil {
			return v
		}
		u.Fragment = ""
		return u.String() + "#invite=" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x49}, 32)) +
			"&email=" + url.QueryEscape(fragment.Get("email"))
	}
	return v
}

// generatedID is the UUID a send puts before the @ of its Message-ID.
var generatedID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// id maps each random id to a stable one with the same prefix, consistently
// across the fixtures of one run.
func (n *normalizer) id(v string) string {
	if fixed, ok := n.ids[v]; ok {
		return fixed
	}
	prefix, _, ok := strings.Cut(v, "_")
	if !ok {
		return v
	}
	count := 1
	for _, fixed := range n.ids {
		if strings.HasPrefix(fixed, prefix+"_") {
			count++
		}
	}
	fixed := fmt.Sprintf("%s_%016x", prefix, count)
	n.ids[v] = fixed
	return fixed
}

// fixed maps each random value of a kind to a stable one that make writes
// from its rank, consistently across the fixtures of one run.
func (n *normalizer) fixed(kind, v string, make func(int) string) string {
	if fixed, ok := n.ids[kind+":"+v]; ok {
		return fixed
	}
	count := 1
	for k := range n.ids {
		if strings.HasPrefix(k, kind+":") {
			count++
		}
	}
	fixed := make(count)
	n.ids[kind+":"+v] = fixed
	return fixed
}

// prefix maps each random API key prefix to a stable one, as id does ids.
func (n *normalizer) prefix(v string) string {
	if fixed, ok := n.prefixes[v]; ok {
		return fixed
	}
	fixed := fmt.Sprintf("%08x", 0x0a0b0c00+len(n.prefixes)+1)
	n.prefixes[v] = fixed
	return fixed
}

func fixURL(raw string, fix func(url.Values)) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	fix(q)
	u.RawQuery = q.Encode()
	return u.String()
}
