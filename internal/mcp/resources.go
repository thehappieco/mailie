package mcp

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"

	"github.com/thehappieco/mailie/internal/service"
)

// Resources: the mailboxes, a folder's newest messages and one message, as
// JSON, read through the same service methods as the tools. A subscription
// to an inbox (mail://{account}/folder/inbox) is told when new mail arrives
// there. Few clients act on that today; wait_for_new_mail is what reliably
// delivers new mail to a model.

const (
	accountsURI      = "mail://accounts"
	messageTemplate  = "mail://{account}/message/{id}"
	folderTemplate   = "mail://{account}/folder/{folder}"
	folderPageSize   = 50
	resourceMIMEType = "application/json"
)

var (
	messageURITemplate = uritemplate.MustNew(messageTemplate)
	folderURITemplate  = uritemplate.MustNew(folderTemplate)
)

func (ss *session) addResources() {
	ss.server.AddResource(&sdk.Resource{
		URI: accountsURI, Name: "accounts", Title: "Mailboxes", MIMEType: resourceMIMEType,
		Description: "The mailboxes this key can reach, as list_accounts gives them.",
	}, ss.resource("accounts", ss.readAccounts))
	// The message template first, as the plan has it; the two cannot match
	// the same URI anyway — a template variable never spans a slash.
	ss.server.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: messageTemplate, Name: "message", Title: "A message", MIMEType: resourceMIMEType,
		Description: "One message with its text body, fetched from its mail server when read and not stored; " +
			"reading it never marks it read. id is Mailie's message id.",
	}, ss.resource("message", ss.readMessage))
	ss.server.AddResourceTemplate(&sdk.ResourceTemplate{
		URITemplate: folderTemplate, Name: "folder", Title: "A folder's newest messages", MIMEType: resourceMIMEType,
		Description: "The newest messages of a folder of a synced mailbox, from the index. folder is a folder id " +
			"from list_folders or a role such as inbox, sent or trash. Subscribe to mail://{account}/folder/inbox " +
			"to be told when new mail arrives there.",
	}, ss.resource("folder", ss.readFolder))
}

// resource wraps a read like addTool wraps a tool: it counts among the key's
// calls, the key is checked before and after, and the log says what was read,
// never what it held.
func (ss *session) resource(name string, read func(context.Context, service.Principal, string, *[]any) (any, error)) sdk.ResourceHandler {
	return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		started := time.Now()
		p := ss.caller(req.Extra)
		uri := req.Params.URI
		var ids []any
		err := ss.running.start(p.KeyPrefix)
		if err == nil {
			defer ss.running.done(p.KeyPrefix)
			err = ss.svc.Recheck(ctx, p)
		}
		var value any
		if err == nil {
			value, err = read(ctx, p, uri, &ids)
		}
		if err == nil {
			err = ss.svc.Recheck(ctx, p)
		}
		ss.logCall(ctx, "resource", name, p, started, err, ids)
		if err != nil {
			if service.CodeOf(err) == service.CodeNotFound {
				return nil, sdk.ResourceNotFoundError(uri)
			}
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: clientError(err).Error()}
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal: internal error"}
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{
			{URI: uri, MIMEType: resourceMIMEType, Text: string(body)},
		}}, nil
	}
}

func (ss *session) readAccounts(ctx context.Context, p service.Principal, _ string, _ *[]any) (any, error) {
	accounts, err := ss.svc.ListAccounts(ctx, p, "")
	if err != nil {
		return nil, err
	}
	return accountsResult{Accounts: accounts}, nil
}

