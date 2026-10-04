package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
)

// Changes a person asked for — flags and moves — written into the index from
// what the mail server answered.
//
// The service changes the mailbox on the account's interactive connection
// and records here what the server confirmed: never what it hoped would
// happen. Each call runs inside the caller's transaction, one per folder the
// action touched, begins by checking the account may still be synced (a
// withdrawal that committed leaves nothing to write into), and journals its
// events in that transaction for the caller to publish after the commit.
//
// None of it is a sync pass, and none of it announces new mail: a moved row
// keeps its id and says where it went (ActionMoved). moves.go is what keeps a
// pass running at the same time from undoing it.

// ActionFlags is what a STORE answered for one folder.
type ActionFlags struct {
	AccountID   string
	FolderID    int64
	UIDValidity uint32
	// Updates are the server's echo: the flags each message has now, and
	// with CONDSTORE its new modification sequence.
	Updates []provider.FlagUpdate
	// Shared applies each row's new flags to the account's other live rows
	// with the same Message-ID. On Gmail a flag belongs to the message, not
	// to one label's copy, and the copies would otherwise contradict it
	// until their own folders' next passes.
	Shared bool
	Now    time.Time
}

// ApplyActionFlags writes what a STORE echoed, announcing message.flags for
// every row whose flags changed: the ones the store named and, with Shared,
// their copies in other folders.
func (s *Store) ApplyActionFlags(ctx context.Context, tx *sql.Tx, b ActionFlags) ([]events.Event, error) {
	if err := RequireSyncEligibleTx(ctx, tx, b.AccountID); err != nil {
		return nil, err
	}
	folder, err := indexFolder(ctx, tx, b.AccountID, b.FolderID, b.UIDValidity)
	if err != nil {
		return nil, err
	}
	now := b.Now
	if now.IsZero() {
		now = s.now()
	}
	var (
		evs    []events.Event
		settle = map[string]bool{}
		// marked are the rows this action set flags on, with the sequence
		// the server echoed for each: a pass's answer read before it must
		// not put the old flags back (moves.go).
		marked = map[int64]int64{}
	)
	announce := func(id, folderID int64, flags []string) error {
		ev, err := events.New(events.TypeMessageFlags, b.AccountID, now, flagsPayload(b.AccountID, id, folderID, flags))
		if err != nil {
			return err
		}
		evs = append(evs, ev)
		return nil
	}
	for _, u := range b.Updates {
		r, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`, folder.ID, b.UIDValidity, uint32(u.UID))
		if err != nil {
			return nil, err
		}
		if !found {
			// Not indexed under this UID any more: the passes will say what
			// became of it.
			continue
		}
		m := meta{flags: NormalizeFlags(u.Flags), modseq: clampInt64(u.ModSeq)}
		changed, err := refreshRow(ctx, tx, r, m, now, nil)
		if err != nil {
			return nil, err
		}
		marked[r.id] = m.modseq
		if r.vanishedAt != 0 {
			settle[r.groupKey] = true
		}
		if changed {
			if err := announce(r.id, folder.ID, m.flags); err != nil {
				return nil, err
			}
		}
		if !b.Shared || r.messageID == "" {
			continue
		}
		copies, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE account_id = ? AND message_id = ? AND id <> ? AND vanished_at = 0 AND stale = 0 ORDER BY id`,
			b.AccountID, r.messageID, r.id)
		if err != nil {
			return nil, err
		}
		for _, c := range copies {
			if _, named := marked[c.id]; !named {
				// Marked even when it already agrees: an answer its folder's
				// pass read before the action would still say otherwise.
				marked[c.id] = 0
			}
			if equalStrings(c.flags, m.flags) {
				continue
			}
			// The flags only: the copy's modification sequence is its own
			// folder's, which its next pass reads.
			if _, err := refreshRow(ctx, tx, c, meta{flags: m.flags}, now, nil); err != nil {
				return nil, err
			}
			if err := announce(c.id, c.folderID, m.flags); err != nil {
				return nil, err
			}
		}
	}
	for key := range settle {
		if _, _, err := settleGroup(ctx, tx, b.AccountID, key); err != nil {
			return nil, err
		}
	}
	s.moves.markFlags(b.AccountID, marked, s.now())
	return journal(ctx, s, tx, evs)
}

