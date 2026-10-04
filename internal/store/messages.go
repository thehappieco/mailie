package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
)

// The message index: metadata only. Phase 2 stores the envelope, INTERNALDATE,
// size, flags, modseq and the BODYSTRUCTURE's parts — never a body, never an
// attachment's bytes. body_text and snippet stay empty; the full-text index
// covers subject, sender and recipients.
//
// Every write here runs inside a transaction the caller owns, begins by
// checking that the account may still be synced (RequireSyncEligibleTx), and
// journals the events it decides on in that same transaction. The caller
// publishes them once the transaction has committed.

// ErrUIDValidityMismatch is a batch computed under a UIDVALIDITY the folder no
// longer has: the folder was resynced (or never recorded one) in between, and
// applying it would attach rows to UIDs that now mean other messages.
var ErrUIDValidityMismatch = errors.New("store: the batch's UIDVALIDITY is not the folder's")

// ApplyMode decides what inserted rows announce.
type ApplyMode int

const (
	// ApplyLive is an incremental pass. An inserted row is message.new or a
	// copy appearing (message.moved{new_copy}), decided by folder class.
	ApplyLive ApplyMode = iota
	// ApplyQuiet announces nothing per message: the initial backfill, and
	// gaps the expunge diff finds below the newest UID already seen.
	ApplyQuiet
	// ApplyResync is a UIDVALIDITY resync in place: rows marked stale by
	// BeginResync are claimed by their new UIDs silently; an unclaimed row is
	// inserted, and announced only if it is newer than the folder's last
	// completed sync.
	ApplyResync
)

// SummaryBatch is one FETCH worth of summaries for one folder.
type SummaryBatch struct {
	AccountID   string
	FolderID    int64
	UIDValidity uint32
	Mode        ApplyMode
	Summaries   []provider.Summary
	// Reclaim maps a summary's UID to the row, in this folder, whose UID the
	// server reassigned — Exchange does this — as the caller confirmed: the
	// old UID no longer exists on the server (see ReclaimCandidates). The row
	// keeps its id, and nothing is announced.
	Reclaim map[imap.UID]int64
	// BackfillCursor, when set, marks this as an initial-sync batch: it is
	// written as the folder's resume point in the same transaction, and the
	// rows inserted count towards the folder's initial progress.
	BackfillCursor *imap.UID
	// ClaimOnly are UIDs a resync fetched only to find rows the index already
	// held: outside the folder's initial window, they may claim a stale row
	// but are never inserted. A resync must not widen what is kept.
	ClaimOnly map[imap.UID]bool
	// ActionMark is what Store.ActionMark said before the summaries were
	// fetched. A row whose flags an action set after it keeps them: the
	// summary was read before that change. Zero is an answer of unknown age,
	// older than any action.
	ActionMark uint64
	Now        time.Time
}

// ApplyResult is what a batch did.
type ApplyResult struct {
	Inserted  int
	Updated   int // already indexed under this UID: flags and modseq refreshed
	Reclaimed int // a row followed its message to a reassigned UID
	Claimed   int // a stale row found its message's new UID during a resync
	// Skipped are ClaimOnly summaries that claimed nothing: not stored.
	Skipped int
	// Moved are summaries of UIDs a person moved out of this folder since
	// the pass fetched them (moves.go): not stored, or the row would be back
	// where it was.
	Moved int
	// Older are summaries of rows whose flags an action set after they were
	// read (SummaryBatch.ActionMark): left as the action recorded them.
	Older int
	// MaxUID is the highest UID in the batch; the folder's max_seen_uid has
	// been raised to at least this.
	MaxUID imap.UID
	// Events were journaled in the transaction; publish them after it commits.
	Events []events.Event
}

// GroupKey is a message's logical identity within an account: its Message-ID
// when it has a usable one, and otherwise a hash of what a person would
// recognise it by. Gmail shows one message once per label, and every copy
// shares this.
func GroupKey(messageID, subject, fromAddr string, internalDate int64) string {
	if strings.Contains(messageID, "@") {
		return "mid:" + messageID
	}
	sum := sha256.Sum256([]byte(subject + "|" + fromAddr + "|" + strconv.FormatInt(internalDate, 10)))
	return "h:" + hex.EncodeToString(sum[:])
}

// BareID strips the angle brackets and whitespace from an RFC 5322 id. Ids are
// stored bare; the brackets are added in exactly one place, when a message is
// built.
func BareID(id string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(id), "<>"))
}

// meta is a summary flattened into the columns it is stored as.
type meta struct {
	uid          imap.UID
	modseq       int64
	messageID    string
	groupKey     string
	inReplyTo    string
	references   string
	subject      string
	fromJSON     string
	toJSON       string
	ccJSON       string
	bccJSON      string
	replyToJSON  string
	from         *provider.Address
	fromAddr     string
	fromText     string
	toText       string
	date         int64
	internalDate int64
	size         int64
	flags        []string
	hasAttach    bool
	parts        []provider.PartInfo
}

func (m meta) flagsJSON() string { return mustJSON(m.flags) }

func normalize(sum provider.Summary, now time.Time) meta {
	m := meta{
		uid:    sum.UID,
		modseq: clampInt64(sum.ModSeq),
		size:   sum.Size,
		flags:  NormalizeFlags(sum.Flags),
		parts:  sum.Parts,
	}
	var from, to, cc, bcc, replyTo []provider.Address
	if env := sum.Envelope; env != nil {
		m.messageID = BareID(env.MessageID)
		ids := make([]string, 0, len(env.InReplyTo))
		for _, id := range env.InReplyTo {
			if id = BareID(id); id != "" {
				ids = append(ids, id)
			}
		}
		m.inReplyTo = strings.Join(ids, " ")
		m.subject = env.Subject
		from, to, cc = addresses(env.From), addresses(env.To), addresses(env.Cc)
		bcc, replyTo = addresses(env.Bcc), addresses(env.ReplyTo)
		if !env.Date.IsZero() {
			m.date = env.Date.Unix()
		}
	}
	refs := make([]string, 0, len(sum.References))
	for _, id := range sum.References {
		if id = BareID(id); id != "" {
			refs = append(refs, id)
		}
	}
	m.references = mustJSON(refs)
	m.fromJSON, m.toJSON, m.ccJSON = mustJSON(from), mustJSON(to), mustJSON(cc)
	m.bccJSON, m.replyToJSON = mustJSON(bcc), mustJSON(replyTo)
	if len(from) > 0 {
		first := from[0]
		m.from = &first
		m.fromAddr = strings.ToLower(first.Email)
	}
	m.fromText = addressText(from)
	m.toText = addressText(append(append(append([]provider.Address{}, to...), cc...), bcc...))

	switch {
	case !sum.InternalDate.IsZero():
		m.internalDate = sum.InternalDate.Unix()
	case m.date != 0:
		m.internalDate = m.date
	default:
		m.internalDate = now.Unix()
	}
	for _, p := range sum.Parts {
		if p.IsAttachment {
			m.hasAttach = true
			break
		}
	}
	m.groupKey = GroupKey(m.messageID, m.subject, m.fromAddr, m.internalDate)
	return m
}

