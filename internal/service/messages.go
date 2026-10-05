package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/mime"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// Searching the index and reading messages.
//
// The privacy boundary, which every function here keeps: the index holds
// metadata only, and a message's text, attachments and original are fetched
// from the mail server when asked for, handed back and never kept — no row,
// no cache, no file beyond the provider's spool, which is removed when the
// caller closes what it was given. Every fetch is BODY.PEEK in a folder
// opened read-only (EXAMINE), so reading never marks a message as read, and
// nothing here writes to the mailbox at all. Every fetch runs on the
// account's interactive connection, the engine's, so reading costs no
// connection beyond the three an account has. Nothing is logged about a
// message but its account and local id: never a subject, an address, a
// filename or any content.

// Limits of the read routes.
const (
	// DefaultPageSize and MaxPageSize bound a page of search results.
	DefaultPageSize = 50
	MaxPageSize     = 100
	// DefaultBodyBytes and MaxBodyBytes bound each decoded body, text and
	// HTML separately.
	DefaultBodyBytes = 1 << 20
	MaxBodyBytes     = 5 << 20
	// MaxRawBytes is the largest original message served.
	MaxRawBytes = 50 << 20
	// minAttachmentCap is the least an attachment may be, encoded; a
	// provider that sends larger mail raises it (see attachmentCap).
	minAttachmentCap = 50 << 20
	// maxBodyFetch bounds the encoded bytes fetched for one body part. The
	// provider interface fetches whole sections, so a body larger than the
	// answer's cap is still fetched to show its beginning — up to this.
	// Past it the body is reported truncated without being fetched: the
	// original is the way to read it.
	maxBodyFetch = 32 << 20
	// maxFromRunes bounds the sender filter.
	maxFromRunes = 256

	// readTimeout and downloadTimeout bound the provider work of one call:
	// a message's bodies, and an attachment or original. The routes carry
	// the same numbers.
	readTimeout     = 60 * time.Second
	downloadTimeout = 10 * time.Minute
)

// Body formats GetMessage returns.
const (
	FormatText = "text"
	FormatHTML = "html"
	FormatBoth = "both"
)

// Address is a name and an address as a header gave them. Either may be
// empty; both are always present.
type Address struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// MessageSummary is one indexed message: one row, in one folder.
type MessageSummary struct {
	// ID is the local row id every message route takes; never the
	// Message-ID header.
	ID         int64  `json:"id"`
	AccountID  string `json:"account_id"`
	FolderID   int64  `json:"folder_id"`
	FolderRole string `json:"folder_role,omitempty"`
	// FolderName is the folder's display name, as the folder listing shows
	// it.
	FolderName string    `json:"folder_name"`
	Subject    string    `json:"subject"`
	From       []Address `json:"from"`
	To         []Address `json:"to"`
	// Date is the Date header, unix seconds; 0 when it was missing or
	// unreadable. InternalDate is when the server received the message, and
	// what listings are ordered by.
	Date           int64 `json:"date"`
	InternalDate   int64 `json:"internal_date"`
	Size           int64 `json:"size"`
	Seen           bool  `json:"seen"`
	Flagged        bool  `json:"flagged"`
	Answered       bool  `json:"answered"`
	Draft          bool  `json:"draft"`
	HasAttachments bool  `json:"has_attachments"`
	// Copies counts the other live rows of the same message in the account:
	// its other Gmail labels, or the same message filed twice.
	Copies int `json:"copies"`
}

