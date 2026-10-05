// Package mcp serves the Model Context Protocol: the tools and resources an
// AI assistant uses to search and read a person's mail and, with a write key,
// to act on it.
//
// It is an adapter over internal/service, exactly as the REST API is. Every
// tool parses its arguments, calls one service method with the caller's key
// and renders what comes back; who may see which mailbox, what a key's scope
// allows, whether the owner allowed actions, and what reaches the mail server
// are all decided there. Nothing here keeps a message: bodies and attachments
// come from the mail server when asked, through the service, and leave in the
// answer. The one thing held is the HTTP transport's event store, bounded in
// bytes and time (eventstore.go).
//
// The log records which tool ran, for which key, how it ended and the ids it
// touched — never an argument or a result: no search text, no subject, no
// address, no body.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/service"
)

// Server builds the MCP server each session talks to.
//
// One sdk.Server per session rather than one for the daemon, because a
// session belongs to the key that opened it: the resource notifications it
// receives are computed for that key alone, and stop with it.
type Server struct {
	svc     *service.Service
	log     *slog.Logger
	version string
	// tools are the tool definitions and schemas is the cache the SDK
	// resolves their schemas into, both shared by every session's server:
	// the schemas are resolved once per process, and a session that closes
	// leaves nothing of its own in the cache.
	tools   map[string]*sdk.Tool
	schemas *sdk.SchemaCache
	// running bounds the calls one key runs at once, over every session and
	// transport it uses.
	running *callLimit
	// waitStep is how long wait_for_new_mail waits between two progress
	// notifications; progressEvery outside tests.
	waitStep time.Duration
	// betweenSteps, when set, runs between two steps of a wait: a test's
	// way into the moment between two subscriptions to the journal.
	betweenSteps func()
}

// New builds the server.
func New(svc *service.Service, log *slog.Logger, version string) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		svc: svc, log: log, version: version, tools: toolDefinitions(), schemas: sdk.NewSchemaCache(),
		running: newCallLimit(maxCallsInFlight), waitStep: progressEvery,
	}
}

// maxCallsInFlight is how many tool calls and resource reads one key may
// have running at once. More than an assistant runs in parallel; few enough
// that one key cannot pin a goroutine and a journal subscription per request
// by the thousand, which a wait held open for minutes would otherwise allow.
const maxCallsInFlight = 16

// callLimit counts the calls each key has running.
type callLimit struct {
	max     int
	mu      sync.Mutex
	running map[string]int
}

func newCallLimit(maxCalls int) *callLimit {
	return &callLimit{max: maxCalls, running: map[string]int{}}
}

var errTooManyCalls = service.Retryable("this key already has as many calls running as it may; wait for one to finish",
	time.Second, nil)

// start admits one more call for key, or refuses it; an admitted call is
// ended with done.
func (l *callLimit) start(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[key] >= l.max {
		return errTooManyCalls
	}
	l.running[key]++
	return nil
}

func (l *callLimit) done(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[key]--; l.running[key] <= 0 {
		delete(l.running, key)
	}
}

func (l *callLimit) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running[key]
}

// instructions is what a client hands its model about this server.
const instructions = `Mailie reads the mailboxes this key was created for, and with a write key acts on them.

Start with list_accounts. search_messages searches the index of synced mailboxes by words in the subject, sender and recipients (never the body); get_message fetches a message's text from its mail server when asked, and get_attachment an attachment. Reading never marks a message as read.

To watch for new mail, call wait_for_new_mail in a loop, passing the next_cursor of each answer as since_cursor.

mark_read, flag_message, move_message and trash_message change the real mailbox, which the person sees in every mail app; they need a write key and the owner's permission for actions in the console. Confirm with the person before moving or trashing. Trash is reversible: the messages stay in the provider's trash and move_message brings them back. Mailie never deletes permanently.

Message ids are Mailie's numbers, not the Message-ID header; folder ids come from list_folders.`

// session is one client's session: its server, and the key that opened it.
type session struct {
	*Server
	p      service.Principal
	server *sdk.Server
	watch  *watcher
}

// NewSession builds the MCP server for a session opened with p, a principal
// from service.AuthenticateTool.
func (s *Server) NewSession(p service.Principal) *sdk.Server {
	ss := &session{Server: s, p: p}
	ss.watch = newWatcher(ss)
	ss.server = sdk.NewServer(&sdk.Implementation{Name: "mailie", Title: "Mailie", Version: s.version}, &sdk.ServerOptions{
		Instructions: instructions,
		// Only what the service decides is logged, by this package: the SDK's
		// own lines would carry resource URIs and nothing an operator needs.
		Logger:             nil,
		SchemaCache:        s.schemas,
		SubscribeHandler:   ss.subscribe,
		UnsubscribeHandler: ss.unsubscribe,
		// No logging capability: nothing is sent to the client as a log.
		Capabilities: &sdk.ServerCapabilities{},
		// Every answer is one key's mail, or the list of what that key
		// reaches: never for an intermediary to keep and hand to anybody
		// else, which is what the protocol's default, "public", allows.
		SetCacheable: func(_ context.Context, _ sdk.Request, c *sdk.Cacheable) {
			c.CacheScope = "private"
			c.TTLMs = 0
		},
	})
	ss.addTools()
	ss.addResources()
	return ss.server
}