func addresses(in []imap.Address) []provider.Address {
	out := make([]provider.Address, 0, len(in))
	for _, a := range in {
		if a.IsGroupStart() || a.IsGroupEnd() {
			continue
		}
		email := a.Addr()
		if email == "" && a.Name == "" {
			continue
		}
		out = append(out, provider.Address{Name: a.Name, Email: email})
	}
	return out
}

// addressText is what the full-text index sees for a list of addresses: names
// and addresses both, so a search finds a person by either.
func addressText(list []provider.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, strings.TrimSpace(a.Name+" "+a.Email))
	}
	return strings.Join(parts, ", ")
}

// recentFlag is \Recent, which is not a property of the message but of the
// session that happened to see it first: the server hands it to one
// connection and the next sees it cleared. Stored, it would flip on every
// reading from another connection and announce a flag change nobody made.
const recentFlag = `\recent`

// NormalizeFlags lowercases, deduplicates and sorts flags, so the same set
// compares equal whichever command read it and whichever server sent it, and
// drops \Recent, which belongs to a session rather than to the message.
func NormalizeFlags(flags []imap.Flag) []string {
	out := make([]string, 0, len(flags))
	seen := map[string]bool{}
	for _, f := range flags {
		v := strings.ToLower(strings.TrimSpace(string(f)))
		if v == "" || v == recentFlag || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

type flagBits struct{ seen, flagged, answered, draft, deleted int }

func bitsOf(flags []string) flagBits {
	var b flagBits
	for _, f := range flags {
		switch f {
		case `\seen`:
			b.seen = 1
		case `\flagged`:
			b.flagged = 1
		case `\answered`:
			b.answered = 1
		case `\draft`:
			b.draft = 1
		case `\deleted`:
			b.deleted = 1
		}
	}
	return b
}

// row is the part of a stored message the engine reasons about.
type row struct {
	id           int64
	folderID     int64
	uidvalidity  uint32
	uid          imap.UID
	modseq       int64
	messageID    string
	groupKey     string
	flags        []string
	vanishedAt   int64
	stale        bool
	internalDate int64
	size         int64
}

const rowColumns = `id, folder_id, uidvalidity, uid, modseq, message_id, group_key, flags_json, vanished_at, stale,
	internal_date, size`

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var (
		r     row
		flags string
		stale int
	)
	if err := sc.Scan(&r.id, &r.folderID, &r.uidvalidity, &r.uid, &r.modseq, &r.messageID, &r.groupKey,
		&flags, &r.vanishedAt, &stale, &r.internalDate, &r.size); err != nil {
		return row{}, err
	}
	r.stale = stale != 0
	if err := json.Unmarshal([]byte(flags), &r.flags); err != nil {
		r.flags = nil
	}
	if r.flags == nil {
		r.flags = []string{}
	}
	return r, nil
}

func loadRows(ctx context.Context, q querier, query string, args ...any) ([]row, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rs.Close() }()
	var out []row
	for rs.Next() {
		r, err := scanRow(rs)
		if err != nil {
			return nil, fmt.Errorf("store: read messages: %w", err)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("store: read messages: %w", err)
	}
	return out, nil
}

func loadRow(ctx context.Context, q querier, query string, args ...any) (row, bool, error) {
	r, err := scanRow(q.QueryRowContext(ctx, query, args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return row{}, false, nil
	case err != nil:
		return row{}, false, fmt.Errorf("store: read message: %w", err)
	}
	return r, true, nil
}

// indexFolder reads the folder a batch writes to and checks it belongs to the
// account and carries the batch's UIDVALIDITY.
func indexFolder(ctx context.Context, tx *sql.Tx, accountID string, folderID int64, uidvalidity uint32) (Folder, error) {
	f, err := folderOn(ctx, tx, accountID, folderID)
	if err != nil {
		return Folder{}, err
	}
	if f.UIDValidity != uidvalidity {
		return Folder{}, fmt.Errorf("%w: folder %d has %d, batch has %d", ErrUIDValidityMismatch, folderID, f.UIDValidity, uidvalidity)
	}
	return f, nil
}

// ApplySummaries writes one batch of summaries into the index, inside the
// caller's transaction, and decides — in that transaction — what each one
// announces.
//
// Per summary, in order:
//
//  1. Already indexed under this UID: flags and modseq are refreshed, and a
//     tombstone is lifted (the server says the message is there).
//  2. A confirmed reclaim (Reclaim): the row whose UID the server reassigned
//     moves to the new UID, keeping its id. Nothing is announced.
//  3. During a resync: a stale row with the same Message-ID (or, without one,
//     the same group key), INTERNALDATE and size is claimed by the new UID.
//     Nothing is announced.
//  4. Otherwise the row is inserted, with its parts. On a live pass:
//     - into an inbox: message.new if no inbox held the message yet — a Gmail
//     label synced first must not hide the arrival — else a copy appearing;
//     - into a folder with no role (a Gmail label, a person's folder):
//     message.new if no folder held it yet, else a copy appearing;
//     - anywhere else (sent, drafts, trash, junk, archive): never new, only
//     a copy appearing.
//     The first live row of a group is its primary; the others point at it.
//
// A flag change on an existing row is message.flags. ApplyQuiet announces
// nothing per message. The folder's max_seen_uid is raised to the batch's
// highest UID, never lowered: deleting the newest message must not make the
// next pass fetch old mail as new.
func (s *Store) ApplySummaries(ctx context.Context, tx *sql.Tx, b SummaryBatch) (ApplyResult, error) {
	if err := RequireSyncEligibleTx(ctx, tx, b.AccountID); err != nil {
		return ApplyResult{}, err
	}
	folder, err := indexFolder(ctx, tx, b.AccountID, b.FolderID, b.UIDValidity)
	if err != nil {
		return ApplyResult{}, err
	}
	now := b.Now
	if now.IsZero() {
		now = s.now()
	}
	var (
		res     ApplyResult
		evs     []events.Event
		claimed = map[int64]bool{}
		settle  = map[string]bool{}
	)
	announce := func(t events.Type, payload any) error {
		ev, err := events.New(t, b.AccountID, now, payload)
		if err != nil {
			return err
		}
		evs = append(evs, ev)
		return nil
	}

	for _, sum := range b.Summaries {
		if sum.UID == 0 {
			continue
		}
		if s.moves.isGone(folder.ID, b.UIDValidity, sum.UID, s.now()) {
			res.Moved++
			continue
		}
		m := normalize(sum, now)
		if m.uid > res.MaxUID {
			res.MaxUID = m.uid
		}

		// 1. Already indexed under this UID.
		existing, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`, folder.ID, b.UIDValidity, uint32(m.uid))
		if err != nil {
			return ApplyResult{}, err
		}
		if found && staleModSeq(m.modseq, existing.modseq) {
			// Read before a change the index already has — a person's STORE
			// echoed a newer modification sequence meanwhile.
			res.Updated++
			continue
		}
		if found && s.moves.flagsNewer(existing.id, b.ActionMark, m.modseq, s.now()) {
			// Read before a person's action set the row's flags, where no
			// modification sequence can say so: without CONDSTORE, or a Gmail
			// label copy the action reached without its new one.
			res.Older++
			continue
		}
		if found {
			changed, err := refreshRow(ctx, tx, existing, m, now, nil)
			if err != nil {
				return ApplyResult{}, err
			}
			if changed && b.Mode != ApplyQuiet {
				if err := announce(events.TypeMessageFlags, flagsPayload(b.AccountID, existing.id, folder.ID, m.flags)); err != nil {
					return ApplyResult{}, err
				}
			}
			if existing.vanishedAt != 0 {
				settle[existing.groupKey] = true
			}
			res.Updated++
			continue
		}

		// 2. A UID the server reassigned, confirmed by the caller.
		if rowID, ok := b.Reclaim[m.uid]; ok && !claimed[rowID] && m.messageID != "" {
			old, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
				WHERE id = ? AND folder_id = ? AND uidvalidity = ? AND message_id = ? AND stale = 0`,
				rowID, folder.ID, b.UIDValidity, m.messageID)
			if err != nil {
				return ApplyResult{}, err
			}
			if found {
				claimed[rowID] = true
				uid := m.uid
				changed, err := refreshRow(ctx, tx, old, m, now, &rowMove{uidvalidity: b.UIDValidity, uid: uid})
				if err != nil {
					return ApplyResult{}, err
				}
				if changed && b.Mode != ApplyQuiet {
					if err := announce(events.TypeMessageFlags, flagsPayload(b.AccountID, old.id, folder.ID, m.flags)); err != nil {
						return ApplyResult{}, err
					}
				}
				settle[old.groupKey] = true
				res.Reclaimed++
				continue
			}
		}

		// 3. A stale row from before a UIDVALIDITY change.
		if b.Mode == ApplyResync {
			var (
				stale row
				found bool
			)
			if m.messageID != "" {
				stale, found, err = loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
					WHERE folder_id = ? AND stale = 1 AND message_id = ? AND internal_date = ? AND size = ?
					ORDER BY id LIMIT 1`, folder.ID, m.messageID, m.internalDate, m.size)
			} else {
				stale, found, err = loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
					WHERE folder_id = ? AND stale = 1 AND group_key = ? AND internal_date = ? AND size = ?
					ORDER BY id LIMIT 1`, folder.ID, m.groupKey, m.internalDate, m.size)
			}
			if err != nil {
				return ApplyResult{}, err
			}
			if found {
				changed, err := refreshRow(ctx, tx, stale, m, now, &rowMove{uidvalidity: b.UIDValidity, uid: m.uid})
				if err != nil {
					return ApplyResult{}, err
				}
				if changed {
					if err := announce(events.TypeMessageFlags, flagsPayload(b.AccountID, stale.id, folder.ID, m.flags)); err != nil {
						return ApplyResult{}, err
					}
				}
				settle[stale.groupKey] = true
				res.Claimed++
				continue
			}
		}

		// 4. A row the index has never held — unless it is outside the
		// window, and only here to claim one.
		if b.ClaimOnly[m.uid] {
			res.Skipped++
			continue
		}
		groupLive, groupVanished, inboxLive, err := groupCopies(ctx, tx, b.AccountID, m.groupKey)
		if err != nil {
			return ApplyResult{}, err
		}
		id, err := insertRow(ctx, tx, b.AccountID, folder.ID, b.UIDValidity, m, now)
		if err != nil {
			return ApplyResult{}, err
		}
		res.Inserted++
		primary, _, err := settleGroup(ctx, tx, b.AccountID, m.groupKey)
		if err != nil {
			return ApplyResult{}, err
		}
		delete(settle, m.groupKey)
		if folder.Role == provider.RoleSent && m.messageID != "" {
			// The copy of a message whose submission ended unknown: the
			// provider filed it, so the server took it.
			sent, err := reconcileSendsTx(ctx, tx, b.AccountID, m.messageID, now)
			if err != nil {
				return ApplyResult{}, err
			}
			evs = append(evs, sent...)
		}

		switch {
		case b.Mode == ApplyQuiet:
			continue
		case b.Mode == ApplyResync && m.internalDate <= unixOrZero(folder.LastSyncedAt):
			// Older than what the folder had already seen: a message the
			// resync could not match to its old row, not new mail.
			continue
		}
		isNew, firstInbox := false, false
		switch folder.Role {
		case provider.RoleInbox:
			isNew, firstInbox = inboxLive == 0, inboxLive == 0
		case provider.RoleNone:
			// Mail a filter filed under a label or a person's folder — or old
			// mail moved or labelled there, which keeps its INTERNALDATE (RFC
			// 3501, RFC 6851). A copy the diff has only tombstoned is still a
			// copy: the message moved here from it.
			isNew = groupLive == 0 && groupVanished == 0 && arrivedSince(m.internalDate, folder.LastSyncedAt)
		}
		if isNew && s.moves.groupMoved(b.AccountID, m.groupKey, s.now()) {
			// A message a person just moved, found in its new folder before
			// the index followed it — or without a new UID to follow it by.
			isNew, firstInbox = false, false
		}
		if isNew {
			err = announce(events.TypeMessageNew, MessageNew{
				AccountID: b.AccountID, MessageID: id, FolderID: folder.ID, FolderRole: string(folder.Role),
				Subject: m.subject, From: m.from, InternalDate: m.internalDate,
				FirstCopy: groupLive == 0, FirstInboxCopy: firstInbox,
			})
		} else {
			to := folder.ID
			err = announce(events.TypeMessageMoved, MessageMoved{
				AccountID: b.AccountID, MessageID: id, FolderID: folder.ID, FolderRole: string(folder.Role),
				NewCopy: true, To: &to, PrimaryID: primary,
			})
		}
		if err != nil {
			return ApplyResult{}, err
		}
	}

	for key := range settle {
		if _, _, err := settleGroup(ctx, tx, b.AccountID, key); err != nil {
			return ApplyResult{}, err
		}
	}

	update := FolderSync{}
	if res.MaxUID != 0 {
		update.MaxSeenUID = &res.MaxUID
	}
	if b.BackfillCursor != nil {
		update.BackfillCursor = b.BackfillCursor
		fetched := folder.InitialFetched + res.Inserted
		update.InitialFetched = &fetched
	}
	if err := UpdateFolderSync(ctx, tx, folder.ID, update); err != nil {
		return ApplyResult{}, err
	}
	if res.Events, err = journal(ctx, s, tx, evs); err != nil {
		return ApplyResult{}, err
	}
	return res, nil
}