// MessagePage is one page of a search. NextCursor continues it, and is
// absent on the last page.
type MessagePage struct {
	Messages   []MessageSummary `json:"messages"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// MessagePart is one MIME part, as the index describes it.
type MessagePart struct {
	// Path is the IMAP section ("1", "1.2"): what the attachment route takes.
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	// Filename is the sender's name for the part, decoded and made safe.
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size"`
	Disposition string `json:"disposition,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
	// IsAttachment is what a person would call an attachment.
	IsAttachment bool `json:"is_attachment"`
}

// MessageBody is the body, fetched from the mail server for this answer and
// not kept. HTML is the sender's markup, unsanitised; HTMLUnsafe is always
// true to say so, and whoever shows it must isolate it.
type MessageBody struct {
	Text       *string `json:"text,omitempty"`
	HTML       *string `json:"html,omitempty"`
	HTMLUnsafe bool    `json:"html_unsafe"`
	// Truncated says a body is longer than was asked for: what is here is
	// its beginning. The original has all of it.
	Truncated bool `json:"truncated"`
	// CharsetFallback says a body's declared charset could not be used, so
	// it was decoded by a guess and some characters may be wrong.
	CharsetFallback bool `json:"charset_fallback"`
}

// Message is a message with its headers, its parts and its body.
type Message struct {
	MessageSummary
	Cc      []Address `json:"cc"`
	Bcc     []Address `json:"bcc"`
	ReplyTo []Address `json:"reply_to"`
	// MessageID is the Message-ID header, without angle brackets.
	MessageID string `json:"message_id"`
	// InReplyTo holds the In-Reply-To ids, bare and space-separated.
	InReplyTo  string        `json:"in_reply_to"`
	References []string      `json:"references"`
	Parts      []MessagePart `json:"parts"`
	Body       MessageBody   `json:"body"`
}

// SearchRequest filters the index.
type SearchRequest struct {
	// AccountID limits the search to one account; empty searches every
	// account the caller may read.
	AccountID string
	// Workspace limits the search to the mailboxes of one workspace the
	// caller is an active member of; empty is every workspace.
	Workspace string
	// FolderID limits the search to one folder of the index. Without it,
	// each message is listed once, however many Gmail labels it carries.
	FolderID int64
	// Query is words to find in the subject, the sender and the
	// recipients. Never the body: the index does not hold it.
	Query string
	// From is part of the sender's name or address.
	From string
	// Unseen, Flagged and HasAttachments filter when set: Unseen true is
	// unread mail only, false read mail only.
	Unseen, Flagged, HasAttachments *bool
	// Since and Until bound when the server received the message: a date
	// (YYYY-MM-DD, UTC), an RFC 3339 time, or unix seconds. Since is
	// inclusive; Until excludes what follows it, and a date includes its
	// whole day.
	Since, Until string
	// Cursor is the NextCursor of the previous page.
	Cursor string
	// Limit is the page size, 1 to MaxPageSize; 0 is DefaultPageSize.
	Limit int
}

// GetMessageRequest names a message and how much of its body to return.
type GetMessageRequest struct {
	ID int64
	// Format is FormatText, FormatHTML or FormatBoth; empty is both.
	Format string
	// MaxBytes caps each decoded body, 1 to MaxBodyBytes; 0 is
	// DefaultBodyBytes.
	MaxBytes int64
}

// Download is a file fetched from the mail server: an original message or
// an attachment. Closing Body removes what was spooled to fetch it and gives
// back its place among the downloads in flight, so the caller must close it,
// read or not, and should not hold it open for a client that stopped
// reading.
type Download struct {
	Body io.ReadCloser
	// Size is how many bytes Body yields, or -1 when that is known only once
	// it is read (an attachment still being decoded).
	Size int64
	// ContentType is safe to serve: a type a browser would run or render
	// actively is application/octet-stream.
	ContentType string
	// Filename is safe to name a downloaded file with, and never empty.
	Filename string
}

// Errors of the read routes.
var (
	errNoMessage   = E(CodeNotFound, "no such message", nil)
	errMessageGone = E(CodeNotFound, "that message no longer exists on the server", nil)
	// Busy is a conflict, not a 404: the message is still there, and the
	// sync engine finds it under its new UID in a moment.
	errMessageBusy = E(CodeConflict,
		"that message's folder is being read again from the server; try again shortly", nil)
)

// SearchMessages lists indexed messages matching req, newest first.
func (s *Service) SearchMessages(ctx context.Context, p Principal, req SearchRequest) (MessagePage, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return MessagePage{}, err
	}
	q, err := parseSearch(req)
	if err != nil {
		return MessagePage{}, err
	}
	if err := s.inWorkspace(ctx, p, req.Workspace); err != nil {
		return MessagePage{}, err
	}
	empty := MessagePage{Messages: []MessageSummary{}}

	var accountIDs []string
	if req.AccountID != "" {
		a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, req.AccountID, needRead)
		if err != nil {
			return MessagePage{}, err
		}
		if req.Workspace != "" && a.WorkspaceID != req.Workspace {
			// Named with a workspace it is not in: as good as absent there.
			return MessagePage{}, E(CodeNotFound, "no such account", nil)
		}
		accountIDs = []string{a.ID}
	} else {
		v := readable(p)
		v.Workspace = req.Workspace
		all, err := s.accounts.Repo().ListVisible(ctx, v)
		if err != nil {
			return MessagePage{}, E(CodeInternal, "listing accounts failed", err)
		}
		for _, a := range all {
			if p.MayAccess(a.ID) {
				accountIDs = append(accountIDs, a.ID)
			}
		}
	}
	if s.store == nil || len(accountIDs) == 0 {
		return empty, nil
	}
	if req.FolderID != 0 {
		f, err := s.store.FolderByID(ctx, req.FolderID)
		switch {
		case errors.Is(err, store.ErrNoFolder):
			return MessagePage{}, E(CodeNotFound, "no such folder", err)
		case err != nil:
			return MessagePage{}, E(CodeInternal, "reading the folder index failed", err)
		}
		if !slices.Contains(accountIDs, f.AccountID) {
			// Somebody else's folder, or one outside the account named:
			// answered like a folder that does not exist.
			return MessagePage{}, E(CodeNotFound, "no such folder", nil)
		}
		accountIDs = []string{f.AccountID}
		q.FolderID = f.ID
	}
	q.AccountIDs = accountIDs

	limit := q.Limit
	q.Limit = limit + 1 // one more says whether there is a next page
	rows, err := s.store.SearchMessages(ctx, q)
	if err != nil {
		return MessagePage{}, E(CodeInternal, "searching the index failed", err)
	}
	page := MessagePage{}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		page.NextCursor = encodeCursor(store.Cursor{InternalDate: last.InternalDate, ID: last.ID})
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	copies, err := s.store.CopyCounts(ctx, ids)
	if err != nil {
		return MessagePage{}, E(CodeInternal, "searching the index failed", err)
	}
	page.Messages = make([]MessageSummary, 0, len(rows))
	for _, r := range rows {
		page.Messages = append(page.Messages, presentSummary(r, copies[r.ID]))
	}
	return page, nil
}

// parseSearch checks what can be checked before anything is read, and turns
// the request into the index's query, without its accounts.
func parseSearch(req SearchRequest) (store.MessageQuery, error) {
	var q store.MessageQuery
	switch {
	case req.Limit == 0:
		q.Limit = DefaultPageSize
	case req.Limit < 1 || req.Limit > MaxPageSize:
		return q, Ef(CodeBadRequest, nil, "limit must be between 1 and %d", MaxPageSize)
	default:
		q.Limit = req.Limit
	}
	if req.FolderID < 0 {
		return q, E(CodeBadRequest, "folder must be a folder id from the folder listing", nil)
	}
	match, err := store.MatchQuery(req.Query)
	if err != nil {
		return q, Ef(CodeBadRequest, err, "the search text is too long: at most %d characters and %d words",
			store.MaxMatchRunes, store.MaxMatchTerms)
	}
	q.Match = match
	if len([]rune(req.From)) > maxFromRunes {
		return q, Ef(CodeBadRequest, nil, "from is too long: at most %d characters", maxFromRunes)
	}
	q.From = strings.TrimSpace(req.From)
	if req.Unseen != nil {
		seen := !*req.Unseen
		q.Seen = &seen
	}
	q.Flagged, q.HasAttachments = req.Flagged, req.HasAttachments
	if q.Since, err = parseInstant("since", req.Since, false); err != nil {
		return q, err
	}
	if q.Until, err = parseInstant("until", req.Until, true); err != nil {
		return q, err
	}
	if req.Cursor != "" {
		c, ok := decodeCursor(req.Cursor)
		if !ok {
			return q, E(CodeBadRequest, "cursor is not one this server handed out; start the search again", nil)
		}
		q.After = &c
	}
	return q, nil
}

// parseInstant reads a search bound: a date, an RFC 3339 time or unix
// seconds. A date as an end bound (until) means the end of that day.
func parseInstant(name, raw string, end bool) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
		return n, nil
	}
	if day, err := time.Parse(time.DateOnly, raw); err == nil {
		if end {
			day = day.AddDate(0, 0, 1)
		}
		return day.Unix(), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Unix(), nil
	}
	return 0, Ef(CodeBadRequest, nil, "%s must be a date (YYYY-MM-DD), an RFC 3339 time or unix seconds", name)
}

// A cursor is the last row of a page, (internal_date, id), as sixteen bytes
// in base64url. Opaque to callers; nothing about it is secret, and a forged
// one only moves a listing the caller may read anyway.
func encodeCursor(c store.Cursor) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(c.InternalDate)) //nolint:gosec // G115: round-trips through int64
	binary.BigEndian.PutUint64(b[8:], uint64(c.ID))           //nolint:gosec // G115: round-trips through int64
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func decodeCursor(s string) (store.Cursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 16 {
		return store.Cursor{}, false
	}
	c := store.Cursor{
		InternalDate: int64(binary.BigEndian.Uint64(b[:8])), //nolint:gosec // G115: round-trips through int64
		ID:           int64(binary.BigEndian.Uint64(b[8:])), //nolint:gosec // G115: round-trips through int64
	}
	if c.ID < 1 {
		return store.Cursor{}, false
	}
	return c, true
}

// GetMessage returns a message with its body, fetched from the mail server
// now and not kept. It never marks the message as read.
func (s *Service) GetMessage(ctx context.Context, p Principal, req GetMessageRequest) (Message, error) {
	format, maxBytes, err := parseBodyRequest(req)
	if err != nil {
		return Message{}, err
	}
	a, row, err := s.readableMessage(ctx, p, req.ID)
	if err != nil {
		return Message{}, err
	}
	parts, err := s.store.MessageParts(ctx, row.ID)
	if err != nil {
		return Message{}, E(CodeInternal, "reading the message failed", err)
	}
	copies, err := s.store.CopyCounts(ctx, []int64{row.ID})
	if err != nil {
		return Message{}, E(CodeInternal, "reading the message failed", err)
	}
	out := presentMessage(row, copies[row.ID], parts)

	plain, html := mime.BodyParts(parts)
	var wanted []*provider.PartInfo
	if plain != nil && format != FormatHTML {
		wanted = append(wanted, plain)
	}
	if html != nil && format != FormatText {
		wanted = append(wanted, html)
	}
	if len(wanted) == 0 {
		return out, nil
	}
	if err := readyToRead(a); err != nil {
		return Message{}, err
	}

	ctx, cancel := ctxWithDeadline(ctx, readTimeout)
	defer cancel()
	fetched := make([]*provider.Part, len(wanted))
	defer func() {
		for _, part := range fetched {
			if part != nil {
				//nolint:errcheck // removing the spool file; nothing to report
				_ = part.Body.Close()
			}
		}
	}()
	tooLarge := make([]bool, len(wanted))
	err = s.fetchFromServer(ctx, a, row, "reading a message from its server failed",
		func(ctx context.Context, sess provider.Session) error {
			for i, info := range wanted {
				if info.Size > maxBodyFetch {
					tooLarge[i] = true
					continue
				}
				part, err := sess.FetchPart(ctx, row.UID, *info, maxBodyFetch)
				if errors.Is(err, provider.ErrTooLarge) {
					// The server's section is larger than the index said.
					tooLarge[i] = true
					continue
				}
				if err != nil {
					return err
				}
				fetched[i] = &part
			}
			return nil
		})
	if err != nil {
		return Message{}, err
	}

	// Decoded after the connection is released: the spooled sections are
	// the only copy, and they go when this returns.
	for i, info := range wanted {
		var decoded mime.Text
		if tooLarge[i] {
			decoded.Truncated = true
		} else {
			decoded, err = mime.DecodeText(*info, fetched[i].Body, maxBytes)
			if err != nil {
				return Message{}, E(CodeInternal, "reading the message's body failed", err)
			}
		}
		body := decoded.Text
		if info == plain {
			out.Body.Text = &body
		} else {
			out.Body.HTML = &body
		}
		out.Body.Truncated = out.Body.Truncated || decoded.Truncated
		out.Body.CharsetFallback = out.Body.CharsetFallback || decoded.CharsetFallback
	}
	return out, nil
}

func parseBodyRequest(req GetMessageRequest) (format string, maxBytes int64, err error) {
	switch req.Format {
	case "", FormatBoth:
		format = FormatBoth
	case FormatText, FormatHTML:
		format = req.Format
	default:
		return "", 0, E(CodeBadRequest, "format must be text, html or both", nil)
	}
	switch {
	case req.MaxBytes == 0:
		maxBytes = DefaultBodyBytes
	case req.MaxBytes < 1 || req.MaxBytes > MaxBodyBytes:
		return "", 0, Ef(CodeBadRequest, nil, "max_bytes must be between 1 and %d", MaxBodyBytes)
	default:
		maxBytes = req.MaxBytes
	}
	return format, maxBytes, nil
}

// GetRaw returns a message exactly as the mail server holds it.
func (s *Service) GetRaw(ctx context.Context, p Principal, id int64) (Download, error) {
	a, row, err := s.readableMessage(ctx, p, id)
	if err != nil {
		return Download{}, err
	}
	if err := readyToRead(a); err != nil {
		return Download{}, err
	}
	// Refused on the index's word before anything is fetched: otherwise the
	// whole message crosses the network, and holds the account's connection,
	// only to be thrown away. The provider's cap below still catches a
	// server whose RFC822.SIZE was wrong.
	if row.Size > MaxRawBytes {
		return Download{}, Ef(CodeBadRequest, nil,
			"the message is larger than %d MiB, the most Mailie downloads", MaxRawBytes>>20)
	}
	release, err := s.downloads.reserve(downloadCaller(p), row.Size)
	if err != nil {
		return Download{}, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			release()
		}
	}()
	ctx, cancel := ctxWithDeadline(ctx, downloadTimeout)
	defer cancel()
	var part provider.Part
	err = s.fetchFromServer(ctx, a, row, "downloading a message from its server failed",
		func(ctx context.Context, sess provider.Session) error {
			var err error
			part, err = sess.FetchRaw(ctx, row.UID, MaxRawBytes)
			return err
		})
	if errors.Is(err, provider.ErrTooLarge) {
		return Download{}, Ef(CodeBadRequest, err,
			"the message is larger than %d MiB, the most Mailie downloads", MaxRawBytes>>20)
	}
	if err != nil {
		return Download{}, err
	}
	handedOver = true
	return Download{
		Body: &releasing{ReadCloser: part.Body, release: release}, Size: part.Size, ContentType: "message/rfc822",
		// Never the subject: a filename travels in a header, and headers
		// end up in logs along the way.
		Filename: fmt.Sprintf("message-%d.eml", row.ID),
	}, nil
}

// GetAttachment returns one part of a message, named by its IMAP section,
// with its transfer encoding undone.
func (s *Service) GetAttachment(ctx context.Context, p Principal, id int64, path string) (Download, error) {
	a, row, info, err := s.attachmentPart(ctx, p, id, path)
	if err != nil {
		return Download{}, err
	}
	return s.fetchAttachment(ctx, p, a, row, info)
}

// MaxInlineAttachment is the most GetAttachmentInline hands back, decoded.
const MaxInlineAttachment = 1 << 20

// InlineAttachment is an attachment small enough to return whole, inside
// the answer itself — what a tool embeds in its result. When it is not,
// TooLarge says so and Data is empty.
type InlineAttachment struct {
	// AccountID is the account the message belongs to.
	AccountID string
	Data      []byte
	TooLarge  bool
	// Size is the part's size on the mail server, transfer-encoded: what the
	// index knows before anything is fetched.
	Size int64
	// ContentType and Filename are as GetAttachment gives them.
	ContentType string
	Filename    string
}

// GetAttachmentInline returns one part of a message whole, decoded, when it
// is at most maxBytes (1 to MaxInlineAttachment; 0 is the most). A part
// whose size on the server already says it cannot fit is answered TooLarge
// without being fetched, so asking about a large attachment costs the
// mailbox's download quota nothing. Nothing is kept: the spooled section
// goes before this returns, and the bytes are the caller's.
func (s *Service) GetAttachmentInline(ctx context.Context, p Principal, id int64, path string, maxBytes int64) (InlineAttachment, error) {
	switch {
	case maxBytes == 0:
		maxBytes = MaxInlineAttachment
	case maxBytes < 1 || maxBytes > MaxInlineAttachment:
		if err := s.authorize(p, auth.ScopeRead); err != nil {
			return InlineAttachment{}, err
		}
		return InlineAttachment{}, Ef(CodeBadRequest, nil, "max_bytes must be between 1 and %d", MaxInlineAttachment)
	}
	a, row, info, err := s.attachmentPart(ctx, p, id, path)
	if err != nil {
		return InlineAttachment{}, err
	}
	out := InlineAttachment{
		AccountID: a.ID, Size: info.Size, ContentType: mime.AttachmentContentType(info), Filename: mime.DownloadName(info),
	}
	if minDecodedSize(info) > maxBytes {
		out.TooLarge = true
		return out, nil
	}
	d, err := s.fetchAttachment(ctx, p, a, row, info)
	if err != nil {
		return InlineAttachment{}, err
	}
	//nolint:errcheck // closing removes the spooled section; nothing to report
	defer func() { _ = d.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(d.Body, maxBytes+1))
	if err != nil {
		return InlineAttachment{}, E(CodeInternal, "reading the attachment failed", err)
	}
	if int64(len(data)) > maxBytes {
		out.TooLarge = true
		return out, nil
	}
	out.Data = data
	return out, nil
}

// minDecodedSize is the least a part of this encoded size can decode to:
// base64 carries three bytes in four characters, plus line breaks;
// quoted-printable at worst three characters per byte; anything else is
// itself.
func minDecodedSize(info provider.PartInfo) int64 {
	switch strings.ToLower(info.Encoding) {
	case "base64":
		return info.Size * 7 / 10
	case "quoted-printable":
		return info.Size / 3
	}
	return info.Size
}

// attachmentPart finds a part the caller may read, with its message and
// account, and refuses before any connection what cannot be fetched.
func (s *Service) attachmentPart(ctx context.Context, p Principal, id int64, path string) (account.Account, store.MessageRow, provider.PartInfo, error) {
	if _, ok := store.ParsePartPath(path); !ok {
		if err := s.authorize(p, auth.ScopeRead); err != nil {
			return account.Account{}, store.MessageRow{}, provider.PartInfo{}, err
		}
		return account.Account{}, store.MessageRow{}, provider.PartInfo{},
			E(CodeBadRequest, "a part is named by its section, such as 2 or 1.2", nil)
	}
	a, row, err := s.readableMessage(ctx, p, id)
	if err != nil {
		return account.Account{}, store.MessageRow{}, provider.PartInfo{}, err
	}
	parts, err := s.store.MessageParts(ctx, row.ID)
	if err != nil {
		return account.Account{}, store.MessageRow{}, provider.PartInfo{}, E(CodeInternal, "reading the message failed", err)
	}
	var info *provider.PartInfo
	for i := range parts {
		if parts[i].PathString() == path {
			info = &parts[i]
			break
		}
	}
	if info == nil {
		return account.Account{}, store.MessageRow{}, provider.PartInfo{}, E(CodeNotFound, "that message has no such part", nil)
	}
	if err := readyToRead(a); err != nil {
		return account.Account{}, store.MessageRow{}, provider.PartInfo{}, err
	}
	return a, row, *info, nil
}

// fetchAttachment fetches a part attachmentPart found into the spool and
// hands it over, decoding as it is read.
func (s *Service) fetchAttachment(ctx context.Context, p Principal, a account.Account, row store.MessageRow,
	info provider.PartInfo,
) (Download, error) {
	limit := attachmentCap(a)
	tooLarge := Ef(CodeBadRequest, nil, "the attachment is larger than %d MiB, the most Mailie downloads", limit>>20)
	if info.Size > limit {
		return Download{}, tooLarge
	}
	release, err := s.downloads.reserve(downloadCaller(p), info.Size)
	if err != nil {
		return Download{}, err
	}
	handedOver := false
	defer func() {
		if !handedOver {
			release()
		}
	}()

	ctx, cancel := ctxWithDeadline(ctx, downloadTimeout)
	defer cancel()
	var part provider.Part
	err = s.fetchFromServer(ctx, a, row, "downloading an attachment from its server failed",
		func(ctx context.Context, sess provider.Session) error {
			var err error
			part, err = sess.FetchPart(ctx, row.UID, info, limit)
			return err
		})
	if errors.Is(err, provider.ErrTooLarge) {
		return Download{}, tooLarge
	}
	if err != nil {
		return Download{}, err
	}
	decoded, _ := mime.NewTransferDecoder(info.Encoding, part.Body)
	handedOver = true
	return Download{
		Body:        &releasing{ReadCloser: readCloser{Reader: decoded, Closer: part.Body}, release: release},
		Size:        -1,
		ContentType: mime.AttachmentContentType(info),
		Filename:    mime.DownloadName(info),
	}, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// attachmentCap is the largest attachment served, encoded: 50 MiB, or the
// provider's own submission limit where that is larger. Every attachment of
// a message GetRaw serves can be downloaded on its own.
func attachmentCap(a account.Account) int64 {
	return max(provider.ProfileFor(a.Provider).SMTPMaxSize, minAttachmentCap)
}

// readableMessage finds a message the caller may read, and its account.
//
// A message in an account the caller may not see is answered exactly as
// one that does not exist, so ids cannot be probed. A row gone from its
// folder, or waiting for a resync to find its new UID, cannot be fetched as
// indexed and is not found either.
func (s *Service) readableMessage(ctx context.Context, p Principal, id int64) (account.Account, store.MessageRow, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return account.Account{}, store.MessageRow{}, err
	}
	if s.store == nil || id < 1 {
		return account.Account{}, store.MessageRow{}, errNoMessage
	}
	row, err := s.store.Message(ctx, id)
	switch {
	case errors.Is(err, store.ErrNoMessage):
		return account.Account{}, store.MessageRow{}, errNoMessage
	case err != nil:
		return account.Account{}, store.MessageRow{}, E(CodeInternal, "reading the message failed", err)
	}
	if !p.MayAccess(row.AccountID) {
		return account.Account{}, store.MessageRow{}, errNoMessage
	}
	a, err := s.accounts.Repo().GetVisible(ctx, row.AccountID, readable(p))
	switch {
	case errors.Is(err, account.ErrNotFound):
		return account.Account{}, store.MessageRow{}, errNoMessage
	case err != nil:
		return account.Account{}, store.MessageRow{}, E(CodeInternal, "reading the account failed", err)
	}
	switch {
	case row.Vanished:
		return account.Account{}, store.MessageRow{}, errMessageGone
	case row.Stale:
		return account.Account{}, store.MessageRow{}, errMessageBusy
	}
	return a, row, nil
}

// readyToRead refuses, before any connection, an account whose mailbox
// cannot be opened as it stands.
func readyToRead(a account.Account) error {
	switch a.State {
	case account.StateNeedsReauth:
		return needsReauth(fmt.Errorf("%w: %s", provider.ErrNeedsReauth, a.StateReason))
	case account.StatePendingAuth:
		return E(CodeConflict, "this account has not been authorised yet", nil)
	case account.StateDisabled:
		return E(CodeConflict, "this account is disabled", nil)
	}
	return nil
}

// fetchFromServer opens the message's folder read-only on the account's
// interactive connection and runs fn there.
//
// EXAMINE rather than SELECT: in a folder opened read-only no command can
// change a flag, so even a fetch that was not a PEEK could not mark the
// message read. The folder's UIDVALIDITY must still be the one the row was
// indexed under; if it is not, the UID names nothing, or another message.
func (s *Service) fetchFromServer(ctx context.Context, a account.Account, row store.MessageRow, what string,
	fn func(context.Context, provider.Session) error,
) error {
	err := s.onServer(ctx, a.ID, func(ctx context.Context, sess provider.Session) error {
		if _, err := sess.Select(ctx, row.FolderName, true, row.UIDValidity); err != nil {
			return err
		}
		// A caller that left while the folder opened — the console cancels
		// a read whenever a person opens the next message — needs nothing
		// fetched for it.
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(ctx, sess)
	})
	if err == nil {
		return nil
	}
	var known *Error
	switch {
	case errors.As(err, &known):
		return err
	case errors.Is(err, provider.ErrMessageGone), errors.Is(err, provider.ErrFolderNotFound):
		return E(CodeNotFound, errMessageGone.Message, err)
	case errors.Is(err, provider.ErrUIDValidityChanged):
		// The folder was renumbered under the index: the UID names nothing
		// now, but the sync engine's resync gives the message its new one.
		return E(CodeConflict, errMessageBusy.Message, err)
	case errors.Is(err, provider.ErrTooLarge):
		// The caller says which limit.
		return err
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		// A caller that went away is not the mail server's failure.
		s.mailboxFailed(a, what, err)
	}
	return fromMailbox(err)
}

// onServer runs fn on the account's interactive connection: the sync
// engine's, so an account never holds more than its three. Only a daemon
// without the engine (tools, some tests) opens one for the call.
func (s *Service) onServer(ctx context.Context, accountID string, fn func(context.Context, provider.Session) error) error {
	if runner, ok := s.sync.(InteractiveRunner); ok {
		return runner.Interactive(ctx, accountID, fn)
	}
	mailbox, err := s.accounts.Mailbox(ctx, accountID)
	if err != nil {
		return err
	}
	return interactiveOnce(ctx, mailbox, fn)
}

func presentSummary(r store.MessageRow, copies int) MessageSummary {
	return MessageSummary{
		ID: r.ID, AccountID: r.AccountID, FolderID: r.FolderID, FolderRole: string(r.FolderRole),
		FolderName: r.FolderDisplayName, Subject: r.Subject, From: presentAddresses(r.From), To: presentAddresses(r.To),
		Date: r.Date, InternalDate: r.InternalDate, Size: r.Size,
		Seen: r.Seen, Flagged: r.Flagged, Answered: r.Answered, Draft: r.Draft, HasAttachments: r.HasAttachments,
		Copies: copies,
	}
}

func presentMessage(r store.MessageRow, copies int, parts []provider.PartInfo) Message {
	out := Message{
		MessageSummary: presentSummary(r, copies),
		Cc:             presentAddresses(r.Cc), Bcc: presentAddresses(r.Bcc), ReplyTo: presentAddresses(r.ReplyTo),
		MessageID: r.MessageID, InReplyTo: r.InReplyTo, References: r.References,
		Parts: make([]MessagePart, 0, len(parts)),
		Body:  MessageBody{HTMLUnsafe: true},
	}
	if out.References == nil {
		out.References = []string{}
	}
	for _, p := range parts {
		out.Parts = append(out.Parts, MessagePart{
			Path: p.PathString(), MIMEType: p.MIMEType, Filename: mime.Filename(p), Size: p.Size,
			Disposition: p.Disposition, ContentID: p.ContentID, IsAttachment: p.IsAttachment,
		})
	}
	return out
}

func presentAddresses(in []provider.Address) []Address {
	out := make([]Address, 0, len(in))
	for _, a := range in {
		out = append(out, Address{Name: a.Name, Email: a.Email})
	}
	return out
}
