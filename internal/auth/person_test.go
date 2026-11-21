package auth_test

import (
	"testing"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/workspace"
)

// A person acting through an extension (auth.KindPerson): a person, with no
// session, and no key.

func TestAPersonActingThroughAnExtensionIsNoKeyAndActsAsThemselves(t *testing.T) {
	for _, ws := range []string{"", workspace.OperatorID, "wsp_0000000000000001"} {
		p := auth.Principal{Kind: auth.KindPerson, UserID: "usr_0000000000000001", Scope: auth.ScopeAdmin, WorkspaceID: ws}
		if p.IsKey() || p.IsInstance() || p.IsWorkspaceKey() {
			t.Errorf("in workspace %q a person acting through an extension is a key: key %v, instance %v, workspace key %v",
				ws, p.IsKey(), p.IsInstance(), p.IsWorkspaceKey())
		}
		if p.IsSession() || !p.IsPerson() || !p.IsDelegated() {
			t.Errorf("a person acting through an extension: session %v, person %v, delegated %v", p.IsSession(),
				p.IsPerson(), p.IsDelegated())
		}
		if got := p.Actor(); got != p.UserID {
			t.Errorf("a person acting through an extension acts as %q, want themselves", got)
		}
	}
	// Every other caller is what it was.
	session := auth.Principal{Kind: auth.KindSession, UserID: "usr_0000000000000002"}
	if !session.IsSession() || !session.IsPerson() || session.IsKey() || session.IsDelegated() || session.Actor() != session.UserID {
		t.Errorf("a session: %+v", session)
	}
	for _, kind := range []auth.Kind{"", auth.KindKey} {
		instance := auth.Principal{Kind: kind, KeyPrefix: "aaaaaaaa", WorkspaceID: workspace.OperatorID}
		if !instance.IsKey() || !instance.IsInstance() || instance.IsWorkspaceKey() || instance.IsPerson() ||
			instance.Actor() != "key:aaaaaaaa" {
			t.Errorf("an instance key of kind %q: %+v", kind, instance)
		}
		key := auth.Principal{Kind: kind, KeyPrefix: "bbbbbbbb", WorkspaceID: "wsp_0000000000000001"}
		if !key.IsKey() || key.IsInstance() || !key.IsWorkspaceKey() || key.IsPerson() || key.Actor() != "key:bbbbbbbb" {
			t.Errorf("a workspace key of kind %q: %+v", kind, key)
		}
	}
	if cli := (auth.Principal{WorkspaceID: workspace.OperatorID}); cli.Actor() != "cli" {
		t.Errorf("the command line acts as %q", cli.Actor())
	}
}
