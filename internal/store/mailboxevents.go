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
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The journal of what was done to mailboxes, and the notices it gives their
// readers (migrations 0020 and 0021; docs/workspaces.md, "Removed
// mailboxes").
//
// A removal, a sync turned off, a recovery, a deletion for good and a
// stranding are each written in the transaction that makes them, with who
// read the mailbox then; a removal and a sync turned off tell each of those
// readers but the person who did it, with a notice they see in the console
// until they dismiss it. The journal never holds an address: a mailbox is
// named by its label (its display name, unless that holds an address or its
// local part, MailboxLabel) and a masked hint of its address. Deleted for
// good, it loses its label, and its hint once no notice still to be read
// needs it to name the mailbox. It has no foreign keys, so a row outlives
// its mailbox and its team, and goes after its retention
// (SweepMailboxEvents), or with the mailbox when a person is closed or a
// link that never connected is abandoned in a local workspace
// (DeleteMailboxEventsTx). It is read per workspace, in order of seq, by an
// extension that reports it elsewhere (mailbox_events_workspace).

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
	// MailboxStranded is a team mailbox left with nobody who reads it, would
	// read it once resumed, or would read it but for a hold, because the one
	// person who did lost it (LostUser, Cause). Nobody is told of it in the
	// console; an extension reads it.
	MailboxStranded MailboxEventKind = "stranded"
)

// Why an entry was written: a stranding's cause, always one of the first
// five, and a sync turned off's, empty but for CauseBoundStopped.
const (
	// CauseMembershipEnded is a membership an external directory ended.
	CauseMembershipEnded = "membership_ended"
	// CauseForgotten is a person an external directory closed, forgotten
	// here.
	CauseForgotten = "forgotten"
	// CauseDisabled is a person disabled here with force.
	CauseDisabled = "disabled"
	// CauseDeleted is a person deleted here with force.
	CauseDeleted = "deleted"
	// CauseReset is a person whose account key was reset with force, which
	// took their sealed grants.
	CauseReset = "reset"
	// CauseBoundStopped is a sync turned off because the consent migration
	// 0011 bound to a person stopped with them (StopBoundTx): a pause the
	// daemon makes on its own, not one a person chose for the mailbox.
	CauseBoundStopped = "bound_stopped"
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
	// (MailboxHint); the label empty once it is deleted for good, and the
	// hint once no notice still to be read needs it.
	Label string
	Hint  string
	// Actor is who did it: "usr_…", "key:<prefix>", "cli", "sweep",
	// "directory" or "operator"; empty once that person is forgotten.
	Actor string
	// At is when, in unix seconds.
	At int64
	// Cause is why: a stranding's (CauseMembershipEnded … CauseReset), and
	// a sync turned off's, empty or CauseBoundStopped; empty on every other
	// kind.
	Cause string
	// LostUser is the person a stranding lost, empty once forgotten: at
	// their deletion, or, when an external directory follows them, at their
	// release. Empty on every other kind.
	LostUser string
	// PurgeAfter is when a removed mailbox may be deleted for good, as its
	// removal recorded it, in unix seconds; zero on every other kind.
	PurgeAfter int64
}

// MailboxJournalRetention is how long the journal keeps an event, with who
// read the mailbox then and the notices it gave: 90 days, then the hourly
// sweep deletes it (SweepMailboxEvents).
const MailboxJournalRetention = 90 * 24 * time.Hour

// ActorSweep is the actor of what the daemon does by itself: a removed
// mailbox deleted for good once its 30 days are over.
const ActorSweep = "sweep"

// ActorDirectory is the actor of what an external directory decides through
// an extension of the daemon: a membership it ended, a person it closed.
const ActorDirectory = "directory"

// ActorOperator is the actor of what the operator decides for a mailbox of
// any workspace, outside the ways its own people have: its sync turned off
// and its index deleted (StopSync). The operator is nobody's reader and no
// person here, whichever instance key they used, which the daemon's log
// names.
const ActorOperator = "operator"

