package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thehappieco/mailie/internal/store"
)

// A workspace's settings, for an extension that reports them elsewhere beside
// its snapshot (snapshot.go; docs/workspaces.md, "What an extension reads,
// and the freshness of an act"): every mailbox, live and removed until it is
// deleted for good, in the snapshot's order, with what its row says of how
// it connects and syncs and what the one reading rule says of who reads it;
// and every member with their status. Never a mailbox's address: nothing
// here selects it. It decides nothing, and reads in the caller's
// transaction, which a read-only one keeps to one moment (store.ReadTx),
// each list with its own bound, so a snapshot too large for one mailbox's
// people does not stop the settings.

// Settings is a workspace's mailboxes and members, read at one moment.
type Settings struct {
	Kind   Kind
	Closed bool
	// Mailboxes are the oldest first, as a snapshot lists them; nil with
	// MailboxesOver when there are more than SnapshotMaxMailboxes.
	Mailboxes     []MailboxSettings
	MailboxesOver bool
	// Members are by user id; nil with MembersOver when there are more than
	// SnapshotMaxPeople.
	Members     []MemberSettings
	MembersOver bool
}

// MailboxSettings is what a mailbox's row and its index say of it. Never its
// address: IMAPHost is the server's name, which the service uses to present
// the provider and the sent copy, and reports neither.
type MailboxSettings struct {
	ID string
	// Provider and AuthKind are the row's: gmail, microsoft or imap; oauth2
	// or password.
	Provider, AuthKind string
	IMAPHost           string
	SaveSentCopy       bool
	// ServerKind is what the last login recorded found the server to be
	// (migration 0027).
	ServerKind string
	// State and StateReason are the row's: pending_auth, active,
	// needs_reauth, disabled or error, and its fixed reason.
	State, StateReason string
	// ConnectedAt is when it first stored something to sign in with, unix
	// seconds; 0 when it never did, or when that is not known (migration
	// 0027).
	ConnectedAt int64
	// LinkedBy is who linked it, raw: "usr_…", "key:<prefix>", "cli", or
	// empty once that person is deleted.
	LinkedBy string
	// Personal is a personal workspace's mailbox, whose sync is its
	// person's.
	Personal bool
	// KeyEpoch is its current epoch, 0 for a mailbox without a key.
	KeyEpoch int
	// NoReader: nobody reads it now, or, removed, would read it once it is
	// recovered (store.KeptReaderSQL, what a snapshot's Reads says).
	NoReader bool
	// ConsentStands is the consent it syncs under standing
	// (store.ConsentStandsSQL); Held, a personal mailbox's sync held by the
	// operator's turn-off.
	ConsentStands, Held bool
	// SyncEnabledAt, SyncEnabledBy and SyncConsentVersion are a team's
	// agreement, raw; Bound, that agreement still the one migration 0011
	// copied from its linker.
	SyncEnabledAt                     int64
	SyncEnabledBy, SyncConsentVersion string
	Bound                             bool
	// SyncPausedAt and SyncPausedBy are its pause, raw: 0 and empty when it
	// is not paused.
	SyncPausedAt int64
	SyncPausedBy string
	// LastOKAt is when its sync last worked, unix seconds; 0 when never, or
	// since its index was deleted.
	LastOKAt int64
	// FoldersTotal and FoldersSynced count its folders that sync, and those
	// whose first sync finished (store.SyncSummary's); Messages, that it
	// indexed a message that is still there.
	FoldersTotal, FoldersSynced int
	Messages                    bool
	// RemovedAt and PurgeAfter are its removal, unix seconds; 0 while it is
	// live.
	RemovedAt, PurgeAfter int64
	// IndexKept is any of its index left, a folder or a message: the test a
	// turn-off asks before it deletes one (store.StopSync).
	IndexKept bool
}

// MemberSettings is a member of a workspace and what holds them back.
type MemberSettings struct {
	UserID string
	Role   Role
	// Status is the membership's.
	Status Status
	// Held is a membership an external directory ended and this server
	// holds (migration 0022).
	Held bool
	// PersonDisabled is the person's status disabled: disabled here, or
	// suspended by an extension, which PersonSuspended says (a disable here
	// clears a suspension and stands over it).
	PersonDisabled, PersonSuspended bool
	// Enrolled is the person having an account key.
	Enrolled bool
}

// SettingsTx reads a workspace's settings inside the caller's transaction.
// ErrNotFound for a workspace nobody knows.
func SettingsTx(ctx context.Context, tx *sql.Tx, workspaceID string) (Settings, error) {
	var (
		out  Settings
		kind string
	)
	err := tx.QueryRowContext(ctx, `SELECT kind, closed_at <> 0 FROM workspaces WHERE id = ?`, workspaceID).
		Scan(&kind, &out.Closed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Settings{}, ErrNotFound
	case err != nil:
		return Settings{}, fmt.Errorf("workspace: read the workspace: %w", err)
	}
	out.Kind = Kind(kind)
	if out.Mailboxes, err = settingsMailboxes(ctx, tx, workspaceID); err != nil {
		return Settings{}, err
	}
	out.MailboxesOver = out.Mailboxes == nil
	if out.Members, err = settingsMembers(ctx, tx, workspaceID); err != nil {
		return Settings{}, err
	}
	out.MembersOver = out.Members == nil
	return out, nil
}