// rowMove is a row changing physical identity: a reclaimed UID, or a stale
// row claimed by a resync.
type rowMove struct {
	uidvalidity uint32
	uid         imap.UID
}

// refreshRow updates an existing row from a fresh summary or flag fetch and
// reports whether its flags changed. It always lifts a tombstone and a stale
// mark: the server has just shown the message exists.
func refreshRow(ctx context.Context, tx *sql.Tx, r row, m meta, now time.Time, move *rowMove) (bool, error) {
	changed := !equalStrings(r.flags, m.flags)
	bits := bitsOf(m.flags)
	uidvalidity, uid := r.uidvalidity, r.uid
	if move != nil {
		uidvalidity, uid = move.uidvalidity, move.uid
	}
	modseq := m.modseq
	if modseq == 0 {
		modseq = r.modseq
	}
	_, err := tx.ExecContext(ctx, `UPDATE messages SET uidvalidity = ?, uid = ?, modseq = ?, flags_json = ?,
		seen = ?, flagged = ?, answered = ?, draft = ?, deleted = ?, vanished_at = 0, stale = 0, updated_at = ?
		WHERE id = ?`, uidvalidity, uint32(uid), modseq, m.flagsJSON(),
		bits.seen, bits.flagged, bits.answered, bits.draft, bits.deleted, now.Unix(), r.id)
	if err != nil {
		return false, fmt.Errorf("store: update message: %w", err)
	}
	return changed, nil
}

