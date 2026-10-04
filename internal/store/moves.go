package store

import (
	"crypto/sha256"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
)

// What the index remembers about actions a person asked for.
//
// An action changes the server on the interactive connection while the sync
// engine keeps reading it on another, so a pass can be halfway through
// looking at the folders involved when the index follows the action. What is
// remembered, for MoveMemory, is what keeps the pass from undoing it:
//
//   - the UID a move took out of its folder: a summary or flag answer for it
//     that a pass fetched before the move and applies after it would put the
//     row back where it was. UIDs are never reused under one UIDVALIDITY, so
//     anything the server says about that UID is from before the move.
//   - the message (its group key): wherever a pass finds it next — the
//     destination, when the server said no new UID, or first when it raced
//     the index — it is a message that moved, never new mail.
//   - a row that left the index for a folder that is not synced (Gmail's All
//     Mail, which archiving moves to), by its old id, where it went and what
//     it is (Identity, hashed), so that moving it back — undoing the archive
//     — can find it, and tell it apart from the others moved back with it.
//   - the rows whose flags an action set, and when in the actions' history
//     (ActionMark): a flag answer a pass read before that, applied after it,
//     would put the old flags back. With CONDSTORE the modification sequence
//     the server echoed tells the answers apart; without it there is none,
//     and on Gmail an action sets the flags of a message's other label copies
//     without their new sequences.
//   - the source UIDs of a move whose outcome is not known — the connection
//     failed while the server was answering — so that if the message did
//     leave, the diffs that find it gone announce a copy leaving, never a
//     deletion. Nothing was deleted.
//   - a move into a folder that already held the message: on Gmail, a label
//     it already had. The move only took the source's label away, so undoing
//     it is adding that label back, never taking the destination's away.
//
// In memory, not in the database: it is only ever about the last few
// minutes, a pass racing a daemon that restarted starts from what the server
// holds, and acting on a mailbox stores nothing beyond what the index already
// holds. Group keys carry a Message-ID, so they are kept hashed.

// MoveMemory is how long an action is remembered: well past a pass that was
// in flight when it happened, and the console's offer to undo it.
const MoveMemory = 10 * time.Minute

type uidKey struct {
	folder      int64
	uidvalidity uint32
	uid         imap.UID
}

type groupRef struct {
	account string
	key     [sha256.Size]byte
}

// LeftRow is a row a move took out of the index, as remembered by its old id.
type LeftRow struct {
	ID        int64
	AccountID string
	// FromFolderID is the folder it left.
	FromFolderID int64
	// FolderID, UIDValidity and UID are where the server put it: a folder
	// that is not synced, and the UID it reported there (zero when it did
	// not).
	FolderID    int64
	UIDValidity uint32
	UID         imap.UID

	group    [sha256.Size]byte
	identity Identity
	expires  time.Time
}

// Identity is the identity of the message the row held, for telling it apart
// from the others a move back lands with.
func (l LeftRow) Identity() Identity { return l.identity }

// LabelMove is a row moved into a folder that already held its message, as
// remembered by its id: on Gmail, the move took the label FromFolderID away
// and the message kept ToFolderID, which it had before.
type LabelMove struct {
	ID           int64
	AccountID    string
	FromFolderID int64
	ToFolderID   int64

	expires time.Time
}

// flagMark is an action's change to a row's flags.
type flagMark struct {
	account string
	// seq is the action's place in the history ActionMark counts.
	seq uint64
	// modseq is the modification sequence the server echoed for the row;
	// zero when it echoed none, or the row is a copy the action reached
	// without one.
	modseq  int64
	expires time.Time
}

type moveMemory struct {
	mu     sync.Mutex
	gone   map[uidKey]time.Time
	maybe  map[uidKey]time.Time
	groups map[groupRef]time.Time
	left   map[int64]LeftRow
	labels map[int64]LabelMove
	flags  map[int64]flagMark
	// actions counts the flag changes recorded, for ActionMark.
	actions uint64
}

