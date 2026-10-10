package service_test

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// browserKey is a mailbox key pair as a person's browser makes one
// (docs/key-scheme.md section 8): the private key stays here, and only its
// public half, its namespace and grants sealed of it go to the server.
type browserKey struct {
	private   []byte
	public    []byte
	namespace string
	epoch     int
}

func newBrowserKey(t *testing.T, namespace string, epoch int) browserKey {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if namespace == "" {
		namespace = keyscheme.NewSealID()
	}
	return browserKey{private: key.Bytes(), public: key.PublicKey().Bytes(), namespace: namespace, epoch: epoch}
}

// sealTo seals the mailbox's private key to a person's account public key,
// bound by their seal id, as the kit's grant (section 9.1).
func (k browserKey) sealTo(t *testing.T, f *fixture, userID string) string {
	t.Helper()
	user, err := f.users.Get(t.Context(), userID)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := keyscheme.SealGrant(user.PublicKey, k.namespace, user.SealID, k.epoch, k.private)
	if err != nil {
		t.Fatalf("seal a grant to %s: %v", userID, err)
	}
	return b64(grant)
}

// link is req as the linker's browser sends it, with this key pair and their
// own grant.
func (k browserKey) link(t *testing.T, f *fixture, p service.Principal, req service.AddAccountRequest) service.AddAccountRequest {
	t.Helper()
	req.PublicKey, req.Namespace, req.Grant = b64(k.public), k.namespace, k.sealTo(t, f, p.UserID)
	return req
}

// stale makes a session's step-up eleven minutes old.
func (f *fixture) stale(t *testing.T, p service.Principal) {
	t.Helper()
	f.exec(t, `UPDATE sessions SET authenticated_at = authenticated_at - 660 WHERE id = ?`, p.SessionID)
}

// stepUp proves the session's person again, as the console's dialog does.
func (f *fixture) stepUp(t *testing.T, p service.Principal) {
	t.Helper()
	if _, err := f.svc.StepUp(t.Context(), p, service.StepUpRequest{AuthKey: authtest.AuthKey}); err != nil {
		t.Fatalf("step up: %v", err)
	}
}

// keyedTeamMailbox links a password mailbox into the team as Ana, with its
// key, and returns its id and the key.
func (tm supportTeam) keyedTeamMailbox(t *testing.T, f *fixture, email string) (string, browserKey) {
	t.Helper()
	key := newBrowserKey(t, "", 1)
	req := f.passwordAccount(t, email)
	req.WorkspaceID = tm.id
	added, err := f.svc.AddAccount(t.Context(), tm.ana, key.link(t, f, tm.ana, req))
	if err != nil {
		t.Fatalf("linking %s: %v", email, err)
	}
	return added.Account.ID, key
}

// accessOf is what a person's own listing says of a mailbox.
func accessOf(t *testing.T, f *fixture, p service.Principal, accountID string) service.Account {
	t.Helper()
	a, err := f.svc.GetAccount(t.Context(), p, accountID)
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	return a
}

