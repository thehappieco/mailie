package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The journal of what was done to mailboxes, and the notices it gives their
// readers (migration 0020; docs/workspaces.md, "Removed mailboxes").
//
// A removal, a sync turned off, a recovery and a deletion for good are each
// written in the transaction that makes them, with who read the mailbox
// then; a removal and a sync turned off tell each of those readers but the
// person who did it, with a notice they see in the console until they
// dismiss it. The journal never holds an address: a mailbox is named by its
// label (its display name, unless that holds an address) and a masked hint
// of its address, both forgotten once it is deleted for good. It has no
// foreign keys, so a row outlives its mailbox and its team, and goes after
// its retention (SweepMailboxEvents), or with the mailbox when a person is
// closed or a link that never connected is abandoned (DeleteMailboxEventsTx).

// MailboxEventKind is what was done to a mailbox.
type MailboxEventKind string

// What can be done to a mailbox, as the journal records it.
const (
	// MailboxRemoved is a mailbox removed: kept for 30 days, recoverable,
	// then deleted for good.
	MailboxRemoved MailboxEventKind = "removed"
	// MailboxSyncPaused is a mailbox's sync turned off, its index deleted: by
	// whoever may, or by a person stopping the consent bound to them.
	MailboxSyncPaused MailboxEventKind = "sync_paused"
	// MailboxRecovered is a removed mailbox recovered, its sync held.
	MailboxRecovered MailboxEventKind = "recovered"
	// MailboxPurged is a mailbox deleted for good.
	MailboxPurged MailboxEventKind = "purged"
)

// Notifies reports whether the readers of a mailbox are told when this is
// done to it: a removal and a sync turned off.
func (k MailboxEventKind) Notifies() bool { return k == MailboxRemoved || k == MailboxSyncPaused }

// MailboxEvent is one entry of the journal.
type MailboxEvent struct {
	// Seq orders the journal; ID is what the console names.
	Seq int64
	ID  string
	// Kind is what was done.
	Kind MailboxEventKind
	// WorkspaceID and AccountID are the mailbox's, kept once both are gone.
	WorkspaceID string
	AccountID   string
	// Label is the mailbox's display name, and Hint its address masked
	// (MailboxHint); both empty once it is deleted for good.
	Label string
	Hint  string
	// Actor is who did it: "usr_…", "key:<prefix>", "cli" or "sweep"; empty
	// once that person is deleted.
	Actor string
	// At is when, in unix seconds.
	At int64
}

// ActorSweep is the actor of what the daemon does by itself: a removed
// mailbox deleted for good once its 30 days are over.
const ActorSweep = "sweep"

// Errors of the journal and the notices.
var (
	// ErrNoSuchMailbox is an event journaled for a mailbox whose row is not
	// there: it is written before the change that deletes it.
	ErrNoSuchMailbox = errors.New("store: no such mailbox")
	// ErrNoNotice is a notice the person does not have.
	ErrNoNotice = errors.New("store: no such notice")
	// ErrNotNotifying is a notice asked for an event nobody is told of.
	ErrNotNotifying = errors.New("store: nobody is told of that")
)

// MailboxHint masks an address for the journal: its first character, an
// ellipsis and its domain, "a…@example.com". Empty for something that is
// not an address.
func MailboxHint(email string) string {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	first, _ := utf8.DecodeRuneInString(email)
	if first == utf8.RuneError {
		return ""
	}
	return string(first) + "…@" + email[at+1:]
}

// mailboxLabel is the name the journal keeps of a mailbox: its display
// name, unless that holds an address, which the journal never does.
func mailboxLabel(displayName string) string {
	if strings.Contains(displayName, "@") {
		return ""
	}
	return displayName
}

// MailboxReadersTx lists, inside the caller's transaction, who reads a
// mailbox now, by the one rule (ReaderSQL): the readers an event records.
// A removed mailbox has none, so a removal reads them before the write that
// removes it.
func MailboxReadersTx(ctx context.Context, tx *sql.Tx, accountID string) ([]string, error) {
	ids, err := queryIDs(ctx, tx, `SELECT g.user_id FROM mailbox_access g
		WHERE g.account_id = ? AND `+ReaderSQL("g")+` ORDER BY g.user_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: read the mailbox's readers: %w", err)
	}
	return ids, nil
}

// JournalMailboxTx writes, inside the transaction that does it, what was
// done to a mailbox: ev names its kind, the mailbox, the actor and the
// moment (At); the mailbox's workspace, label and hint are read from its
// row, which must still be there (ErrNoSuchMailbox), and a deletion for
// good records neither label nor hint, since it forgets them. readers are
// who read the mailbox then (MailboxReadersTx). With notify, each of them
// but the actor gets a notice, of a removal or a sync turned off only
// (ErrNotNotifying). It returns the event's seq and id.
func JournalMailboxTx(ctx context.Context, tx *sql.Tx, ev MailboxEvent, readers []string, notify bool) (int64, string, error) {
	if notify && !ev.Kind.Notifies() {
		return 0, "", fmt.Errorf("%w: %s", ErrNotNotifying, ev.Kind)
	}
	var displayName, email string
	err := tx.QueryRowContext(ctx, `SELECT workspace_id, display_name, email FROM accounts WHERE id = ?`, ev.AccountID).
		Scan(&ev.WorkspaceID, &displayName, &email)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", fmt.Errorf("%w: %s", ErrNoSuchMailbox, ev.AccountID)
	case err != nil:
		return 0, "", fmt.Errorf("store: journal the mailbox: %w", err)
	}
	ev.Label, ev.Hint = "", ""
	if ev.Kind != MailboxPurged {
		ev.Label, ev.Hint = mailboxLabel(displayName), MailboxHint(email)
	}
	var raw [8]byte
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(raw[:])
	ev.ID = "mbe_" + hex.EncodeToString(raw[:])
	if err := tx.QueryRowContext(ctx, `INSERT INTO mailbox_events(id, kind, workspace_id, account_id, label, hint,
		  actor, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING seq`,
		ev.ID, string(ev.Kind), ev.WorkspaceID, ev.AccountID, ev.Label, ev.Hint, ev.Actor, ev.At).Scan(&ev.Seq); err != nil {
		return 0, "", fmt.Errorf("store: journal the mailbox: %w", err)
	}
	for _, userID := range readers {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_event_readers(event_seq, user_id) VALUES (?, ?)
			ON CONFLICT DO NOTHING`, ev.Seq, userID); err != nil {
			return 0, "", fmt.Errorf("store: record who read the mailbox: %w", err)
		}
		if !notify || userID == ev.Actor {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notices(user_id, event_seq, created_at) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, userID, ev.Seq, ev.At); err != nil {
			return 0, "", fmt.Errorf("store: tell a reader: %w", err)
		}
	}
	return ev.Seq, ev.ID, nil
}