func insertRow(ctx context.Context, tx *sql.Tx, accountID string, folderID int64, uidvalidity uint32, m meta, now time.Time) (int64, error) {
	bits := bitsOf(m.flags)
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO messages(account_id, folder_id, uidvalidity, uid, modseq,
		message_id, group_key, in_reply_to, references_json, subject, from_json, to_json, cc_json, bcc_json,
		reply_to_json, from_addr, from_text, to_text, date, internal_date, size, flags_json,
		seen, flagged, answered, draft, deleted, has_attachments, first_seen_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		accountID, folderID, uidvalidity, uint32(m.uid), m.modseq,
		m.messageID, m.groupKey, m.inReplyTo, m.references, m.subject, m.fromJSON, m.toJSON, m.ccJSON, m.bccJSON,
		m.replyToJSON, m.fromAddr, m.fromText, m.toText, m.date, m.internalDate, m.size, m.flagsJSON(),
		bits.seen, bits.flagged, bits.answered, bits.draft, bits.deleted, boolInt(m.hasAttach), now.Unix(), now.Unix(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert message: %w", err)
	}
	for _, p := range m.parts {
		path := p.PathString()
		if path == "" {
			continue
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO parts(msg_id, path, mime_type, charset, encoding,
			disposition, filename, content_id, size, is_body, is_attachment) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, path, strings.ToLower(p.MIMEType), paramValue(p.Params, "charset"), strings.ToLower(p.Encoding),
			strings.ToLower(p.Disposition), p.Filename, BareID(p.ContentID), p.Size,
			boolInt(p.IsBody), boolInt(p.IsAttachment))
		if err != nil {
			return 0, fmt.Errorf("store: insert part: %w", err)
		}
	}
	return id, nil
}

func paramValue(params map[string]string, key string) string {
	for k, v := range params {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// groupCopies counts the rows of a group in the account: those live, those
// tombstoned (gone from their folder at the last diff, not yet deleted), and
// the live ones in an inbox.
func groupCopies(ctx context.Context, tx *sql.Tx, accountID, groupKey string) (live, vanished, inbox int, err error) {
	err = tx.QueryRowContext(ctx, `SELECT coalesce(sum(m.vanished_at = 0), 0), coalesce(sum(m.vanished_at <> 0), 0),
			coalesce(sum(m.vanished_at = 0 AND f.role = 'inbox'), 0)
		FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE m.account_id = ? AND m.group_key = ?`, accountID, groupKey).Scan(&live, &vanished, &inbox)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("store: count a message's copies: %w", err)
	}
	return live, vanished, inbox, nil
}

// arrivalSkew is how far a server's clock may lag this one's before mail it
// received after a pass looks, by its INTERNALDATE, as if it came before.
const arrivalSkew = 2 * time.Minute

// arrivedSince reports whether a message's INTERNALDATE says it reached the
// server after the folder's last pass started — within arrivalSkew — or the
// folder has never finished one. A message that did not was moved or copied
// in: MOVE and COPY keep the INTERNALDATE, and so does a Gmail label added
// to old mail.
func arrivedSince(internalDate int64, lastPass time.Time) bool {
	return lastPass.IsZero() || internalDate > lastPass.Add(-arrivalSkew).Unix()
}

// settleGroup makes one row the group's primary and points the others at it,
// and returns the primary and how many live rows the group has.
//
// The primary is the oldest live row that no resync is waiting on; only when
// there is none does a stale row stand in, and only when nothing is live a
// tombstone. Listings show primaries that are live and not stale, so a group
// with such a copy must never be represented by one that has vanished — that
// would hide the message until the tombstone is swept — or by one waiting for
// its folder's resync, which would hide it for as long as the resync takes.
func settleGroup(ctx context.Context, tx *sql.Tx, accountID, groupKey string) (primary int64, live int, err error) {
	members, err := groupMembers(ctx, tx, accountID, groupKey)
	if err != nil {
		return 0, 0, err
	}
	if len(members) == 0 {
		return 0, 0, nil
	}
	primary = members[0].id
	for i, m := range members {
		if m.vanished == 0 {
			live++
		}
		var err error
		switch {
		case i == 0 && m.dupOf.Valid:
			_, err = tx.ExecContext(ctx, `UPDATE messages SET dup_of = NULL WHERE id = ?`, m.id)
		case i > 0 && (!m.dupOf.Valid || m.dupOf.Int64 != primary):
			_, err = tx.ExecContext(ctx, `UPDATE messages SET dup_of = ? WHERE id = ?`, primary, m.id)
		}
		if err != nil {
			return 0, 0, fmt.Errorf("store: settle a message group: %w", err)
		}
	}
	return primary, live, nil
}

type groupMember struct {
	id       int64
	vanished int64
	dupOf    sql.NullInt64
}

// groupMembers lists a group's rows: live ones first, and of those the ones
// no resync is waiting on; oldest first within each.
func groupMembers(ctx context.Context, tx *sql.Tx, accountID, groupKey string) ([]groupMember, error) {
	rs, err := tx.QueryContext(ctx, `SELECT id, vanished_at, dup_of FROM messages
		WHERE account_id = ? AND group_key = ? ORDER BY vanished_at <> 0, stale <> 0, id`, accountID, groupKey)
	if err != nil {
		return nil, fmt.Errorf("store: read a message group: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rs.Close() }()
	var members []groupMember
	for rs.Next() {
		var m groupMember
		if err := rs.Scan(&m.id, &m.vanished, &m.dupOf); err != nil {
			return nil, fmt.Errorf("store: read a message group: %w", err)
		}
		members = append(members, m)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("store: read a message group: %w", err)
	}
	return members, nil
}

// deleteRowsTx deletes rows and announces each: message.deleted when no live
// copy of the message is left in the account, a copy removed
// (message.moved{to: null}) when one is — or when the row's UID is one a
// person's move may have taken away (MayHaveMoved): the message left for
// another folder, it was not deleted. Groups are settled after, so the
// surviving copy becomes the primary.
func (s *Store) deleteRowsTx(ctx context.Context, tx *sql.Tx, accountID string, rows []row, folders map[int64]Folder, now time.Time) ([]events.Event, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, r.id); err != nil {
			return nil, fmt.Errorf("store: delete message: %w", err)
		}
	}
	type outcome struct{ primary, live int64 }
	groups := map[string]outcome{}
	for _, r := range rows {
		if _, done := groups[r.groupKey]; done {
			continue
		}
		primary, live, err := settleGroup(ctx, tx, accountID, r.groupKey)
		if err != nil {
			return nil, err
		}
		groups[r.groupKey] = outcome{primary: primary, live: int64(live)}
	}
	evs := make([]events.Event, 0, len(rows))
	for _, r := range rows {
		f, ok := folders[r.folderID]
		if !ok {
			var err error
			if f, err = folderOn(ctx, tx, accountID, r.folderID); err != nil {
				return nil, err
			}
			folders[r.folderID] = f
		}
		g := groups[r.groupKey]
		var (
			ev  events.Event
			err error
		)
		if g.live > 0 || s.moves.maybeGone(r.folderID, r.uidvalidity, r.uid, s.now()) {
			ev, err = events.New(events.TypeMessageMoved, accountID, now, MessageMoved{
				AccountID: accountID, MessageID: r.id, FolderID: r.folderID, FolderRole: string(f.Role),
				NewCopy: false, To: nil, PrimaryID: g.primary,
			})
		} else {
			ev, err = events.New(events.TypeMessageDeleted, accountID, now, MessageDeleted{
				AccountID: accountID, MessageID: r.id, FolderID: r.folderID, FolderRole: string(f.Role),
			})
		}
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// ReclaimCandidate is an indexed row that may be a summary's message under an
// older UID: same folder, same UIDVALIDITY, same Message-ID, INTERNALDATE and
// size, another UID.
type ReclaimCandidate struct {
	UID    imap.UID // the summary's UID
	RowID  int64
	OldUID imap.UID
	// Vanished rows were already found missing by an expunge diff, so the
	// old UID is known to be gone; the others need confirming.
	Vanished bool
}

// ReclaimCandidates finds, for summaries the index does not hold under their
// UID, rows that may be the same message under the UID it had before the
// server reassigned it (Exchange does this after moving a mailbox).
//
// A candidate is not proof: a genuine duplicate looks the same. The caller
// confirms by fetching the old UID — an empty answer means it is gone and the
// row can follow its message — and passes the confirmed ones to
// ApplySummaries as Reclaim. Messages without a Message-ID are never
// candidates.
func (s *Store) ReclaimCandidates(ctx context.Context, folderID int64, uidvalidity uint32, sums []provider.Summary) ([]ReclaimCandidate, error) {
	var out []ReclaimCandidate
	for _, sum := range sums {
		if sum.UID == 0 {
			continue
		}
		m := normalize(sum, s.now())
		if m.messageID == "" {
			continue
		}
		var one int
		err := s.r.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`,
			folderID, uidvalidity, uint32(m.uid)).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: find reclaim candidates: %w", err)
		}
		rows, err := loadRows(ctx, s.r, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND message_id = ? AND internal_date = ? AND size = ?
			  AND uid <> ? AND stale = 0 ORDER BY id`,
			folderID, uidvalidity, m.messageID, m.internalDate, m.size, uint32(m.uid))
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out = append(out, ReclaimCandidate{UID: m.uid, RowID: r.id, OldUID: r.uid, Vanished: r.vanishedAt != 0})
		}
	}
	return out, nil
}

// FlagBatch is a FETCH (UID FLAGS [MODSEQ]) answer for one folder.
type FlagBatch struct {
	AccountID   string
	FolderID    int64
	UIDValidity uint32
	Updates     []provider.FlagUpdate
	// HighestModSeq, when non-zero, is recorded as the folder's CONDSTORE
	// position in the same transaction. Pass it only once the fetch it came
	// with has completed without error.
	HighestModSeq uint64
	// ActionMark is what Store.ActionMark said before the flags were
	// fetched, as for SummaryBatch: an answer read before an action set a
	// row's flags leaves them alone. Zero is older than any action.
	ActionMark uint64
	Now        time.Time
}

// FlagResult is what a flag batch did.
type FlagResult struct {
	Changed int
	// Unknown are UIDs the server reported that the index does not hold:
	// new messages, for the caller to fetch as summaries.
	Unknown []imap.UID
	Events  []events.Event
}

// ApplyFlags writes flag updates, announcing each row whose flags changed.
// A UID the server reports is a UID that exists, so a tombstone on it is
// lifted.
func (s *Store) ApplyFlags(ctx context.Context, tx *sql.Tx, b FlagBatch) (FlagResult, error) {
	if err := RequireSyncEligibleTx(ctx, tx, b.AccountID); err != nil {
		return FlagResult{}, err
	}
	folder, err := indexFolder(ctx, tx, b.AccountID, b.FolderID, b.UIDValidity)
	if err != nil {
		return FlagResult{}, err
	}
	now := b.Now
	if now.IsZero() {
		now = s.now()
	}
	var (
		res    FlagResult
		evs    []events.Event
		settle = map[string]bool{}
	)
	for _, u := range b.Updates {
		if u.UID == 0 || s.moves.isGone(folder.ID, b.UIDValidity, u.UID, s.now()) {
			// A UID a person moved away since this was read: known, and gone.
			continue
		}
		r, found, err := loadRow(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND uid = ?`, folder.ID, b.UIDValidity, uint32(u.UID))
		if err != nil {
			return FlagResult{}, err
		}
		if !found {
			res.Unknown = append(res.Unknown, u.UID)
			continue
		}
		m := meta{flags: NormalizeFlags(u.Flags), modseq: clampInt64(u.ModSeq)}
		if staleModSeq(m.modseq, r.modseq) || s.moves.flagsNewer(r.id, b.ActionMark, m.modseq, s.now()) {
			// Read before a person's action set these flags.
			continue
		}
		changed := !equalStrings(r.flags, m.flags)
		if !changed && r.vanishedAt == 0 && (m.modseq == 0 || m.modseq == r.modseq) {
			continue
		}
		if _, err := refreshRow(ctx, tx, r, m, now, nil); err != nil {
			return FlagResult{}, err
		}
		if r.vanishedAt != 0 {
			settle[r.groupKey] = true
		}
		if changed {
			res.Changed++
			ev, err := events.New(events.TypeMessageFlags, b.AccountID, now, flagsPayload(b.AccountID, r.id, folder.ID, m.flags))
			if err != nil {
				return FlagResult{}, err
			}
			evs = append(evs, ev)
		}
	}
	for key := range settle {
		if _, _, err := settleGroup(ctx, tx, b.AccountID, key); err != nil {
			return FlagResult{}, err
		}
	}
	if b.HighestModSeq != 0 {
		if err := UpdateFolderSync(ctx, tx, folder.ID, FolderSync{HighestModSeq: &b.HighestModSeq}); err != nil {
			return FlagResult{}, err
		}
	}
	if res.Events, err = journal(ctx, s, tx, evs); err != nil {
		return FlagResult{}, err
	}
	return res, nil
}

// staleModSeq reports whether an answer's modification sequence is older than
// the one the row already has: with CONDSTORE a message's MODSEQ only grows,
// so the answer was read before a change the index has recorded — a flag a
// person just set, echoed by the server with its new sequence. Applying it
// would put the old flags back until the next pass.
func staleModSeq(answer, stored int64) bool {
	return answer != 0 && stored != 0 && answer < stored
}

func flagsPayload(accountID string, id, folderID int64, flags []string) MessageFlags {
	bits := bitsOf(flags)
	if flags == nil {
		flags = []string{}
	}
	return MessageFlags{
		AccountID: accountID, MessageID: id, FolderID: folderID, Flags: flags,
		Seen: bits.seen == 1, Flagged: bits.flagged == 1, Answered: bits.answered == 1,
		Draft: bits.draft == 1, Deleted: bits.deleted == 1,
	}
}

// UIDDiff is one UID SEARCH of a folder, compared with the index.
type UIDDiff struct {
	AccountID   string
	FolderID    int64
	UIDValidity uint32
	// Floor is the folder's backfill floor: rows and server UIDs below it
	// are outside the window the index holds.
	Floor imap.UID
	// Upto, when non-zero, leaves rows above it alone: they were indexed
	// after the search was taken and cannot be judged by it.
	Upto imap.UID
	// ServerUIDs is what the search returned.
	ServerUIDs []imap.UID
	Now        time.Time
}

// DiffResult is what a diff did.
type DiffResult struct {
	// Missing are UIDs the server has above the floor that the index holds
	// in no form. Those above the folder's max_seen_uid arrived after sync
	// started: fetch them as new mail. The others are gaps — mostly a
	// message whose INTERNALDATE fell outside the initial window — to fetch
	// quietly, and only if they are inside the window after all.
	Missing    []imap.UID
	Tombstoned int // absent for the first time: hidden, kept
	Deleted    int // absent for the second consecutive time: gone
	Revived    int // absent last time, present now
	Events     []events.Event
}

// ApplyUIDDiff compares the server's UIDs with the index, both ways.
//
// A row the server no longer lists is tombstoned the first time (vanished_at:
// hidden from listings, kept) and deleted only when the next diff finds it
// absent again. A search that failed halfway, or a UID the server reassigned,
// therefore never destroys a row on one reading. A tombstoned row the server
// lists again is revived: absences must be consecutive. Deleting announces
// message.deleted, or a copy removed when another folder still holds the
// message; nothing is announced for a tombstone.
func (s *Store) ApplyUIDDiff(ctx context.Context, tx *sql.Tx, d UIDDiff) (DiffResult, error) {
	if err := RequireSyncEligibleTx(ctx, tx, d.AccountID); err != nil {
		return DiffResult{}, err
	}
	folder, err := indexFolder(ctx, tx, d.AccountID, d.FolderID, d.UIDValidity)
	if err != nil {
		return DiffResult{}, err
	}
	now := d.Now
	if now.IsZero() {
		now = s.now()
	}
	server := make(map[imap.UID]bool, len(d.ServerUIDs))
	for _, uid := range d.ServerUIDs {
		if uid >= d.Floor && uid != 0 {
			server[uid] = true
		}
	}
	upto := uint32(d.Upto)
	if upto == 0 {
		upto = ^uint32(0)
	}
	local, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages
		WHERE folder_id = ? AND uidvalidity = ? AND uid >= ? AND uid <= ? AND stale = 0 ORDER BY uid`,
		folder.ID, d.UIDValidity, uint32(d.Floor), upto)
	if err != nil {
		return DiffResult{}, err
	}
	var (
		res    DiffResult
		gone   []row
		held   = make(map[imap.UID]bool, len(local))
		settle = map[string]bool{}
	)
	for _, r := range local {
		held[r.uid] = true
		switch present := server[r.uid]; {
		case present && r.vanishedAt != 0:
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET vanished_at = 0, updated_at = ? WHERE id = ?`, now.Unix(), r.id); err != nil {
				return DiffResult{}, fmt.Errorf("store: revive message: %w", err)
			}
			settle[r.groupKey] = true
			res.Revived++
		case !present && r.vanishedAt == 0:
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET vanished_at = ?, updated_at = ? WHERE id = ?`, now.Unix(), now.Unix(), r.id); err != nil {
				return DiffResult{}, fmt.Errorf("store: tombstone message: %w", err)
			}
			settle[r.groupKey] = true
			res.Tombstoned++
		case !present:
			gone = append(gone, r)
		}
	}
	// Rows above Upto were not examined, but they are held: a server UID
	// among them is not missing.
	if d.Upto != 0 {
		above, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages
			WHERE folder_id = ? AND uidvalidity = ? AND uid > ? AND stale = 0`, folder.ID, d.UIDValidity, upto)
		if err != nil {
			return DiffResult{}, err
		}
		for _, r := range above {
			held[r.uid] = true
		}
	}
	for uid := range server {
		if !held[uid] {
			res.Missing = append(res.Missing, uid)
		}
	}
	sort.Slice(res.Missing, func(i, j int) bool { return res.Missing[i] < res.Missing[j] })

	evs, err := s.deleteRowsTx(ctx, tx, d.AccountID, gone, map[int64]Folder{folder.ID: folder}, now)
	if err != nil {
		return DiffResult{}, err
	}
	res.Deleted = len(gone)
	for _, r := range gone {
		delete(settle, r.groupKey) // deleteRowsTx settled it
	}
	for key := range settle {
		if _, _, err := settleGroup(ctx, tx, d.AccountID, key); err != nil {
			return DiffResult{}, err
		}
	}
	if err := UpdateFolderSync(ctx, tx, folder.ID, FolderSync{LastFullUIDScanAt: &now}); err != nil {
		return DiffResult{}, err
	}
	if res.Events, err = journal(ctx, s, tx, evs); err != nil {
		return DiffResult{}, err
	}
	return res, nil
}

// ResyncStart is where a resync begins.
type ResyncStart struct {
	// Oldest is the oldest INTERNALDATE among the stale rows, zero when there
	// are none. Search from there, not from the initial window, or everything
	// older than the window would be deleted at the end.
	Oldest time.Time
	// From is what the folder was when this resync first began:
	// ResyncFromLive or ResyncFromInitial. A resync resumed after an
	// interruption keeps what the first attempt recorded.
	From string
}

// BeginResync starts resynchronising a folder whose UIDVALIDITY changed,
// without deleting anything: every row is marked stale and stays visible, the
// folder takes the new UIDVALIDITY with its UID marks cleared, and ApplyResync
// batches then claim stale rows by content.
//
// from is what the folder is now, ResyncFromLive or ResyncFromInitial. It is
// recorded only when no resync is in progress: resuming one that a dropped
// connection or a failure interrupted — by then the folder says "resync" or
// "error", not what it was — keeps what the first attempt recorded, so it
// ends the same way.
func (s *Store) BeginResync(ctx context.Context, tx *sql.Tx, accountID string, folderID int64, st provider.FolderStatus, from string) (ResyncStart, error) {
	if err := RequireSyncEligibleTx(ctx, tx, accountID); err != nil {
		return ResyncStart{}, err
	}
	folder, err := folderOn(ctx, tx, accountID, folderID)
	if err != nil {
		return ResyncStart{}, err
	}
	if folder.ResyncFrom != "" {
		from = folder.ResyncFrom
	}
	if from != ResyncFromInitial {
		from = ResyncFromLive
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET stale = 1 WHERE folder_id = ? AND uidvalidity <> ?`,
		folderID, st.UIDValidity); err != nil {
		return ResyncStart{}, fmt.Errorf("store: mark rows stale: %w", err)
	}
	if err := unstalePrimaries(ctx, tx, accountID, folderID); err != nil {
		return ResyncStart{}, err
	}
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT min(internal_date) FROM messages WHERE folder_id = ? AND stale = 1`,
		folderID).Scan(&oldest); err != nil {
		return ResyncStart{}, fmt.Errorf("store: find the oldest stale row: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE folders SET uidvalidity = ?, uidnext = ?, highest_modseq = ?, server_count = ?,
		max_seen_uid = 0, backfill_floor = 0, backfill_cursor = 0, sync_state = 'resync', resync_from = ? WHERE id = ?`,
		st.UIDValidity, uint32(st.UIDNext), clampInt64(st.HighestModSeq), st.NumMessages, from, folderID)
	if err != nil {
		return ResyncStart{}, fmt.Errorf("store: begin resync: %w", err)
	}
	out := ResyncStart{From: from}
	if oldest.Valid {
		out.Oldest = time.Unix(oldest.Int64, 0).UTC()
	}
	return out, nil
}

// unstalePrimaries hands the primary of every group this folder's resync
// has just made stale to a copy elsewhere that is live and not stale: a
// Gmail message in the INBOX and All Mail must stay in the listing of
// everything, as its All Mail copy, while the INBOX is read again. A claim
// settles the group once more, and the row the resync finds becomes the
// primary again.
func unstalePrimaries(ctx context.Context, tx *sql.Tx, accountID string, folderID int64) error {
	keys, err := stalePrimaryGroups(ctx, tx, folderID)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if _, _, err := settleGroup(ctx, tx, accountID, key); err != nil {
			return err
		}
	}
	return nil
}

