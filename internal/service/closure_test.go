package service_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// exec runs a statement against the fixture's database, for the rows no use
// case writes yet: folders, messages, events, webhooks.
func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Writer().ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(query)[0], err)
	}
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// issue makes a key and returns its prefix and its presented form.
func (f *fixture) issue(t *testing.T, userID string, accounts ...string) (string, string) {
	t.Helper()
	// A key acting as a person is one they created, agreeing to the key terms.
	terms := ""
	if userID != "" {
		terms = service.DefaultKeyTermsVersion
	}
	secret, key, err := f.keys.Issue(t.Context(), auth.NewKeyRequest{
		Name: "closure test", Scope: auth.ScopeRead, UserID: userID, AccountIDs: accounts, TermsVersion: terms,
	})
	if err != nil {
		t.Fatal(err)
	}
	return key.Prefix, secret
}

// seedMailbox writes what sync will one day write for an account: a folder, a
// message with a part and a body, a draft, a send and an event.
func (f *fixture) seedMailbox(t *testing.T, accountID, words string) int64 {
	t.Helper()
	now := time.Now().Unix()
	var folder int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO folders(account_id, name, display_name) VALUES (?, 'INBOX', 'Inbox') RETURNING id`,
		accountID).Scan(&folder); err != nil {
		t.Fatal(err)
	}
	var msg int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO messages(account_id, folder_id, uidvalidity, uid, group_key, subject, body_text,
		 internal_date, first_seen_at, updated_at) VALUES (?, ?, 1, 1, ?, ?, ?, ?, ?, ?) RETURNING id`,
		accountID, folder, "h:"+words, words, words, now, now, now).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO parts(msg_id, path, mime_type) VALUES (?, '1', 'text/plain')`, msg)
	f.exec(t, `INSERT INTO bodies(msg_id, headers_raw, size, fetched_at, last_access_at) VALUES (?, x'00', 1, ?, ?)`,
		msg, now, now)
	f.exec(t, `INSERT INTO drafts(id, account_id, compose_json, compose_hash, message_id_hdr, state, created_at,
		updated_at, expires_at) VALUES (?, ?, '{}', 'h', 'm@x', 'open', ?, ?, ?)`,
		"drf_"+accountID[4:], accountID, now, now, now)
	f.exec(t, `INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_at,
		updated_at) VALUES (?, 'k', 'h', 'm@x', 'sent', ?, ?)`, accountID, now, now)
	var seq int64
	if err := f.db.Writer().QueryRowContext(t.Context(),
		`INSERT INTO events(type, account_id, payload_json, created_at) VALUES ('message.new', ?, '{}', ?) RETURNING seq`,
		accountID, now).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func (f *fixture) webhook(t *testing.T, id string, accounts []string, events ...int64) {
	t.Helper()
	list, err := json.Marshal(accounts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	f.exec(t, `INSERT INTO webhooks(id, url, secret_ciphertext, keyid, accounts_json, created_at, updated_at)
		VALUES (?, 'https://hooks.example/x', x'00', 1, ?, ?, ?)`, id, string(list), now, now)
	for _, seq := range events {
		f.exec(t, `INSERT INTO webhook_deliveries(webhook_id, event_seq, next_attempt_at, status, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?)`, id, seq, now, now, now)
	}
}

// tables lists every table in the schema, the full-text index's own tables
// included.
func (f *fixture) tables(t *testing.T) []string {
	t.Helper()
	tables, err := f.db.Reader().QueryContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tables.Close() }()
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "users") || !slices.Contains(names, "webhook_deliveries") {
		t.Fatalf("the schema scan found only %v", names)
	}
	return names
}

// onDisk reports each needle still present, live or not, in the database file
// or its write-ahead log.
func (f *fixture) onDisk(t *testing.T, needles ...string) []string {
	t.Helper()
	var found []string
	for _, file := range []string{f.db.Path(), f.db.Path() + "-wal"} {
		raw, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		for _, needle := range needles {
			if n := bytes.Count(raw, []byte(needle)); n > 0 {
				found = append(found, fmt.Sprintf("%s holds %q %d times", filepath.Base(file), needle, n))
			}
		}
	}
	return found
}

// mentions lists every row, in every table the migrations made, that holds
// any of needles in any column.
func (f *fixture) mentions(t *testing.T, needles []string) []string {
	t.Helper()
	names := f.tables(t)

	var found []string
	for _, name := range names {
		found = append(found, f.mentionsIn(t, name, needles)...)
	}
	return found
}

// mentionsIn is mentions for one table.
func (f *fixture) mentionsIn(t *testing.T, name string, needles []string) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), `SELECT * FROM "`+name+`"`)
	if err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			var text string
			switch v := v.(type) {
			case string:
				text = v
			case []byte:
				text = string(v)
			default:
				continue
			}
			for _, needle := range needles {
				if strings.Contains(strings.ToLower(text), strings.ToLower(needle)) {
					found = append(found, fmt.Sprintf("%s.%s holds %q", name, columns[i], needle))
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestDeletingAPersonLeavesNoRowThatNamesThemOrTheirMailboxes(t *testing.T) {
	f := newFixture(t)
	bob := f.person(t, "bob@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	if _, err := f.users.SetName(t.Context(), ana.UserID, "Ana Quaresma"); err != nil {
		t.Fatal(err)
	}
	anaHome := f.mailbox(t, ana, "ana@mail.example")
	anaWork := f.mailbox(t, ana, "ana.work@mail.example")
	bobs := f.mailbox(t, bob, "bob@mail.example")
	shared := f.mailbox(t, admin(), "shared@mail.example")

	// A team bob owns, where ana administers, reads bob's team mailbox, and
	// gave dan access to it; and a team nobody but ana is in.
	workspaces := workspace.NewRepository(f.db, nil)
	support, err := workspaces.CreateTeam(t.Context(), "Support", bob.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	dan := f.person(t, "dan@example.com", auth.RoleMember)
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		if err := workspaces.AddMemberTx(t.Context(), tx, support.ID, ana.UserID, workspace.RoleAdmin, time.Now()); err != nil {
			return err
		}
		return workspaces.AddMemberTx(t.Context(), tx, support.ID, dan.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	teamBox, err := f.repo.Create(t.Context(), account.Account{ID: "acc_00000000000000aa", Email: "team@mail.example",
		Provider: "imap", AuthKind: "password", IMAPHost: "h", IMAPPort: 993, SMTPHost: "h", SMTPPort: 465,
		SMTPTLS: "implicit", LoginUser: "team", OwnerUserID: bob.UserID, WorkspaceID: support.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.SetGrant(t.Context(), teamBox.ID, ana.UserID, workspace.Flags{Read: true}, bob.UserID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaces.SetGrant(t.Context(), teamBox.ID, dan.UserID, workspace.Flags{Manage: true}, ana.UserID, nil); err != nil {
		t.Fatal(err)
	}
	anaAlone, err := workspaces.CreateTeam(t.Context(), "Ana alone", ana.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	anaPersonal, err := workspaces.PersonalOf(t.Context(), ana.UserID)
	if err != nil {
		t.Fatal(err)
	}

	anaEvent := f.seedMailbox(t, anaHome, "zebra quarterly")
	f.seedMailbox(t, anaWork, "zebra annual")
	bobEvent := f.seedMailbox(t, bobs, "giraffe weekly")

	anaKey, _ := f.issue(t, ana.UserID, anaHome)
	// Instance keys restricted to people's mailboxes, as a schema-7 server
	// may still hold: an instance key is made now only for the operator
	// workspace's, so their rows are written as the migration kept them.
	onlyAnas, onlyAnasSecret := f.issue(t, "", shared)
	mixed, mixedSecret := f.issue(t, "", shared)
	f.exec(t, `INSERT INTO api_key_accounts(key_prefix, account_id) VALUES (?, ?), (?, ?), (?, ?)`,
		onlyAnas, anaHome, mixed, anaHome, mixed, bobs)
	f.exec(t, `DELETE FROM api_key_accounts WHERE account_id = ?`, shared)

	// A consent attempt ana started on an account that is not hers (she was
	// an owner once), and one somebody else started on hers.
	expires := time.Now().Add(10 * time.Minute).Unix()
	f.exec(t, `INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, redirect_uri, expires_at)
		VALUES ('state-by-ana', ?, ?, 'web', 'https://c.example/oauth/return', ?)`, shared, ana.UserID, expires)
	f.exec(t, `INSERT INTO oauth_pending(state, account_id, flow, redirect_uri, expires_at)
		VALUES ('state-on-anas', ?, 'pasted', 'http://127.0.0.1:1/oauth/callback', ?)`, anaWork, expires)

	// What she sent from the operator's mailbox, when she was an owner: the
	// record stays with the mailbox and stops saying who asked.
	f.exec(t, `INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, created_by, user_id,
		created_at, updated_at) VALUES (?, 'k-ana', 'h', 'm@x', 'sent', ?, ?, ?, ?)`,
		shared, ana.UserID, ana.UserID, time.Now().Unix(), time.Now().Unix())
	// And its notice in the journal, which names its sender too.
	f.exec(t, `INSERT INTO events(type, account_id, payload_json, created_at) VALUES ('send.finished', ?, ?, ?)`,
		shared, `{"account_id":"`+shared+`","key":"k-ana","state":"sent","user_id":"`+ana.UserID+`"}`, time.Now().Unix())

	f.webhook(t, "whk_onlyanas", []string{anaHome}, anaEvent)
	f.webhook(t, "whk_mixed", []string{anaHome, bobs}, anaEvent, bobEvent)
	f.webhook(t, "whk_every", []string{}, anaEvent, bobEvent)

	now := time.Now().Unix()
	for _, inv := range []struct {
		hash                     string
		email, createdBy, usedBy string
		usedAt                   int64
	}{
		{"01", "ana@example.com", bob.UserID, ana.UserID, now}, // the one she signed up with
		{"02", "ana@example.com", "cli", "", 0},                // a second one, never used
		{"03", "carol@example.com", ana.UserID, "", 0},         // one she sent
	} {
		f.exec(t, `INSERT INTO invites(id, code_hash, email, role, created_by, created_at, expires_at, used_at, used_by)
			VALUES (?, ?, ?, 'member', ?, ?, ?, ?, ?)`, "inv_00000000000000"+inv.hash,
			[]byte(strings.Repeat(inv.hash, 32)[:32]), inv.email, inv.createdBy, now, now+3600, inv.usedAt, inv.usedBy)
	}

	// The scan below proves something only about tables that hold rows. A
	// table a later migration adds starts out empty here, and would pass
	// without anyone having decided what deleting a person does to it; this
	// makes that decision someone's.
	unseeded := map[string]string{
		"meta":             "instance settings; nothing about anyone",
		"attachment_blobs": "nothing writes it before phase 2, and then its rows and files need their own deletion",
	}
	for _, name := range f.tables(t) {
		if _, ok := unseeded[name]; !ok && f.count(t, `SELECT count(*) FROM "`+name+`"`) == 0 {
			t.Errorf("table %s holds no rows, so this test proves nothing about it: seed it for ana and "+
				"for someone else, or list it in unseeded with the reason it can hold nothing of hers", name)
		}
	}

	deleted, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "Ana@Example.com"})
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	want := service.DeletedUser{
		ID: ana.UserID, Email: "ana@example.com",
		AccountsRemoved: 2, SessionsDeleted: 1, KeysDeleted: 1, InvitesDeleted: 2,
		TeamsDeleted: 1, // "Ana alone", whose only member she was
	}
	if deleted != want {
		t.Errorf("deleted = %+v, want %+v", deleted, want)
	}

	needles := []string{
		ana.UserID, "ana@example.com", ana.SessionID, anaKey,
		anaHome, anaWork, "ana@mail.example", "ana.work@mail.example",
		"state-by-ana", "state-on-anas", anaPersonal.ID, anaAlone.ID, "Ana alone",
	}
	if found := f.mentions(t, needles); len(found) > 0 {
		t.Errorf("rows still name ana or her mailboxes:\n  %s", strings.Join(found, "\n  "))
	}
	// Gone from the files too, not only from the tables: what a deletion
	// frees would otherwise sit in free pages and in the write-ahead log
	// until SQLite happened to overwrite it.
	if found := f.onDisk(t, ana.UserID, "ana@example.com", "Ana Quaresma", "ana@mail.example", "ana.work@mail.example"); len(found) > 0 {
		t.Errorf("deleted, but still on disk:\n  %s", strings.Join(found, "\n  "))
	}
	if n := f.count(t, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'zebra'`); n != 0 {
		t.Errorf("the full-text index still finds %d of her messages", n)
	}

	// Everything that was not hers is still there.
	for what, n := range map[string]int{
		"bob":                              f.count(t, `SELECT count(*) FROM users WHERE id = ?`, bob.UserID),
		"bob's session":                    f.count(t, `SELECT count(*) FROM sessions WHERE user_id = ?`, bob.UserID),
		"bob's mailbox":                    f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, bobs),
		"the shared mailbox":               f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, shared),
		"bob's credentials":                f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, bobs),
		"bob's message":                    f.count(t, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'giraffe'`),
		"bob's event":                      f.count(t, `SELECT count(*) FROM events WHERE seq = ?`, bobEvent),
		"carol's invite":                   f.count(t, `SELECT count(*) FROM invites WHERE email = 'carol@example.com' AND created_by = ''`),
		"the mixed hook":                   f.count(t, `SELECT count(*) FROM webhooks WHERE id = 'whk_mixed' AND accounts_json = ?`, `["`+bobs+`"]`),
		"the hook for all":                 f.count(t, `SELECT count(*) FROM webhook_deliveries WHERE webhook_id = 'whk_every' AND event_seq = ?`, bobEvent),
		"bob's team":                       f.count(t, `SELECT count(*) FROM workspaces WHERE id = ?`, support.ID),
		"the team mailbox":                 f.count(t, `SELECT count(*) FROM accounts WHERE id = ?`, teamBox.ID),
		"the grant she gave dan, unsigned": f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ? AND manage AND granted_by = ''`, teamBox.ID, dan.UserID),
		"her send from the shared mailbox": f.count(t,
			`SELECT count(*) FROM sends WHERE account_id = ? AND idempotency_key = 'k-ana' AND user_id = '' AND created_by = ''`, shared),
		"its notice, unsigned": f.count(t, `SELECT count(*) FROM events WHERE type = 'send.finished' AND account_id = ?
			AND json_extract(payload_json, '$.key') = 'k-ana' AND json_extract(payload_json, '$.user_id') IS NULL`, shared),
	} {
		if n != 1 {
			t.Errorf("%s: %d rows, want 1", what, n)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM webhooks WHERE id = 'whk_onlyanas'`); n != 0 {
		t.Error("a webhook for her mailbox alone survived, and would now read as every account")
	}

	// A key that was limited to her mailbox alone is revoked, not widened to
	// every account; one that also reached bob's keeps only bob's.
	if _, err := f.svc.Authenticate(t.Context(), onlyAnasSecret, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("a key limited to a deleted mailbox still works: %v", err)
	}
	if n := f.count(t, `SELECT count(*) FROM api_keys WHERE prefix = ? AND revoked_at <> 0`, onlyAnas); n != 1 {
		t.Error("the key limited to her mailbox was not revoked")
	}
	p, err := f.svc.Authenticate(t.Context(), mixedSecret, nil)
	if err != nil {
		t.Fatalf("a key that also reached another mailbox stopped working: %v", err)
	}
	if !slices.Equal(p.AccountIDs, []string{bobs}) || p.KeyPrefix != mixed {
		t.Errorf("the mixed key is now restricted to %v, want only %s", p.AccountIDs, bobs)
	}
}

func TestDisablingAPersonEndsEveryWayTheyHadIn(t *testing.T) {
	f := newFixture(t)
	f.person(t, "owner@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	phone := authtest.SignIn(t, f.users, "ana@example.com")
	_, key := f.issue(t, ana.UserID)
	anas := f.mailbox(t, ana, "ana@mail.example")

	disabled, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"})
	if err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if disabled.ID != ana.UserID || disabled.SessionsEnded != 2 || disabled.KeysRevoked != 1 {
		t.Errorf("disabled = %+v, want two sessions ended and one key revoked", disabled)
	}
	for name, token := range map[string]string{"session": phone, "key": key} {
		if _, err := f.svc.Authenticate(t.Context(), token, nil); service.CodeOf(err) != service.CodeUnauthorized {
			t.Errorf("a disabled person's %s still works: %v", name, err)
		}
	}
	// Disabling is not deleting: the mailbox waits for the second step.
	if _, err := f.repo.Get(t.Context(), anas); err != nil {
		t.Errorf("disabling removed a mailbox: %v", err)
	}

	// Switching her back on revives nothing, the key included.
	if err := f.users.SetDisabled(t.Context(), ana.UserID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(t.Context(), key, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("re-enabling brought a revoked key back: %v", err)
	}

	again, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"})
	if err != nil || again.SessionsEnded != 0 || again.KeysRevoked != 0 {
		t.Errorf("disabling twice: %+v, %v", again, err)
	}
}

func TestTheLastActiveOwnerIsOnlyClosedOnPurpose(t *testing.T) {
	f := newFixture(t)
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	f.person(t, "member@example.com", auth.RoleMember)
	other := f.person(t, "other@example.com", auth.RoleOwner)
	if err := f.users.SetDisabled(t.Context(), other.UserID, true); err != nil {
		t.Fatal(err)
	}

	// Another owner exists, but disabled: nobody could administer the
	// instance from the console.
	req := service.CloseUserRequest{Email: "owner@example.com"}
	if _, err := f.svc.DisableUser(t.Context(), admin(), req); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("disabling the last active owner: %v, want conflict", err)
	}
	if _, err := f.svc.DeleteUser(t.Context(), admin(), req); service.CodeOf(err) != service.CodeConflict {
		t.Errorf("deleting the last active owner: %v, want conflict", err)
	}
	if _, err := f.users.Get(t.Context(), owner.UserID); err != nil {
		t.Fatalf("a refused deletion still deleted: %v", err)
	}
	// The disabled owner is not the last active one: that is still the
	// first, so she goes without force.
	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "other@example.com"}); err != nil {
		t.Errorf("deleting a disabled owner while another is active: %v", err)
	}

	req.Force = true
	if _, err := f.svc.DisableUser(t.Context(), admin(), req); err != nil {
		t.Errorf("disabling with force: %v", err)
	}
	if _, err := f.svc.DeleteUser(t.Context(), admin(), req); err != nil {
		t.Errorf("deleting with force: %v", err)
	}
	// Members are never the last owner.
	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "member@example.com"}); err != nil {
		t.Errorf("deleting a member: %v", err)
	}
}

func TestOnlyTheOperatorOrAnInstanceOwnerSignedInClosesAnAccount(t *testing.T) {
	f := newFixture(t)
	owner := f.person(t, "owner@example.com", auth.RoleOwner)
	f.person(t, "keeper@example.com", auth.RoleOwner)
	member := f.person(t, "member@example.com", auth.RoleMember)
	f.person(t, "ana@example.com", auth.RoleMember)
	req := service.CloseUserRequest{Email: "ana@example.com"}

	for name, p := range map[string]service.Principal{
		"member":              member,
		"read key":            reader(),
		"restricted admin":    {KeyPrefix: "cccccccc", Scope: auth.ScopeAdmin, AccountIDs: []string{"acc_x"}},
		"key acting as owner": {KeyPrefix: "dddddddd", Scope: auth.ScopeAdmin, UserID: owner.UserID, UserRole: auth.RoleOwner},
	} {
		if _, err := f.svc.DisableUser(t.Context(), p, req); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s could disable: %v", name, err)
		}
		if _, err := f.svc.DeleteUser(t.Context(), p, req); service.CodeOf(err) != service.CodeNotAuthorized {
			t.Errorf("%s could delete: %v", name, err)
		}
	}
	if _, err := f.users.GetByEmail(t.Context(), "ana@example.com"); err != nil {
		t.Fatalf("a refused caller removed ana: %v", err)
	}
	// An owner of the instance signed in administers its people, never
	// herself: another owner, or the operator, closes her account.
	self := service.CloseUserRequest{Email: "owner@example.com"}
	if _, err := f.svc.DisableUser(t.Context(), owner, self); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("an owner disabling herself: %v", err)
	}
	if _, err := f.svc.DisableUser(t.Context(), owner, req); err != nil {
		t.Errorf("an owner signed in disabling a person: %v", err)
	}
	if _, err := f.svc.DeleteUser(t.Context(), owner, req); err != nil {
		t.Errorf("an owner signed in deleting a person: %v", err)
	}
	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "not an address"}); service.CodeOf(err) != service.CodeBadRequest {
		t.Errorf("a malformed address: %v", err)
	}
	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "nobody@example.com"}); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("disabling an unknown address: %v", err)
	}
}

func TestDeletingAnAddressThatWasOnlyInvitedDeletesItsInvites(t *testing.T) {
	// The policy keeps an invitee's address from the moment they are
	// invited, so an erasure request can come from someone with no account.
	f := newFixture(t)
	for range 2 {
		if _, _, err := f.users.CreateInvite(t.Context(), auth.NewInvite{Email: "bea@example.com", Role: auth.RoleMember, CreatedBy: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.users.CreateInvite(t.Context(), auth.NewInvite{Email: "cid@example.com", Role: auth.RoleMember, CreatedBy: "cli"}); err != nil {
		t.Fatal(err)
	}

	deleted, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "bea@example.com"})
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if deleted.ID != "" || deleted.InvitesDeleted != 2 || deleted.AccountsRemoved != 0 {
		t.Errorf("deleted = %+v, want two invites and no account", deleted)
	}
	if found := f.mentions(t, []string{"bea@example.com"}); len(found) > 0 {
		t.Errorf("rows still name her: %v", found)
	}
	if n := f.count(t, `SELECT count(*) FROM invites WHERE email = 'cid@example.com'`); n != 1 {
		t.Errorf("somebody else's invite went too")
	}
	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "bea@example.com"}); service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("deleting again: %v, want not_found", err)
	}
}

func TestClosingAnAccountStopsTheConsentItsOwnerLeftWaiting(t *testing.T) {
	// A listener left open for a person who is gone, or switched off, would
	// still take a redirect and answer it.
	f, _ := consentFixture(t, "")
	keeper := f.person(t, "keeper@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	bea := f.person(t, "bea@example.com", auth.RoleMember)
	// A team keeper links a mailbox in, which ana and bea manage too.
	workspaces := workspace.NewRepository(f.db, nil)
	team, err := workspaces.CreateTeam(t.Context(), "Support", keeper.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []service.Principal{ana, bea} {
		if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
			return workspaces.AddMemberTx(t.Context(), tx, team.ID, p.UserID, workspace.RoleMember, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}

	listening := func(flow *service.AuthFlow) string {
		t.Helper()
		redirect, err := url.Parse(redirectURIOf(t, flow.AuthURL))
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.Dial("tcp", redirect.Host)
		if err != nil {
			t.Fatalf("the listener was not up to begin with: %v", err)
		}
		_ = conn.Close()
		return redirect.Host
	}
	closed := func(host, what string) {
		t.Helper()
		if conn, err := net.Dial("tcp", host); err == nil {
			_ = conn.Close()
			t.Errorf("%s still answers", what)
		}
	}

	own, err := f.svc.AddAccount(t.Context(), ana, service.AddAccountRequest{Email: "ana@gmail.com", Flow: "loopback"})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := f.svc.AddAccount(t.Context(), keeper, service.AddAccountRequest{
		Email: "team@gmail.com", Flow: "loopback", WorkspaceID: team.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []service.Principal{ana, bea} {
		if _, err := workspaces.SetGrant(t.Context(), shared.Account.ID, p.UserID,
			workspace.Flags{Read: true, Manage: true}, keeper.UserID, nil); err != nil {
			t.Fatal(err)
		}
	}
	onShared, err := f.svc.StartOAuth(t.Context(), ana, shared.Account.ID, "loopback")
	if err != nil {
		t.Fatal(err)
	}
	ownHost, sharedHost := listening(own.Auth), listening(onShared)

	if _, err := f.svc.DeleteUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	closed(ownHost, "the listener for her deleted mailbox")
	closed(sharedHost, "the listener for her attempt on the team's mailbox")
	if _, err := f.repo.Get(t.Context(), shared.Account.ID); err != nil {
		t.Errorf("the team's mailbox went with her: %v", err)
	}

	beas, err := f.svc.StartOAuth(t.Context(), bea, shared.Account.ID, "loopback")
	if err != nil {
		t.Fatal(err)
	}
	beaHost := listening(beas)
	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	closed(beaHost, "the listener of a disabled person")
	if n := f.count(t, `SELECT count(*) FROM oauth_pending WHERE owner_user_id = ?`, bea.UserID); n != 0 {
		t.Errorf("a disabled person's attempt is still pending")
	}
}

func TestRemovingTheOnlyMailboxAKeyWasLimitedToRevokesTheKey(t *testing.T) {
	// A key's restriction is its rows in api_key_accounts, and a key with no
	// rows reaches every account. The rows cascade away with the account.
	f := newFixture(t)
	a := f.mailbox(t, admin(), "a@mail.example")
	b := f.mailbox(t, admin(), "b@mail.example")
	_, onlyA := f.issue(t, "", a)
	_, both := f.issue(t, "", a, b)

	if err := f.svc.RemoveAccount(t.Context(), admin(), a); err != nil {
		t.Fatal(err)
	}
	if p, err := f.svc.Authenticate(t.Context(), onlyA, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Fatalf("a key limited to a removed mailbox now reaches %v (err %v)", ids(t, f, p), err)
	}
	p, err := f.svc.Authenticate(t.Context(), both, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, f, p); !slices.Equal(got, []string{b}) {
		t.Errorf("the key limited to both now sees %v, want only %s", got, b)
	}
}

func TestARemovedMailboxLeavesNeitherItsSettingsNorItsCredentialsOnDisk(t *testing.T) {
	// The policy: removing a mailbox deletes its settings and credentials
	// from the database, which has to include the file's free pages and
	// the write-ahead log.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	gone := f.mailbox(t, ana, "leaving.soon@mail.example")
	kept := f.mailbox(t, ana, "staying@mail.example")
	var sealed []byte
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT ciphertext FROM credentials WHERE account_id = ?`, gone).Scan(&sealed); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.RemoveAccount(t.Context(), ana, gone); err != nil {
		t.Fatal(err)
	}
	if found := f.onDisk(t, gone, "leaving.soon@mail.example", string(sealed)); len(found) > 0 {
		t.Errorf("removed, but still on disk:\n  %s", strings.Join(found, "\n  "))
	}
	if _, err := f.repo.Get(t.Context(), kept); err != nil {
		t.Errorf("the other mailbox went too: %v", err)
	}
}

