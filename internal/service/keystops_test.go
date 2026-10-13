package service_test

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

// carryOver makes a key of the workspace as migration 0012 leaves a person's
// key that reached mailboxes of several workspaces: no workspace, holding
// read on the mailboxes named, of whichever workspaces, beside what it held.
// The schema refuses both moving a key and a carried-over key gaining a
// mailbox, so the scene is set past those two triggers, which come back as
// 0012 made them.
func (f *fixture) carryOver(t *testing.T, prefix string, accounts ...string) {
	t.Helper()
	ctx := t.Context()
	err := f.db.Write(ctx, func(tx *sql.Tx) error {
		stmts := []string{
			`DROP TRIGGER api_keys_workspace_fixed`,
			`DROP TRIGGER key_access_own_workspace`,
			`UPDATE api_keys SET workspace_id = NULL, origin = 'person-all' WHERE prefix = '` + prefix + `'`,
		}
		for _, id := range accounts {
			stmts = append(stmts, `INSERT INTO key_access(key_prefix, account_id, workspace_id, read, created_at, updated_at)
				SELECT '`+prefix+`', id, workspace_id, 1, 1, 1 FROM accounts WHERE id = '`+id+`'`)
		}
		stmts = append(stmts,
			`CREATE TRIGGER api_keys_workspace_fixed BEFORE UPDATE OF workspace_id ON api_keys
			 WHEN NEW.workspace_id IS NOT OLD.workspace_id
			 BEGIN SELECT RAISE(ABORT, 'a key never moves to another workspace'); END`,
			`CREATE TRIGGER key_access_own_workspace BEFORE INSERT ON key_access
			 WHEN NOT EXISTS (SELECT 1 FROM api_keys k WHERE k.prefix = NEW.key_prefix AND k.workspace_id = NEW.workspace_id
			                    AND k.workspace_id <> 'wsp_operator')
			 BEGIN SELECT RAISE(ABORT, 'a key holds mailboxes of its own workspace only'); END`)
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("carrying the key over: %v", err)
	}
}

// stopped reads whether a key stopped, and why.
func (f *fixture) stopped(t *testing.T, prefix string) (bool, workspace.KeyStopReason) {
	t.Helper()
	var (
		revoked bool
		reason  workspace.KeyStopReason
	)
	if err := f.db.Reader().QueryRowContext(t.Context(),
		`SELECT revoked_at <> 0, stop_reason FROM api_keys WHERE prefix = ?`, prefix).Scan(&revoked, &reason); err != nil {
		t.Fatalf("key %s: %v", prefix, err)
	}
	return revoked, reason
}

// recoverAccount recovers a person's account with their recovery code, as
// their browser would: a new password and recovery code over the same
// account key.
func (f *fixture) recoverAccount(t *testing.T, email string) {
	t.Helper()
	opened, err := f.svc.OpenRecovery(t.Context(), service.RecoverOpenRequest{Email: email, RecoveryProof: authtest.RecoveryProof})
	if err != nil {
		t.Fatal(err)
	}
	in := authtest.Enrolment(t)
	enc := base64.RawURLEncoding.EncodeToString
	if err := f.svc.FinishRecovery(t.Context(), service.RecoverFinishRequest{
		Ticket: opened.Ticket, CurrentRecoveryProof: authtest.RecoveryProof, AuthKey: in.AuthKey, KDF: opened.KDF,
		PasswordWrap: enc(in.PasswordWrap), RecoveryWrap: enc(in.RecoveryWrap), RecoveryProof: in.RecoveryProof,
	}); err != nil {
		t.Fatal(err)
	}
}

// created makes a read key of the workspace that p creates, holding nothing.
func (f *fixture) created(t *testing.T, p service.Principal, workspaceID string) string {
	t.Helper()
	k, err := f.svc.CreateWorkspaceKey(t.Context(), p, workspaceID, keyRequest("k", "read"))
	if err != nil {
		t.Fatal(err)
	}
	return k.Prefix
}

func TestEveryWayAKeyStopsSaysWhy(t *testing.T) {
	// Each way on a team of its own: Ana, its owner, reads its mailbox;
	// Carol is an admin who reads nothing; Olga owns the instance.
	for _, c := range []struct {
		name string
		want workspace.KeyStopReason
		stop func(t *testing.T, f *fixture, tm keysTeam, olga service.Principal) string
	}{
		{"revoked by the operator", workspace.KeyStopManual,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				must(t, f.svc.RevokeAPIKey(t.Context(), admin(), k))
				return k
			}},
		{"revoked by an owner of its workspace", workspace.KeyStopManual,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				must(t, f.svc.RevokeWorkspaceKey(t.Context(), tm.ana, tm.id, k))
				return k
			}},
		{"revoked by its creator", workspace.KeyStopManual,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				must(t, f.svc.RevokeMyAPIKey(t.Context(), tm.carol, k))
				return k
			}},
		{"its creator left the workspace", workspace.KeyStopCreatorLeft,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				dan := tm.join(t, f, "dan@example.com", workspace.RoleOwner)
				k := f.created(t, dan, tm.id)
				must(t, f.svc.RemoveMember(t.Context(), dan, tm.id, dan.UserID))
				return k
			}},
		{"its creator was removed from the workspace", workspace.KeyStopCreatorLeft,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				must(t, f.svc.RemoveMember(t.Context(), tm.ana, tm.id, tm.carol.UserID))
				return k
			}},
		{"its creator was disabled", workspace.KeyStopCreatorDisabled,
			func(t *testing.T, f *fixture, tm keysTeam, olga service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				_, err := f.svc.DisableUser(t.Context(), olga, service.CloseUserRequest{Email: "carol@example.com"})
				must(t, err)
				return k
			}},
		{"its creator was deleted", workspace.KeyStopCreatorDeleted,
			func(t *testing.T, f *fixture, tm keysTeam, olga service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				_, err := f.svc.DeleteUser(t.Context(), olga, service.CloseUserRequest{Email: "carol@example.com"})
				must(t, err)
				return k
			}},
		{"the last mailbox it was restricted to was removed", workspace.KeyStopRestrictionEmptied,
			func(t *testing.T, f *fixture, _ keysTeam, _ service.Principal) string {
				a := f.mailbox(t, admin(), "a@mail.example")
				k, _ := f.issue(t, "", a)
				_, err := f.svc.RemoveAccount(t.Context(), admin(), a, service.RemoveAccountRequest{Confirm: a})
				must(t, err)
				return k
			}},
		{"a carried-over key lost its last mailbox", workspace.KeyStopRestrictionEmptied,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				f.carryOver(t, k, tm.shared)
				must(t, f.svc.RevokeKeyAccess(t.Context(), tm.ana, tm.id, k, tm.shared))
				return k
			}},
		{"an owner revoked a carried-over key in the last workspace it held mailboxes in", workspace.KeyStopManual,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				f.carryOver(t, k, tm.shared)
				must(t, f.svc.RevokeWorkspaceKey(t.Context(), tm.ana, tm.id, k))
				return k
			}},
		{"the creator of a carried-over key left the last workspace it held mailboxes in", workspace.KeyStopCreatorLeft,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				f.carryOver(t, k, tm.shared)
				must(t, f.svc.RemoveMember(t.Context(), tm.ana, tm.id, tm.carol.UserID))
				return k
			}},
		{"its creator recovered their account with their recovery code", workspace.KeyStopAccountRecovered,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				f.recoverAccount(t, "carol@example.com")
				return k
			}},
		{"its creator used a reset invitation", workspace.KeyStopAccountRecovered,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				f.resetKey(t, tm.carol.UserID, "carol@example.com")
				return k
			}},
		{"its creator's account was recovered after it was created", workspace.KeyStopAccountRecovered,
			func(t *testing.T, f *fixture, tm keysTeam, _ service.Principal) string {
				k := f.created(t, tm.carol, tm.id)
				n, err := f.svc.RevokeKeysCreatedBefore(t.Context(), tm.carol.UserID, time.Now().Add(time.Minute),
					workspace.KeyStopAccountRecovered)
				must(t, err)
				if n != 1 {
					t.Errorf("%d keys stopped, want Carol's one", n)
				}
				return k
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			tm := newKeysTeam(t, f)
			olga := f.person(t, "olga@example.com", auth.RoleOwner)
			k := c.stop(t, f, tm, olga)
			if revoked, reason := f.stopped(t, k); !revoked || reason != c.want {
				t.Errorf("the key is revoked %v, for %q; want revoked, for %q", revoked, reason, c.want)
			}
		})
	}

	// A carried-over key that still holds a mailbox of another workspace
	// loses this one's and does not stop, whoever takes them.
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	own := f.mailbox(t, tm.carol, "carol@mail.example")
	revoked := f.created(t, tm.carol, tm.id)
	f.carryOver(t, revoked, tm.shared, own)
	must(t, f.svc.RevokeWorkspaceKey(t.Context(), tm.ana, tm.id, revoked))
	left := f.created(t, tm.carol, tm.id)
	f.carryOver(t, left, tm.shared, own)
	must(t, f.svc.RemoveMember(t.Context(), tm.ana, tm.id, tm.carol.UserID))
	for _, k := range []string{revoked, left} {
		if stopped, reason := f.stopped(t, k); stopped || reason != "" {
			t.Errorf("a carried-over key holding a mailbox elsewhere stopped, for %q", reason)
		}
		if n := f.count(t, `SELECT count(*) FROM key_access WHERE key_prefix = ? AND workspace_id = ?`, k, tm.id); n != 0 {
			t.Errorf("a carried-over key still holds %d of the team's mailboxes", n)
		}
	}
}