func newMoveMemory() *moveMemory {
	return &moveMemory{
		gone: map[uidKey]time.Time{}, maybe: map[uidKey]time.Time{}, groups: map[groupRef]time.Time{},
		left: map[int64]LeftRow{}, labels: map[int64]LabelMove{}, flags: map[int64]flagMark{},
	}
}

func hashGroup(key string) [sha256.Size]byte { return sha256.Sum256([]byte(key)) }

// prune drops what has expired. Called with mu held.
func (mm *moveMemory) prune(now time.Time) {
	for _, uids := range []map[uidKey]time.Time{mm.gone, mm.maybe} {
		for k, until := range uids {
			if !now.Before(until) {
				delete(uids, k)
			}
		}
	}
	for k, until := range mm.groups {
		if !now.Before(until) {
			delete(mm.groups, k)
		}
	}
	for k, l := range mm.left {
		if !now.Before(l.expires) {
			delete(mm.left, k)
		}
	}
	for k, l := range mm.labels {
		if !now.Before(l.expires) {
			delete(mm.labels, k)
		}
	}
	for k, f := range mm.flags {
		if !now.Before(f.expires) {
			delete(mm.flags, k)
		}
	}
}

func (mm *moveMemory) markGone(folder int64, uidvalidity uint32, uid imap.UID, now time.Time) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	mm.gone[uidKey{folder, uidvalidity, uid}] = now.Add(MoveMemory)
}

func (mm *moveMemory) isGone(folder int64, uidvalidity uint32, uid imap.UID, now time.Time) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	until, ok := mm.gone[uidKey{folder, uidvalidity, uid}]
	return ok && now.Before(until)
}

func (mm *moveMemory) markMaybeGone(folder int64, uidvalidity uint32, uid imap.UID, now time.Time) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	mm.maybe[uidKey{folder, uidvalidity, uid}] = now.Add(MoveMemory)
}

func (mm *moveMemory) maybeGone(folder int64, uidvalidity uint32, uid imap.UID, now time.Time) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	until, ok := mm.maybe[uidKey{folder, uidvalidity, uid}]
	return ok && now.Before(until)
}

func (mm *moveMemory) markGroup(account string, key [sha256.Size]byte, now time.Time) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	mm.groups[groupRef{account, key}] = now.Add(MoveMemory)
}

func (mm *moveMemory) groupMoved(account, groupKey string, now time.Time) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	until, ok := mm.groups[groupRef{account, hashGroup(groupKey)}]
	return ok && now.Before(until)
}

func (mm *moveMemory) remember(l LeftRow, now time.Time) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	l.expires = now.Add(MoveMemory)
	mm.left[l.ID] = l
}

func (mm *moveMemory) leftRow(id int64, now time.Time) (LeftRow, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	l, ok := mm.left[id]
	if !ok || !now.Before(l.expires) {
		return LeftRow{}, false
	}
	return l, true
}

// forgetLeft forgets what a row's last move was remembered for: it has come
// back, or moved again.
func (mm *moveMemory) forgetLeft(id int64) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	delete(mm.left, id)
	delete(mm.labels, id)
}

func (mm *moveMemory) rememberLabel(l LabelMove, now time.Time) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	l.expires = now.Add(MoveMemory)
	mm.labels[l.ID] = l
}

func (mm *moveMemory) labelMove(id int64, now time.Time) (LabelMove, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	l, ok := mm.labels[id]
	if !ok || !now.Before(l.expires) {
		return LabelMove{}, false
	}
	return l, true
}

func (mm *moveMemory) mark() uint64 {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	return mm.actions
}

// markFlags records one action's flag changes: each row's id, with the
// modification sequence the server echoed for it (zero for none).
func (mm *moveMemory) markFlags(account string, rows map[int64]int64, now time.Time) {
	if len(rows) == 0 {
		return
	}
	mm.mu.Lock()
	defer mm.mu.Unlock()
	mm.prune(now)
	mm.actions++
	for id, modseq := range rows {
		mm.flags[id] = flagMark{account: account, seq: mm.actions, modseq: modseq, expires: now.Add(MoveMemory)}
	}
}

