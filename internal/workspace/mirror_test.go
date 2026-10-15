package workspace_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Mirroring an external directory: the workspaces and memberships its
// documents name (mirror.go).

// mirrorAt is when the tests below apply what a directory says.
var mirrorAt = time.Unix(10_000, 0)

// write runs fn in one write, failing the test on an error.
func (f *fixture) write(fn func(tx *sql.Tx) error) {
	f.t.Helper()
	if err := f.try(fn); err != nil {
		f.t.Fatal(err)
	}
}

// try runs fn in one write and returns its error.
func (f *fixture) try(fn func(tx *sql.Tx) error) error {
	f.t.Helper()
	return f.db.Write(f.t.Context(), fn)
}

// mirrorTeam mirrors a team as a directory's document names it.
func (f *fixture) mirrorTeam(d workspace.Mirrored) (created, changed bool, accounts []string) {
	f.t.Helper()
	d.Kind = workspace.KindTeam
	f.write(func(tx *sql.Tx) error {
		var err error
		created, changed, accounts, err = workspace.MirrorWorkspaceTx(f.t.Context(), tx, d, mirrorAt)
		return err
	})
	return created, changed, accounts
}

// mirrorMember writes a membership a document lists.
func (f *fixture) mirrorMember(workspaceID, userID string, role workspace.Role) bool {
	f.t.Helper()
	var changed bool
	f.write(func(tx *sql.Tx) error {
		var err error
		changed, err = workspace.MirrorMemberTx(f.t.Context(), tx, workspaceID, userID, role, mirrorAt)
		return err
	})
	return changed
}

// end ends a membership a document omits, held when hold says so and its end
// would strand a mailbox.
func (f *fixture) end(workspaceID, userID string, hold bool) workspace.EndResult {
	f.t.Helper()
	var res workspace.EndResult
	f.write(func(tx *sql.Tx) error {
		var err error
		res, err = workspace.EndMembershipTx(f.t.Context(), tx, workspaceID, userID, hold, mirrorAt)
		return err
	})
	return res
}

// replacePersonal gives a person the personal workspace a document names.
func (f *fixture) replacePersonal(userID, id string) (replaced string, pending bool, stopped int, err error) {
	f.t.Helper()
	err = f.try(func(tx *sql.Tx) error {
		var err error
		replaced, pending, stopped, err = workspace.ReplacePersonalTx(f.t.Context(), tx, userID, id, mirrorAt)
		return err
	})
	return replaced, pending, stopped, err
}

// suspend suspends a person, as an extension would.
func (f *fixture) suspend(userID string) {
	f.t.Helper()
	f.exec(`UPDATE users SET status = 'disabled', suspended_at = 500 WHERE id = ?`, userID)
}

// inviteInto writes an invite into a team for email, made by createdBy,
// waiting.
func (f *fixture) inviteInto(workspaceID, email, createdBy string) {
	f.t.Helper()
	f.exec(`INSERT INTO invites(code_hash, email, role, created_by, created_at, expires_at, id, workspace_id, workspace_role)
		VALUES (randomblob(32), ?, 'member', ?, 100, 99999999999, 'inv_' || lower(hex(randomblob(8))), ?, 'member')`,
		email, createdBy, workspaceID)
}

// stranded lists the journal's strandings as "account cause actor lost".
func (f *fixture) stranded() []string {
	f.t.Helper()
	return queryAll(f.t, f, `SELECT account_id || ' ' || cause || ' ' || actor || ' ' || lost_user FROM mailbox_events
		WHERE kind = 'stranded' ORDER BY seq`)
}

const mirroredTeam = "9f0c1d2e-0000-4000-8000-0000000000a1"