func TestTheKeyListingsSayWhyAKeyStopped(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	ctx := t.Context()
	live := f.created(t, tm.ana, tm.id)
	revoked := f.created(t, tm.carol, tm.id)
	must(t, f.svc.RevokeWorkspaceKey(ctx, tm.ana, tm.id, revoked))
	dan := tm.join(t, f, "dan@example.com", workspace.RoleAdmin)
	left := f.created(t, dan, tm.id)
	must(t, f.svc.RemoveMember(ctx, tm.ana, tm.id, dan.UserID))
	// A key revoked before the reason was recorded says none.
	f.exec(t, `INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at, revoked_at, workspace_id,
		terms_version, created_by) VALUES ('0f0f0f0f', 'h', 'Before', 'read', 100, 9999999999, 200, ?, ?, ?)`,
		tm.id, service.DefaultKeyTermsVersion, tm.carol.UserID)
	want := map[string]workspace.KeyStopReason{live: "", revoked: workspace.KeyStopManual,
		left: workspace.KeyStopCreatorLeft, "0f0f0f0f": ""}

	team, err := f.svc.ListWorkspaceKeys(ctx, tm.ana, tm.id)
	must(t, err)
	mine, err := f.svc.ListMyAPIKeys(ctx, tm.carol)
	must(t, err)
	operator, err := f.svc.ListAPIKeys(ctx, admin())
	must(t, err)
	seen := map[string]int{}
	for _, k := range append(team, mine...) {
		seen[k.Prefix]++
		if k.StopReason != want[k.Prefix] {
			t.Errorf("key %s is listed stopped for %q, want %q", k.Prefix, k.StopReason, want[k.Prefix])
		}
		encoded, err := json.Marshal(k)
		must(t, err)
		if says := strings.Contains(string(encoded), `"stop_reason"`); says != (want[k.Prefix] != "") {
			t.Errorf("key %s answered %s", k.Prefix, encoded)
		}
	}
	for _, k := range operator {
		if _, ours := want[k.Prefix]; !ours {
			continue
		}
		seen[k.Prefix]++
		if k.StopReason != want[k.Prefix] {
			t.Errorf("the operator lists key %s stopped for %q, want %q", k.Prefix, k.StopReason, want[k.Prefix])
		}
	}
	// The team's listing and the operator's name all four; Carol's, her
	// two.
	for prefix, n := range map[string]int{live: 2, revoked: 3, left: 2, "0f0f0f0f": 3} {
		if seen[prefix] != n {
			t.Errorf("key %s was listed %d times, want %d", prefix, seen[prefix], n)
		}
	}
}

