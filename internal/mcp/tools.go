package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/service"
)

// The tools. Each one calls a single service method with the caller's key:
// the service authorises, the tool only translates. Read tools never change
// anything, not even \Seen; write tools change the real mailbox and are
// annotated so a client can tell which is which, since annotations are per
// tool — which is why the trash is a tool of its own.

// Limits the tools apply on top of the service's.
const (
	// defaultSearchLimit is a page of search results for a model: fewer
	// than the REST default, since each one lands in a context window.
	defaultSearchLimit = 20
	// defaultBodyBytes is how much of a body get_message returns unless
	// asked for more.
	defaultBodyBytes = 64 << 10
	// defaultWait and maxWait bound wait_for_new_mail. The most is under the
	// four minutes claude.ai and Claude Desktop give a tool call.
	defaultWait = 45 * time.Second
	maxWait     = 230 * time.Second
	// progressEvery is how often a long wait tells a client that asked for
	// progress that it is still waiting.
	progressEvery = 25 * time.Second
	// requiresUserInteraction is the _meta key Claude Code reads to ask the
	// person before calling a tool.
	requiresUserInteraction = "anthropic/requiresUserInteraction"
)

// call is one tool call in flight.
type call struct {
	p   service.Principal
	req *sdk.CallToolRequest
	// content is the result for clients that read content rather than
	// structuredContent: a text rendering, and for an attachment the file.
	content []sdk.Content
	// ids are what the log may say about the call.
	ids []any
}

func (c *call) text(s string) { c.content = append(c.content, &sdk.TextContent{Text: s}) }

// addTool registers one of the process's tools, by name, with a handler that
// runs between two checks of the key: one before anything is read, and one
// before the answer leaves, so a key revoked while a call ran does not see
// its result. A key already running as many calls as it may is refused
// before either.
func addTool[In, Out any](ss *session, name string, fn func(context.Context, *call, In) (Out, error)) {
	t, ok := ss.tools[name]
	if !ok {
		panic("mcp: no tool definition for " + name)
	}
	sdk.AddTool(ss.server, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		var zero Out
		started := time.Now()
		c := &call{p: ss.caller(req.Extra), req: req}
		err := ss.running.start(c.p.KeyPrefix)
		if err == nil {
			defer ss.running.done(c.p.KeyPrefix)
			err = ss.svc.Recheck(ctx, c.p)
		}
		var out Out
		if err == nil {
			out, err = fn(ctx, c, in)
		}
		if err == nil {
			err = ss.svc.Recheck(ctx, c.p)
		}
		ss.logCall(ctx, "tool", t.Name, c.p, started, err, c.ids)
		if err != nil {
			return nil, zero, clientError(err)
		}
		return &sdk.CallToolResult{Content: c.content}, out, nil
	})
}

func (ss *session) addTools() {
	addTool(ss, "list_accounts", ss.listAccounts)
	addTool(ss, "list_folders", ss.listFolders)
	addTool(ss, "search_messages", ss.searchMessages)
	addTool(ss, "get_message", ss.getMessage)
	addTool(ss, "get_attachment", ss.getAttachment)
	addTool(ss, "wait_for_new_mail", ss.waitForNewMail)
	addTool(ss, "mark_read", ss.markRead)
	addTool(ss, "flag_message", ss.flagMessage)
	addTool(ss, "move_message", ss.moveMessage)
	addTool(ss, "trash_message", ss.trashMessage)
}