// Errors of the journal and the notices.
var (
	// ErrNoSuchMailbox is an event journaled for a mailbox whose row is not
	// there: it is written before the change that deletes it.
	ErrNoSuchMailbox = errors.New("store: no such mailbox")
	// ErrNoNotice is a notice the person does not have.
	ErrNoNotice = errors.New("store: no such notice")
	// ErrNotNotifying is a notice asked for an event nobody is told of.
	ErrNotNotifying = errors.New("store: nobody is told of that")
	// ErrNoLostPerson is a stranding that names no person it lost.
	ErrNoLostPerson = errors.New("store: a stranding names the person it lost")
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

// MailboxLabel is the name the journal, and what an extension reports of a
// mailbox, keep of it: its display name, unless that holds an address or
// the local part of the mailbox's own (email up to its last "@", of three
// characters or more), compared without regard to case, which would say the
// address all the same. Empty then. An email without "@" counts whole as
// its local part. Both are compared in a normal form (labelForm), so a
// character nobody sees, a decomposed accent or a fullwidth at sign never
// hides the address; what is kept is the display name as written.
func MailboxLabel(displayName, email string) string {
	name := labelForm(displayName)
	if strings.ContainsAny(name, "@\uff20\ufe6b") {
		return ""
	}
	local := email
	if at := strings.LastIndex(email, "@"); at >= 0 {
		local = email[:at]
	}
	local = labelForm(local)
	if utf8.RuneCountInString(local) >= 3 && strings.Contains(strings.ToLower(name), strings.ToLower(local)) {
		return ""
	}
	return displayName
}

// labelForm is s as MailboxLabel compares it: in NFC, without control
// characters, format characters (zero-width ones, the soft hyphen, the word
// joiner, bidirectional controls) or the combining grapheme joiner, and with
// every run of white space one space, trimmed.
func labelForm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFC.String(s) {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == '\u034f':
			continue
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return b.String()
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
// done to a mailbox: ev names its kind, the mailbox, the actor, the moment
// (At), and its cause and the person it lost when it has them; the mailbox's
// workspace, label (MailboxLabel) and hint are read from its row, which must
// still be there (ErrNoSuchMailbox), and so is a removal's end
// (purge_after), so a removal is journaled once the row records it. A
// deletion for good records neither label nor hint, since it forgets them.
// A stranding names the person it lost (ErrNoLostPerson), who must still
// exist: readers name people. readers are who read the mailbox then
// (MailboxReadersTx). With notify, each of them but the actor gets a notice,
// of a removal or a sync turned off only (ErrNotNotifying). It returns the
// event's seq and id.
func JournalMailboxTx(ctx context.Context, tx *sql.Tx, ev MailboxEvent, readers []string, notify bool) (int64, string, error) {
	if notify && !ev.Kind.Notifies() {
		return 0, "", fmt.Errorf("%w: %s", ErrNotNotifying, ev.Kind)
	}
	if ev.Kind == MailboxStranded && ev.LostUser == "" {
		return 0, "", ErrNoLostPerson
	}
	var displayName, email string
	var purgeAfter int64
	err := tx.QueryRowContext(ctx, `SELECT workspace_id, display_name, email, purge_after FROM accounts WHERE id = ?`,
		ev.AccountID).Scan(&ev.WorkspaceID, &displayName, &email, &purgeAfter)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, "", fmt.Errorf("%w: %s", ErrNoSuchMailbox, ev.AccountID)
	case err != nil:
		return 0, "", fmt.Errorf("store: journal the mailbox: %w", err)
	}
	ev.Label, ev.Hint = "", ""
	if ev.Kind != MailboxPurged {
		ev.Label, ev.Hint = MailboxLabel(displayName, email), MailboxHint(email)
	}
	ev.PurgeAfter = 0
	if ev.Kind == MailboxRemoved {
		ev.PurgeAfter = purgeAfter
	}
	var raw [8]byte
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(raw[:])
	ev.ID = "mbe_" + hex.EncodeToString(raw[:])
	if err := tx.QueryRowContext(ctx, `INSERT INTO mailbox_events(id, kind, workspace_id, account_id, label, hint,
		  actor, created_at, cause, lost_user, purge_after) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING seq`,
		ev.ID, string(ev.Kind), ev.WorkspaceID, ev.AccountID, ev.Label, ev.Hint, ev.Actor, ev.At,
		ev.Cause, ev.LostUser, ev.PurgeAfter).Scan(&ev.Seq); err != nil {
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
// of every event of these mailboxes, and the hint of each no reader holds a
// notice of: what a deletion for good does to the journal it keeps. A
// notice still to be read names the mailbox deleted for good by that masked
// hint alone, which is forgotten in its turn once no notice needs it
// (DismissNotice, SweepMailboxEvents).
func ForgetMailboxNamesTx(ctx context.Context, tx *sql.Tx, accountIDs []string) error {
	if len(accountIDs) == 0 {
		return nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return fmt.Errorf("store: encode ids: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_events
		SET label = '',
		    hint = CASE WHEN EXISTS (SELECT 1 FROM notices n WHERE n.event_seq = mailbox_events.seq) THEN hint ELSE '' END
		WHERE account_id IN (SELECT value FROM json_each(?)) AND (label <> '' OR hint <> '')`, string(list)); err != nil {
		return fmt.Errorf("store: forget the mailboxes' names: %w", err)
	}
	return nil
}

// forgetUnneededHintsTx forgets, inside the caller's transaction, the hint
// of every event of a mailbox deleted for good that no reader holds a notice
// of any longer, among those whose seq is in seqs, or every one for nil.
func forgetUnneededHintsTx(ctx context.Context, tx *sql.Tx, seqs []int64) error {
	query := `UPDATE mailbox_events SET hint = ''
		WHERE hint <> '' AND NOT EXISTS (SELECT 1 FROM notices n WHERE n.event_seq = mailbox_events.seq)
		  AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = mailbox_events.account_id)`
	var args []any
	if seqs != nil {
		list, err := json.Marshal(seqs)
		if err != nil {
			return fmt.Errorf("store: encode seqs: %w", err)
		}
		query += ` AND seq IN (SELECT value FROM json_each(?))`
		args = append(args, string(list))
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("store: forget the hints no notice needs: %w", err)
	}
	return nil
}

// DeleteMailboxEventsTx deletes, inside the caller's transaction, every
// event of these mailboxes, with who read them and the notices they gave:
// what goes with a person who is closed, and with a link that never
// connected in a local workspace. In a workspace from another source such a
// link's deletion is journaled instead, for an extension to report.
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
		       e.actor, e.created_at, e.cause, e.lost_user, e.purge_after, n.created_at, coalesce(w.name, ''),
		       a.id IS NOT NULL, coalesce(a.email, ''), coalesce(a.removed_at, 0), coalesce(a.purge_after, 0)
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
			&n.Event.Hint, &n.Event.Actor, &n.Event.At, &n.Event.Cause, &n.Event.LostUser, &n.Event.PurgeAfter,
			&n.CreatedAt, &n.WorkspaceName, &n.Kept, &n.Email, &n.RemovedAt, &n.PurgeAfter); err != nil {
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

// LatestNotice is the seq of the newest event a person holds a notice of, 0
// when they hold none: a live stream compares it with the last it saw to
// tell them of a new one. A notice is given only with the event it tells of,
// so a new one is always newer than every other.
func (s *Store) LatestNotice(ctx context.Context, userID string) (int64, error) {
	var seq int64
	if err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(event_seq), 0) FROM notices WHERE user_id = ?`, userID).
		Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: read the newest notice: %w", err)
	}
	return seq, nil
}

// DismissNotice deletes a person's notice of the event eventID ("mbe_…");
// ErrNoNotice when they have none. The last notice of a mailbox deleted for
// good takes the masked hint the journal kept for it.
func (s *Store) DismissNotice(ctx context.Context, userID, eventID string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		var seq int64
		err := tx.QueryRowContext(ctx, `DELETE FROM notices
			WHERE user_id = ? AND event_seq = (SELECT seq FROM mailbox_events WHERE id = ?) RETURNING event_seq`,
			userID, eventID).Scan(&seq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNoNotice
		case err != nil:
			return fmt.Errorf("store: dismiss the notice: %w", err)
		}
		return forgetUnneededHintsTx(ctx, tx, []int64{seq})
	})
}

// SweepMailboxEvents deletes the events written before before (unix
// seconds), with who read the mailboxes then and the notices they gave, and
// reports how many it deleted: the journal's retention. It also forgets the
// hints of mailboxes deleted for good that no notice needs any longer, a
// person deleted with theirs included.
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
		return forgetUnneededHintsTx(ctx, tx, nil)
	})
	return int(n), err
}
