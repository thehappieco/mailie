package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A person's own settings as an extension reports them elsewhere
// (docs/workspaces.md, "What an extension reads, and the freshness of an
// act"): their consents, whether they have an account key, when they last
// signed out of every session, and their notices, each named by what the
// journal kept of its mailbox — its label and masked hint, never its
// address — with where the mailbox stands now. Read in the caller's
// transaction, which a read-only one keeps to one moment (ReadTx). It
// decides nothing.

// MaxPersonNotices is the most notices PersonSettingsTx reads.
const MaxPersonNotices = 1000

// PersonSettingsRow is what the users row and the notices say of a person.
type PersonSettingsRow struct {
	// SyncConsent, ActionsConsent and SendConsent are each consent's moment
	// and revision, 0 and "" when it is off; the withdrawals are 0 when
	// never withdrawn, or given since, as the users row keeps them.
	SyncConsentAt, ActionsConsentAt, SendConsentAt                int64
	SyncConsentVersion, ActionsConsentVersion, SendConsentVersion string
	ActionsConsentWithdrawnAt, SendConsentWithdrawnAt             int64
	// Enrolled is having an account key.
	Enrolled bool
	// SignedOutEverywhereAt is when they last signed out of every session,
	// 0 when never or not known (migration 0027).
	SignedOutEverywhereAt int64
	// Notices are the newest first, at most the limit read.
	Notices []PersonNoticeRow
	// More is notices beyond the limit.
	More bool
}

// PersonNoticeRow is one of a person's notices: the journal entry it tells
// of, and its mailbox and workspace as they stand now. It never carries the
// mailbox's address.
type PersonNoticeRow struct {
	Event MailboxEvent
	// Team is the notice's workspace being a team now: false for a
	// personal workspace, and once it is gone.
	Team bool
	// Kept is the mailbox's row being there, live or removed; Removed, the
	// mailbox removed now, kept until PurgeAfter (unix seconds).
	Kept, Removed bool
	PurgeAfter    int64
	// PurgedAt is when the mailbox was deleted for good, as its purged entry
	// says, unix seconds; 0 while the journal holds no such entry.
	PurgedAt int64
	// ActorMember is a person who acted ("usr_…") holding a membership of
	// the notice's workspace now, whatever its status.
	ActorMember bool
}

// PersonSettingsTx reads a person's settings and their newest notices, at
// most limit (1 to MaxPersonNotices; out of that range, MaxPersonNotices),
// inside the caller's transaction. ErrNoSuchUser for nobody.
func PersonSettingsTx(ctx context.Context, tx *sql.Tx, userID string, limit int) (PersonSettingsRow, error) {
	if limit < 1 || limit > MaxPersonNotices {
		limit = MaxPersonNotices
	}
	var out PersonSettingsRow
	err := tx.QueryRowContext(ctx, `SELECT sync_consent_at, sync_consent_version, actions_consent_at,
		       actions_consent_version, actions_consent_withdrawn_at, send_consent_at, send_consent_version,
		       send_consent_withdrawn_at, public_key IS NOT NULL, signed_out_everywhere_at
		  FROM users WHERE id = ?`, userID).Scan(&out.SyncConsentAt, &out.SyncConsentVersion, &out.ActionsConsentAt,
		&out.ActionsConsentVersion, &out.ActionsConsentWithdrawnAt, &out.SendConsentAt, &out.SendConsentVersion,
		&out.SendConsentWithdrawnAt, &out.Enrolled, &out.SignedOutEverywhereAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PersonSettingsRow{}, ErrNoSuchUser
	case err != nil:
		return PersonSettingsRow{}, fmt.Errorf("store: read the person's settings: %w", err)
	}
	// Never a.email: a notice names its mailbox by what the journal kept.
	rows, err := tx.QueryContext(ctx, `SELECT e.seq, e.id, e.kind, e.workspace_id, e.account_id, e.label, e.hint,
		       e.actor, e.created_at, e.cause, e.lost_user, e.purge_after,
		       EXISTS (SELECT 1 FROM workspaces w WHERE w.id = e.workspace_id AND w.kind = 'team'),
		       a.id IS NOT NULL, coalesce(a.removed_at, 0) <> 0, coalesce(a.purge_after, 0),
		       coalesce((SELECT max(p.created_at) FROM mailbox_events p
		                  WHERE p.account_id = e.account_id AND p.kind = 'purged'), 0),
		       e.actor GLOB 'usr_*' AND EXISTS (SELECT 1 FROM workspace_members m
		                                         WHERE m.workspace_id = e.workspace_id AND m.user_id = e.actor)
		  FROM notices n
		  JOIN mailbox_events e ON e.seq = n.event_seq
		  LEFT JOIN accounts a ON a.id = e.account_id
		 WHERE n.user_id = ?
		 ORDER BY e.seq DESC LIMIT ?`, userID, limit+1)
	if err != nil {
		return PersonSettingsRow{}, fmt.Errorf("store: read the person's notices: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out.Notices = []PersonNoticeRow{}
	for rows.Next() {
		var (
			n    PersonNoticeRow
			kind string
		)
		if err := rows.Scan(&n.Event.Seq, &n.Event.ID, &kind, &n.Event.WorkspaceID, &n.Event.AccountID, &n.Event.Label,
			&n.Event.Hint, &n.Event.Actor, &n.Event.At, &n.Event.Cause, &n.Event.LostUser, &n.Event.PurgeAfter, &n.Team,
			&n.Kept, &n.Removed, &n.PurgeAfter, &n.PurgedAt, &n.ActorMember); err != nil {
			return PersonSettingsRow{}, fmt.Errorf("store: read the person's notices: %w", err)
		}
		n.Event.Kind = MailboxEventKind(kind)
		if !n.Removed {
			n.PurgeAfter = 0
		}
		if len(out.Notices) == limit {
			out.More = true
			continue
		}
		out.Notices = append(out.Notices, n)
	}
	if err := rows.Err(); err != nil {
		return PersonSettingsRow{}, fmt.Errorf("store: read the person's notices: %w", err)
	}
	return out, nil
}