// toolDefinitions are the tools, built once per process: every session
// registers these same values. The SDK's schema cache keys a schema written
// by hand by its pointer, forever; a copy built per session would be resolved
// again for each one and kept after it closed.
func toolDefinitions() map[string]*sdk.Tool {
	actionNote := " Changes the real mailbox, as every mail app will show it. Needs a write key and the owner's " +
		"permission for actions in the console. ids are 1 to 100 messages of one account."
	tools := []*sdk.Tool{{
		Name:  "list_accounts",
		Title: "List mailboxes",
		Description: "Lists the mailboxes this key can reach: id, address, provider, state, whether sync is on " +
			"(only a synced mailbox can be searched) and which actions it offers (archive, trash). " +
			"Other tools take the id as account.",
		InputSchema: object(nil),
		Annotations: readAnnotations(false),
	}, {
		Name:  "list_folders",
		Title: "List folders",
		Description: "Lists a mailbox's folders with their id, role (inbox, sent, drafts, trash, junk, archive, all) " +
			"and message counts. The id is what search_messages takes as folder and move_message as to. A mailbox " +
			"that is not synced yet has its folders listed by its mail server.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"account": accountSchema("The account id, from list_accounts."),
		}, "account"),
		// Open world: a mailbox the index has no folders for is asked on its
		// mail server (service.ListFolders).
		Annotations: readAnnotations(true),
	}, {
		Name:  "search_messages",
		Title: "Search messages",
		Description: "Searches the index of synced mailboxes, newest first. q matches words in the subject, sender " +
			"and recipients, the last word as a prefix; message bodies are not indexed and not searched. " +
			"Without folder, each message is listed once even when it is in several folders (Gmail labels). " +
			"Returns up to limit messages (default 20) and, when there are more, a next_cursor to pass as cursor. " +
			"Searching reads only the index and marks nothing as read.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"account": accountSchema("Only this account's messages; every account when absent."),
			"folder":  {Type: "integer", Minimum: ptr(1.0), Description: "Only this folder, by its id from list_folders."},
			"q": {Type: "string", MaxLength: ptr(256),
				Description: "Words to find in the subject, sender or recipients (at most 16 words)."},
			"from": {Type: "string", MaxLength: ptr(256), Description: "Part of the sender's name or address."},
			"since": {Type: "string",
				Description: "Received at or after: a date (YYYY-MM-DD, UTC), an RFC 3339 time or unix seconds."},
			"until": {Type: "string",
				Description: "Received before: a date (the whole day included), an RFC 3339 time or unix seconds."},
			"unseen":          {Type: "boolean", Description: "true: only unread messages; false: only read ones."},
			"flagged":         {Type: "boolean", Description: "true: only starred messages; false: only unstarred."},
			"has_attachments": {Type: "boolean", Description: "true: only messages with attachments; false: only without."},
			"limit": {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(float64(service.MaxPageSize)),
				Default: json.RawMessage(strconv.Itoa(defaultSearchLimit)), Description: "How many messages at most."},
			"cursor": {Type: "string", Description: "The next_cursor of the previous page, to continue it."},
		}),
		Annotations: readAnnotations(false),
	}, {
		Name:  "get_message",
		Title: "Read a message",
		Description: "Fetches one message from its mail server now: headers, parts (attachments with the part " +
			"get_attachment takes) and the body as text (default), html or both, each up to max_bytes (default " +
			"65536); truncated=true says there is more. Nothing is stored, and the message is not marked as " +
			"read. The id comes from search_messages or wait_for_new_mail.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"id": {Type: "integer", Minimum: ptr(1.0), Description: "The message id."},
			"format": {Type: "string", Enum: []any{service.FormatText, service.FormatHTML, service.FormatBoth},
				Default: json.RawMessage(`"text"`), Description: "Which body to return."},
			"max_bytes": {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(float64(service.MaxBodyBytes)),
				Default: json.RawMessage(strconv.Itoa(defaultBodyBytes)), Description: "The most of each body to return."},
		}, "id"),
		Annotations: readAnnotations(true),
	}, {
		Name:  "get_attachment",
		Title: "Read an attachment",
		Description: "Fetches one attachment from the mail server and returns it embedded, when it is at most " +
			"max_bytes (at most 1 MiB). A larger one is not fetched: the answer says too_large, with its size and " +
			"the REST path that downloads it with the same key. Nothing is stored; the message is not marked read.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"id": {Type: "integer", Minimum: ptr(1.0), Description: "The message id."},
			"part": {Type: "string", Pattern: `^[1-9][0-9]*(\.[1-9][0-9]*)*$`,
				Description: "The part, as get_message lists it: 2, or 1.2."},
			"max_bytes": {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(float64(service.MaxInlineAttachment)),
				Default:     json.RawMessage(strconv.Itoa(service.MaxInlineAttachment)),
				Description: "The largest attachment to return."},
		}, "id", "part"),
		Annotations: readAnnotations(true),
	}, {
		Name:  "wait_for_new_mail",
		Title: "Wait for new mail",
		Description: "Waits until new mail arrives — in an inbox, or in a folder without a role — and returns it " +
			"as soon as it does, or when timeout_seconds pass (default 45, at most 230). Pass the next_cursor of " +
			"the previous answer as since_cursor to miss nothing and see nothing twice; without it, waits for mail " +
			"from now on. timed_out=true means nothing arrived. lagged=true means the cursor is older than the " +
			"server keeps: use search_messages rather than trusting the answer to be complete. Waiting in vain " +
			"is not an error.",
		InputSchema: object(map[string]*jsonschema.Schema{
			"account":      accountSchema("Only this account's mail; every account when absent."),
			"since_cursor": {Type: "integer", Minimum: ptr(0.0), Description: "The next_cursor of the previous answer."},
			"timeout_seconds": {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(maxWait.Seconds()),
				Default: json.RawMessage(strconv.Itoa(int(defaultWait.Seconds()))), Description: "How long to wait."},
		}),
		Annotations: readAnnotations(false),
	}, {
		Name:        "mark_read",
		Title:       "Mark as read or unread",
		Description: "Marks messages as read (read=true, the default) or unread (read=false)." + actionNote,
		InputSchema: object(map[string]*jsonschema.Schema{
			"ids":  idsSchema(),
			"read": {Type: "boolean", Default: json.RawMessage(`true`), Description: "true: read; false: unread."},
		}, "ids"),
		Annotations: writeAnnotations(false, true),
	}, {
		Name:        "flag_message",
		Title:       "Star or unstar",
		Description: "Stars (flagged=true, the default) or unstars (flagged=false) messages." + actionNote,
		InputSchema: object(map[string]*jsonschema.Schema{
			"ids":     idsSchema(),
			"flagged": {Type: "boolean", Default: json.RawMessage(`true`), Description: "true: star; false: unstar."},
		}, "ids"),
		Annotations: writeAnnotations(false, true),
	}, {
		Name:  "move_message",
		Title: "Move or archive",
		Description: "Moves messages to \"archive\", to \"inbox\", or to a folder by its id from list_folders; not " +
			"to the trash, which is trash_message. A moved message keeps its id; ids in removed left the index " +
			"(archived on Gmail, say), and moving those to \"inbox\" within a few minutes undoes it." + actionNote,
		InputSchema: object(map[string]*jsonschema.Schema{
			"ids": idsSchema(),
			"to": {
				Description: `"archive", "inbox", or a folder id from list_folders.`,
				AnyOf: []*jsonschema.Schema{
					{Type: "string", Enum: []any{service.MoveToArchive, service.MoveToInbox}},
					{Type: "string", Pattern: `^[1-9][0-9]*$`},
					{Type: "integer", Minimum: ptr(1.0)},
				},
			},
		}, "ids", "to"),
		Annotations: writeAnnotations(false, true),
	}, {
		Name:  "trash_message",
		Title: "Move to trash",
		Description: "Moves messages to the account's trash folder. Reversible: they stay in the provider's trash " +
			"for as long as the provider keeps it, and move_message brings them back; Mailie never deletes " +
			"permanently. Ask the person before calling it." + actionNote,
		InputSchema: object(map[string]*jsonschema.Schema{"ids": idsSchema()}, "ids"),
		Annotations: writeAnnotations(true, false),
		Meta:        sdk.Meta{requiresUserInteraction: true},
	}}
	out := make(map[string]*sdk.Tool, len(tools))
	for _, t := range tools {
		out[t.Name] = t
	}
	return out
}