func TestADocumentCreatesAWorkspaceWithTheDirectorysIDAndTheSourcesName(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person("ana@example.org")

	created, changed, _ := f.mirrorTeam(workspace.Mirrored{ID: mirroredTeam, Name: "  Support\tdesk \x07 ", Version: 3})
	if !created || !changed {
		t.Fatalf("mirroring a new team: created %v, changed %v", created, changed)
	}
	if !f.mirrorMember(mirroredTeam, ana.ID, workspace.RoleAdmin) {
		t.Fatal("writing Ana's membership changed nothing")
	}
	if _, pending, _, err := f.replacePersonal(ana.ID, "personal.ana-1"); err != nil || pending {
		t.Fatalf("giving Ana her personal workspace: pending %v, %v", pending, err)
	}

	team, err := f.ws.Get(ctx, mirroredTeam)
	if err != nil || team.Kind != workspace.KindTeam || team.Source != workspace.SourcePlatform ||
		team.Name != "Support desk" || !team.ClosedAt.IsZero() {
		t.Fatalf("the team mirrored: %+v (%v)", team, err)
	}
	if v := f.count(`SELECT source_version FROM workspaces WHERE id = ?`, mirroredTeam); v != 3 {
		t.Errorf("the team took version %d, want the document's 3", v)
	}
	if m, err := f.ws.Member(ctx, mirroredTeam, ana.ID); err != nil || m.Role != workspace.RoleAdmin || !m.Active() {
		t.Errorf("Ana's membership: %+v (%v)", m, err)
	}
	personal, err := f.ws.PersonalOf(ctx, ana.ID)
	if err != nil || personal.ID != "personal.ana-1" || personal.Source != workspace.SourcePlatform {
		t.Fatalf("Ana's personal workspace: %+v (%v)", personal, err)
	}
	if m, err := f.ws.Member(ctx, personal.ID, ana.ID); err != nil || m.Role != workspace.RoleOwner || !m.Active() {
		t.Errorf("Ana in her personal workspace: %+v (%v)", m, err)
	}

	// A team the directory closed is created closed.
	f.mirrorTeam(workspace.Mirrored{ID: "closed-team", Name: "Archive", Closed: true, Version: 1})
	if closed, err := f.ws.Get(ctx, "closed-team"); err != nil || closed.ClosedAt.IsZero() {
		t.Errorf("a team the directory closed: %+v (%v)", closed, err)
	}
	// Mirrored again at the same version, nothing changes.
	if created, changed, _ := f.mirrorTeam(workspace.Mirrored{ID: mirroredTeam, Name: "Other", Version: 3}); created || changed {
		t.Errorf("mirroring the team again at its version: created %v, changed %v", created, changed)
	}
}

func TestADirectoryIDNeverLooksLikeALocalOne(t *testing.T) {
	for id, valid := range map[string]bool{
		"0b4a3c5e-0000-4000-8000-000000000001": true,
		"Team_1.a-b":                           true,
		strings.Repeat("a", 64):                true,
		"":                                     false,
		strings.Repeat("a", 65):                false,
		"wsp_0011223344556677":                 false,
		"wsp_operator":                         false,
		"team/1":                               false,
		"team 1":                               false,
		"équipe":                               false,
	} {
		if got := workspace.ValidExternalID(id); got != valid {
			t.Errorf("ValidExternalID(%q) = %v, want %v", id, got, valid)
		}
	}

	f := newFixture(t)
	ana := f.person("ana@example.org")
	before := f.count(`SELECT count(*) FROM workspaces`)
	err := f.try(func(tx *sql.Tx) error {
		_, _, _, err := workspace.MirrorWorkspaceTx(f.t.Context(), tx, workspace.Mirrored{ID: "wsp_0011223344556677",
			Kind: workspace.KindTeam, Name: "Looks local", Version: 1}, mirrorAt)
		return err
	})
	want(t, "a team whose id looks like this server's", err, workspace.ErrInvalidExternalID)
	_, _, _, err = f.replacePersonal(ana.ID, "wsp_0011223344556677")
	want(t, "a personal workspace whose id looks like this server's", err, workspace.ErrInvalidExternalID)
	if n := f.count(`SELECT count(*) FROM workspaces`); n != before {
		t.Errorf("%d workspaces, want the %d there were", n, before)
	}
}

