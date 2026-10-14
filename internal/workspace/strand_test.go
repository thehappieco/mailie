package workspace_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestAStrandingIsJournaledNamingWhomItLostAndTellsNobody(t *testing.T) {
	// Ana and Bea are members of a team from another source; Bea alone
	// reads its mailbox, which an external directory leaves stranded when
	// it ends her membership.
	f := newFixture(t)
	ana, bea := f.person("ana@example.org"), f.person("bea@example.org")
	team := f.elsewhere("0b4a3c5e-0000-4000-8000-000000000002", "Elsewhere",
		map[string]workspace.Role{ana.ID: workspace.RoleOwner, bea.ID: workspace.RoleMember})
	box := f.link(team.ID, ana.ID, "support@example.org")
	f.grant(box.ID, bea.ID, readOnly())
	at := time.Unix(5000, 0)

	var got []workspace.Stranding
	if err := f.db.Write(t.Context(), func(tx *sql.Tx) (err error) {
		got, err = workspace.JournalStrandedTx(t.Context(), tx, []string{box.ID}, bea.ID, store.CauseMembershipEnded,
			store.ActorDirectory, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AccountID != box.ID || got[0].WorkspaceID != team.ID || got[0].Seq == 0 {
		t.Fatalf("the strandings: %+v", got)
	}
	rows := queryAll(t, f, `SELECT e.kind || ' ' || e.cause || ' ' || e.actor || ' ' || e.lost_user || ' ' || e.created_at
		|| ' ' || r.user_id FROM mailbox_events e JOIN mailbox_event_readers r ON r.event_seq = e.seq WHERE e.seq = ?`, got[0].Seq)
	if want := []string{"stranded membership_ended directory " + bea.ID + " 5000 " + bea.ID}; !slices.Equal(rows, want) {
		t.Errorf("the journal holds %v, want %v", rows, want)
	}
	if n := f.count(`SELECT count(*) FROM notices`); n != 0 {
		t.Errorf("a stranding gave %d notices", n)
	}
}