// Schemas are written by hand: inference cannot say that an id is positive,
// that a format is one of three words or that a list holds at most a
// hundred ids, and a model reads those limits from the schema.

func object(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	if props == nil {
		props = map[string]*jsonschema.Schema{}
	}
	// Unknown arguments are refused rather than ignored, as REST refuses
	// unknown fields: a misspelt filter must not quietly widen a search.
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}}
}

// accountSchema is an account id argument: its shape, so that free text in
// its place is refused before the tool runs.
func accountSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: accountIDPattern, MaxLength: ptr(accountIDLen),
		Description: description}
}

func idsSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "array", MinItems: ptr(1), MaxItems: ptr(service.MaxActionIDs),
		Items:       &jsonschema.Schema{Type: "integer", Minimum: ptr(1.0)},
		Description: "Message ids, from search_messages or wait_for_new_mail.",
	}
}

func readAnnotations(openWorld bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{
		ReadOnlyHint: true, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(openWorld),
	}
}

// writeAnnotations are the actions': they reach the mail server, an outside
// world, and change it.
func writeAnnotations(destructive, idempotent bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{
		ReadOnlyHint: false, DestructiveHint: ptr(destructive), IdempotentHint: idempotent, OpenWorldHint: ptr(true),
	}
}

func ptr[T any](v T) *T { return &v }