// ActionMove is what a move answered for rows of one folder.
type ActionMove struct {
	AccountID string
	// FromFolderID and FromUIDValidity are the folder the rows were in and
	// the UIDVALIDITY their UIDs were read under.
	FromFolderID    int64
	FromUIDValidity uint32
	// ToFolderID is where they went; DestUIDValidity is its UIDVALIDITY as
	// the server reported it with the new UIDs, zero when it reported none.
	ToFolderID      int64
	DestUIDValidity uint32
	Moves           []MovedRow
	// Held are the destination's UIDs the index held for these messages
	// before the move was sent (HeldCopies). A move that reports one of them
	// as a row's new UID put the message where it already was — on Gmail, a
	// label it already had — and only took the source's label away; that is
	// remembered (LabelMoveOf), so that undoing it adds the label back
	// instead of taking the destination's away.
	Held map[imap.UID]bool
	// DestMark, when the new UIDs came from the server's COPYUID, is the
	// destination's high-water mark as the index had it before the move was
	// sent. A move gives what it moves UIDs above every UID the folder had,
	// so one it reports at or below the mark is a copy that was there
	// already, whether or not the index holds it: the same as Held.
	DestMark imap.UID
	Now      time.Time
}

// MovedRow is one row and where the server put it.
type MovedRow struct {
	ID int64
	// UID is the row's UID in the folder it left: what the move named.
	UID imap.UID
	// DestUID is its UID in the destination: from a COPYUID of one UID, from
	// one of several once the message there was read and found to be this
	// row's (Identity), or found by its Message-ID. Zero when none of that
	// placed it: the row leaves the index, and the destination's pass indexes
	// the message as one that moved.
	DestUID imap.UID
}

// Identity is what the index knows a message by without its UID: its
// Message-ID (bare, empty when it has none), its INTERNALDATE and its size,
// as the index stores them. It is what pairs each message a COPY or MOVE of
// several named with the UID it landed at, which COPYUID does not say
// reliably (provider.MoveResult.Paired). It does not tell every two messages
// apart: two without a Message-ID, or with one a sender reuses, from the same
// second and of the same size have the same identity and different content.
// So it pairs a message only within a set where no other message has it.
// Hashed: it carries a Message-ID, and a row that left the index keeps it in
// memory (LeftRow).
type Identity [sha256.Size]byte

// IdentityOf is the identity of the message a summary describes, read as
// the index reads a summary. One with neither INTERNALDATE nor Date gets a
// date no row can have, and pairs with nothing.
func IdentityOf(sum provider.Summary) Identity {
	var (
		messageID string
		internal  int64
	)
	if env := sum.Envelope; env != nil {
		messageID = BareID(env.MessageID)
		if !env.Date.IsZero() {
			internal = env.Date.Unix()
		}
	}
	if !sum.InternalDate.IsZero() {
		internal = sum.InternalDate.Unix()
	}
	return identity(messageID, internal, sum.Size)
}

// Identity is the identity of the message the row holds.
func (r MessageRow) Identity() Identity { return identity(r.MessageID, r.InternalDate, r.Size) }

func identity(messageID string, internalDate, size int64) Identity {
	return sha256.Sum256([]byte(messageID + "\x00" + strconv.FormatInt(internalDate, 10) + "\x00" +
		strconv.FormatInt(size, 10)))
}

// ActionMoveResult is what a move did to the index.
type ActionMoveResult struct {
	// Kept are rows that followed their message into the destination,
	// under the same id.
	Kept []int64
	// Removed are rows that left the index: the destination is not synced,
	// or the message's new UID there is not known and the destination's pass
	// will index it.
	Removed []int64
	Events  []events.Event
}