// flagsNewer reports whether an action set a row's flags after a flag answer
// was read — read is what ActionMark said before it was — so that the answer
// must not replace them. An answer whose modification sequence is past the
// one the action's own change was echoed with was read after it.
func (mm *moveMemory) flagsNewer(id int64, read uint64, answer int64, now time.Time) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	f, ok := mm.flags[id]
	switch {
	case !ok || !now.Before(f.expires) || f.seq <= read:
		return false
	case f.modseq != 0 && answer > f.modseq:
		return false
	}
	return true
}

// forget drops everything remembered about the accounts: their index has
// just been deleted.
func (mm *moveMemory) forget(accountIDs []string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	drop := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		drop[id] = true
	}
	for k := range mm.groups {
		if drop[k.account] {
			delete(mm.groups, k)
		}
	}
	for k, l := range mm.left {
		if drop[l.AccountID] {
			delete(mm.left, k)
		}
	}
	for k, l := range mm.labels {
		if drop[l.AccountID] {
			delete(mm.labels, k)
		}
	}
	for k, f := range mm.flags {
		if drop[f.account] {
			delete(mm.flags, k)
		}
	}
	// UIDs are by folder, and the folders went with the index: nothing a
	// pass could apply to them is left.
}

// ExpectMoves records, before a move is sent to the server, that these
// messages of the account are about to move: a pass that finds one of them
// in its new folder before the index has followed announces it as a message
// that moved, never as new mail.
func (s *Store) ExpectMoves(accountID string, groupKeys []string) {
	now := s.now()
	for _, key := range groupKeys {
		s.moves.markGroup(accountID, hashGroup(key), now)
	}
}

// ExpectReturn is ExpectMoves for a row that left the index.
func (s *Store) ExpectReturn(l LeftRow) {
	s.moves.markGroup(l.AccountID, l.group, s.now())
}

// MayHaveMoved records that a move of these UIDs out of a folder was sent
// and its outcome is not known: the connection failed before the server's
// answer arrived. The index keeps the rows, since the server may not have
// moved anything; if it did, the diffs that find them gone announce each as
// a copy leaving (message.moved with to null), never as message.deleted,
// and the destination's pass, told by ExpectMoves, finds the message there
// as one that moved.
func (s *Store) MayHaveMoved(folderID int64, uidvalidity uint32, uids []imap.UID) {
	now := s.now()
	for _, uid := range uids {
		s.moves.markMaybeGone(folderID, uidvalidity, uid, now)
	}
}

// Returned forgets what the last moves of these rows were remembered for —
// where a row that left the index went (LeftIndex), a move that only took a
// label away (LabelMoveOf) — once they have been moved back and the index
// could not follow them: the server did not say where they landed, or not
// in a way that could be trusted. The destination's pass indexes them, as
// messages that moved, and there is nothing left to undo.
func (s *Store) Returned(ids []int64) {
	for _, id := range ids {
		s.moves.forgetLeft(id)
	}
}

// LeftIndex finds a row a move took out of the index in the last
// MoveMemory, by the id it had.
func (s *Store) LeftIndex(id int64) (LeftRow, bool) {
	return s.moves.leftRow(id, s.now())
}

// LabelMoveOf finds, by its id, a row that moved in the last MoveMemory into
// a folder that already held its message (ApplyActionMove's Held), and is
// still there.
func (s *Store) LabelMoveOf(id int64) (LabelMove, bool) {
	return s.moves.labelMove(id, s.now())
}

// ActionMark is where the history of flag changes actions recorded stands
// now. A pass takes it before it fetches flags or summaries, and hands it to
// ApplyFlags or ApplySummaries with the answer (FlagBatch.ActionMark,
// SummaryBatch.ActionMark): flags an action set after the mark are newer than
// the answer, which leaves them alone.
func (s *Store) ActionMark() uint64 {
	return s.moves.mark()
}