// Arguments and results.

type noArgs struct{}

type accountsResult struct {
	Accounts []service.Account `json:"accounts"`
}

func (ss *session) listAccounts(ctx context.Context, c *call, _ noArgs) (accountsResult, error) {
	accounts, err := ss.svc.ListAccounts(ctx, c.p, "")
	if err != nil {
		return accountsResult{}, err
	}
	c.text(accountsText(accounts))
	return accountsResult{Accounts: accounts}, nil
}

type accountArgs struct {
	Account string `json:"account"`
}

type foldersResult struct {
	AccountID string           `json:"account_id"`
	Folders   []service.Folder `json:"folders"`
}

func (ss *session) listFolders(ctx context.Context, c *call, in accountArgs) (foldersResult, error) {
	c.ids = idAttrs(in.Account)
	folders, err := ss.svc.ListFolders(ctx, c.p, in.Account)
	if err != nil {
		return foldersResult{}, err
	}
	c.text(foldersText(folders))
	return foldersResult{AccountID: in.Account, Folders: folders}, nil
}

type searchArgs struct {
	Account        string `json:"account,omitempty"`
	Folder         int64  `json:"folder,omitempty"`
	Q              string `json:"q,omitempty"`
	From           string `json:"from,omitempty"`
	Since          string `json:"since,omitempty"`
	Until          string `json:"until,omitempty"`
	Unseen         *bool  `json:"unseen,omitempty"`
	Flagged        *bool  `json:"flagged,omitempty"`
	HasAttachments *bool  `json:"has_attachments,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}

func (ss *session) searchMessages(ctx context.Context, c *call, in searchArgs) (service.MessagePage, error) {
	// Which account and folder only: the search itself never reaches the log.
	c.ids = idAttrs(in.Account)
	if in.Folder != 0 {
		c.ids = append(c.ids, "folder", in.Folder)
	}
	page, err := ss.svc.SearchMessages(ctx, c.p, service.SearchRequest{
		AccountID: in.Account, FolderID: in.Folder, Query: in.Q, From: in.From, Since: in.Since, Until: in.Until,
		Unseen: in.Unseen, Flagged: in.Flagged, HasAttachments: in.HasAttachments, Limit: in.Limit, Cursor: in.Cursor,
	})
	if err != nil {
		return service.MessagePage{}, err
	}
	c.ids = append(c.ids, "results", len(page.Messages))
	c.text(pageText(page))
	return page, nil
}

type messageArgs struct {
	ID       int64  `json:"id"`
	Format   string `json:"format,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

func (ss *session) getMessage(ctx context.Context, c *call, in messageArgs) (service.Message, error) {
	c.ids = idAttrs("", in.ID)
	m, err := ss.svc.GetMessage(ctx, c.p, service.GetMessageRequest{ID: in.ID, Format: in.Format, MaxBytes: in.MaxBytes})
	if err != nil {
		return service.Message{}, err
	}
	c.ids = idAttrs(m.AccountID, m.ID)
	c.text(messageText(m))
	return m, nil
}

type attachmentArgs struct {
	ID       int64  `json:"id"`
	Part     string `json:"part"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

// attachmentResult describes the attachment; its bytes, when they fit, are
// the embedded resource beside it.
type attachmentResult struct {
	ID          int64  `json:"id"`
	Part        string `json:"part"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	// Size is the part's size on the mail server, transfer-encoded.
	Size     int64 `json:"size"`
	TooLarge bool  `json:"too_large"`
	// Bytes is the decoded size of what is embedded.
	Bytes int `json:"bytes,omitempty"`
	// DownloadPath is the REST route that downloads a part too large to
	// embed, with the same key.
	DownloadPath string `json:"download_path,omitempty"`
}

func (ss *session) getAttachment(ctx context.Context, c *call, in attachmentArgs) (attachmentResult, error) {
	c.ids = idAttrs("", in.ID)
	a, err := ss.svc.GetAttachmentInline(ctx, c.p, in.ID, in.Part, in.MaxBytes)
	if err != nil {
		return attachmentResult{}, err
	}
	c.ids = append(idAttrs(a.AccountID, in.ID), "part", in.Part, "too_large", a.TooLarge)
	out := attachmentResult{
		ID: in.ID, Part: in.Part, Filename: a.Filename, ContentType: a.ContentType, Size: a.Size, TooLarge: a.TooLarge,
	}
	if a.TooLarge {
		out.DownloadPath = fmt.Sprintf("/v1/messages/%d/attachments/%s", in.ID, in.Part)
		c.text(fmt.Sprintf("%s (%s, %s on the server) is too large to return here. Download it from %s with the "+
			"same key as a bearer token.", a.Filename, a.ContentType, sizeText(a.Size), out.DownloadPath))
		return out, nil
	}
	out.Bytes = len(a.Data)
	c.text(fmt.Sprintf("%s (%s, %s), embedded below.", a.Filename, a.ContentType, sizeText(int64(len(a.Data)))))
	c.content = append(c.content, &sdk.EmbeddedResource{Resource: &sdk.ResourceContents{
		URI:      fmt.Sprintf("mail://%s/message/%d/attachment/%s", a.AccountID, in.ID, in.Part),
		MIMEType: a.ContentType,
		Blob:     a.Data,
	}})
	return out, nil
}

type waitArgs struct {
	Account        string `json:"account,omitempty"`
	SinceCursor    int64  `json:"since_cursor,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type waitResult struct {
	TimedOut bool `json:"timed_out"`
	// Lagged says the cursor was older than the journal keeps.
	Lagged     bool              `json:"lagged"`
	Messages   []service.NewMail `json:"messages"`
	NextCursor int64             `json:"next_cursor"`
}

// waitForNewMail waits in steps of at most progressEvery (waitStep). Between two it
// checks the key again — a revoked one stops waiting within a step — and
// tells a client that asked for progress that it is still waiting, which
// also keeps a connection with an idle timeout open. The cursor carries from
// step to step, so nothing that arrives between two is missed.
func (ss *session) waitForNewMail(ctx context.Context, c *call, in waitArgs) (waitResult, error) {
	c.ids = idAttrs(in.Account)
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = defaultWait
	}
	var accounts []string
	if in.Account != "" {
		accounts = []string{in.Account}
	}
	started := time.Now()
	deadline := started.Add(timeout)
	token := c.req.Params.GetProgressToken()
	result := waitResult{Messages: []service.NewMail{}, NextCursor: in.SinceCursor}
	cursor := in.SinceCursor
	for {
		step := min(time.Until(deadline), ss.waitStep).Round(time.Second)
		if step < time.Second {
			break
		}
		res, err := ss.svc.WaitForNewMail(ctx, c.p, cursor, step, service.EventFilter{AccountIDs: accounts})
		if err != nil {
			return waitResult{}, err
		}
		cursor = res.NextCursor
		result.NextCursor = cursor
		result.Lagged = result.Lagged || res.Lagged
		if !res.TimedOut {
			for _, ev := range res.Events {
				if m, ok := ev.NewMail(); ok {
					result.Messages = append(result.Messages, m)
				}
			}
			break
		}
		if gone(ctx) || time.Until(deadline) < time.Second {
			// A client that left, or cancelled the call, is answered as a
			// wait that timed out, which is true, and costs nothing if
			// nobody reads it.
			break
		}
		if err := ss.svc.Recheck(ctx, c.p); err != nil {
			return waitResult{}, err
		}
		if ss.betweenSteps != nil {
			ss.betweenSteps()
		}
		if token != nil {
			//nolint:errcheck // progress is a courtesy; the wait goes on without it
			_ = c.req.Session.NotifyProgress(ctx, &sdk.ProgressNotificationParams{
				ProgressToken: token, Message: "waiting for new mail",
				Progress: time.Since(started).Round(time.Second).Seconds(), Total: timeout.Seconds(),
			})
		}
	}
	result.TimedOut = len(result.Messages) == 0
	c.ids = append(c.ids, "results", len(result.Messages), "cursor", result.NextCursor)
	c.text(waitText(result))
	return result, nil
}

// gone reports whether the caller stopped waiting for the answer.
func gone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

type markReadArgs struct {
	IDs  []int64 `json:"ids"`
	Read bool    `json:"read"`
}

func (ss *session) markRead(ctx context.Context, c *call, in markReadArgs) (service.ActionResult, error) {
	c.ids = idAttrs("", in.IDs...)
	res, err := ss.svc.SetFlags(ctx, c.p, service.SetFlagsRequest{IDs: in.IDs, Seen: &in.Read})
	if err != nil {
		return service.ActionResult{}, err
	}
	verb := "Marked %s as read."
	if !in.Read {
		verb = "Marked %s as unread."
	}
	c.text(actionText(verb, res))
	return res, nil
}

type flagArgs struct {
	IDs     []int64 `json:"ids"`
	Flagged bool    `json:"flagged"`
}

func (ss *session) flagMessage(ctx context.Context, c *call, in flagArgs) (service.ActionResult, error) {
	c.ids = idAttrs("", in.IDs...)
	res, err := ss.svc.SetFlags(ctx, c.p, service.SetFlagsRequest{IDs: in.IDs, Flagged: &in.Flagged})
	if err != nil {
		return service.ActionResult{}, err
	}
	verb := "Starred %s."
	if !in.Flagged {
		verb = "Unstarred %s."
	}
	c.text(actionText(verb, res))
	return res, nil
}

// destination is move_message's to: a string, or a folder id as a number.
type destination string

func (d *destination) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*d = destination(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("to must be archive, inbox or a folder id")
	}
	*d = destination(strconv.FormatInt(n, 10))
	return nil
}

type moveArgs struct {
	IDs []int64     `json:"ids"`
	To  destination `json:"to"`
}

func (ss *session) moveMessage(ctx context.Context, c *call, in moveArgs) (service.ActionResult, error) {
	c.ids = idAttrs("", in.IDs...)
	res, err := ss.svc.MoveMessages(ctx, c.p, service.MoveRequest{IDs: in.IDs, To: string(in.To)})
	if err != nil {
		return service.ActionResult{}, err
	}
	c.text(actionText("Moved %s.", res))
	return res, nil
}

type trashArgs struct {
	IDs []int64 `json:"ids"`
}

func (ss *session) trashMessage(ctx context.Context, c *call, in trashArgs) (service.ActionResult, error) {
	c.ids = idAttrs("", in.IDs...)
	res, err := ss.svc.TrashMessages(ctx, c.p, service.TrashRequest{IDs: in.IDs})
	if err != nil {
		return service.ActionResult{}, err
	}
	c.text(actionText("Moved %s to the trash; move_message brings them back.", res))
	return res, nil
}