// external is a person who signs in only through an identity provider, with
// no account key yet.
func (f *fixture) external(t *testing.T, subject, email string) service.Principal {
	t.Helper()
	session, err := f.svc.SignInExternal(t.Context(), vouched(subject, email))
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.users.AuthenticateSession(t.Context(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// readFlag is a PUT of read alone, with grant when not empty.
func readFlag(grant string) service.GrantRequest {
	req := grantRequest(true, false, false, false)
	req.Grant = grant
	return req
}

// holding lists the columns of a table's rows, by name, whose text or bytes
// contain any of needles exactly.
func (f *fixture) holding(t *testing.T, table string, needles [][]byte) []string {
	t.Helper()
	rows, err := f.db.Reader().QueryContext(t.Context(), `SELECT * FROM "`+table+`"`)
	if err != nil {
		t.Fatal(err)
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
			var raw []byte
			switch v := v.(type) {
			case string:
				raw = []byte(v)
			case []byte:
				raw = v
			default:
				continue
			}
			for _, needle := range needles {
				if bytes.Contains(raw, needle) {
					found = append(found, table+"."+columns[i])
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestAPersonLinksAMailboxWithItsKeyOnThePasswordAndTheOAuthPaths(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	for _, c := range []struct {
		what string
		req  service.AddAccountRequest
	}{
		{"the password form", f.passwordAccount(t, "ana@mail.example")},
		{"the OAuth flow", service.AddAccountRequest{Email: "ana@gmail.com", Flow: "loopback"}},
	} {
		key := newBrowserKey(t, "", 1)
		added, err := f.svc.AddAccount(t.Context(), ana, key.link(t, f, ana, c.req))
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		shown := added.Account
		want := service.MailboxKeyPair{Epoch: 1, PublicKey: b64(key.public), Namespace: key.namespace}
		if shown.MailboxKey == nil || *shown.MailboxKey != want || !shown.Access.Read || shown.Access.WaitingKey {
			t.Errorf("%s: the link answered %+v and %+v, want the key %+v read", c.what, shown.MailboxKey, shown.Access, want)
		}
		state, err := f.svc.MailboxKey(t.Context(), ana, shown.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.Epoch != 1 || state.Namespace != key.namespace || state.Grant == "" {
			t.Errorf("%s: the mailbox key reads %+v", c.what, state)
		}
		// Her own grant opens with her account key to the mailbox's: what
		// her browser checks (section 9.2) is what was stored.
		if got := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND user_id = ? AND epoch = 1`,
			shown.ID, ana.UserID); got != 1 {
			t.Errorf("%s: %d grants of the linker's", c.what, got)
		}
	}
	// Resuming the OAuth link carries no key: the row carries the one its
	// link wrote.
	accounts, err := f.svc.ListAccounts(t.Context(), ana, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accounts {
		if a.State != string(account.StatePendingAuth) {
			continue
		}
		if _, err := f.svc.StartOAuth(t.Context(), ana, a.ID, "loopback"); err != nil {
			t.Fatal(err)
		}
		if f.count(t, `SELECT count(*) FROM mailbox_keys WHERE account_id = ?`, a.ID) != 1 {
			t.Error("resuming a link wrote another key")
		}
	}
}

func TestALinkWithoutItsKeyOrWithAMalformedOneStoresNothing(t *testing.T) {
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	good := func() service.AddAccountRequest {
		return newBrowserKey(t, "", 1).link(t, f, ana, f.passwordAccount(t, "ana@mail.example"))
	}
	for what, change := range map[string]func(*service.AddAccountRequest){
		"no key at all":           func(r *service.AddAccountRequest) { r.PublicKey, r.Namespace, r.Grant = "", "", "" },
		"no grant":                func(r *service.AddAccountRequest) { r.Grant = "" },
		"no namespace":            func(r *service.AddAccountRequest) { r.Namespace = "" },
		"a short public key":      func(r *service.AddAccountRequest) { r.PublicKey = b64(make([]byte, 31)) },
		"a public key of nothing": func(r *service.AddAccountRequest) { r.PublicKey = b64(make([]byte, 32)) },
		"an uppercase namespace":  func(r *service.AddAccountRequest) { r.Namespace = "9D035F2B-81D0-420E-90E2-BB16E950497B" },
		"a grant at epoch 2":      func(r *service.AddAccountRequest) { r.Grant = b64(grantAt(2)) },
		"a grant of Wappie's":     func(r *service.AddAccountRequest) { g := grantAt(1); g[0], g[1] = 'W', 'S'; r.Grant = b64(g) },
		"a grant padded":          func(r *service.AddAccountRequest) { r.Grant = base64.URLEncoding.EncodeToString(grantAt(1)) },
	} {
		req := good()
		change(&req)
		_, err := f.svc.AddAccount(t.Context(), ana, req)
		wantCode(t, what, err, service.CodeBadRequest)
	}
	// An instance key links an operator mailbox, which has no key.
	_, err := f.svc.AddAccount(t.Context(), admin(), good())
	wantCode(t, "an instance key's link with a key", err, service.CodeBadRequest)
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Errorf("%d mailboxes stored by refused links", n)
	}
	// A namespace another mailbox uses is a conflict, found in the
	// transaction that would store it.
	taken := good()
	if _, err := f.svc.AddAccount(t.Context(), ana, taken); err != nil {
		t.Fatal(err)
	}
	again := newBrowserKey(t, taken.Namespace, 1).link(t, f, ana, f.passwordAccount(t, "ana.work@mail.example"))
	_, err = f.svc.AddAccount(t.Context(), ana, again)
	wantCode(t, "a namespace another mailbox uses", err, service.CodeConflict)
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 1 {
		t.Errorf("%d mailboxes after a refused namespace, want the first only", n)
	}
}

func TestAPersonWithoutAnAccountKeyCannotLinkAMailbox(t *testing.T) {
	// Signed in through an identity provider, with no account key yet:
	// nothing could be sealed to them, so nothing is linked.
	f := newFixture(t)
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	_, err := f.svc.AddAccount(t.Context(), cy, keyed(cy, service.AddAccountRequest{Email: "cy@gmail.com", Flow: "loopback"}))
	wantCode(t, "linking without an account key", err, service.CodeConflict)
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Errorf("%d mailboxes stored", n)
	}
}

func TestReadOnAKeyedMailboxIsGivenWithTheRecipientsGrantAndRefusedWithout(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	give := func(req service.GrantRequest) error {
		_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, tm.bea.UserID, req)
		return err
	}
	wantCode(t, "read without Bea's grant", give(readFlag("")), service.CodeBadRequest)
	wantCode(t, "read with a grant of another epoch", give(readFlag(b64(grantAt(2)))), service.CodeConflict)
	wantCode(t, "read with a malformed grant", give(readFlag("not-a-grant")), service.CodeBadRequest)
	if g := accessOf(t, f, tm.ana, shared); g.ID == "" {
		t.Fatal("Ana lost the mailbox")
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id = ?`, shared, tm.bea.UserID); n != 0 {
		t.Fatalf("a refused grant stored Bea's flags")
	}

	if err := give(readFlag(key.sealTo(t, f, tm.bea.UserID))); err != nil {
		t.Fatalf("read with Bea's grant: %v", err)
	}
	if a := accessOf(t, f, tm.bea, shared); !a.Access.Read || a.Access.WaitingKey {
		t.Errorf("Bea, given read with her grant: %+v", a.Access)
	}
	if g := held(t, f, tm.ana, tm.id, shared, tm.bea.UserID); !g.Read || !g.Sealed {
		t.Errorf("the directory lists Bea as %+v", g)
	}
	// A grant comes only with the read it gives: Bea holds read now.
	wantCode(t, "a grant with read already held", give(readFlag(key.sealTo(t, f, tm.bea.UserID))), service.CodeBadRequest)
	// Taking read takes the grant, and giving it again needs a new one.
	if err := f.svc.RevokeAccess(t.Context(), tm.ana, shared, tm.bea.UserID, workspace.Flags{Read: true}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, tm.bea.UserID); n != 0 {
		t.Errorf("%d grants of Bea's after read was taken", n)
	}
	wantCode(t, "read again without a grant", give(readFlag("")), service.CodeBadRequest)
}

func TestReadIsGivenByTheFlagAloneToAMemberWithoutAnAccountKeyWhoThenWaitsForTheKey(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing is sealed to a person with no account key, and the flag alone
	// needs no step-up.
	_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, cy.UserID, readFlag(key.sealTo(t, f, tm.ana.UserID)))
	wantCode(t, "a grant for a person without an account key", err, service.CodeConflict)
	f.stale(t, tm.ana)
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, cy.UserID, readFlag("")); err != nil {
		t.Fatalf("read by the flag alone: %v", err)
	}
	a := accessOf(t, f, cy, shared)
	if a.Access.Read || !a.Access.WaitingKey || a.MailboxKey == nil {
		t.Errorf("Cy holds the flag without the key: %+v, key %+v", a.Access, a.MailboxKey)
	}
	_, err = f.svc.ListFolders(t.Context(), cy, shared)
	wantCode(t, "Cy listing the folders while she waits", err, service.CodeNotAuthorized)
	if g := held(t, f, tm.ana, tm.id, shared, cy.UserID); !g.Read || g.Sealed {
		t.Errorf("the directory lists Cy as %+v", g)
	}
	dir, err := f.svc.AccessDirectory(t.Context(), tm.ana, tm.id)
	if err != nil {
		t.Fatal(err)
	}
	if dir[0].Readers != 1 || dir[0].Epoch != 1 {
		t.Errorf("the directory counts %d readers at epoch %d, want Ana alone at 1", dir[0].Readers, dir[0].Epoch)
	}
}

func TestAMemberWaitingForTheKeyIsRefusedTheAccessRoutesNotToldTheMailboxIsMissing(t *testing.T) {
	// A member who holds the read flag and waits for the key sees the
	// mailbox's card, so changing who holds what on it is not_authorized
	// for them, as for any member who holds a grant; only someone who holds
	// nothing on it is told it does not exist.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, cy.UserID, readFlag("")); err != nil {
		t.Fatal(err)
	}
	if a := accessOf(t, f, cy, shared); a.Access.Read || a.Access.Act || !a.Access.WaitingKey {
		t.Fatalf("Cy holds the flag and waits for the key: %+v", a.Access)
	}
	for _, c := range []struct {
		who  string
		p    service.Principal
		want service.Code
	}{
		{"Cy (waiting for the key)", cy, service.CodeNotAuthorized},
		{"Bea (holding nothing)", tm.bea, service.CodeNotFound},
	} {
		_, err := f.svc.SetAccess(t.Context(), c.p, shared, tm.ana.UserID, grantRequest(true, false, false, false))
		wantCode(t, c.who+" setting Ana's access", err, c.want)
		err = f.svc.RevokeAccess(t.Context(), c.p, shared, tm.ana.UserID, workspace.Flags{Send: true})
		wantCode(t, c.who+" taking Ana's send", err, c.want)
	}
}

func TestOnlySomeoneWhoReadsAKeyedMailboxGivesReadOnIt(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	// Carol administers and reads nothing: she seals nothing, and passes on
	// no read.
	_, err := f.svc.SetAccess(t.Context(), carol, shared, dan.UserID, readFlag(key.sealTo(t, f, dan.UserID)))
	wantCode(t, "an admin who does not read giving read", err, service.CodeNotAuthorized)
	// Ana gives Carol the flag, which Carol cannot use without the key she
	// is not given: an admin waiting for the key passes on nothing either.
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, carol.UserID, tm.ana.UserID)
	if a := accessOf(t, f, carol, shared); a.Access.Read || !a.Access.WaitingKey {
		t.Fatalf("Carol with the flag and no key: %+v", a.Access)
	}
	_, err = f.svc.SetAccess(t.Context(), carol, shared, dan.UserID, readFlag(key.sealTo(t, f, dan.UserID)))
	wantCode(t, "an admin waiting for the key giving read", err, service.CodeNotAuthorized)
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, dan.UserID, readFlag(key.sealTo(t, f, dan.UserID))); err != nil {
		t.Fatalf("Ana, who reads it, giving Dan read: %v", err)
	}
}

func TestOnlySomeoneWhoReadsAKeyedMailboxGivesAKeyReadOnIt(t *testing.T) {
	// A key reads by what it holds and needs no grant: giving it read is
	// giving read, and passes only from a person who reads the mailbox now
	// by the one rule (docs/key-scheme.md section 12.13), never from one who
	// holds the flag and waits for the key.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, carol.UserID, tm.ana.UserID)
	if a := accessOf(t, f, carol, shared); a.Access.Read || !a.Access.WaitingKey {
		t.Fatalf("Carol with the flag and no key: %+v", a.Access)
	}
	_, err := f.svc.CreateWorkspaceKey(t.Context(), carol, tm.id, keyRequest("carol's", "read", reads(shared)))
	wantCode(t, "Carol, waiting for the key, creating a key that reads it", err, service.CodeNotAuthorized)
	sends, err := f.svc.CreateWorkspaceKey(t.Context(), carol, tm.id, keyRequest("carol's", "send",
		service.KeyMailboxRequest{AccountID: shared, Send: true}))
	if err != nil {
		t.Fatalf("Carol giving a key send: %v", err)
	}
	_, err = f.svc.SetKeyAccess(t.Context(), carol, tm.id, sends.Prefix, shared, keyAccess(true, false, true))
	wantCode(t, "Carol, waiting for the key, adding read to a key", err, service.CodeNotAuthorized)
	if n := f.count(t, `SELECT count(*) FROM key_access WHERE account_id = ? AND read = 1`, shared); n != 0 {
		t.Fatalf("%d keys read the mailbox after refused requests", n)
	}

	// Supplied the key, she reads it, and passes read on to a key.
	if _, err := f.svc.SupplyKey(t.Context(), tm.ana, shared, carol.UserID,
		service.SupplyKeyRequest{Epoch: 1, Grant: key.sealTo(t, f, carol.UserID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateWorkspaceKey(t.Context(), carol, tm.id, keyRequest("carol's reader", "read", reads(shared))); err != nil {
		t.Errorf("Carol, who reads it now, creating a key that reads it: %v", err)
	}
	if _, err := f.svc.SetKeyAccess(t.Context(), carol, tm.id, sends.Prefix, shared, keyAccess(true, false, true)); err != nil {
		t.Errorf("Carol, who reads it now, adding read to a key: %v", err)
	}
}

func TestAnyReaderSuppliesTheKeyToAMemberWhoWaitsForIt(t *testing.T) {
	f, engine := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, tm.bea.UserID, readFlag(key.sealTo(t, f, tm.bea.UserID))); err != nil {
		t.Fatal(err)
	}
	carol := tm.join(t, f, "carol@example.com", workspace.RoleMember)
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, carol.UserID, readFlag(key.sealTo(t, f, carol.UserID))); err != nil {
		t.Fatal(err)
	}
	// Carol's account key is reset: her grant goes, her flag stays.
	code, _, err := f.users.CreateReset(t.Context(), carol.UserID, false, "cli")
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.svc.CompleteReset(t.Context(), service.ResetRequest{Reset: code, Email: "carol@example.com",
		Enrolment: wireEnrolment(t)}, "authtest")
	if err != nil {
		t.Fatal(err)
	}
	carol, err = f.users.AuthenticateSession(t.Context(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a := accessOf(t, f, carol, shared); a.Access.Read || !a.Access.WaitingKey {
		t.Fatalf("Carol after her reset: %+v", a.Access)
	}
	theirs, err := f.svc.MailboxKey(t.Context(), carol, shared)
	if err != nil {
		t.Fatal(err)
	}
	var suppliers []string
	for _, s := range theirs.Suppliers {
		suppliers = append(suppliers, s.UserID)
	}
	slices.Sort(suppliers)
	want := []string{tm.ana.UserID, tm.bea.UserID}
	slices.Sort(want)
	if theirs.Grant != "" || !slices.Equal(suppliers, want) {
		t.Errorf("Carol's console reads %+v, want Ana and Bea as who can supply it", theirs)
	}
	bea, err := f.svc.MailboxKey(t.Context(), tm.bea, shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(bea.Waiting) != 1 || bea.Waiting[0].UserID != carol.UserID || bea.Waiting[0].PublicKey == "" {
		t.Errorf("Bea's console offers %+v, want Carol", bea.Waiting)
	}

	supply := func(p service.Principal, to string, epoch int, grant string) error {
		_, err := f.svc.SupplyKey(t.Context(), p, shared, to, service.SupplyKeyRequest{Epoch: epoch, Grant: grant})
		return err
	}
	// Carol, who waits, supplies nobody; a grant at another epoch is a
	// conflict; one who holds no flag is given no key; a person outside the
	// team does not exist for it.
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	wantCode(t, "Carol supplying herself", supply(carol, carol.UserID, 1, b64(grantAt(1))), service.CodeNotAuthorized)
	wantCode(t, "a grant at epoch 2", supply(tm.bea, carol.UserID, 2, b64(grantAt(2))), service.CodeConflict)
	wantCode(t, "a grant of another epoch than it names", supply(tm.bea, carol.UserID, 1, b64(grantAt(2))), service.CodeBadRequest)
	wantCode(t, "Dan, who holds no flag", supply(tm.bea, dan.UserID, 1, key.sealTo(t, f, dan.UserID)), service.CodeConflict)
	eve := f.person(t, "eve@example.com", auth.RoleMember)
	wantCode(t, "Eve, outside the team", supply(tm.bea, eve.UserID, 1, b64(grantAt(1))), service.CodeNotFound)
	wantCode(t, "an API key", supply(admin(), carol.UserID, 1, b64(grantAt(1))), service.CodeNotAuthorized)

	// Bea, a member who reads it, supplies the key: no owner or admin needed.
	reconciled := len(engine.reconciledIDs())
	got, err := f.svc.SupplyKey(t.Context(), tm.bea, shared, carol.UserID,
		service.SupplyKeyRequest{Epoch: 1, Grant: key.sealTo(t, f, carol.UserID)})
	if err != nil {
		t.Fatalf("Bea supplying the key: %v", err)
	}
	if got.UserID != carol.UserID || got.Epoch != 1 || got.GrantedBy != tm.bea.UserID {
		t.Errorf("the supplied grant is %+v", got)
	}
	if a := accessOf(t, f, carol, shared); !a.Access.Read || a.Access.WaitingKey {
		t.Errorf("Carol once supplied: %+v", a.Access)
	}
	if !slices.Contains(engine.reconciledIDs()[reconciled:], shared) {
		t.Error("the engine did not hear of the mailbox whose readers changed")
	}
	wantCode(t, "supplying it twice", supply(tm.ana, carol.UserID, 1, key.sealTo(t, f, carol.UserID)), service.CodeConflict)
}

func TestTheMailboxKeyIsReadOnlyByAPersonWhoHoldsReadOnIt(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	eve := f.person(t, "eve@example.com", auth.RoleMember)
	_, err := f.svc.MailboxKey(t.Context(), carol, shared)
	wantCode(t, "an admin who holds no read", err, service.CodeNotAuthorized)
	_, err = f.svc.MailboxKey(t.Context(), tm.bea, shared)
	wantCode(t, "a member who holds nothing", err, service.CodeNotFound)
	_, err = f.svc.MailboxKey(t.Context(), eve, shared)
	wantCode(t, "a person outside the team", err, service.CodeNotFound)
	_, err = f.svc.MailboxKey(t.Context(), admin(), shared)
	wantCode(t, "an API key", err, service.CodeNotAuthorized)
	state, err := f.svc.MailboxKey(t.Context(), tm.ana, shared)
	if err != nil {
		t.Fatal(err)
	}
	if state.Grant == "" || len(state.Suppliers) != 0 || len(state.KeylessReaders) != 0 {
		t.Errorf("Ana's console reads %+v", state)
	}
}

func TestTheFirstKeyOfAMailboxComesWithAGrantForEveryoneWhoReadsItWithAnAccountKey(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const orders = "acc_00000000000000e1"
	tm.link(t, f, orders, "orders@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleAdmin)
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	// Bea and Cy are given read by the flag, as on any mailbox without a key.
	for _, p := range []service.Principal{tm.bea, cy} {
		if _, err := f.svc.SetAccess(t.Context(), tm.ana, orders, p.UserID, readFlag("")); err != nil {
			t.Fatal(err)
		}
	}
	state, err := f.svc.MailboxKey(t.Context(), tm.ana, orders)
	if err != nil {
		t.Fatal(err)
	}
	if state.Epoch != 0 || len(state.KeylessReaders) != 1 || state.KeylessReaders[0].UserID != tm.bea.UserID {
		t.Fatalf("Ana's console offers the first key to %+v, want Bea alone (Cy has no account key)", state.KeylessReaders)
	}
	key := newBrowserKey(t, "", 1)
	first := func(p service.Principal, to ...string) error {
		req := service.FirstKeyRequest{PublicKey: b64(key.public), Namespace: key.namespace}
		for _, id := range to {
			req.Grants = append(req.Grants, service.GrantToRequest{UserID: id, Grant: key.sealTo(t, f, id)})
		}
		_, err := f.svc.WriteFirstKey(t.Context(), p, orders, req)
		return err
	}
	wantCode(t, "Carol, an admin who does not read it", first(carol, carol.UserID, tm.ana.UserID, tm.bea.UserID),
		service.CodeNotAuthorized)
	wantCode(t, "Ana without Bea's grant", first(tm.ana, tm.ana.UserID), service.CodeConflict)
	wantCode(t, "Ana with a grant for Carol, who holds no read", first(tm.ana, tm.ana.UserID, tm.bea.UserID, carol.UserID),
		service.CodeConflict)
	wantCode(t, "Ana without her own", first(tm.ana, tm.bea.UserID), service.CodeConflict)
	wantCode(t, "no grant at all", first(tm.ana), service.CodeBadRequest)
	if n := f.count(t, `SELECT count(*) FROM mailbox_keys`); n != 0 {
		t.Fatalf("%d keys written by refused requests", n)
	}
	if err := first(tm.ana, tm.ana.UserID, tm.bea.UserID); err != nil {
		t.Fatalf("Ana's first key: %v", err)
	}
	for _, p := range []service.Principal{tm.ana, tm.bea} {
		if a := accessOf(t, f, p, orders); !a.Access.Read || a.MailboxKey == nil || a.MailboxKey.Namespace != key.namespace {
			t.Errorf("%s after the first key: %+v, key %+v", p.UserID, a.Access, a.MailboxKey)
		}
	}
	// Cy held the flag with no account key: she waits for it now.
	if a := accessOf(t, f, cy, orders); a.Access.Read || !a.Access.WaitingKey {
		t.Errorf("Cy after the first key: %+v", a.Access)
	}
	wantCode(t, "a second first key", first(tm.ana, tm.ana.UserID, tm.bea.UserID), service.CodeConflict)
}

func TestAStreamHearsAFirstKeyTakeReadFromAReaderWithoutAnAccountKey(t *testing.T) {
	// Cy reads a mailbox without a key by the flag, and has no account key:
	// its first key, written by Ana, is sealed to nobody of hers, and her
	// open stream says she no longer reads it. Ana, who reads it before and
	// after, hears nothing.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	const orders = "acc_00000000000000e1"
	tm.link(t, f, orders, "orders@mail.example")
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, orders, cy.UserID, readFlag("")); err != nil {
		t.Fatal(err)
	}
	streams := map[string]*service.Stream{}
	for who, p := range map[string]service.Principal{"Cy": cy, "Ana": tm.ana} {
		st, err := f.svc.Subscribe(t.Context(), p, 0, service.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		if changes, err := st.CheckAccess(t.Context()); err != nil || len(changes) != 0 {
			t.Fatalf("%s's new stream reports %+v %v", who, changes, err)
		}
		streams[who] = st
	}
	key := newBrowserKey(t, "", 1)
	if _, err := f.svc.WriteFirstKey(t.Context(), tm.ana, orders, service.FirstKeyRequest{
		PublicKey: b64(key.public), Namespace: key.namespace,
		Grants: []service.GrantToRequest{{UserID: tm.ana.UserID, Grant: key.sealTo(t, f, tm.ana.UserID)}},
	}); err != nil {
		t.Fatal(err)
	}
	if changes, err := streams["Cy"].CheckAccess(t.Context()); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: orders, Read: false}) {
		t.Errorf("Cy's stream after the first key: %+v %v", changes, err)
	}
	if changes, err := streams["Ana"].CheckAccess(t.Context()); err != nil || len(changes) != 0 {
		t.Errorf("Ana's stream after her first key: %+v %v", changes, err)
	}
}

func TestAStreamHearsTheSuppliedKeyGiveRead(t *testing.T) {
	// Bea holds the flag on a mailbox that has a key, with an account key
	// and no grant: she waits for it. Ana supplies it, and Bea's open stream
	// says she reads it now; Ana's, whose reading did not change, hears
	// nothing.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, tm.bea.UserID, tm.ana.UserID)
	streams := map[string]*service.Stream{}
	for who, p := range map[string]service.Principal{"Bea": tm.bea, "Ana": tm.ana} {
		st, err := f.svc.Subscribe(t.Context(), p, 0, service.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		if changes, err := st.CheckAccess(t.Context()); err != nil || len(changes) != 0 {
			t.Fatalf("%s's new stream reports %+v %v", who, changes, err)
		}
		streams[who] = st
	}
	if _, err := f.svc.SupplyKey(t.Context(), tm.ana, shared, tm.bea.UserID,
		service.SupplyKeyRequest{Epoch: 1, Grant: key.sealTo(t, f, tm.bea.UserID)}); err != nil {
		t.Fatal(err)
	}
	if changes, err := streams["Bea"].CheckAccess(t.Context()); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: shared, Read: true}) {
		t.Errorf("Bea's stream once supplied: %+v %v", changes, err)
	}
	if changes, err := streams["Ana"].CheckAccess(t.Context()); err != nil || len(changes) != 0 {
		t.Errorf("Ana's stream after she supplied the key: %+v %v", changes, err)
	}
}

func TestOnlyAPersonalMailboxsPersonWritesItANewKeyAndItsOlderGrantsGo(t *testing.T) {
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	ana := tm.ana
	bob := f.person(t, "bob@example.com", auth.RoleMember)
	key := newBrowserKey(t, "", 1)
	added, err := f.svc.AddAccount(t.Context(), ana, key.link(t, f, ana, f.passwordAccount(t, "ana@mail.example")))
	if err != nil {
		t.Fatal(err)
	}
	mine := added.Account.ID
	next := newBrowserKey(t, key.namespace, 2)
	write := func(p service.Principal, accountID string, k browserKey, epoch int) error {
		_, err := f.svc.WriteNewKey(t.Context(), p, accountID, service.NewKeyRequest{
			Epoch: epoch, PublicKey: b64(k.public), Grant: k.sealTo(t, f, p.UserID),
		})
		return err
	}
	wantCode(t, "Bob writing Ana's mailbox a key", write(bob, mine, next, 2), service.CodeNotFound)
	skipped := newBrowserKey(t, key.namespace, 3)
	wantCode(t, "an epoch skipped", write(ana, mine, skipped, 3), service.CodeConflict)
	wantCode(t, "the current epoch again", write(ana, mine, key, 1), service.CodeConflict)
	if err := write(ana, mine, next, 2); err != nil {
		t.Fatalf("Ana's new key: %v", err)
	}
	a := accessOf(t, f, ana, mine)
	if a.MailboxKey == nil || a.MailboxKey.Epoch != 2 || a.MailboxKey.Namespace != key.namespace || !a.Access.Read {
		t.Errorf("after a new key: %+v, %+v", a.MailboxKey, a.Access)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND epoch < 2`, mine); n != 0 {
		t.Errorf("%d grants of an older epoch left", n)
	}
	// A team mailbox is never given a new key, whoever asks.
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	wantCode(t, "a team mailbox's new key", write(tm.ana, shared, newBrowserKey(t, "", 2), 2), service.CodeNotAuthorized)
}

func TestAResetPersonWaitsForTheKeysOfTheirMailboxesUntilANewKeyOrAReaderGivesThemBack(t *testing.T) {
	f, engine := newSyncFixture(t)
	tm := newSupportTeam(t, f)
	home := newBrowserKey(t, "", 1)
	added, err := f.svc.AddAccount(t.Context(), tm.ana, home.link(t, f, tm.ana, f.passwordAccount(t, "ana@mail.example")))
	if err != nil {
		t.Fatal(err)
	}
	mine := added.Account.ID
	if _, err := f.svc.GrantSyncConsent(t.Context(), tm.ana, f.consent().Sync); err != nil {
		t.Fatal(err)
	}
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	tm.syncOn(t, f, shared)
	if !f.eligible(t, shared) || !f.eligible(t, mine) {
		t.Fatal("her mailboxes do not sync before the reset")
	}
	// Ana alone reads the team mailbox: her reset needs force, and leaves
	// it with nobody who can read it.
	if _, _, err := f.users.CreateReset(t.Context(), tm.ana.UserID, false, "cli"); err == nil {
		t.Fatal("a reset of the last reader of a team mailbox with a key went ahead without force")
	}
	code, _, err := f.users.CreateReset(t.Context(), tm.ana.UserID, true, "cli")
	if err != nil {
		t.Fatal(err)
	}
	before := len(engine.reconciledIDs())
	session, err := f.svc.CompleteReset(t.Context(), service.ResetRequest{Reset: code, Email: "ana@example.com",
		Enrolment: wireEnrolment(t)}, "authtest")
	if err != nil {
		t.Fatal(err)
	}
	ana, err := f.users.AuthenticateSession(t.Context(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{mine, shared} {
		if a := accessOf(t, f, ana, id); a.Access.Read || !a.Access.WaitingKey {
			t.Errorf("%s after the reset: %+v", id, a.Access)
		}
		if !slices.Contains(engine.reconciledIDs()[before:], id) {
			t.Errorf("the engine did not hear of %s after the reset", id)
		}
	}
	// The team mailbox nobody reads stops; her personal one keeps syncing
	// under her consent, which her reset did not take, while she waits for
	// its key.
	if f.eligible(t, shared) {
		t.Error("the team mailbox nobody can read still syncs")
	}
	if !f.eligible(t, mine) {
		t.Error("her personal mailbox stopped syncing while she waits for its key")
	}
	// A new key for her own mailbox, which her new account key opens; her
	// stream, opened while she waited, says she reads it again.
	st, err := f.svc.Subscribe(t.Context(), ana, 0, service.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	next := newBrowserKey(t, home.namespace, 2)
	if _, err := f.svc.WriteNewKey(t.Context(), ana, mine, service.NewKeyRequest{
		Epoch: 2, PublicKey: b64(next.public), Grant: next.sealTo(t, f, ana.UserID),
	}); err != nil {
		t.Fatalf("a new key after the reset: %v", err)
	}
	if a := accessOf(t, f, ana, mine); !a.Access.Read || a.Access.WaitingKey {
		t.Errorf("her own mailbox with its new key: %+v", a.Access)
	}
	if changes, err := st.CheckAccess(t.Context()); err != nil || len(changes) != 1 ||
		changes[0] != (service.AccessChange{AccountID: mine, Read: true}) {
		t.Errorf("her stream after her new key: %+v %v", changes, err)
	}
	if !f.eligible(t, mine) {
		t.Error("her personal mailbox does not sync with its new key")
	}
}

func TestEveryKeyWriteNeedsAStepUpWithinTenMinutes(t *testing.T) {
	// Each action is refused, writing nothing, on a session whose sign-in or
	// step-up is more than ten minutes old, and accepted on the same session
	// once it steps up (docs/key-scheme.md section 11).
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, key := tm.keyedTeamMailbox(t, f, "support@mail.example")
	carol := tm.join(t, f, "carol@example.com", workspace.RoleMember)
	const orders = "acc_00000000000000e1"
	tm.link(t, f, orders, "orders@mail.example")
	personal := newBrowserKey(t, "", 1)
	added, err := f.svc.AddAccount(t.Context(), tm.ana, personal.link(t, f, tm.ana, f.passwordAccount(t, "ana@mail.example")))
	if err != nil {
		t.Fatal(err)
	}
	mine := added.Account.ID
	ordersKey := newBrowserKey(t, "", 1)
	next := newBrowserKey(t, personal.namespace, 2)

	actions := []struct {
		what    string
		do      func() error
		written func() int
	}{{
		"linking a mailbox",
		func() error {
			_, err := f.svc.AddAccount(t.Context(), tm.ana,
				newBrowserKey(t, "", 1).link(t, f, tm.ana, f.passwordAccount(t, "ana.work@mail.example")))
			return err
		},
		func() int { return f.count(t, `SELECT count(*) FROM accounts WHERE email = 'ana.work@mail.example'`) },
	}, {
		"giving read with a grant",
		func() error {
			_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, tm.bea.UserID, readFlag(key.sealTo(t, f, tm.bea.UserID)))
			return err
		},
		func() int { return f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, tm.bea.UserID) },
	}, {
		"supplying the key",
		func() error {
			_, err := f.svc.SupplyKey(t.Context(), tm.ana, shared, carol.UserID,
				service.SupplyKeyRequest{Epoch: 1, Grant: key.sealTo(t, f, carol.UserID)})
			return err
		},
		func() int { return f.count(t, `SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, carol.UserID) },
	}, {
		"writing a first key",
		func() error {
			_, err := f.svc.WriteFirstKey(t.Context(), tm.ana, orders, service.FirstKeyRequest{
				PublicKey: b64(ordersKey.public), Namespace: ordersKey.namespace,
				Grants: []service.GrantToRequest{{UserID: tm.ana.UserID, Grant: ordersKey.sealTo(t, f, tm.ana.UserID)}},
			})
			return err
		},
		func() int { return f.count(t, `SELECT count(*) FROM mailbox_keys WHERE account_id = ?`, orders) },
	}, {
		"writing a new key",
		func() error {
			_, err := f.svc.WriteNewKey(t.Context(), tm.ana, mine, service.NewKeyRequest{
				Epoch: 2, PublicKey: b64(next.public), Grant: next.sealTo(t, f, tm.ana.UserID),
			})
			return err
		},
		func() int {
			return f.count(t, `SELECT count(*) FROM mailbox_keys WHERE account_id = ? AND epoch = 2`, mine)
		},
	}}
	// Carol holds the flag with no key: a waiting member to supply.
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, carol.UserID, tm.ana.UserID)
	for _, a := range actions {
		before := a.written()
		f.stale(t, tm.ana)
		wantCode(t, a.what+" eleven minutes after the step-up", a.do(), service.CodeNotAuthorized)
		if a.written() != before {
			t.Errorf("%s with a stale step-up wrote something", a.what)
		}
		f.stepUp(t, tm.ana)
		if err := a.do(); err != nil {
			t.Errorf("%s right after a step-up: %v", a.what, err)
		}
		if a.written() == before {
			t.Errorf("%s after a step-up wrote nothing", a.what)
		}
	}
	// A sign-in counts as one: a session just opened needs no step-up.
	fresh, err := f.users.AuthenticateSession(t.Context(), authtest.SignIn(t, f.users, "ana@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AddAccount(t.Context(), fresh, keyed(fresh, f.passwordAccount(t, "ana.home@mail.example"))); err != nil {
		t.Errorf("linking right after a sign-in: %v", err)
	}
}

func TestALinkWhoseStepUpEndedDuringItsLoginStoresNothing(t *testing.T) {
	// The step-up asked before the mail server is dialled only fails a stale
	// one early: the transaction that would store the mailbox, its key and
	// the linker's grant asks again, and a step-up whose ten minutes ended
	// while the login took its time stores nothing (docs/key-scheme.md
	// section 11).
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	req := newBrowserKey(t, "", 1).link(t, f, ana, f.passwordAccount(t, "ana@mail.example"))
	ended := f.duringLogin(t, func() error {
		_, err := f.db.Writer().ExecContext(t.Context(),
			`UPDATE sessions SET authenticated_at = authenticated_at - 660 WHERE id = ?`, ana.SessionID)
		return err
	})
	_, err := f.svc.AddAccount(t.Context(), ana, req)
	ended()
	wantCode(t, "a link whose step-up ended during its login", err, service.CodeNotAuthorized)
	if n := f.count(t, `SELECT count(*) FROM accounts`); n != 0 {
		t.Errorf("%d mailboxes stored", n)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_keys`) + f.count(t, `SELECT count(*) FROM mailbox_grants`); n != 0 {
		t.Errorf("%d mailbox keys and grants stored", n)
	}
	// Stepped up again, the same link goes through.
	f.stepUp(t, ana)
	if _, err := f.svc.AddAccount(t.Context(), ana, req); err != nil {
		t.Errorf("the link after a step-up: %v", err)
	}
}

func TestChangingFlagsWithoutAGrantNeedsNoStepUp(t *testing.T) {
	// The step-up guards keys and grants; flags alone change as before,
	// however old the session's step-up.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	shared, _ := tm.keyedTeamMailbox(t, f, "support@mail.example")
	dan := tm.join(t, f, "dan@example.com", workspace.RoleMember)
	cy := f.external(t, "subject-of-cy", "cy@example.com")
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return tm.ws.AddMemberTx(t.Context(), tx, tm.id, cy.UserID, workspace.RoleMember, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	f.stale(t, tm.ana)
	// In order: Dan's manage is given before it is taken.
	for _, c := range []struct {
		what   string
		change func() error
	}{
		{"Ana's own send off", func() error {
			_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, tm.ana.UserID, grantRequest(true, true, false, false))
			return err
		}},
		{"manage for Dan", func() error {
			_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, dan.UserID, grantRequest(false, false, false, true))
			return err
		}},
		{"read by the flag for Cy, who has no account key", func() error {
			_, err := f.svc.SetAccess(t.Context(), tm.ana, shared, cy.UserID, readFlag(""))
			return err
		}},
		{"taking Dan's manage", func() error {
			return f.svc.RevokeAccess(t.Context(), tm.ana, shared, dan.UserID, workspace.Flags{Manage: true})
		}},
	} {
		if err := c.change(); err != nil {
			t.Errorf("%s with a stale step-up: %v", c.what, err)
		}
	}
}

func TestAKeySeesNoMailboxKeyAndWaitsForNone(t *testing.T) {
	// API keys are unchanged in phase 3 (docs/key-scheme.md section 12.15):
	// a key reads by what it holds, and sees no key material.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	mine := f.mailbox(t, ana, "ana@mail.example")
	// A key of her personal workspace, holding read on her mailbox.
	key := keyOf(t, f, ana, auth.ScopeRead)
	accounts, err := f.svc.ListAccounts(t.Context(), key, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].MailboxKey != nil || accounts[0].Access.WaitingKey || !accounts[0].Access.Read {
		t.Errorf("the key lists %+v", accounts)
	}
	_, err = f.svc.MailboxKey(t.Context(), key, mine)
	wantCode(t, "a key reading the mailbox key", err, service.CodeNotAuthorized)
	_, err = f.svc.WriteFirstKey(t.Context(), key, mine, service.FirstKeyRequest{})
	wantCode(t, "a key writing a first key", err, service.CodeNotAuthorized)
	_, err = f.svc.WriteNewKey(t.Context(), key, mine, service.NewKeyRequest{})
	wantCode(t, "a key writing a new key", err, service.CodeNotAuthorized)
}

func TestNoMailboxPrivateKeyReachesTheDatabase(t *testing.T) {
	// The server never receives a mailbox's private key: every request
	// carries its public half, its namespace and grants sealed of it, which
	// the server cannot open. After every flow that writes one, no column of
	// any table, nor the database file or its log, holds a private key in
	// any spelling.
	f := newFixture(t)
	tm := newSupportTeam(t, f)
	var keys []browserKey
	shared, sharedKey := tm.keyedTeamMailbox(t, f, "support@mail.example")
	keys = append(keys, sharedKey)
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, shared, tm.bea.UserID, readFlag(sharedKey.sealTo(t, f, tm.bea.UserID))); err != nil {
		t.Fatal(err)
	}
	carol := tm.join(t, f, "carol@example.com", workspace.RoleMember)
	f.exec(t, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage, granted_by, created_at, updated_at)
		VALUES (?, ?, ?, 1, 0, 0, 0, ?, 0, 0)`, shared, tm.id, carol.UserID, tm.ana.UserID)
	if _, err := f.svc.SupplyKey(t.Context(), tm.bea, shared, carol.UserID,
		service.SupplyKeyRequest{Epoch: 1, Grant: sharedKey.sealTo(t, f, carol.UserID)}); err != nil {
		t.Fatal(err)
	}
	const orders = "acc_00000000000000e1"
	tm.link(t, f, orders, "orders@mail.example")
	if _, err := f.svc.SetAccess(t.Context(), tm.ana, orders, tm.bea.UserID, readFlag("")); err != nil {
		t.Fatal(err)
	}
	first := newBrowserKey(t, "", 1)
	keys = append(keys, first)
	if _, err := f.svc.WriteFirstKey(t.Context(), tm.ana, orders, service.FirstKeyRequest{
		PublicKey: b64(first.public), Namespace: first.namespace, Grants: []service.GrantToRequest{
			{UserID: tm.ana.UserID, Grant: first.sealTo(t, f, tm.ana.UserID)},
			{UserID: tm.bea.UserID, Grant: first.sealTo(t, f, tm.bea.UserID)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	home := newBrowserKey(t, "", 1)
	keys = append(keys, home)
	added, err := f.svc.AddAccount(t.Context(), tm.ana, home.link(t, f, tm.ana, service.AddAccountRequest{
		Email: "ana@gmail.com", Flow: "loopback",
	}))
	if err != nil {
		t.Fatal(err)
	}
	next := newBrowserKey(t, home.namespace, 2)
	keys = append(keys, next)
	if _, err := f.svc.WriteNewKey(t.Context(), tm.ana, added.Account.ID, service.NewKeyRequest{
		Epoch: 2, PublicKey: b64(next.public), Grant: next.sealTo(t, f, tm.ana.UserID),
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM mailbox_keys`); n != 4 {
		t.Fatalf("%d mailbox keys written, want 4", n)
	}

	var spellings [][]byte
	for _, k := range keys {
		spellings = append(spellings, k.private, []byte(b64(k.private)),
			[]byte(base64.StdEncoding.EncodeToString(k.private)), []byte(hex.EncodeToString(k.private)))
	}
	for _, name := range f.tables(t) {
		for _, column := range f.holding(t, name, spellings) {
			t.Errorf("%s holds a mailbox private key", column)
		}
	}
	var needles []string
	for _, s := range spellings {
		needles = append(needles, string(s))
	}
	if found := f.onDisk(t, needles...); len(found) > 0 {
		t.Errorf("the database's files hold a mailbox private key: %v", found)
	}
	// What the server does hold is only public: each key's public half.
	for _, k := range keys {
		if n := f.count(t, `SELECT count(*) FROM mailbox_keys WHERE public_key = ?`, k.public); n != 1 {
			t.Errorf("%d rows hold the public half of a key, want 1", n)
		}
	}
}