// stalePrimaryGroups lists the groups whose primary is a stale row of the
// folder while another copy is live and not stale.
func stalePrimaryGroups(ctx context.Context, tx *sql.Tx, folderID int64) ([]string, error) {
	rs, err := tx.QueryContext(ctx, `SELECT DISTINCT m.group_key FROM messages m
		WHERE m.folder_id = ? AND m.stale = 1 AND m.dup_of IS NULL
		  AND EXISTS (SELECT 1 FROM messages o WHERE o.account_id = m.account_id AND o.group_key = m.group_key
		              AND o.id <> m.id AND o.stale = 0 AND o.vanished_at = 0)`, folderID)
	if err != nil {
		return nil, fmt.Errorf("store: find stale primaries: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rs.Close() }()
	var keys []string
	for rs.Next() {
		var key string
		if err := rs.Scan(&key); err != nil {
			return nil, fmt.Errorf("store: find stale primaries: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("store: find stale primaries: %w", err)
	}
	return keys, nil
}

// FinishResync ends a resync: the rows no new UID claimed are deleted — those
// are real deletions, and are announced as such — the folder goes live, and
// one folder.changed closes it: resync_done, or initial_done when the resync
// began before the folder's initial sync had finished (BeginResync recorded
// it), because this resync is what finished it. A consumer waiting for a
// folder's initial_done must get one.
func (s *Store) FinishResync(ctx context.Context, tx *sql.Tx, accountID string, folderID int64, now time.Time) ([]events.Event, error) {
	if err := RequireSyncEligibleTx(ctx, tx, accountID); err != nil {
		return nil, err
	}
	folder, err := folderOn(ctx, tx, accountID, folderID)
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = s.now()
	}
	stale, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages WHERE folder_id = ? AND stale = 1`, folderID)
	if err != nil {
		return nil, err
	}
	evs, err := s.deleteRowsTx(ctx, tx, accountID, stale, map[int64]Folder{folder.ID: folder}, now)
	if err != nil {
		return nil, err
	}
	live, none := FolderStateLive, ""
	if err := UpdateFolderSync(ctx, tx, folderID, FolderSync{SyncState: &live, LastSyncedAt: &now, ResyncFrom: &none}); err != nil {
		return nil, err
	}
	change := FolderResyncDone
	if folder.ResyncFrom == ResyncFromInitial {
		change = FolderInitialDone
		if err := completeInitialCount(ctx, tx, folderID); err != nil {
			return nil, err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT local_count FROM folders WHERE id = ?`, folderID).Scan(&count); err != nil {
		return nil, fmt.Errorf("store: finish resync: %w", err)
	}
	ev, err := events.New(events.TypeFolderChanged, accountID, now, FolderChanged{
		AccountID: accountID, FolderID: folder.ID, Name: folder.Name, Role: string(folder.Role),
		Change: change, Count: count,
	})
	if err != nil {
		return nil, err
	}
	return journal(ctx, s, tx, append(evs, ev))
}

// HasStaleRows reports whether a folder still holds rows a UIDVALIDITY
// resync has not yet claimed or deleted: a resync that was interrupted, and
// must be finished before the folder is treated as live again.
func (s *Store) HasStaleRows(ctx context.Context, folderID int64) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE folder_id = ? AND stale = 1 LIMIT 1`, folderID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: look for stale rows: %w", err)
	}
	return true, nil
}

// LocalUIDs lists the UIDs the index holds for a folder under a UIDVALIDITY,
// between lo and hi inclusive (hi zero: no upper bound), ascending. Stale rows
// are never included; tombstoned ones only when asked.
func (s *Store) LocalUIDs(ctx context.Context, folderID int64, uidvalidity uint32, lo, hi imap.UID, includeVanished bool) ([]imap.UID, error) {
	upper := uint32(hi)
	if upper == 0 {
		upper = ^uint32(0)
	}
	query := `SELECT uid FROM messages WHERE folder_id = ? AND uidvalidity = ? AND uid >= ? AND uid <= ? AND stale = 0`
	if !includeVanished {
		query += ` AND vanished_at = 0`
	}
	rs, err := s.r.QueryContext(ctx, query+` ORDER BY uid`, folderID, uidvalidity, uint32(lo), upper)
	if err != nil {
		return nil, fmt.Errorf("store: list indexed uids: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rs.Close() }()
	var out []imap.UID
	for rs.Next() {
		var uid uint32
		if err := rs.Scan(&uid); err != nil {
			return nil, fmt.Errorf("store: list indexed uids: %w", err)
		}
		out = append(out, imap.UID(uid))
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("store: list indexed uids: %w", err)
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Only strings and plain structs reach here; a failure would be a
		// programming error, and an empty list is the honest fallback.
		return "[]"
	}
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func clampInt64(v uint64) int64 {
	if v > 1<<63-1 {
		return 1<<63 - 1
	}
	return int64(v)
}
