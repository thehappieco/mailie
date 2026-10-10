package workspace_test

import (
	"bytes"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Mailbox keys and sealed grants (docs/key-scheme.md sections 8, 9 and 12.11
// to 12.15): who reads by the one rule, and what each write of a key or a
// grant checks of the data.

// legacy is a member who has not enrolled in the key scheme: no account key,
// so nothing can be sealed to them.
func (f *fixture) legacy(email string) auth.User {
	f.t.Helper()
	return authtest.NewLegacyUser(f.t, f.db, email, auth.RoleMember)
}

// enrol gives a person who had none an account public key, as a reset
// invitation does for a person from before the key scheme.
func (f *fixture) enrol(userID string) {
	f.t.Helper()
	if _, err := f.db.Writer().ExecContext(f.t.Context(), `UPDATE users SET public_key = ? WHERE id = ?`,
		authtest.PublicKey(f.t), userID); err != nil {
		f.t.Fatal(err)
	}
}

// rekey replaces a person's account public key, as their reset would; the
// grants it deletes are the caller's to delete.
func (f *fixture) rekey(userID string) {
	f.t.Helper()
	if _, err := f.db.Writer().ExecContext(f.t.Context(),
		`UPDATE users SET public_key = ?, key_replaced_at = key_replaced_at + 1 WHERE id = ?`,
		authtest.PublicKey(f.t), userID); err != nil {
		f.t.Fatal(err)
	}
}

// sealed is a grant's shape at epoch for userID, stated to be sealed to their
// account public key now, as a browser that read them just before sends it.
func (f *fixture) sealed(userID string, epoch int) *workspace.Sealed {
	f.t.Helper()
	s := authtest.Sealed(f.t, f.db, userID, epoch)
	return &s
}

// give gives read and what else flags hold with a grant at the current epoch,
// as an owner or an admin who reads would.
func (f *fixture) give(accountID, userID string, flags workspace.Flags, epoch int) {
	f.t.Helper()
	if _, err := f.ws.SetGrantSealed(f.t.Context(), accountID, userID, flags, f.sealed(userID, epoch), "usr_test", nil); err != nil {
		f.t.Fatalf("SetGrantSealed: %v", err)
	}
}

func (f *fixture) access(userID, accountID string) (workspace.Flags, bool) {
	f.t.Helper()
	held, err := f.ws.Access(f.t.Context(), userID, []string{accountID})
	if err != nil {
		f.t.Fatal(err)
	}
	flags, ok := held[accountID]
	return flags, ok
}

func (f *fixture) reads(accountID, userID string) bool {
	f.t.Helper()
	var reads bool
	if err := f.db.Write(f.t.Context(), func(tx *sql.Tx) error {
		var err error
		reads, err = workspace.ReadsNowTx(f.t.Context(), tx, accountID, userID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return reads
}

func (f *fixture) hasReader(accountID string) bool {
	f.t.Helper()
	var has bool
	if err := f.db.Write(f.t.Context(), func(tx *sql.Tx) error {
		var err error
		has, err = workspace.HasReaderTx(f.t.Context(), tx, accountID)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return has
}

func (f *fixture) waiting(userID, accountID string) bool {
	f.t.Helper()
	w, err := f.ws.WaitingForKey(f.t.Context(), userID, []string{accountID})
	if err != nil {
		f.t.Fatal(err)
	}
	return w[accountID]
}

func (f *fixture) directoryEntry(workspaceID, accountID string) workspace.MailboxAccess {
	f.t.Helper()
	dir, err := f.ws.Directory(f.t.Context(), workspaceID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, mb := range dir {
		if mb.AccountID == accountID {
			return mb
		}
	}
	f.t.Fatalf("%s is not in the directory of %s", accountID, workspaceID)
	return workspace.MailboxAccess{}
}

func sealedOf(mb workspace.MailboxAccess, userID string) bool {
	for _, g := range mb.Grants {
		if g.UserID == userID {
			return g.Sealed
		}
	}
	return false
}

func userIDs(people []workspace.Recipient) []string {
	out := []string{}
	for _, p := range people {
		out = append(out, p.UserID)
	}
	slices.Sort(out)
	return out
}

func TestAKeylessMailboxIsReadByTheFlagAlone(t *testing.T) {
	f := newFixture(t)
	ana, bea, dan := f.person("ana@example.org"), f.person("bea@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readAct())
	f.grant(box.ID, dan.ID, readOnly())

	for _, who := range []string{ana.ID, bea.ID, dan.ID} {
		if held, _ := f.access(who, box.ID); !held.Read || !f.reads(box.ID, who) || f.waiting(who, box.ID) {
			t.Errorf("%s holds read on a mailbox without a key and does not read it: %+v", who, held)
		}
	}
	if held, _ := f.access(bea.ID, box.ID); !held.Act {
		t.Errorf("bea's act on a mailbox without a key: %+v", held)
	}
	mb := f.directoryEntry(team.ID, box.ID)
	if mb.Readers != 3 || mb.NoReader || mb.Epoch != 0 || sealedOf(mb, ana.ID) {
		t.Errorf("the directory says %d readers, no reader %v, epoch %d", mb.Readers, mb.NoReader, mb.Epoch)
	}
	if _, err := f.ws.CurrentKey(t.Context(), box.ID); !errors.Is(err, workspace.ErrKeyless) {
		t.Errorf("CurrentKey of a mailbox without a key: %v", err)
	}
	// Read is given there with the flag alone, and a grant is refused.
	cid := f.person("cid@example.org")
	f.join(team.ID, cid.ID, workspace.RoleMember)
	_, err := f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readOnly(), f.sealed(cid.ID, 1), ana.ID, nil)
	want(t, "a grant on a mailbox without a key", err, workspace.ErrKeyless)
	f.grant(box.ID, cid.ID, readOnly())
	if !f.reads(box.ID, cid.ID) {
		t.Error("read given by the flag on a mailbox without a key does not read it")
	}
}

func TestAMemberWithTheFlagButNoGrantDoesNotReadAKeyedMailbox(t *testing.T) {
	f := newFixture(t)
	ana, dan := f.person("ana@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	key := authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	if key.Epoch != 1 || key.CreatedBy != ana.ID {
		t.Fatalf("the first key: %+v", key)
	}
	// Dan has no account key: read is given to him by the flag, act with it,
	// and he waits for the key.
	f.grant(box.ID, dan.ID, workspace.Flags{Read: true, Act: true, Send: true})

	held, present := f.access(dan.ID, box.ID)
	if !present || held.Read || held.Act || !held.Send {
		t.Errorf("dan, waiting for the key: %+v (present %v); want the card, send, and neither read nor act", held, present)
	}
	if f.reads(box.ID, dan.ID) || !f.waiting(dan.ID, box.ID) {
		t.Error("dan reads a mailbox whose key he does not hold")
	}
	if held, _ := f.access(ana.ID, box.ID); !held.Read || !held.Act || f.waiting(ana.ID, box.ID) {
		t.Errorf("ana, who holds the key: %+v", held)
	}
	mb := f.directoryEntry(team.ID, box.ID)
	if mb.Readers != 1 || mb.Epoch != 1 || !sealedOf(mb, ana.ID) || sealedOf(mb, dan.ID) {
		t.Errorf("the directory: %d readers at epoch %d, ana sealed %v, dan sealed %v", mb.Readers, mb.Epoch,
			sealedOf(mb, ana.ID), sealedOf(mb, dan.ID))
	}
	g, err := f.ws.Grant(t.Context(), box.ID, dan.ID)
	if err != nil || !g.Read || g.Sealed {
		t.Errorf("dan's grant: %+v, %v; want the flag and no key", g, err)
	}

	// The key state his console reads: who can supply it.
	state, err := f.ws.KeyState(t.Context(), box.ID, dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Key == nil || state.Key.Epoch != 1 || state.Grant != nil || !state.HoldsFlag || state.Reads ||
		!slices.Equal(userIDs(state.Suppliers), []string{ana.ID}) {
		t.Errorf("dan's key state: %+v", state)
	}
	// He has no account key, so nobody can seal him one yet: he is not
	// listed as waiting until he enrols.
	if len(state.Waiting) != 0 {
		t.Errorf("waiting before he enrols: %v", userIDs(state.Waiting))
	}
	f.enrol(dan.ID)
	if state, err = f.ws.KeyState(t.Context(), box.ID, ana.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(userIDs(state.Waiting), []string{dan.ID}) || !bytes.Equal(state.Grant, mustGrant(t, f, box.ID, ana.ID)) ||
		!state.Reads || len(state.KeylessReaders) != 0 {
		t.Errorf("ana's key state once dan enrolled: %+v", state)
	}
}

// mustGrant is a person's sealed grant at the mailbox key's current epoch.
func mustGrant(t *testing.T, f *fixture, accountID, userID string) []byte {
	t.Helper()
	var g []byte
	if err := f.db.Reader().QueryRowContext(t.Context(), `SELECT grant FROM mailbox_grants WHERE account_id = ? AND user_id = ?
		ORDER BY epoch DESC LIMIT 1`, accountID, userID).Scan(&g); err != nil {
		t.Fatalf("%s's grant on %s: %v", userID, accountID, err)
	}
	return g
}

func TestTheLastReaderOfAKeyedTeamMailboxCountsOnlyGrantHolders(t *testing.T) {
	f := newFixture(t)
	olga, ana, dan := f.person("olga@example.org"), f.person("ana@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", olga, map[string]workspace.Role{ana.ID: workspace.RoleAdmin, dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, dan.ID, readOnly())
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)

	// Dan holds the flag and waits for the key: ana is the last reader.
	_, err := f.ws.Revoke(t.Context(), box.ID, ana.ID, readOnly(), nil)
	want(t, "taking read from the last reader while another waits for the key", err, workspace.ErrLastReader)
	err = f.ws.RemoveMember(t.Context(), team.ID, ana.ID, nil)
	want(t, "removing the last reader", err, workspace.ErrLastReader)
	members, err := f.ws.Members(t.Context(), team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		switch {
		case m.UserID == ana.ID && !slices.Equal(m.LastReaderOf, []string{box.ID}):
			t.Errorf("ana is the last reader of %v", m.LastReaderOf)
		case m.UserID == dan.ID && len(m.LastReaderOf) != 0:
			t.Errorf("dan, who waits for the key, is the last reader of %v", m.LastReaderOf)
		}
	}
	var keyed, blocks []string
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		keyed, err = workspace.LastReaderOfKeyedTx(t.Context(), tx, ana.ID)
		if err != nil {
			return err
		}
		b, err := workspace.BlocksTx(t.Context(), tx, ana.ID)
		blocks = b.LastReaderOf
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keyed, []string{box.ID}) || !slices.Equal(blocks, []string{box.ID}) {
		t.Errorf("ana's last-reader blocks: reset %v, closing %v", keyed, blocks)
	}
	// Taking dan's flag leaves the readers as they were.
	if _, err := f.ws.Revoke(t.Context(), box.ID, dan.ID, readOnly(), nil); err != nil {
		t.Errorf("taking read from someone waiting for the key: %v", err)
	}
	f.grant(box.ID, dan.ID, readOnly())

	// Once he holds the key, ana is no longer the last.
	f.enrol(dan.ID)
	if _, err := f.ws.SupplyGrant(t.Context(), box.ID, dan.ID, ana.ID, 1, *f.sealed(dan.ID, 1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ws.Revoke(t.Context(), box.ID, ana.ID, workspace.Flags{}, nil); err != nil {
		t.Errorf("taking read from ana once dan reads: %v", err)
	}
}

func TestTheResetsLastReaderGuardLeavesOutMailboxesWithoutAKey(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	team := f.team("Support", ana, nil)
	box := f.link(team.ID, ana.ID, "support@example.org")
	var keyed []string
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		var err error
		keyed, err = workspace.LastReaderOfKeyedTx(t.Context(), tx, ana.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Deleting her grants takes nothing there: she reads it by the flag.
	if len(keyed) != 0 {
		t.Errorf("the last reader of a mailbox without a key blocks a reset: %v", keyed)
	}
	if _, err := f.ws.Revoke(t.Context(), box.ID, ana.ID, readOnly(), nil); !errors.Is(err, workspace.ErrLastReader) {
		t.Errorf("revoking her read is still guarded: %v", err)
	}
}

func TestGivingReadOnAKeyedMailboxTakesTheRecipientsGrant(t *testing.T) {
	f := newFixture(t)
	ana, cid, dan := f.person("ana@example.org"), f.person("cid@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{cid.ID: workspace.RoleMember, dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)

	_, err := f.ws.SetGrant(t.Context(), box.ID, cid.ID, readOnly(), ana.ID, nil)
	want(t, "read without the grant to someone with an account key", err, workspace.ErrSealedGrantNeeded)
	malformed := f.sealed(cid.ID, 1)
	malformed.Grant[2] = 2
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readOnly(), malformed, ana.ID, nil)
	want(t, "a grant of another version", err, keyscheme.ErrShape)
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readOnly(), f.sealed(cid.ID, 2), ana.ID, nil)
	want(t, "a grant at another epoch", err, workspace.ErrEpoch)
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, dan.ID, readOnly(), f.sealed(dan.ID, 1), ana.ID, nil)
	want(t, "a grant to someone without an account key", err, workspace.ErrNotEnrolled)
	// Sealed, by what the browser says, to a key cid no longer has, or to
	// another person's.
	stale := f.sealed(cid.ID, 1)
	stale.SealedTo = authtest.PublicKey(t)
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readOnly(), stale, ana.ID, nil)
	want(t, "a grant sealed to another account key", err, workspace.ErrSealedToAnother)
	stale.SealedTo = nil
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readOnly(), stale, ana.ID, nil)
	want(t, "a grant that names no account key", err, workspace.ErrSealedToAnother)
	if n := f.count(`SELECT count(*) FROM mailbox_access WHERE account_id = ? AND user_id IN (?, ?)`, box.ID, cid.ID, dan.ID); n != 0 {
		t.Fatalf("refused changes stored %d grants", n)
	}

	g, err := f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, readAct(), f.sealed(cid.ID, 1), ana.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Read || !g.Act || !g.Sealed || !f.reads(box.ID, cid.ID) {
		t.Errorf("cid, given read with the grant: %+v", g)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND user_id = ? AND epoch = 1 AND granted_by = ?`,
		box.ID, cid.ID, ana.ID); n != 1 {
		t.Errorf("cid's grant was written %d times by ana", n)
	}
	// A change that adds no read takes no grant: supplying the key is how
	// someone who holds the flag gets one.
	_, err = f.ws.SetGrantSealed(t.Context(), box.ID, cid.ID, workspace.Flags{Read: true, Act: true, Send: true},
		f.sealed(cid.ID, 1), ana.ID, nil)
	want(t, "a grant with a change that adds no read", err, workspace.ErrSealedGrantUnwanted)
	// Nor does it need one: manage beside the read held, as the operator's
	// command line sends it.
	if _, err := f.ws.SetGrant(t.Context(), box.ID, cid.ID, workspace.Flags{Read: true, Act: true, Manage: true}, "cli", nil); err != nil {
		t.Errorf("manage for someone who reads: %v", err)
	}
	// To someone without an account key, read is the flag alone.
	f.grant(box.ID, dan.ID, readOnly())
	if f.reads(box.ID, dan.ID) || !f.waiting(dan.ID, box.ID) {
		t.Error("dan, given read by the flag, reads a mailbox with a key")
	}
}

func TestTakingReadDeletesTheGrantsInTheSameTransaction(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid, eve, fay := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org"),
		f.person("eve@example.org"), f.person("fay@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember,
		eve.ID: workspace.RoleMember, fay.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	for _, who := range []string{bea.ID, cid.ID, eve.ID, fay.ID} {
		f.give(box.ID, who, workspace.Flags{Read: true, Send: true}, 1)
	}
	grants := func(userID string) int {
		t.Helper()
		return f.count(`SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND user_id = ?`, box.ID, userID)
	}

	// Setting the flags without read: the grant goes, send stays.
	if _, err := f.ws.SetGrant(t.Context(), box.ID, bea.ID, workspace.Flags{Send: true}, ana.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Revoking read alone, which updates the row: the grant goes too.
	if _, err := f.ws.Revoke(t.Context(), box.ID, cid.ID, readOnly(), nil); err != nil {
		t.Fatal(err)
	}
	// A membership disabled, then enabled again: nothing comes back.
	disabled := workspace.StatusDisabled
	if _, err := f.ws.SetMember(t.Context(), team.ID, eve.ID, workspace.MemberChange{Status: &disabled}, nil); err != nil {
		t.Fatal(err)
	}
	active := workspace.StatusActive
	if _, err := f.ws.SetMember(t.Context(), team.ID, eve.ID, workspace.MemberChange{Status: &active}, nil); err != nil {
		t.Fatal(err)
	}
	// A member removed.
	if err := f.ws.RemoveMember(t.Context(), team.ID, fay.ID, nil); err != nil {
		t.Fatal(err)
	}
	for _, who := range []auth.User{bea, cid, eve, fay} {
		if n := grants(who.ID); n != 0 {
			t.Errorf("%s keeps %d grants after losing read", who.Email, n)
		}
	}
	if g, err := f.ws.Grant(t.Context(), box.ID, cid.ID); err != nil || g.Read || !g.Send {
		t.Errorf("cid after losing read: %+v, %v; want send alone", g, err)
	}
	// Giving read back needs a new grant.
	_, err := f.ws.SetGrant(t.Context(), box.ID, cid.ID, workspace.Flags{Read: true, Send: true}, ana.ID, nil)
	want(t, "read given back without a grant", err, workspace.ErrSealedGrantNeeded)
	f.give(box.ID, cid.ID, workspace.Flags{Read: true, Send: true}, 1)
	if !f.reads(box.ID, cid.ID) {
		t.Error("cid, given read back with a new grant, does not read")
	}
	if n := grants(ana.ID); n != 1 {
		t.Errorf("ana, who kept read, holds %d grants", n)
	}
}

func TestAMailboxKeyIsWrittenOnce(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	team := f.team("Support", ana, nil)
	box := f.link(team.ID, ana.ID, "support@example.org")
	other := f.link(team.ID, ana.ID, "billing@example.org")
	first := authtest.KeyMailbox(t, f.db, box.ID, ana.ID)

	_, err := f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(),
		Grants: []workspace.GrantTo{{UserID: ana.ID, Grant: authtest.Grant(t, 1), SealedTo: authtest.AccountPublicKey(t, f.db, ana.ID)}},
	}, nil)
	want(t, "a second first key", err, workspace.ErrKeyed)
	err = f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.WriteLinkKeyTx(t.Context(), tx, box.ID, ana.ID, authtest.LinkKey(t), f.db.Now())
	})
	want(t, "a link's key on a mailbox that has one", err, workspace.ErrKeyed)
	_, err = f.ws.WriteFirstKey(t.Context(), other.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: first.Namespace,
		Grants: []workspace.GrantTo{{UserID: ana.ID, Grant: authtest.Grant(t, 1), SealedTo: authtest.AccountPublicKey(t, f.db, ana.ID)}},
	}, nil)
	want(t, "another mailbox's namespace", err, workspace.ErrNamespaceTaken)

	got, err := f.ws.CurrentKey(t.Context(), box.ID)
	if err != nil || got.Epoch != 1 || !bytes.Equal(got.PublicKey, first.PublicKey) || got.Namespace != first.Namespace {
		t.Errorf("the key after the refusals: %+v, %v; want %+v", got, err, first)
	}
	// Whatever writes the database, the schema refuses a change.
	if _, err := f.db.Writer().ExecContext(t.Context(), `UPDATE mailbox_keys SET public_key = ? WHERE account_id = ?`,
		authtest.PublicKey(t), box.ID); err == nil {
		t.Error("the schema let a mailbox key change")
	}
	keys, err := f.ws.CurrentKeys(t.Context(), []string{box.ID, other.ID})
	if err != nil || len(keys) != 1 || keys[box.ID].Namespace != first.Namespace {
		t.Errorf("CurrentKeys: %+v, %v; want the keyed mailbox's alone", keys, err)
	}
}

func TestAFirstKeyComesWithAGrantForEveryoneWhoHoldsReadWithAnAccountKey(t *testing.T) {
	f := newFixture(t)
	olga, ana, bea, cid, dan := f.person("olga@example.org"), f.person("ana@example.org"), f.person("bea@example.org"),
		f.person("cid@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", olga, map[string]workspace.Role{ana.ID: workspace.RoleAdmin, bea.ID: workspace.RoleMember,
		cid.ID: workspace.RoleMember, dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	f.grant(box.ID, dan.ID, readOnly())
	f.grant(box.ID, cid.ID, workspace.Flags{Send: true})

	// Whom ana's console seals to beside her: bea; not dan, who has no
	// account key, nor cid, who does not hold read.
	state, err := f.ws.KeyState(t.Context(), box.ID, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Key != nil || !state.Reads || !slices.Equal(userIDs(state.KeylessReaders), []string{bea.ID}) ||
		len(state.Waiting) != 0 {
		t.Fatalf("ana's key state on a mailbox without a key: %+v", state)
	}
	grant := func(userIDs ...string) []workspace.GrantTo {
		var out []workspace.GrantTo
		for _, id := range userIDs {
			s := authtest.Sealed(t, f.db, id, 1)
			out = append(out, workspace.GrantTo{UserID: id, Grant: s.Grant, SealedTo: s.SealedTo})
		}
		return out
	}
	first := func(writer string, grants []workspace.GrantTo) error {
		_, err := f.ws.WriteFirstKey(t.Context(), box.ID, writer, workspace.FirstKey{
			PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(), Grants: grants,
		}, nil)
		return err
	}
	want(t, "bea's grant missing", first(ana.ID, grant(ana.ID)), workspace.ErrGrantsIncomplete)
	want(t, "a grant for cid, who does not hold read", first(ana.ID, grant(ana.ID, bea.ID, cid.ID)), workspace.ErrGrantsIncomplete)
	want(t, "two grants for bea", first(ana.ID, grant(ana.ID, bea.ID, bea.ID)), workspace.ErrGrantsIncomplete)
	want(t, "the owner who does not read it", first(olga.ID, grant(olga.ID, ana.ID, bea.ID)), workspace.ErrNotReader)
	want(t, "dan, who reads it and has no account key", first(dan.ID, grant(ana.ID, bea.ID)), workspace.ErrNotEnrolled)
	_, err = f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(),
		Grants: append([]workspace.GrantTo{{UserID: ana.ID, Grant: authtest.Grant(t, 2)}}, grant(bea.ID)...),
	}, nil)
	want(t, "a grant at epoch 2", err, workspace.ErrEpoch)
	// Bea's sealed, by what ana's browser says, to a key bea no longer has:
	// read before her reset.
	stale := grant(ana.ID, bea.ID)
	stale[1].SealedTo = authtest.PublicKey(t)
	want(t, "bea's grant sealed to another account key", first(ana.ID, stale), workspace.ErrSealedToAnother)
	mine := grant(ana.ID, bea.ID)
	mine[0].SealedTo = mine[1].SealedTo
	want(t, "ana's grant sealed to bea's account key", first(ana.ID, mine), workspace.ErrSealedToAnother)
	_, err = f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: make([]byte, 32), Namespace: keyscheme.NewSealID(), Grants: grant(ana.ID, bea.ID),
	}, nil)
	want(t, "a public key of low order", err, keyscheme.ErrPublicKey)
	_, err = f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: "not-a-namespace", Grants: grant(ana.ID, bea.ID),
	}, nil)
	want(t, "a namespace outside its spelling", err, keyscheme.ErrBinding)
	refusal := errors.New("the step-up is stale")
	_, err = f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(), Grants: grant(ana.ID, bea.ID),
	}, func(*sql.Tx) error { return refusal })
	want(t, "the check's refusal", err, refusal)
	if n := f.count(`SELECT (SELECT count(*) FROM mailbox_keys) + (SELECT count(*) FROM mailbox_grants)`); n != 0 {
		t.Fatalf("refused first keys left %d rows", n)
	}

	key, err := f.ws.WriteFirstKey(t.Context(), box.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(), Grants: grant(bea.ID, ana.ID),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key.Epoch != 1 || key.CreatedBy != ana.ID {
		t.Errorf("the first key: %+v", key)
	}
	for _, who := range []string{ana.ID, bea.ID} {
		if !f.reads(box.ID, who) {
			t.Errorf("%s does not read after the first key", who)
		}
	}
	// From the first key on, dan waits for it.
	if f.reads(box.ID, dan.ID) || !f.waiting(dan.ID, box.ID) {
		t.Error("dan reads a mailbox with a key without its grant")
	}
	// An operator mailbox never gets one.
	ops := f.link(workspace.OperatorID, "", "ops@example.org")
	_, err = f.ws.WriteFirstKey(t.Context(), ops.ID, ana.ID, workspace.FirstKey{
		PublicKey: authtest.PublicKey(t), Namespace: keyscheme.NewSealID(), Grants: grant(ana.ID),
	}, nil)
	want(t, "an operator mailbox's first key", err, workspace.ErrOperator)
}

func TestTheKeyIsSuppliedOnlyToAMemberWhoHoldsReadWithoutAGrant(t *testing.T) {
	f := newFixture(t)
	ana, bea, cid, dan, gil := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cid@example.org"),
		f.legacy("dan@example.org"), f.person("gil@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember, cid.ID: workspace.RoleMember,
		dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	f.grant(box.ID, dan.ID, readOnly())
	f.grant(box.ID, cid.ID, workspace.Flags{Send: true})
	unkeyed := f.link(team.ID, ana.ID, "billing@example.org")
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)

	supply := func(recipient, giver string, epoch int, grant []byte) error {
		_, err := f.ws.SupplyGrant(t.Context(), box.ID, recipient, giver, epoch,
			workspace.Sealed{Grant: grant, SealedTo: authtest.AccountPublicKey(t, f.db, recipient)}, nil)
		return err
	}
	want(t, "to someone without an account key", supply(dan.ID, bea.ID, 1, authtest.Grant(t, 1)), workspace.ErrNotEnrolled)
	f.enrol(dan.ID)
	want(t, "by someone who does not read it", supply(dan.ID, cid.ID, 1, authtest.Grant(t, 1)), workspace.ErrNotReader)
	want(t, "to someone who does not hold read", supply(cid.ID, ana.ID, 1, authtest.Grant(t, 1)), workspace.ErrNoReadFlag)
	want(t, "to someone who is not a member", supply(gil.ID, ana.ID, 1, authtest.Grant(t, 1)), workspace.ErrNotMember)
	want(t, "at another epoch", supply(dan.ID, ana.ID, 2, authtest.Grant(t, 2)), workspace.ErrEpoch)
	want(t, "a grant at another epoch than it says", supply(dan.ID, ana.ID, 1, authtest.Grant(t, 2)), workspace.ErrEpoch)
	want(t, "a grant of another length", supply(dan.ID, ana.ID, 1, authtest.Grant(t, 1)[:80]), keyscheme.ErrShape)
	want(t, "to someone who holds it", supply(bea.ID, ana.ID, 1, authtest.Grant(t, 1)), workspace.ErrSealedGrantExists)
	_, err := f.ws.SupplyGrant(t.Context(), unkeyed.ID, dan.ID, ana.ID, 1, *f.sealed(dan.ID, 1), nil)
	want(t, "on a mailbox without a key", err, workspace.ErrKeyless)
	// Sealed, by what the browser says, to the key dan had before he was
	// reset: it would count him a reader and never open.
	before := authtest.AccountPublicKey(t, f.db, dan.ID)
	f.rekey(dan.ID)
	_, err = f.ws.SupplyGrant(t.Context(), box.ID, dan.ID, bea.ID, 1, workspace.Sealed{Grant: authtest.Grant(t, 1), SealedTo: before}, nil)
	want(t, "a grant sealed to dan's account key before his reset", err, workspace.ErrSealedToAnother)
	if !f.waiting(dan.ID, box.ID) {
		t.Fatal("a refused supply stored dan's grant")
	}

	// Any reader may: bea is a member, neither owner nor admin.
	g, err := f.ws.SupplyGrant(t.Context(), box.ID, dan.ID, bea.ID, 1, *f.sealed(dan.ID, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.UserID != dan.ID || g.Epoch != 1 || g.GrantedBy != bea.ID || g.WorkspaceID != team.ID || len(g.Grant) != keyscheme.GrantLen {
		t.Errorf("the supplied grant: %+v", g)
	}
	if !f.reads(box.ID, dan.ID) || f.waiting(dan.ID, box.ID) {
		t.Error("dan does not read once supplied the key")
	}
	if held, _ := f.access(dan.ID, box.ID); held.Act {
		t.Errorf("supplying the key gave dan act: %+v", held)
	}
	want(t, "a second time", supply(dan.ID, bea.ID, 1, authtest.Grant(t, 1)), workspace.ErrSealedGrantExists)
}

func TestANewKeyForAPersonalMailboxDeletesTheGrantsOfOlderEpochs(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	mine := f.link(f.personal(ana.ID), ana.ID, "ana@mail.example")
	keyless := f.link(f.personal(ana.ID), ana.ID, "ana.old@mail.example")
	team := f.team("Support", ana, map[string]workspace.Role{bea.ID: workspace.RoleMember})
	shared := f.link(team.ID, ana.ID, "support@example.org")
	first := authtest.KeyMailbox(t, f.db, mine.ID, ana.ID)
	authtest.KeyMailbox(t, f.db, shared.ID, ana.ID)

	// A reset took her grants: she waits for the key of her own mailbox.
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.DropSealedGrantsOfTx(t.Context(), tx, ana.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if f.reads(mine.ID, ana.ID) || !f.waiting(ana.ID, mine.ID) {
		t.Fatal("ana reads her mailbox after losing its grant")
	}

	next := func(accountID, personID string, epoch, grantEpoch int) error {
		_, err := f.ws.WriteNextKey(t.Context(), accountID, personID, workspace.NextKey{
			Epoch: epoch, PublicKey: authtest.PublicKey(t), Grant: authtest.Grant(t, grantEpoch),
		}, nil)
		return err
	}
	want(t, "a skipped epoch", next(mine.ID, ana.ID, 3, 3), workspace.ErrEpoch)
	want(t, "the current epoch", next(mine.ID, ana.ID, 1, 1), workspace.ErrEpoch)
	want(t, "a grant at another epoch", next(mine.ID, ana.ID, 2, 1), workspace.ErrEpoch)
	want(t, "epoch 0", next(mine.ID, ana.ID, 0, 1), keyscheme.ErrBinding)
	want(t, "someone else's mailbox", next(mine.ID, bea.ID, 2, 2), workspace.ErrNoMailbox)
	want(t, "a team mailbox", next(shared.ID, ana.ID, 2, 2), workspace.ErrTeamKey)
	want(t, "a mailbox without a key", next(keyless.ID, ana.ID, 1, 1), workspace.ErrKeyless)

	key, err := f.ws.WriteNextKey(t.Context(), mine.ID, ana.ID, workspace.NextKey{
		Epoch: 2, PublicKey: authtest.PublicKey(t), Grant: authtest.Grant(t, 2),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key.Epoch != 2 || key.Namespace != first.Namespace || key.CreatedBy != ana.ID {
		t.Errorf("the new key: %+v; want epoch 2 in the namespace %s", key, first.Namespace)
	}
	if !f.reads(mine.ID, ana.ID) {
		t.Error("ana does not read her mailbox with its new key")
	}
	// A grant at epoch 1 opens nothing from now on, and none is left; the
	// key rows stay, written once.
	if _, err := f.db.Writer().ExecContext(t.Context(), `INSERT INTO mailbox_grants(account_id, workspace_id, user_id, epoch,
		grant, granted_by, created_at) VALUES (?, ?, ?, 1, ?, '', 0)`, mine.ID, f.personal(ana.ID), ana.ID, authtest.Grant(t, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ws.WriteNextKey(t.Context(), mine.ID, ana.ID, workspace.NextKey{
		Epoch: 3, PublicKey: authtest.PublicKey(t), Grant: authtest.Grant(t, 3),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_grants WHERE account_id = ? AND epoch < 3`, mine.ID); n != 0 {
		t.Errorf("%d grants of older epochs are left", n)
	}
	if n := f.count(`SELECT count(*) FROM mailbox_keys WHERE account_id = ?`, mine.ID); n != 3 {
		t.Errorf("%d key rows, want the three epochs'", n)
	}
}

func TestMembersCarryWhatAGrantToThemIsSealedTo(t *testing.T) {
	f := newFixture(t)
	ana, dan := f.person("ana@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{dan.ID: workspace.RoleMember})
	members, err := f.ws.Members(t.Context(), team.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		switch m.UserID {
		case ana.ID:
			if m.SealID != ana.SealID || !bytes.Equal(m.PublicKey, ana.PublicKey) {
				t.Errorf("ana: seal id %q, public key %x", m.SealID, m.PublicKey)
			}
		case dan.ID:
			if m.SealID != dan.SealID || m.PublicKey != nil {
				t.Errorf("dan, not enrolled: seal id %q, public key %x", m.SealID, m.PublicKey)
			}
		}
	}
}

func TestATeamMailboxWithAKeySyncsOnlyWhileSomeoneHoldsItsGrant(t *testing.T) {
	f := newFixture(t)
	ana, dan := f.person("ana@example.org"), f.legacy("dan@example.org")
	team := f.team("Support", ana, map[string]workspace.Role{dan.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, dan.ID, readOnly())
	if _, err := f.db.SetMailboxSync(t.Context(), box.ID, true, ana.ID, "sync-3", nil); err != nil {
		t.Fatal(err)
	}
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	permitted := func() bool {
		t.Helper()
		got, err := f.db.SyncPermitted(t.Context(), []string{box.ID})
		if err != nil {
			t.Fatal(err)
		}
		return got[box.ID]
	}
	if !permitted() || !f.hasReader(box.ID) {
		t.Fatal("a team mailbox ana reads does not sync")
	}
	// Ana's grants go (a reset): dan holds the flag and no key, so nobody
	// reads it, and it stores nothing more.
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		return workspace.DropSealedGrantsOfTx(t.Context(), tx, ana.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if permitted() || f.hasReader(box.ID) {
		t.Error("a team mailbox nobody holds the key of syncs")
	}
	_, unread, err := f.db.TeamSyncNotices(t.Context())
	if err != nil || !slices.Equal(unread, []string{box.ID}) {
		t.Errorf("read by nobody at start: %v, %v", unread, err)
	}
	if mb := f.directoryEntry(team.ID, box.ID); mb.Readers != 0 || !mb.NoReader {
		t.Errorf("the directory: %d readers, no reader %v", mb.Readers, mb.NoReader)
	}
}

func TestDeletingAPersonBlanksWhoWroteAKeyAndWhoGaveAGrant(t *testing.T) {
	f := newFixture(t)
	olga, ana, bea := f.person("olga@example.org"), f.person("ana@example.org"), f.person("bea@example.org")
	team := f.team("Support", olga, map[string]workspace.Role{ana.ID: workspace.RoleAdmin, bea.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)

	if err := f.db.Write(t.Context(), func(tx *sql.Tx) error {
		if _, err := workspace.DeletePersonTx(t.Context(), tx, ana.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `DELETE FROM users WHERE id = ?`, ana.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT (SELECT count(*) FROM mailbox_keys WHERE created_by = ?1)
		+ (SELECT count(*) FROM mailbox_grants WHERE granted_by = ?1 OR user_id = ?1)`, ana.ID); n != 0 {
		t.Errorf("%d key and grant rows still name ana", n)
	}
	// What she gave stays: the key, and bea's grant, which she reads by.
	if !f.reads(box.ID, bea.ID) {
		t.Error("bea lost the key ana sealed her")
	}
}