// ApplyActionMove makes the index follow a move the server confirmed.
//
// Into a synced folder whose UIDVALIDITY is the one the server reported, a
// row whose new UID is known is rewritten in place — folder, UIDVALIDITY,
// UID, modification sequence reset — and keeps its id, its parts and its
// place in its group. A row the index already holds at that UID — a pass saw
// the message arrive first, or, on Gmail, the message already carried that
// label and the server reported the copy it had — is the same message, and
// gives way to it. A move into a folder that already had the message (Held,
// DestMark) is remembered as one that only took the source's label away
// (LabelMoveOf). The destination's
// high-water mark moves up over new UIDs that follow it without a gap, so
// the next pass does not fetch them again; a gap is left alone, because it
// may be mail that arrived meanwhile, which must still be fetched as new.
//
// Any other row leaves the index, with the group's primary handed on as for
// any deletion, and is announced as a copy going away. One whose new place
// the server named, in a folder that is not synced, is remembered for
// MoveMemory by its old id, so moving it back can find it.
//
// Every source UID is remembered as moved (moves.go), whatever became of its
// row.
func (s *Store) ApplyActionMove(ctx context.Context, tx *sql.Tx, b ActionMove) (ActionMoveResult, error) {
	if err := RequireSyncEligibleTx(ctx, tx, b.AccountID); err != nil {
		return ActionMoveResult{}, err
	}
	from, err := folderOn(ctx, tx, b.AccountID, b.FromFolderID)
	if err != nil {
		return ActionMoveResult{}, err
	}
	to, err := folderOn(ctx, tx, b.AccountID, b.ToFolderID)
	if err != nil {
		return ActionMoveResult{}, err
	}
	now := b.Now
	if now.IsZero() {
		now = s.now()
	}
	placeable := to.Synced && to.Selectable && to.MissingSince.IsZero() && to.UIDValidity != 0 &&
		b.DestUIDValidity == to.UIDValidity

	var (
		res    ActionMoveResult
		evs    []events.Event
		gone   []row
		landed = map[imap.UID]bool{}
	)
	announce := func(r row, folder Folder, dest *int64, newPrimary int64) error {
		payload := ActionMoved{
			AccountID: b.AccountID, MessageID: r.id, FolderID: folder.ID, FolderRole: string(folder.Role),
			To: dest, ToFolderID: dest, FromFolderID: from.ID, PrimaryID: newPrimary,
		}
		ev, err := events.New(events.TypeMessageMoved, b.AccountID, now, payload)
		if err != nil {
			return err
		}
		evs = append(evs, ev)
		return nil
	}
	for _, mv := range b.Moves {
		s.moves.markGone(from.ID, b.FromUIDValidity, mv.UID, s.now())
		// Whatever an earlier move of the row was remembered for, this one
		// supersedes it.
		s.moves.forgetLeft(mv.ID)
		r, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages WHERE id = ?`, mv.ID)
		if err != nil {
			return ActionMoveResult{}, err
		}
		if !found || r.folderID != from.ID || r.uidvalidity != b.FromUIDValidity || r.uid != mv.UID {
			// The index moved on while the server was asked — a diff
			// deleted the row, a resync claimed it — and the passes will say
			// where the message is.
			continue
		}
		if !placeable || mv.DestUID == 0 {
			gone = append(gone, r)
			if mv.DestUID != 0 {
				s.moves.remember(LeftRow{
					ID: r.id, AccountID: b.AccountID, FromFolderID: from.ID,
					FolderID: to.ID, UIDValidity: b.DestUIDValidity, UID: mv.DestUID, group: hashGroup(r.groupKey),
					identity: identity(r.messageID, r.internalDate, r.size),
				}, s.now())
			}
			continue
		}
		raced, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`, to.ID, to.UIDValidity, uint32(mv.DestUID))
		if err != nil {
			return ActionMoveResult{}, err
		}
		if found {
			// The same message, already indexed there: a pass saw it arrive
			// before the index followed it, or it already had the label. The
			// row that moved takes its place.
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, raced.id); err != nil {
				return ActionMoveResult{}, fmt.Errorf("store: drop a row a pass raced in: %w", err)
			}
			if raced.groupKey != r.groupKey {
				if _, _, err := settleGroup(ctx, tx, b.AccountID, raced.groupKey); err != nil {
					return ActionMoveResult{}, err
				}
			}
			ev, err := events.New(events.TypeMessageMoved, b.AccountID, now, MessageMoved{
				AccountID: b.AccountID, MessageID: raced.id, FolderID: to.ID, FolderRole: string(to.Role),
				PrimaryID: r.id,
			})
			if err != nil {
				return ActionMoveResult{}, err
			}
			evs = append(evs, ev)
		}
		if b.Held[mv.DestUID] || mv.DestUID <= b.DestMark {
			// The message was there before the move was sent — not a pass
			// that raced the index to a UID the move gave it.
			s.moves.rememberLabel(LabelMove{
				ID: r.id, AccountID: b.AccountID, FromFolderID: from.ID, ToFolderID: to.ID,
			}, s.now())
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET folder_id = ?, uidvalidity = ?, uid = ?, modseq = 0,
			vanished_at = 0, stale = 0, updated_at = ? WHERE id = ?`,
			to.ID, to.UIDValidity, uint32(mv.DestUID), now.Unix(), r.id); err != nil {
			return ActionMoveResult{}, fmt.Errorf("store: move a row: %w", err)
		}
		primary, _, err := settleGroup(ctx, tx, b.AccountID, r.groupKey)
		if err != nil {
			return ActionMoveResult{}, err
		}
		dest := to.ID
		if err := announce(r, to, &dest, primary); err != nil {
			return ActionMoveResult{}, err
		}
		res.Kept = append(res.Kept, r.id)
		landed[mv.DestUID] = true
	}

	for _, r := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, r.id); err != nil {
			return ActionMoveResult{}, fmt.Errorf("store: take a moved row out of the index: %w", err)
		}
	}
	for _, r := range gone {
		primary, _, err := settleGroup(ctx, tx, b.AccountID, r.groupKey)
		if err != nil {
			return ActionMoveResult{}, err
		}
		if err := announce(r, from, nil, primary); err != nil {
			return ActionMoveResult{}, err
		}
		res.Removed = append(res.Removed, r.id)
	}

	if err := raiseMark(ctx, tx, to, landed); err != nil {
		return ActionMoveResult{}, err
	}
	if res.Events, err = journal(ctx, s, tx, evs); err != nil {
		return ActionMoveResult{}, err
	}
	return res, nil
}

// raiseMark moves a folder's high-water mark up over new UIDs that follow it
// without a gap. A UID beyond a gap stays above the mark: the gap may be mail
// that arrived meanwhile, and the next pass must fetch that as new; it finds
// the moved row already indexed under its UID and leaves it be.
func raiseMark(ctx context.Context, tx *sql.Tx, f Folder, uids map[imap.UID]bool) error {
	mark := f.MaxSeenUID
	for uids[mark+1] {
		mark++
	}
	if mark == f.MaxSeenUID {
		return nil
	}
	return UpdateFolderSync(ctx, tx, f.ID, FolderSync{MaxSeenUID: &mark})
}

// ActionReturn is a message a person moved back into a synced folder out of
// one that is not (Gmail's All Mail, after an archive), or copied back into
// the folder a move took it out of when the move only took a label away
// (LabelMoveOf), with the summary the server gave for it there.
type ActionReturn struct {
	AccountID string
	// LeftID is the id the row had before it left the index, or, for a copy
	// back, the id of the row that stays where it is.
	LeftID       int64
	FromFolderID int64
	ToFolderID   int64
	// UIDValidity is the destination's, as the summary was read under it.
	UIDValidity uint32
	Summary     provider.Summary
	Now         time.Time
}

// ApplyActionReturn indexes a message moved back from a folder that is not
// synced, or copied back, under a new id — its old row is gone, or stays
// where it is — and announces it as a row that moved in, never as new mail.
// A message the destination already holds under that UID keeps the row it
// has. It returns the row's id.
func (s *Store) ApplyActionReturn(ctx context.Context, tx *sql.Tx, b ActionReturn) (int64, []events.Event, error) {
	if err := RequireSyncEligibleTx(ctx, tx, b.AccountID); err != nil {
		return 0, nil, err
	}
	to, err := indexFolder(ctx, tx, b.AccountID, b.ToFolderID, b.UIDValidity)
	if err != nil {
		return 0, nil, err
	}
	now := b.Now
	if now.IsZero() {
		now = s.now()
	}
	m := normalize(b.Summary, now)
	existing, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
		WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`, to.ID, b.UIDValidity, uint32(m.uid))
	if err != nil {
		return 0, nil, err
	}
	s.moves.forgetLeft(b.LeftID)
	if found {
		return existing.id, nil, nil
	}
	id, err := insertRow(ctx, tx, b.AccountID, to.ID, b.UIDValidity, m, now)
	if err != nil {
		return 0, nil, err
	}
	primary, _, err := settleGroup(ctx, tx, b.AccountID, m.groupKey)
	if err != nil {
		return 0, nil, err
	}
	dest := to.ID
	ev, err := events.New(events.TypeMessageMoved, b.AccountID, now, ActionMoved{
		AccountID: b.AccountID, MessageID: id, FolderID: to.ID, FolderRole: string(to.Role), NewCopy: true,
		To: &dest, ToFolderID: &dest, FromFolderID: b.FromFolderID, PrimaryID: primary,
	})
	if err != nil {
		return 0, nil, err
	}
	if err := raiseMark(ctx, tx, to, map[imap.UID]bool{m.uid: true}); err != nil {
		return 0, nil, err
	}
	evs, err := journal(ctx, s, tx, []events.Event{ev})
	if err != nil {
		return 0, nil, err
	}
	return id, evs, nil
}