func TestARequestAlreadyRunningWhenItsPersonIsDisabledCannotAnswer(t *testing.T) {
	// The transports re-check the caller just before writing a response.
	// Disabling has to fail that check for a request that authenticated a
	// moment earlier, by session or by a key acting as the person.
	f := newFixture(t)
	f.person(t, "owner@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	_, secret := f.issue(t, ana.UserID)
	byKey, err := f.svc.Authenticate(t.Context(), secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]service.Principal{"session": ana, "key": byKey} {
		if err := f.svc.Recheck(t.Context(), p); err != nil {
			t.Fatalf("%s before disabling: %v", name, err)
		}
	}

	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]service.Principal{"session": ana, "key": byKey} {
		if err := f.svc.Recheck(t.Context(), p); service.CodeOf(err) != service.CodeUnauthorized {
			t.Errorf("a %s request that began before she was disabled can still answer: %v", name, err)
		}
	}
}

func TestConsentFinishingAfterItsOwnerWasDisabledStoresNoGrant(t *testing.T) {
	// The person approved at the provider, the console posted the code, and
	// while the code was being exchanged the operator disabled her. Her
	// session is already gone, so she will never hear the answer; what
	// matters is that no grant is written for someone switched off.
	f, idp := consentFixture(t, "https://console.mailie.example")
	f.person(t, "owner@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	added, err := f.svc.AddAccount(t.Context(), ana, service.AddAccountRequest{Email: "ana@gmail.com"})
	if err != nil {
		t.Fatal(err)
	}
	var disabled error
	idp.onIssue = func(string) {
		_, disabled = f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"})
	}

	_, err = f.svc.CompleteOAuth(t.Context(), ana,
		"https://console.mailie.example/oauth/return?code=the-code&state="+stateOf(t, added.Auth))
	if disabled != nil {
		t.Fatalf("disabling during the exchange: %v", disabled)
	}
	if idp.exchangeCount() != 1 {
		t.Fatalf("the code was exchanged %d times; the race needs exactly one", idp.exchangeCount())
	}
	if service.CodeOf(err) != service.CodeNotFound {
		t.Errorf("completing: %v, want not_found, as for any attempt a disabled person left", err)
	}
	if n := f.count(t, `SELECT count(*) FROM credentials WHERE account_id = ?`, added.Account.ID); n != 0 {
		t.Errorf("a grant was stored for a disabled person")
	}
	if a := f.stateOfAccount(t, added.Account.ID); a.State == account.StateActive {
		t.Errorf("the account became active for a disabled person")
	}
}

func TestAMailboxAddedByARequestThatOutlivedItsPersonIsNotStored(t *testing.T) {
	// ana's request authenticated, then spent seconds logging in to her mail
	// server, and she was disabled meanwhile. The principal it carries is
	// the one from before; the account must not be created for her anyway.
	f := newFixture(t)
	f.person(t, "owner@example.com", auth.RoleOwner)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	if _, err := f.svc.DisableUser(t.Context(), admin(), service.CloseUserRequest{Email: "ana@example.com"}); err != nil {
		t.Fatal(err)
	}

	_, err := f.svc.AddAccount(t.Context(), ana, f.passwordAccount(t, "late@mail.example"))
	if service.CodeOf(err) != service.CodeUnauthorized {
		t.Errorf("adding for a disabled person: %v, want unauthorized", err)
	}
	if n := f.count(t, `SELECT count(*) FROM accounts WHERE email = 'late@mail.example'`); n != 0 {
		t.Errorf("an account was created for a disabled person")
	}
	// The instance's own mailboxes have no person to check.
	if _, err := f.svc.AddAccount(t.Context(), admin(), f.passwordAccount(t, "team@mail.example")); err != nil {
		t.Errorf("an instance key adding a mailbox: %v", err)
	}
}
