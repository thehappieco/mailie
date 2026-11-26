package app

import (
	"context"
	"log/slog"

	"github.com/thehappieco/mailie/internal/store"
)

// workspaceMailboxesLine is what the daemon says at start of a database that
// holds mailboxes outside the operator workspace.
const workspaceMailboxesLine = "mailboxes outside the operator workspace, in personal or team workspaces: " +
	"no instance key reaches them"

// workspaceMailboxesHint is where an operator learns to move them.
const workspaceMailboxesHint = "to reach them with an instance key, link each address again into the operator " +
	`workspace: see "Moving from the open console" in the README`

// warnWorkspaceMailboxes says at start how many mailboxes the database holds
// outside the operator workspace, personal and team, live and removed (kept
// until deleted for good), and where the README says how to move them: an
// instance key reaches only the operator workspace's mailboxes, and a mailbox
// never changes workspace, so one linked into a person's or a team's
// workspace stays out of its reach until its address is linked again into
// the operator's. Counts only, never an address, a label or an id; nothing
// when there is none.
//
// The daemon calls it only where nobody signs in: never where people sign in
// only through an extension (Options.ExternalSignInOnly), where personal and
// team workspaces are what the edition is made of, nor where they sign in
// with a password (MAIL_PASSWORD_SIGN_IN), where those workspaces are theirs
// to use. A count that fails is logged and the start goes
// on: this changes nothing and decides nothing, and a server that stopped
// for want of a hint would be down for advice.
func warnWorkspaceMailboxes(ctx context.Context, db *store.Store, logger *slog.Logger) {
	n, err := db.CountWorkspaceMailboxes(ctx)
	if err != nil {
		logger.Warn("counting the mailboxes outside the operator workspace failed; whether any are out of an "+
			"instance key's reach is not said at this start", "err", err)
		return
	}
	if n.Total() == 0 {
		return
	}
	logger.Warn(workspaceMailboxesLine,
		"personal_live", n.PersonalLive, "personal_removed", n.PersonalRemoved,
		"team_live", n.TeamLive, "team_removed", n.TeamRemoved,
		"hint", workspaceMailboxesHint)
}