// RunStdio serves one session over stdin and stdout, for a client that
// launches the daemon itself, until the client hangs up or ctx ends.
func (s *Server) RunStdio(ctx context.Context, p service.Principal) error {
	err := s.NewSession(p).Run(ctx, &sdk.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// caller is the principal a request acts as: the key the HTTP transport just
// authenticated, or, on stdio, the key the session was opened with. Over
// HTTP the two are always the same key — the transport binds a session to
// the key that opened it — and the fresh one carries any scope change.
func (ss *session) caller(extra *sdk.RequestExtra) service.Principal {
	if extra != nil && extra.TokenInfo != nil {
		if p, ok := extra.TokenInfo.Extra[principalExtra].(service.Principal); ok {
			return p
		}
	}
	return ss.p
}

// principalExtra is where the HTTP transport leaves the principal in the
// token info the SDK hands to every request.
const principalExtra = "mailie.principal"

// clientError renders a service error for the client: the code and the
// service's own message, never the cause — which may quote the mail server —
// and how long to wait when there is a wait.
func clientError(err error) error {
	msg := string(service.CodeOf(err)) + ": " + service.MessageOf(err)
	if retry := service.RetryAfterOf(err); retry > 0 {
		msg += fmt.Sprintf(" (try again in %d seconds)", int(retry.Round(time.Second).Seconds()))
	}
	return errors.New(msg)
}

// logCall records one tool call or resource read: its name, the key, how it
// ended, how long it took, and the ids it touched. Never an argument or a
// result.
func (ss *session) logCall(ctx context.Context, kind, name string, p service.Principal, started time.Time, err error, ids []any) {
	attrs := append([]any{kind, name, "key", p.KeyPrefix, "ms", time.Since(started).Milliseconds()}, ids...)
	switch code := service.CodeOf(err); {
	case err == nil:
		ss.log.InfoContext(ctx, "mcp "+kind, append(attrs, "outcome", "ok")...)
	case errors.Is(ctx.Err(), context.Canceled):
		ss.log.DebugContext(ctx, "mcp "+kind+" abandoned by the client", append(attrs, "outcome", code, "err", err)...)
	case code == service.CodeInternal:
		ss.log.ErrorContext(ctx, "mcp "+kind+" failed", append(attrs, "outcome", code, "err", err)...)
	default:
		// Without the cause: a refusal's cause can quote what was asked.
		ss.log.InfoContext(ctx, "mcp "+kind+" refused", append(attrs, "outcome", code)...)
	}
}

// idAttrs are the ids a log line may carry. An account is named only when it
// has the shape of an account id: what a client passes as one, or writes into
// a resource's URI, is otherwise its own text, which the log never holds.
func idAttrs(account string, ids ...int64) []any {
	var out []any
	if accountIDShape.MatchString(account) {
		out = append(out, "account", account)
	}
	switch len(ids) {
	case 0:
	case 1:
		out = append(out, "message", ids[0])
	default:
		out = append(out, "messages", ids)
	}
	return out
}

// accountIDPattern is the shape of an account id, as internal/account makes
// them: acc_ and 16 hex digits.
const (
	accountIDPattern = `^acc_[0-9a-f]{16}$`
	accountIDLen     = len("acc_") + 16
)

var accountIDShape = regexp.MustCompile(accountIDPattern)

// watcher turns new mail into resource notifications for the inboxes this
// session subscribed to. It waits on the event journal with the session's
// own key, so it hears only of the mailboxes that key may read, drops an
// inbox the key can no longer read, and stops when the key stops working,
// when nothing is subscribed, or when the session ends.
type watcher struct {
	ss *session
	// start runs a watch until its context ends: run, outside tests.
	start func(context.Context)

	mu       sync.Mutex
	accounts map[string]bool
	cancel   context.CancelFunc
	watching sync.Once
}

func newWatcher(ss *session) *watcher {
	w := &watcher{ss: ss, accounts: map[string]bool{}}
	w.start = w.run
	return w
}

// watchWait is how long each wait on the journal lasts; the key is checked
// again between two.
const watchWait = 50 * time.Second

// add watches one more inbox. The watch outlives the subscribe request that
// starts it, so it keeps that request's values and not its cancellation.
func (w *watcher) add(ctx context.Context, sess *sdk.ServerSession, accountID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.accounts[accountID] = true
	if w.cancel == nil {
		ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		w.cancel = cancel
		go w.start(ctx)
	}
	if sess != nil {
		w.watching.Do(func() {
			go func() {
				// Wait returns whatever ended the connection; either way
				// there is nobody left to notify.
				//nolint:errcheck // the session's end is the only news here
				_ = sess.Wait()
				w.stop()
			}()
		})
	}
}

// remove stops watching one inbox, and stops the watch when it was the last.
// Deciding and stopping happen under one lock: between the two, an add for
// another inbox would find the watch still running, start none, and be left
// with none once it stopped.
func (w *watcher) remove(accountID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.accounts, accountID)
	if len(w.accounts) == 0 {
		w.stopLocked()
	}
}

func (w *watcher) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

// stopLocked cancels the watch. Requires w.mu.
func (w *watcher) stopLocked() {
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

func (w *watcher) subscribed() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]bool, len(w.accounts))
	for id := range w.accounts {
		out[id] = true
	}
	return out
}

