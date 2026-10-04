package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
)

// Reading the index: search and one message, for the API. Nothing here
// writes. Reading a message fetches its body from the mail server, which is
// the service's business; what the index holds is metadata only, and that is
// all these return.

// ErrNoMessage is a message id that is not in the index.
var ErrNoMessage = errors.New("store: no such message")

// ErrBadMatch is search text the full-text index cannot take: too long, or
// too many words.
var ErrBadMatch = errors.New("store: unusable search text")

// Search text limits. A person types a few words; a query past these is not
// a search, and a long MATCH expression is work for nothing.
const (
	MaxMatchRunes = 256
	MaxMatchTerms = 16
)

// MessageRow is one indexed message as the API presents it.
type MessageRow struct {
	ID                int64
	AccountID         string
	FolderID          int64
	FolderName        string // what SELECT takes
	FolderDisplayName string
	FolderRole        provider.FolderRole
	UIDValidity       uint32
	UID               imap.UID
	MessageID         string // the RFC 5322 header, bare
	GroupKey          string // the message's identity within the account (GroupKey)
	InReplyTo         string // bare ids, space-separated
	References        []string
	Subject           string
	From, To, Cc      []provider.Address
	Bcc, ReplyTo      []provider.Address
	Date              int64 // the Date header; 0 when unparsable
	InternalDate      int64
	Size              int64
	Seen, Flagged     bool
	Answered, Draft   bool
	HasAttachments    bool
	// Vanished is a row missing from its folder at the last diff; Stale one
	// waiting for a UIDVALIDITY resync to find its new UID. Neither can be
	// read from the server as indexed.
	Vanished bool
	Stale    bool
}

// Cursor is a position in the listing order: (internal_date DESC, id DESC).
type Cursor struct {
	InternalDate int64
	ID           int64
}

// MessageQuery is a search of the index.
type MessageQuery struct {
	// AccountIDs are the accounts searched. Required: an empty list finds
	// nothing, never everything.
	AccountIDs []string
	// FolderID limits the search to one folder, whose rows are all listed.
	// Without it every message is listed once: label copies (Gmail shows a
	// message once per label) are folded into the group's primary row.
	FolderID int64
	// Match is a full-text expression from MatchQuery; empty matches all.
	Match string
	// From is a substring of the sender's name or address.
	From string
	// Seen, Flagged and HasAttachments filter when set.
	Seen, Flagged, HasAttachments *bool
	// Since and Until bound the INTERNALDATE, in unix seconds: Since
	// inclusive, Until exclusive. Zero is unbounded.
	Since, Until int64
	// After continues a listing: only rows after this position.
	After *Cursor
	// Limit caps the rows returned.
	Limit int
}

const messageRowColumns = `m.id, m.account_id, m.folder_id, f.name, f.display_name, f.role, m.uidvalidity, m.uid,
	m.message_id, m.group_key, m.in_reply_to, m.references_json, m.subject, m.from_json, m.to_json, m.cc_json, m.bcc_json,
	m.reply_to_json, m.date, m.internal_date, m.size, m.seen, m.flagged, m.answered, m.draft, m.has_attachments,
	m.vanished_at, m.stale`

func scanMessageRow(sc interface{ Scan(...any) error }) (MessageRow, error) {
	var (
		r                                             MessageRow
		role, refs, from, to, cc, bcc, replyTo        string
		seen, flagged, answered, draft, attach, stale int
		vanished                                      int64
	)
	err := sc.Scan(&r.ID, &r.AccountID, &r.FolderID, &r.FolderName, &r.FolderDisplayName, &role, &r.UIDValidity,
		&r.UID, &r.MessageID, &r.GroupKey, &r.InReplyTo, &refs, &r.Subject, &from, &to, &cc, &bcc, &replyTo, &r.Date,
		&r.InternalDate, &r.Size, &seen, &flagged, &answered, &draft, &attach, &vanished, &stale)
	if err != nil {
		return MessageRow{}, err
	}
	r.FolderRole = provider.FolderRole(role)
	r.References = stringList(refs)
	r.From, r.To, r.Cc = addressList(from), addressList(to), addressList(cc)
	r.Bcc, r.ReplyTo = addressList(bcc), addressList(replyTo)
	r.Seen, r.Flagged, r.Answered, r.Draft = seen != 0, flagged != 0, answered != 0, draft != 0
	r.HasAttachments, r.Vanished, r.Stale = attach != 0, vanished != 0, stale != 0
	return r, nil
}