// settingsFolders is the folders of the mailbox aliased a whose sync counts,
// as store.SyncSummary counts them.
const settingsFolders = `FROM folders f WHERE f.account_id = a.id AND f.synced = 1 AND f.selectable = 1
	AND f.missing_since = 0`

// settingsMailboxes reads a workspace's mailboxes, oldest first; nil past
// SnapshotMaxMailboxes.
func settingsMailboxes(ctx context.Context, tx *sql.Tx, workspaceID string) ([]MailboxSettings, error) {
	//nolint:gosec // G202: built from the rule's fragments and constants alone; every value is bound
	rows, err := tx.QueryContext(ctx, `SELECT a.id, a.provider, a.auth_kind, a.imap_host, a.save_sent_copy,
		       a.server_kind, a.state, a.state_reason, a.connected_at, a.linked_by, a.owner_user_id IS NOT NULL,
		       coalesce(`+store.CurrentEpochSQL("a.id")+`, 0),
		       NOT EXISTS (SELECT 1 FROM mailbox_access g WHERE g.account_id = a.id AND `+store.KeptReaderSQL("g")+`),
		       `+store.ConsentStandsSQL("a")+`, a.sync_held_at <> 0,
		       a.sync_enabled_at, a.sync_enabled_by, a.sync_consent_version, a.sync_enabled_via = 'migration',
		       a.sync_paused_at, a.sync_paused_by, a.last_ok_at,
		       (SELECT count(*) `+settingsFolders+`),
		       (SELECT coalesce(sum(f.sync_state IN ('live', 'error') AND f.uidvalidity <> 0 AND f.backfill_cursor = 0), 0)
		          `+settingsFolders+`),
		       EXISTS (SELECT 1 FROM messages m WHERE m.account_id = a.id AND m.vanished_at = 0),
		       a.removed_at, a.purge_after,
		       EXISTS (SELECT 1 FROM folders WHERE account_id = a.id) OR EXISTS (SELECT 1 FROM messages WHERE account_id = a.id)
		  FROM accounts a WHERE a.workspace_id = ? ORDER BY a.created_at, a.rowid LIMIT ?`,
		workspaceID, SnapshotMaxMailboxes+1)
	if err != nil {
		return nil, fmt.Errorf("workspace: read the settings' mailboxes: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []MailboxSettings{}
	for rows.Next() {
		var mb MailboxSettings
		if err := rows.Scan(&mb.ID, &mb.Provider, &mb.AuthKind, &mb.IMAPHost, &mb.SaveSentCopy, &mb.ServerKind, &mb.State,
			&mb.StateReason, &mb.ConnectedAt, &mb.LinkedBy, &mb.Personal, &mb.KeyEpoch, &mb.NoReader, &mb.ConsentStands,
			&mb.Held, &mb.SyncEnabledAt, &mb.SyncEnabledBy, &mb.SyncConsentVersion, &mb.Bound, &mb.SyncPausedAt,
			&mb.SyncPausedBy, &mb.LastOKAt, &mb.FoldersTotal, &mb.FoldersSynced, &mb.Messages, &mb.RemovedAt,
			&mb.PurgeAfter, &mb.IndexKept); err != nil {
			return nil, fmt.Errorf("workspace: read the settings' mailboxes: %w", err)
		}
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the settings' mailboxes: %w", err)
	}
	if len(out) > SnapshotMaxMailboxes {
		return nil, nil
	}
	return out, nil
}

// settingsMembers reads a workspace's members, by user id; nil past
// SnapshotMaxPeople.
func settingsMembers(ctx context.Context, tx *sql.Tx, workspaceID string) ([]MemberSettings, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.user_id, m.role, m.status, m.held_at <> 0, u.status = 'disabled',
		       u.suspended_at <> 0, u.public_key IS NOT NULL
		  FROM workspace_members m JOIN users u ON u.id = m.user_id
		 WHERE m.workspace_id = ? ORDER BY m.user_id LIMIT ?`, workspaceID, SnapshotMaxPeople+1)
	if err != nil {
		return nil, fmt.Errorf("workspace: read the settings' members: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []MemberSettings{}
	for rows.Next() {
		var m MemberSettings
		if err := rows.Scan(&m.UserID, &m.Role, &m.Status, &m.Held, &m.PersonDisabled, &m.PersonSuspended,
			&m.Enrolled); err != nil {
			return nil, fmt.Errorf("workspace: read the settings' members: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read the settings' members: %w", err)
	}
	if len(out) > SnapshotMaxPeople {
		return nil, nil
	}
	return out, nil
}