func TestAPersonalWorkspaceFromADocumentReplacesAnEmptyLocalOne(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana := f.person("ana@example.org")
	local := f.personal(ana.ID)

	replaced, pending, stopped, err := f.replacePersonal(ana.ID, "personal-ana")
	if err != nil || pending || replaced != local || stopped != 0 {
		t.Fatalf("replacing Ana's empty local workspace: replaced %q, pending %v, stopped %d (%v)",
			replaced, pending, stopped, err)
	}
	if _, err := f.ws.Get(ctx, local); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the local personal workspace outlived its replacement: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM workspace_members WHERE workspace_id = ?`, local); n != 0 {
		t.Errorf("the replaced workspace kept %d memberships", n)
	}
	w, err := f.ws.PersonalOf(ctx, ana.ID)
	if err != nil || w.ID != "personal-ana" || w.Source != workspace.SourcePlatform || w.PersonID != ana.ID {
		t.Fatalf("Ana's personal workspace now: %+v (%v)", w, err)
	}
	// The same document again changes nothing.
	if replaced, pending, _, err := f.replacePersonal(ana.ID, "personal-ana"); err != nil || pending || replaced != "" {
		t.Errorf("the same personal workspace again: replaced %q, pending %v (%v)", replaced, pending, err)
	}
}

func TestAPersonalWorkspaceHoldingOnlyStoppedKeysIsReplaced(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	local := f.personal(ana.ID)
	revoked := f.key(auth.ScopeRead, local, ana.ID)
	expired := f.key(auth.ScopeRead, local, ana.ID)
	f.exec(`UPDATE api_keys SET revoked_at = 50, stop_reason = 'manual' WHERE prefix = ?`, revoked)
	f.exec(`UPDATE api_keys SET expires_at = 60 WHERE prefix = ?`, expired)

	replaced, pending, stopped, err := f.replacePersonal(ana.ID, "personal-ana")
	if err != nil || pending || replaced != local || stopped != 2 {
		t.Fatalf("replacing a workspace holding only stopped keys: replaced %q, pending %v, stopped %d (%v)",
			replaced, pending, stopped, err)
	}
	if n := f.count(`SELECT count(*) FROM api_keys WHERE prefix IN (?, ?)`, revoked, expired); n != 0 {
		t.Errorf("%d stopped keys outlived their workspace", n)
	}
}

func TestAPersonalWorkspaceHoldingAMailboxOrALiveKeyIsNotReplaced(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, cy := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cy@example.org")
	f.link(f.personal(ana.ID), ana.ID, "ana@mail.example")
	removed := f.link(f.personal(bea.ID), bea.ID, "bea@mail.example")
	f.remove(removed.ID, bea.ID)
	f.key(auth.ScopeRead, f.personal(cy.ID), cy.ID)

	for _, who := range []auth.User{ana, bea, cy} {
		local := f.personal(who.ID)
		replaced, pending, _, err := f.replacePersonal(who.ID, "personal-"+strings.Split(who.Email, "@")[0])
		if err != nil || !pending || replaced != "" {
			t.Errorf("%s: replaced %q, pending %v (%v), want it pending", who.Email, replaced, pending, err)
		}
		if w, err := f.ws.PersonalOf(ctx, who.ID); err != nil || w.ID != local {
			t.Errorf("%s's personal workspace is %+v (%v), want the local one kept", who.Email, w, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM workspaces WHERE source <> 'local'`); n != 0 {
		t.Errorf("%d workspaces from another source were created", n)
	}
}