// HeldCopies lists the UIDs of a folder's rows under a UIDVALIDITY that
// belong to any of the groups (GroupKey), live or not: where the index
// already holds these messages in the folder, before a move into it is sent
// (ActionMove.Held).
func (s *Store) HeldCopies(ctx context.Context, folderID int64, uidvalidity uint32, groupKeys []string) (map[imap.UID]bool, error) {
	out := map[imap.UID]bool{}
	if len(groupKeys) == 0 {
		return out, nil
	}
	keys, err := json.Marshal(groupKeys)
	if err != nil {
		return nil, fmt.Errorf("store: read held copies: %w", err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT uid FROM messages WHERE folder_id = ? AND uidvalidity = ?
		AND group_key IN (SELECT value FROM json_each(?))`, folderID, uidvalidity, string(keys))
	if err != nil {
		return nil, fmt.Errorf("store: read held copies: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("store: read held copies: %w", err)
		}
		out[imap.UID(uid)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read held copies: %w", err)
	}
	return out, nil
}

// HeldUIDs reports which of uids the index holds in a folder under a
// UIDVALIDITY, live or not: a move that found its message by Message-ID
// among several must not take a row that is already another's.
func (s *Store) HeldUIDs(ctx context.Context, folderID int64, uidvalidity uint32, uids []imap.UID) (map[imap.UID]bool, error) {
	out := map[imap.UID]bool{}
	if len(uids) == 0 {
		return out, nil
	}
	sorted := append([]imap.UID(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	lo, hi := sorted[0], sorted[len(sorted)-1]
	rows, err := s.r.QueryContext(ctx, `SELECT uid FROM messages WHERE folder_id = ? AND uidvalidity = ?
		AND uid BETWEEN ? AND ?`, folderID, uidvalidity, uint32(lo), uint32(hi))
	if err != nil {
		return nil, fmt.Errorf("store: read held uids: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	want := make(map[imap.UID]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}
	for rows.Next() {
		var uid uint32
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("store: read held uids: %w", err)
		}
		if want[imap.UID(uid)] {
			out[imap.UID(uid)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read held uids: %w", err)
	}
	return out, nil
}