func (ss *session) readMessage(ctx context.Context, p service.Principal, uri string, ids *[]any) (any, error) {
	vars := messageURITemplate.Match(uri)
	account := vars.Get("account").String()
	id, err := strconv.ParseInt(vars.Get("id").String(), 10, 64)
	if account == "" || err != nil || id < 1 {
		return nil, errNoResource
	}
	*ids = idAttrs(account, id)
	m, err := ss.svc.GetMessage(ctx, p, service.GetMessageRequest{ID: id, Format: service.FormatText,
		MaxBytes: defaultBodyBytes})
	if err != nil {
		return nil, err
	}
	if m.AccountID != account {
		// The id exists, in another of the key's mailboxes: this URI names
		// nothing.
		return nil, errNoResource
	}
	return m, nil
}

// folderResource is a folder's newest messages.
type folderResource struct {
	AccountID  string                   `json:"account_id"`
	FolderID   int64                    `json:"folder_id"`
	Role       string                   `json:"role,omitempty"`
	Name       string                   `json:"name"`
	Messages   []service.MessageSummary `json:"messages"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

func (ss *session) readFolder(ctx context.Context, p service.Principal, uri string, ids *[]any) (any, error) {
	vars := folderURITemplate.Match(uri)
	account, name := vars.Get("account").String(), vars.Get("folder").String()
	if account == "" || name == "" {
		return nil, errNoResource
	}
	*ids = idAttrs(account)
	folders, err := ss.svc.ListFolders(ctx, p, account)
	if err != nil {
		return nil, err
	}
	// A folder id, or a role standing for the folder that has it. Only the
	// index's listing has ids: a mailbox that is not synced has no folder to
	// read here.
	id, err := strconv.ParseInt(name, 10, 64)
	byID := err == nil
	var folder *service.Folder
	for i, f := range folders {
		if f.ID != 0 && ((byID && f.ID == id) || (!byID && f.Role == name)) {
			folder = &folders[i]
			break
		}
	}
	if folder == nil {
		return nil, errNoResource
	}
	*ids = append(*ids, "folder", folder.ID)
	page, err := ss.svc.SearchMessages(ctx, p, service.SearchRequest{AccountID: account, FolderID: folder.ID,
		Limit: folderPageSize})
	if err != nil {
		return nil, err
	}
	return folderResource{AccountID: account, FolderID: folder.ID, Role: folder.Role, Name: folder.DisplayName,
		Messages: page.Messages, NextCursor: page.NextCursor}, nil
}

var errNoResource = service.E(service.CodeNotFound, "no such resource", nil)

// subscribe accepts a subscription to an inbox the key may read
// (service.MayFollow). Every other resource is refused: nothing would ever be
// said about it.
func (ss *session) subscribe(ctx context.Context, req *sdk.SubscribeRequest) error {
	started := time.Now()
	p := ss.caller(req.Extra)
	account, ok := inboxOf(req.Params.URI)
	if !ok {
		err := service.E(service.CodeBadRequest,
			"only an inbox can be subscribed to: mail://{account}/folder/inbox", nil)
		ss.logCall(ctx, "subscription", "subscribe", p, started, err, nil)
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: clientError(err).Error()}
	}
	err := ss.svc.Recheck(ctx, p)
	if err == nil {
		err = ss.svc.MayFollow(ctx, p, account)
	}
	ss.logCall(ctx, "subscription", "subscribe", p, started, err, idAttrs(account))
	if err != nil {
		if service.CodeOf(err) == service.CodeNotFound {
			return sdk.ResourceNotFoundError(req.Params.URI)
		}
		return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: clientError(err).Error()}
	}
	ss.watch.add(ctx, req.Session, account)
	return nil
}

func (ss *session) unsubscribe(_ context.Context, req *sdk.UnsubscribeRequest) error {
	if account, ok := inboxOf(req.Params.URI); ok {
		ss.watch.remove(account)
	}
	return nil
}

// inboxOf reads the account out of mail://{account}/folder/inbox.
func inboxOf(uri string) (string, bool) {
	vars := folderURITemplate.Match(uri)
	if vars.Get("folder").String() != inboxRole {
		return "", false
	}
	account := vars.Get("account").String()
	return account, account != "" && inboxURI(account) == uri
}