func TestADocumentNeverRepointsAWorkspace(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	f.mirrorTeam(workspace.Mirrored{ID: mirroredTeam, Name: "Support", Version: 1})
	if _, _, _, err := f.replacePersonal(bea.ID, "personal-bea"); err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO workspaces(id, kind, source, name, created_at, updated_at) VALUES ('local-team', 'team', 'local', 'Here', 1, 1)`)
	anas := f.personal(ana.ID)

	_, _, _, err := f.replacePersonal(ana.ID, mirroredTeam)
	want(t, "a team named as Ana's personal workspace", err, workspace.ErrKindChanged)
	_, _, _, err = f.replacePersonal(ana.ID, "personal-bea")
	want(t, "Bea's personal workspace named as Ana's", err, workspace.ErrRepointed)
	err = f.try(func(tx *sql.Tx) error {
		_, _, _, err := workspace.MirrorWorkspaceTx(f.t.Context(), tx, workspace.Mirrored{ID: "personal-bea", Kind: workspace.KindTeam,
			Name: "Taken", Version: 9}, mirrorAt)
		return err
	})
	want(t, "Bea's personal workspace named as a team", err, workspace.ErrKindChanged)
	err = f.try(func(tx *sql.Tx) error {
		_, _, _, err := workspace.MirrorWorkspaceTx(f.t.Context(), tx, workspace.Mirrored{ID: "local-team", Kind: workspace.KindTeam,
			Name: "Taken", Version: 9}, mirrorAt)
		return err
	})
	want(t, "a local team named by a directory", err, workspace.ErrRepointed)

	if f.personal(ana.ID) != anas || f.personal(bea.ID) != "personal-bea" {
		t.Error("a refused document moved a personal workspace")
	}
	if names := queryAll(t, f, `SELECT name FROM workspaces WHERE id IN ('local-team', ?) ORDER BY id`, mirroredTeam); !slices.Equal(names,
		[]string{"Support", "Here"}) {
		t.Errorf("the teams are named %v", names)
	}
}

func TestAMembershipFromADocumentIsWrittenForASuspendedPersonAndInAClosedTeam(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	bea := f.person("bea@example.org")
	f.suspend(bea.ID)
	f.mirrorTeam(workspace.Mirrored{ID: mirroredTeam, Name: "Archive", Closed: true, Version: 1})

	if !f.mirrorMember(mirroredTeam, bea.ID, workspace.RoleMember) {
		t.Fatal("writing a suspended person's membership of a closed team changed nothing")
	}
	m, err := f.ws.Member(ctx, mirroredTeam, bea.ID)
	if err != nil || m.Status != workspace.StatusActive || m.Role != workspace.RoleMember || !m.PersonSuspended || m.Active() {
		t.Errorf("Bea's membership: %+v (%v), want it active and counting for nothing while she is suspended", m, err)
	}
	if f.mirrorMember(mirroredTeam, bea.ID, workspace.RoleMember) {
		t.Error("the same membership again changed something")
	}
}

func TestAMembershipEndedFromADocumentTakesGrantsInvitesAndKeysWithoutRefusingTheLastReaderOrOwner(t *testing.T) {
	// Ana is the team's only owner and the only reader of its mailbox, which
	// has a key: a local change would refuse to remove her. A directory's
	// document ends her membership all the same, and takes what a removal
	// takes.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.elsewhere(mirroredTeam, "Support", map[string]workspace.Role{ana.ID: workspace.RoleOwner, bea.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	authtest.KeyMailbox(t, f.db, box.ID, ana.ID)
	f.inviteInto(team.ID, "dan@example.org", ana.ID)
	f.inviteInto(team.ID, ana.Email, bea.ID)
	key := f.key(auth.ScopeRead, team.ID, ana.ID, workspace.KeyGrant{AccountID: box.ID, Flags: readOnly()})

	res := f.end(team.ID, ana.ID, false)
	if res.Held || res.KeysRevoked != 1 || len(res.Stranded) != 1 {
		t.Fatalf("ending Ana's membership: %+v", res)
	}
	for what, n := range map[string]int{
		"her membership":               f.count(`SELECT count(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, team.ID, ana.ID),
		"her flags":                    f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ?`, ana.ID),
		"her sealed grants":            f.count(`SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, ana.ID),
		"the invite she made, waiting": f.count(`SELECT count(*) FROM invites WHERE created_by = ? AND expires_at > ?`, ana.ID, mirrorAt.Unix()),
		"an invite for her address":    f.count(`SELECT count(*) FROM invites WHERE email = ?`, ana.Email),
	} {
		if n != 0 {
			t.Errorf("%s outlived the end of her membership: %d", what, n)
		}
	}
	if reason := queryAll(t, f, `SELECT stop_reason FROM api_keys WHERE prefix = ? AND revoked_at <> 0`, key); !slices.Equal(reason,
		[]string{string(workspace.KeyStopCreatorLeft)}) {
		t.Errorf("the key she created there stopped for %v", reason)
	}
	if n := f.count(`SELECT count(*) FROM workspace_members WHERE workspace_id = ?`, team.ID); n != 1 {
		t.Errorf("the team has %d members, want Bea alone, without an owner", n)
	}
}