// addressList reads a stored address list. A value that does not parse is
// shown as no addresses rather than failing the listing it is in.
func addressList(raw string) []provider.Address {
	var out []provider.Address
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []provider.Address{}
	}
	return out
}

func stringList(raw string) []string {
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// SearchMessages lists the rows matching q, newest first by INTERNALDATE and
// then by id, which is the order a Cursor continues. Rows that are gone from
// their folder (vanished) or waiting for a resync (stale) are never listed.
func (s *Store) SearchMessages(ctx context.Context, q MessageQuery) ([]MessageRow, error) {
	if len(q.AccountIDs) == 0 || q.Limit <= 0 {
		return []MessageRow{}, nil
	}
	var (
		where []string
		args  []any
	)
	add := func(clause string, v ...any) {
		where = append(where, clause)
		args = append(args, v...)
	}
	add(`m.account_id IN (`+placeholders(len(q.AccountIDs))+`)`, anys(q.AccountIDs)...)
	// Spelled out as the partial indexes are, so the planner can use them.
	add(`m.vanished_at = 0`)
	add(`m.stale = 0`)
	if q.FolderID != 0 {
		add(`m.folder_id = ?`, q.FolderID)
	} else {
		add(`m.dup_of IS NULL`)
	}
	if q.Match != "" {
		add(`m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`, q.Match)
	}
	if from := strings.TrimSpace(q.From); from != "" {
		add(`m.from_text LIKE ? ESCAPE '\'`, "%"+escapeLike(from)+"%")
	}
	if q.Seen != nil {
		add(`m.seen = ?`, boolInt(*q.Seen))
	}
	if q.Flagged != nil {
		add(`m.flagged = ?`, boolInt(*q.Flagged))
	}
	if q.HasAttachments != nil {
		add(`m.has_attachments = ?`, boolInt(*q.HasAttachments))
	}
	if q.Since != 0 {
		add(`m.internal_date >= ?`, q.Since)
	}
	if q.Until != 0 {
		add(`m.internal_date < ?`, q.Until)
	}
	if q.After != nil {
		add(`(m.internal_date < ? OR (m.internal_date = ? AND m.id < ?))`,
			q.After.InternalDate, q.After.InternalDate, q.After.ID)
	}
	args = append(args, q.Limit)

	//nolint:gosec // G202: every clause above is a constant; values are bound
	query := `SELECT ` + messageRowColumns + ` FROM messages m JOIN folders f ON f.id = m.folder_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY m.internal_date DESC, m.id DESC LIMIT ?`
	rows, err := s.r.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []MessageRow{}
	for rows.Next() {
		r, err := scanMessageRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: search messages: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	return out, nil
}

// Message reads one row, whatever its state: the caller decides what a
// vanished or stale row means.
func (s *Store) Message(ctx context.Context, id int64) (MessageRow, error) {
	r, err := scanMessageRow(s.r.QueryRowContext(ctx, `SELECT `+messageRowColumns+`
		FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return MessageRow{}, ErrNoMessage
	case err != nil:
		return MessageRow{}, fmt.Errorf("store: read message: %w", err)
	}
	return r, nil
}

// MessageParts reads a message's MIME parts as BODYSTRUCTURE described them,
// in IMAP section order (1, 1.1, 1.2, 2, 10).
func (s *Store) MessageParts(ctx context.Context, id int64) ([]provider.PartInfo, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT path, mime_type, charset, encoding, disposition, filename,
		content_id, size, is_body, is_attachment FROM parts WHERE msg_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("store: read parts: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := []provider.PartInfo{}
	for rows.Next() {
		var (
			p               provider.PartInfo
			path, charset   string
			isBody, isAttch int
		)
		if err := rows.Scan(&path, &p.MIMEType, &charset, &p.Encoding, &p.Disposition, &p.Filename,
			&p.ContentID, &p.Size, &isBody, &isAttch); err != nil {
			return nil, fmt.Errorf("store: read parts: %w", err)
		}
		parsed, ok := ParsePartPath(path)
		if !ok {
			// Written by the index from a provider path, so this cannot
			// happen; a row that does is skipped, not served.
			continue
		}
		p.Path = parsed
		if charset != "" {
			p.Params = map[string]string{"charset": charset}
		}
		p.IsBody, p.IsAttachment = isBody != 0, isAttch != 0
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read parts: %w", err)
	}
	sortParts(out)
	return out, nil
}

// maxPartDepth bounds a section path. MIME nests, but nothing a person
// receives is thirty levels deep.
const maxPartDepth = 32

// ParsePartPath reads an IMAP section path such as "1.2": numbers from 1,
// separated by dots.
func ParsePartPath(s string) ([]int, bool) {
	if s == "" || len(s) > 256 {
		return nil, false
	}
	fields := strings.Split(s, ".")
	if len(fields) > maxPartDepth {
		return nil, false
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		if f == "" || len(f) > 6 || f[0] == '0' {
			return nil, false
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func sortParts(parts []provider.PartInfo) {
	slices.SortFunc(parts, func(a, b provider.PartInfo) int { return slices.Compare(a.Path, b.Path) })
}

// CopyCounts says, for each row, how many other live rows of the same
// message the account holds: the other Gmail labels it carries, or the same
// message filed twice. One query for a whole page.
func (s *Store) CopyCounts(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	//nolint:gosec // G202: only placeholders are interpolated
	rows, err := s.r.QueryContext(ctx, `SELECT m.id, (SELECT count(*) FROM messages c
			WHERE c.account_id = m.account_id AND c.group_key = m.group_key
			  AND c.vanished_at = 0 AND c.stale = 0 AND c.id <> m.id)
		FROM messages m WHERE m.id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: count copies: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: count copies: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count copies: %w", err)
	}
	return out, nil
}

// FolderByID reads one folder of the index, whichever account it belongs to.
// The caller checks that its account is one the reader may see.
func (s *Store) FolderByID(ctx context.Context, id int64) (Folder, error) {
	f, err := scanFolder(s.r.QueryRowContext(ctx, `SELECT `+folderColumns+` FROM folders WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Folder{}, ErrNoFolder
	case err != nil:
		return Folder{}, fmt.Errorf("store: read folder: %w", err)
	}
	return f, nil
}

// MatchQuery turns what a person typed into a full-text expression over the
// three indexed columns — subject, sender, recipients — that means only
// "these words": every word is quoted, so nothing typed can be an FTS5
// operator (AND, OR, NOT, NEAR, a column filter, ^, *, parentheses), and the
// last word matches as a prefix, so a search narrows while it is typed.
//
// Words with no letter or digit are dropped; none left is "" (no text
// filter). Text past MaxMatchRunes or MaxMatchTerms words is ErrBadMatch
// rather than cut: a search that silently ignored part of what was asked
// for would show more than asked.
func MatchQuery(input string) (string, error) {
	if n := len([]rune(input)); n > MaxMatchRunes {
		return "", fmt.Errorf("%w: %d characters, at most %d", ErrBadMatch, n, MaxMatchRunes)
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, input)
	var terms []string
	for _, word := range strings.Fields(clean) {
		if strings.IndexFunc(word, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) < 0 {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	if len(terms) > MaxMatchTerms {
		return "", fmt.Errorf("%w: %d words, at most %d", ErrBadMatch, len(terms), MaxMatchTerms)
	}
	if len(terms) == 0 {
		return "", nil
	}
	terms[len(terms)-1] += "*"
	return "{subject from_text to_text} : (" + strings.Join(terms, " ") + ")", nil
}

// escapeLike makes s match itself in a LIKE with ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

func anys(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