// ForgetMailboxNamesTx forgets, inside the caller's transaction, the label
// and the hint of every event of these mailboxes: what a deletion for good
// does to the journal it keeps.
func ForgetMailboxNamesTx(ctx context.Context, tx *sql.Tx, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("store: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_events SET label = '', hint = ''
		WHERE account_id IN (SELECT value FROM json_each(?)) AND (label <> '' OR hint <> '')`, string(list)); err != nil {
		return fmt.Errorf("store: forget the mailboxes' names: %w", err)
	}
	return nil
}

// DeleteMailboxEventsTx deletes, inside the caller's transaction, every
// event of these mailboxes, with who read them and the notices they gave:
// what goes with a person who is closed, and with a link that never
// connected.
func DeleteMailboxEventsTx(ctx context.Context, tx *sql.Tx, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("store: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_events WHERE account_id IN (SELECT value FROM json_each(?))`,
		string(list)); err != nil {
		return fmt.Errorf("store: delete the mailboxes' journal: %w", err)
	}
	return nil
}

// Notice is one of a person's notices: the event it tells of, and the
// mailbox and its workspace as they stand now.
type Notice struct {
	Event MailboxEvent
	// CreatedAt is when the person was told, in unix seconds.
	CreatedAt int64
	// WorkspaceName is the workspace's name now: empty for a personal
	// workspace, which has none, and for one that is gone.
	WorkspaceName string
	// Kept is the mailbox's row being there, live or removed; Email is its
	// address then, empty once it is deleted for good.
	Kept  bool
	Email string
	// RemovedAt and PurgeAfter are the mailbox's removal now, in unix
	// seconds: zero while it is live, and once it is deleted for good.
	RemovedAt  int64
	PurgeAfter int64
}

// Notices lists a person's notices, newest first.
func (s *Store) Notices(ctx context.Context, userID string) ([]Notice, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT e.seq, e.id, e.kind, e.workspace_id, e.account_id, e.label, e.hint,
		       e.actor, e.created_at, n.created_at, coalesce(w.name, ''), a.id IS NOT NULL, coalesce(a.email, ''),
		       coalesce(a.removed_at, 0), coalesce(a.purge_after, 0)
		  FROM notices n
		  JOIN mailbox_events e ON e.seq = n.event_seq
		  LEFT JOIN workspaces w ON w.id = e.workspace_id
		  LEFT JOIN accounts a ON a.id = e.account_id
		 WHERE n.user_id = ? ORDER BY e.seq DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: list the notices: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Notice
	for rows.Next() {
		var (
			n    Notice
			kind string
		)
		if err := rows.Scan(&n.Event.Seq, &n.Event.ID, &kind, &n.Event.WorkspaceID, &n.Event.AccountID, &n.Event.Label,
			&n.Event.Hint, &n.Event.Actor, &n.Event.At, &n.CreatedAt, &n.WorkspaceName, &n.Kept, &n.Email,
			&n.RemovedAt, &n.PurgeAfter); err != nil {
			return nil, fmt.Errorf("store: list the notices: %w", err)
		}
		n.Event.Kind = MailboxEventKind(kind)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the notices: %w", err)
	}
	return out, nil
}

// DismissNotice deletes a person's notice of the event eventID ("mbe_…");
// ErrNoNotice when they have none.
func (s *Store) DismissNotice(ctx context.Context, userID, eventID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM notices
			WHERE user_id = ? AND event_seq = (SELECT seq FROM mailbox_events WHERE id = ?)`, userID, eventID)
		if err != nil {
			return fmt.Errorf("store: dismiss the notice: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: dismiss the notice: %w", err)
		}
		if n == 0 {
			return ErrNoNotice
		}
		return nil
	})
}

// SweepMailboxEvents deletes the events written before before (unix
// seconds), with who read the mailboxes then and the notices they gave, and
// reports how many it deleted: the journal's retention.
func (s *Store) SweepMailboxEvents(ctx context.Context, before int64) (int, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM mailbox_events WHERE created_at < ?`, before)
		if err != nil {
			return fmt.Errorf("store: sweep the mailbox journal: %w", err)
		}
		n, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: sweep the mailbox journal: %w", err)
		}
		return nil
	})
	return int(n), err
}