func TestAMembershipEndedFromADocumentStrandsWhatItsPersonAloneReadOrWouldRead(t *testing.T) {
	// Bea alone reads one mailbox, Cy, suspended, alone would read another
	// once resumed, and Dan reads a third with Ana. Ending each membership
	// strands the first two, naming whom each lost, and not the third.
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, cy, dan := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cy@example.org"),
		f.person("dan@example.org")
	team := f.elsewhere(mirroredTeam, "Support", map[string]workspace.Role{ana.ID: workspace.RoleOwner,
		bea.ID: workspace.RoleMember, cy.ID: workspace.RoleMember, dan.ID: workspace.RoleMember})
	beas, cys, shared := f.link(team.ID, ana.ID, "one@example.org"), f.link(team.ID, ana.ID, "two@example.org"),
		f.link(team.ID, ana.ID, "three@example.org")
	f.grant(beas.ID, bea.ID, readOnly())
	f.grant(cys.ID, cy.ID, readOnly())
	f.grant(shared.ID, dan.ID, readOnly())
	for _, id := range []string{beas.ID, cys.ID} {
		if _, err := f.ws.Revoke(ctx, id, ana.ID, workspace.Flags{Read: true}, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.suspend(cy.ID)

	for who, wantStranded := range map[string]string{bea.ID: beas.ID, cy.ID: cys.ID, dan.ID: ""} {
		res := f.end(team.ID, who, false)
		var got []string
		for _, s := range res.Stranded {
			got = append(got, s.AccountID)
			if s.WorkspaceID != team.ID || s.Seq == 0 {
				t.Errorf("a stranding of %s: %+v", who, s)
			}
		}
		if wantStranded == "" && len(got) != 0 || wantStranded != "" && !slices.Equal(got, []string{wantStranded}) {
			t.Errorf("ending %s's membership stranded %v, want %q", who, got, wantStranded)
		}
	}
	got := f.stranded()
	slices.Sort(got)
	wantRows := []string{
		beas.ID + " membership_ended directory " + bea.ID,
		cys.ID + " membership_ended directory " + cy.ID,
	}
	slices.Sort(wantRows)
	if !slices.Equal(got, wantRows) {
		t.Errorf("the journal holds %v, want %v", got, wantRows)
	}
}

func TestTheLastMemberLeavingATeamFromAnotherSourceLeavesTheTeamAndItsMailboxes(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana@example.org")
	team := f.elsewhere(mirroredTeam, "Support", map[string]workspace.Role{ana.ID: workspace.RoleOwner})
	box := f.link(team.ID, ana.ID, "support@example.org")

	res := f.end(team.ID, ana.ID, false)
	if len(res.Stranded) != 1 || res.Stranded[0].AccountID != box.ID {
		t.Errorf("Ana leaving stranded %+v, want the team's mailbox", res.Stranded)
	}
	if f.count(`SELECT count(*) FROM workspaces WHERE id = ?`, team.ID) != 1 ||
		f.count(`SELECT count(*) FROM accounts WHERE id = ?`, box.ID) != 1 {
		t.Error("the team or its mailbox went with its last member here")
	}
	if n := f.count(`SELECT count(*) FROM workspace_members WHERE workspace_id = ?`, team.ID); n != 0 {
		t.Errorf("the team has %d members", n)
	}
}

// heldTeam is a team from another source whose mailbox has a key and Bea
// alone reads, Ana its owner reading nothing.
func heldTeam(t *testing.T, f *fixture) (ana, bea auth.User, box string) {
	t.Helper()
	ana, bea = f.person("ana@example.org"), f.person("bea@example.org")
	team := f.elsewhere(mirroredTeam, "Support", map[string]workspace.Role{ana.ID: workspace.RoleOwner, bea.ID: workspace.RoleMember})
	mailbox := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(mailbox.ID, bea.ID, readOnly())
	authtest.KeyMailbox(t, f.db, mailbox.ID, ana.ID)
	if _, err := f.ws.Revoke(t.Context(), mailbox.ID, ana.ID, workspace.Flags{Read: true}, nil); err != nil {
		t.Fatal(err)
	}
	return ana, bea, mailbox.ID
}

func TestAHeldMembershipReadsNothingAndKeepsItsGrants(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	ana, bea, box := heldTeam(t, f)
	key := f.key(auth.ScopeRead, mirroredTeam, bea.ID, workspace.KeyGrant{AccountID: box, Flags: readOnly()})

	res := f.end(mirroredTeam, bea.ID, true)
	if !res.Held || res.KeysRevoked != 1 || len(res.Stranded) != 0 {
		t.Fatalf("ending Bea's membership while it would strand the mailbox: %+v, want it held", res)
	}
	m, err := f.ws.Member(ctx, mirroredTeam, bea.ID)
	if err != nil || !m.Held || m.Status != workspace.StatusDisabled || m.Active() {
		t.Fatalf("Bea's membership: %+v (%v), want it held", m, err)
	}
	if f.count(`SELECT count(*) FROM mailbox_access WHERE user_id = ? AND read = 1`, bea.ID) != 1 ||
		f.count(`SELECT count(*) FROM mailbox_grants WHERE user_id = ?`, bea.ID) != 1 {
		t.Error("the hold did not keep Bea's flag and her grant")
	}
	if f.reads(box, bea.ID) || f.hasReader(box) {
		t.Error("a held member reads the mailbox")
	}
	if f.revokedAt(key) == 0 {
		t.Error("the key Bea created there still works while she is held")
	}
	if len(f.stranded()) != 0 {
		t.Error("a hold journaled a stranding")
	}
	// Held again, it stays held, as it was.
	if again := f.end(mirroredTeam, bea.ID, true); !again.Held || !again.Kept || again.KeysRevoked != 0 {
		t.Errorf("holding a held membership again: %+v", again)
	}
	// Ana reads nothing: her end strands nothing, so nothing holds it.
	if res := f.end(mirroredTeam, ana.ID, true); res.Held {
		t.Error("a membership whose end strands nothing was held")
	}
}

func TestAMembershipListedAgainReleasesItsHold(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	_, bea, box := heldTeam(t, f)
	f.end(mirroredTeam, bea.ID, true)

	if !f.mirrorMember(mirroredTeam, bea.ID, workspace.RoleMember) {
		t.Fatal("listing a held membership again changed nothing")
	}
	m, err := f.ws.Member(ctx, mirroredTeam, bea.ID)
	if err != nil || m.Held || !m.Active() {
		t.Fatalf("Bea's membership: %+v (%v), want it active", m, err)
	}
	if !f.reads(box, bea.ID) {
		t.Error("released, Bea does not read with the grant her hold kept")
	}
}

func TestEndingTheLastOfTwoHeldMembershipsStrandsTheMailboxOnceNamingThatPerson(t *testing.T) {
	f := newFixture(t)
	ana, bea, cy := f.person("ana@example.org"), f.person("bea@example.org"), f.person("cy@example.org")
	team := f.elsewhere(mirroredTeam, "Support", map[string]workspace.Role{ana.ID: workspace.RoleOwner,
		bea.ID: workspace.RoleMember, cy.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	f.grant(box.ID, cy.ID, readOnly())
	if _, err := f.ws.Revoke(t.Context(), box.ID, ana.ID, workspace.Flags{Read: true}, nil); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE workspace_members SET status = 'disabled', held_at = 900 WHERE workspace_id = ? AND user_id IN (?, ?)`,
		team.ID, bea.ID, cy.ID)

	if res := f.end(team.ID, bea.ID, false); len(res.Stranded) != 0 {
		t.Errorf("ending Bea's hold while Cy's keeps the mailbox stranded %+v", res.Stranded)
	}
	res := f.end(team.ID, cy.ID, false)
	if len(res.Stranded) != 1 || res.Stranded[0].AccountID != box.ID {
		t.Errorf("ending the last hold stranded %+v, want the mailbox", res.Stranded)
	}
	if got := f.stranded(); !slices.Equal(got, []string{box.ID + " " + store.CauseMembershipEnded + " directory " + cy.ID}) {
		t.Errorf("the journal holds %v, want one stranding naming Cy", got)
	}
}

func TestEndingTheMembershipOfARemovedMailboxsLastKeptReaderIsHeldOrStrandsIt(t *testing.T) {
	// A removed mailbox Bea alone would read once recovered is kept for her
	// as a live one is: her end is held while the directory's generation is
	// unconfirmed, and strands it, journaled, once it ends.
	f := newFixture(t)
	ana, bea, box := heldTeam(t, f)
	f.remove(box, ana.ID)

	if res := f.end(mirroredTeam, bea.ID, true); !res.Held || len(res.Stranded) != 0 {
		t.Fatalf("ending Bea's membership while it would strand the removed mailbox: %+v, want it held", res)
	}
	res := f.end(mirroredTeam, bea.ID, false)
	if res.Held || len(res.Stranded) != 1 || res.Stranded[0].AccountID != box {
		t.Errorf("ending her held membership: %+v, want the removed mailbox stranded", res)
	}
	if got := f.stranded(); !slices.Equal(got, []string{box + " " + store.CauseMembershipEnded + " directory " + bea.ID}) {
		t.Errorf("the journal holds %v, want one stranding naming Bea", got)
	}
}