func (w *watcher) run(ctx context.Context) {
	svc, p := w.ss.svc, w.ss.p
	// keyWorks checks the key, and stops the watch when it no longer works.
	keyWorks := func() bool {
		err := svc.Recheck(ctx, p)
		if err == nil {
			return true
		}
		if ctx.Err() == nil {
			msg := "mcp subscriptions stopped: the key no longer works"
			if service.CodeOf(err) == service.CodeConflict {
				// The key works, without a mailbox the session was
				// opened with: a new session subscribes again.
				msg = "mcp subscriptions stopped: the key no longer reaches a mailbox the session was opened with"
			}
			w.ss.log.Info(msg, "key", p.KeyPrefix, "outcome", service.CodeOf(err))
		}
		w.stop()
		return false
	}
	var cursor int64
	for ctx.Err() == nil {
		if !keyWorks() {
			return
		}
		res, err := svc.WaitForNewMail(ctx, p, cursor, watchWait, service.EventFilter{})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.ss.log.Warn("mcp subscriptions: waiting for new mail failed; trying again", "key", p.KeyPrefix,
				"outcome", service.CodeOf(err), "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		// The wait may have lasted most of a minute: a key revoked during it
		// hears nothing of what arrived.
		if !keyWorks() {
			return
		}
		cursor = res.NextCursor
		subscribed := w.followed(ctx, svc, p)
		changed := map[string]bool{}
		for _, ev := range res.Events {
			if m, ok := ev.NewMail(); ok && m.FolderRole == inboxRole && subscribed[m.AccountID] {
				changed[m.AccountID] = true
			}
		}
		if res.Lagged {
			// Mail may have arrived that no cursor brings back: every
			// subscribed inbox may have changed.
			changed = subscribed
		}
		for accountID := range changed {
			if ctx.Err() != nil {
				return
			}
			//nolint:errcheck // a notification nobody can receive is not a failure of the watch
			_ = w.ss.server.ResourceUpdated(ctx, &sdk.ResourceUpdatedNotificationParams{URI: inboxURI(accountID)})
		}
	}
}

// followed is the subscribed inboxes the key may still read, asked of the
// service after every wait: access may have changed during it, and a lagged
// wait marks every inbox followed as changed without the event gate's say.
// An inbox the key can no longer read is dropped from the watch — subscribing
// to it again is refused as any other would be — and the watch stops when
// none is left. One the service could not decide on stays subscribed but is
// not announced this time.
func (w *watcher) followed(ctx context.Context, svc *service.Service, p service.Principal) map[string]bool {
	out := map[string]bool{}
	for accountID := range w.subscribed() {
		err := svc.MayFollow(ctx, p, accountID)
		switch code := service.CodeOf(err); {
		case err == nil:
			out[accountID] = true
		case code == service.CodeNotFound || code == service.CodeNotAuthorized:
			if ctx.Err() != nil {
				return out
			}
			w.ss.log.Info("mcp subscription dropped: the key can no longer read the inbox",
				append([]any{"key", p.KeyPrefix, "outcome", code}, idAttrs(accountID)...)...)
			w.remove(accountID)
		default:
			if ctx.Err() == nil {
				w.ss.log.Warn("mcp subscriptions: checking access to a subscribed inbox failed",
					append([]any{"key", p.KeyPrefix, "err", err}, idAttrs(accountID)...)...)
			}
		}
	}
	return out
}

// inboxRole is the folder role subscriptions are about.
const inboxRole = "inbox"

func inboxURI(accountID string) string { return "mail://" + accountID + "/folder/" + inboxRole }