func TestOnlyAReasonAKeyStopsForStopsAPersonsKeys(t *testing.T) {
	f := newFixture(t)
	tm := newKeysTeam(t, f)
	k := f.created(t, tm.carol, tm.id)
	for _, reason := range []workspace.KeyStopReason{"", "expired"} {
		_, err := f.svc.RevokeKeysCreatedBefore(t.Context(), tm.carol.UserID, time.Now().Add(time.Minute), reason)
		wantCode(t, "the reason "+string(reason), err, service.CodeBadRequest)
	}
	_, err := f.svc.RevokeKeysCreatedBefore(t.Context(), "", time.Now().Add(time.Minute), workspace.KeyStopAccountRecovered)
	wantCode(t, "nobody", err, service.CodeBadRequest)
	_, err = f.svc.RevokeKeysCreatedBefore(t.Context(), tm.carol.UserID, time.Time{}, workspace.KeyStopAccountRecovered)
	wantCode(t, "no moment", err, service.CodeBadRequest)
	if stopped, _ := f.stopped(t, k); stopped {
		t.Error("a refused call stopped the key")
	}
}

// must stops the test on an error the scene cannot go on without.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestASessionARecoveryEndedCreatesNoKey(t *testing.T) {
	// Ana's session was authenticated before her recovery and asks for a key
	// after it, as a request in flight while the recovery commits would: the
	// recovery stopped every key she had created, and misses none.
	f := newFixture(t)
	ana := f.person(t, "ana@example.com", auth.RoleMember)
	personal := authtest.Personal(t, f.db, ana.UserID)
	before := f.created(t, ana, personal)
	f.recoverAccount(t, "ana@example.com")

	_, err := f.svc.CreateWorkspaceKey(t.Context(), ana, personal, keyRequest("after", "read"))
	wantCode(t, "a key asked for by a session the recovery ended", err, service.CodeUnauthorized)
	if n := f.count(t, `SELECT count(*) FROM api_keys WHERE created_by = ?`, ana.UserID); n != 1 {
		t.Errorf("Ana created %d keys, want the one from before her recovery", n)
	}
	if revoked, reason := f.stopped(t, before); !revoked || reason != workspace.KeyStopAccountRecovered {
		t.Errorf("her key from before is revoked %v, for %q", revoked, reason)
	}
	// Signed in again, with her new password, she creates keys as before.
	again, err := f.users.AuthenticateSession(t.Context(), authtest.SignIn(t, f.users, "ana@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	after := f.created(t, again, personal)
	if revoked, _ := f.stopped(t, after); revoked {
		t.Error("a key created after the recovery, by a session of it, stopped")
	}
}
