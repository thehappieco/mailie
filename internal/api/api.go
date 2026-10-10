// Package api serves the REST surface, the event stream and the webhook
// registry.
//
// It is an adapter: it parses transport input, calls one service method and
// renders the result. The routing is the standard library's ServeMux with Go
// 1.22 method-and-path patterns, and each package mounts its own routes, so
// the set of endpoints can be read in one place without a framework's
// indirection in the way.
package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/ratelimit"
	"github.com/thehappieco/mailie/internal/service"
)

// Handler owns the REST routes.
type Handler struct {
	Service *service.Service
	// Limits meters bearer traffic: every request spends from its address,
	// and only failed authentications spend from the tight bucket, so polling
	// with a good credential is never throttled and guessing one is.
	Limits *ratelimit.Auth
	// SignInLimits meters sign-in, sign-up, password changes, recovery and
	// step-up, where every attempt is a guess at a person's secret and costs
	// an Argon2id derivation, and the challenge.
	SignInLimits *ratelimit.Auth
	Metrics      *obs.Metrics
	Log          *slog.Logger
	// AdminAPI exposes the key-management routes. Off by default: keys are
	// issued from the command line, and an endpoint that mints credentials
	// should not answer by accident.
	AdminAPI bool
	// EventPing is how often an idle event stream sends a keep-alive and
	// re-checks its credential. Zero is 15 seconds.
	EventPing time.Duration
	// DownloadStall is how long a download may go without the client taking
	// a byte before it is abandoned, and its spooled file removed. Zero is
	// one minute.
	DownloadStall time.Duration
	// Version is reported by the health probe.
	Version string
	// Started is when the daemon came up.
	Started time.Time
}

// Mount registers the routes on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	// Unauthenticated, and deliberately uninformative: enough for a process
	// supervisor to see the daemon is alive, nothing about which accounts
	// exist or whether any of them is failing.
	mux.Handle("GET /v1/healthz", h.public(opts(), h.healthz))

	// Signing in is the other way in without a credential, which is why it
	// has its own, much tighter limits. The password never comes: the
	// browser derives an auth key under what the challenge answers
	// (docs/key-scheme.md section 12). The upgrade's one password in clear
	// (/v1/auth/upgrade/login and /enrol) left in the release after the one
	// that brought the key scheme: its routes are not found now.
	mux.Handle("POST /v1/auth/challenge", h.public(opts(), h.challenge))
	mux.Handle("POST /v1/auth/login", h.public(opts(), h.signIn))
	mux.Handle("POST /v1/auth/signup/open", h.public(opts(), h.openSignUp))
	mux.Handle("POST /v1/auth/signup", h.public(opts(), h.signUp))
	mux.Handle("POST /v1/auth/reset/open", h.public(opts(), h.openReset))
	mux.Handle("POST /v1/auth/reset", h.public(opts(), h.completeReset))
	mux.Handle("POST /v1/auth/recover/open", h.public(opts(), h.openRecovery))
	mux.Handle("POST /v1/auth/recover/finish", h.public(opts(), h.finishRecovery))
	// The rest of /v1/auth is for a signed-in person. The scope named here
	// admits any credential; the service refuses keys, so the answer says
	// why rather than only that.
	mux.Handle("GET /v1/auth/me", h.authenticated(auth.ScopeRead, opts(), h.me))
	mux.Handle("POST /v1/auth/logout", h.authenticated(auth.ScopeRead, opts(), h.signOut))
	mux.Handle("POST /v1/auth/password/begin", h.authenticated(auth.ScopeRead, opts(), h.beginPasswordChange))
	mux.Handle("POST /v1/auth/password/finish", h.authenticated(auth.ScopeRead, opts(), h.finishPasswordChange))
	mux.Handle("POST /v1/auth/recovery", h.authenticated(auth.ScopeRead, opts(), h.replaceRecovery))
	mux.Handle("POST /v1/auth/stepup", h.authenticated(auth.ScopeRead, opts(), h.stepUp))
	mux.Handle("PUT /v1/auth/profile", h.authenticated(auth.ScopeRead, opts(), h.updateProfile))
	mux.Handle("POST /v1/users/invites", h.authenticated(auth.ScopeAdmin, opts(), h.createInvite))
	// A team invite, accepted by a person who already has an account here.
	mux.Handle("POST /v1/auth/invites/accept", h.authenticated(auth.ScopeRead, opts(), h.acceptInvite))
	// Closing a person's account, for the operator. The address is in the
	// body rather than the path, so no access log along the way records it.
	// Deleting cascades through everything indexed for their mailboxes,
	// which is given a minute.
	mux.Handle("POST /v1/users/disable", h.authenticated(auth.ScopeAdmin, opts(), h.disableUser))
	mux.Handle("POST /v1/users/delete",
		h.authenticated(auth.ScopeAdmin, opts().withTimeout(60*time.Second), h.deleteUser))

	// Workspaces: the ones the caller belongs to, any key included; and,
	// for a person signed in or the operator, administering a team — its
	// name, members and invites — and who holds what on its mailboxes.
	mux.Handle("GET /v1/workspaces", h.authenticated(auth.ScopeRead, opts(), h.listWorkspaces))
	mux.Handle("POST /v1/workspaces", h.authenticated(auth.ScopeAdmin, opts(), h.createWorkspace))
	mux.Handle("PATCH /v1/workspaces/{id}", h.authenticated(auth.ScopeAdmin, opts(), h.renameWorkspace))
	mux.Handle("GET /v1/workspaces/{id}/members", h.authenticated(auth.ScopeRead, opts(), h.listMembers))
	mux.Handle("PATCH /v1/workspaces/{id}/members/{user}", h.authenticated(auth.ScopeAdmin, opts(), h.setMember))
	mux.Handle("DELETE /v1/workspaces/{id}/members/{user}", h.authenticated(auth.ScopeAdmin, opts(), h.removeMember))
	mux.Handle("GET /v1/workspaces/{id}/invites", h.authenticated(auth.ScopeRead, opts(), h.listTeamInvites))
	mux.Handle("POST /v1/workspaces/{id}/invites", h.authenticated(auth.ScopeAdmin, opts(), h.createTeamInvite))
	mux.Handle("DELETE /v1/workspaces/{id}/invites/{invite}", h.authenticated(auth.ScopeAdmin, opts(), h.revokeTeamInvite))
	mux.Handle("GET /v1/workspaces/{id}/access", h.authenticated(auth.ScopeRead, opts(), h.accessDirectory))
	// A workspace's API keys: created, listed and revoked by its owners and
	// admins signed in, who also give and take each key's mailboxes, and
	// list its sends. The scope admits any credential; the service refuses
	// a key, so no key mints, sees or changes another.
	mux.Handle("GET /v1/workspaces/{id}/apikeys", h.authenticated(auth.ScopeRead, opts(), h.listWorkspaceKeys))
	mux.Handle("POST /v1/workspaces/{id}/apikeys", h.authenticated(auth.ScopeRead, opts(), h.createWorkspaceKey))
	mux.Handle("DELETE /v1/workspaces/{id}/apikeys/{prefix}", h.authenticated(auth.ScopeRead, opts(), h.revokeWorkspaceKey))
	mux.Handle("PUT /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account}",
		h.authenticated(auth.ScopeRead, opts(), h.setKeyAccess))
	mux.Handle("DELETE /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account}",
		h.authenticated(auth.ScopeRead, opts(), h.revokeKeyAccess))
	mux.Handle("GET /v1/workspaces/{id}/apikeys/{prefix}/sends", h.authenticated(auth.ScopeRead, opts(), h.listKeySends))
	mux.Handle("PUT /v1/accounts/{id}/access/{user}", h.authenticated(auth.ScopeAdmin, opts(), h.setAccess))
	mux.Handle("DELETE /v1/accounts/{id}/access/{user}", h.authenticated(auth.ScopeAdmin, opts(), h.revokeAccess))
	// A mailbox's key (docs/key-scheme.md sections 8, 9 and 12.11 to 12.15):
	// what a person's console reads to seal and open grants, the first key
	// of a mailbox that has none, a personal mailbox's next one, and the key
	// handed to a member who holds read without it. The scope admits any
	// credential; the service refuses a key, which seals and opens nothing.
	mux.Handle("GET /v1/accounts/{id}/mailbox-key", h.authenticated(auth.ScopeRead, opts(), h.mailboxKey))
	mux.Handle("POST /v1/accounts/{id}/mailbox-key", h.authenticated(auth.ScopeRead, opts(), h.writeFirstKey))
	mux.Handle("PUT /v1/accounts/{id}/mailbox-key", h.authenticated(auth.ScopeRead, opts(), h.writeNewKey))
	mux.Handle("PUT /v1/accounts/{id}/grants/{user}", h.authenticated(auth.ScopeRead, opts(), h.supplyKey))

	mux.Handle("GET /v1/providers", h.authenticated(auth.ScopeRead, opts(), h.providers))
	mux.Handle("GET /v1/accounts", h.authenticated(auth.ScopeRead, opts(), h.listAccounts))
	mux.Handle("GET /v1/accounts/{id}", h.authenticated(auth.ScopeRead, opts(), h.getAccount))
	// Adding a password account logs in to its server first, which is given
	// thirty seconds of its own.
	mux.Handle("POST /v1/accounts", h.authenticated(auth.ScopeAdmin, opts().withTimeout(45*time.Second), h.addAccount))
	// Removing a mailbox repeats its id in ?confirm=, or nothing is removed.
	mux.Handle("DELETE /v1/accounts/{id}", h.authenticated(auth.ScopeAdmin, opts(), h.removeAccount))
	mux.Handle("POST /v1/accounts/{id}/oauth/start", h.authenticated(auth.ScopeAdmin, opts(), h.startOAuth))
	// The exchange behind a callback runs detached for up to forty seconds, so
	// the code survives a caller that gives up; the route outlasts it, so the
	// caller that waits hears how it ended instead of a timeout.
	mux.Handle("POST /v1/accounts/oauth/callback",
		h.authenticated(auth.ScopeAdmin, opts().withTimeout(45*time.Second), h.completeOAuth))
	// Listing folders talks to the mail server, so it gets a longer deadline
	// than a database read.
	mux.Handle("GET /v1/accounts/{id}/folders",
		h.authenticated(auth.ScopeRead, opts().withTimeout(60*time.Second), h.listFolders))

	// Mail: a search of the index, and a message read from its mail server
	// on request, never kept. A search reads the database; a message's body
	// is fetched from the server; an attachment or an original is fetched
	// whole before it is sent, and may be large.
	mux.Handle("GET /v1/messages", h.authenticated(auth.ScopeRead, opts(), h.searchMessages))
	mux.Handle("GET /v1/messages/{id}",
		h.authenticated(auth.ScopeRead, opts().withTimeout(messageTimeout), h.getMessage))
	mux.Handle("GET /v1/messages/{id}/raw",
		h.authenticated(auth.ScopeRead, opts().withTimeout(downloadTimeout), h.getRaw))
	mux.Handle("GET /v1/messages/{id}/attachments/{path}",
		h.authenticated(auth.ScopeRead, opts().withTimeout(downloadTimeout), h.getAttachment))
	// Changing messages on their mail server, when someone who may act on
	// the mailbox asks and has allowed it: the write scope, and the default
	// thirty seconds and 64 KiB, which a hundred ids fit in many times over.
	mux.Handle("PATCH /v1/messages/{id}", h.authenticated(auth.ScopeWrite, opts(), h.patchMessage))
	mux.Handle("POST /v1/messages/flags", h.authenticated(auth.ScopeWrite, opts(), h.setFlags))
	mux.Handle("POST /v1/messages/move", h.authenticated(auth.ScopeWrite, opts(), h.moveMessages))
	mux.Handle("POST /v1/messages/trash", h.authenticated(auth.ScopeWrite, opts(), h.trashMessages))

	// Consent to sync is the signed-in person's own: the scope admits any
	// credential, and the service refuses a key's attempt to give or take
	// it back. Withdrawing deletes everything indexed for the person's
	// mailboxes and compacts what held it, which is given two minutes.
	mux.Handle("GET /v1/me/sync-consent", h.authenticated(auth.ScopeRead, opts(), h.syncConsent))
	mux.Handle("POST /v1/me/sync-consent", h.authenticated(auth.ScopeRead, opts(), h.grantSyncConsent))
	mux.Handle("DELETE /v1/me/sync-consent",
		h.authenticated(auth.ScopeRead, opts().withTimeout(150*time.Second), h.withdrawSyncConsent))
	// Consent to actions, like consent to sync: the person's own, given and
	// taken back only by them signed in. Withdrawing deletes nothing.
	mux.Handle("GET /v1/me/actions-consent", h.authenticated(auth.ScopeRead, opts(), h.actionsConsent))
	mux.Handle("POST /v1/me/actions-consent", h.authenticated(auth.ScopeRead, opts(), h.grantActionsConsent))
	mux.Handle("DELETE /v1/me/actions-consent", h.authenticated(auth.ScopeRead, opts(), h.withdrawActionsConsent))
	// Consent to sending, like the other two: the person's own, given and
	// taken back only by them signed in. Withdrawing deletes nothing.
	mux.Handle("GET /v1/me/send-consent", h.authenticated(auth.ScopeRead, opts(), h.sendConsent))
	mux.Handle("POST /v1/me/send-consent", h.authenticated(auth.ScopeRead, opts(), h.grantSendConsent))
	mux.Handle("DELETE /v1/me/send-consent", h.authenticated(auth.ScopeRead, opts(), h.withdrawSendConsent))
	// Sending: the send scope, a body as large as the largest message a
	// provider accepts with its attachments, and three minutes, which the
	// in-request retries of a server that says "later" fit in. The status
	// of a send is a database read.
	mux.Handle("POST /v1/messages/send", h.authenticated(auth.ScopeSend,
		opts().withTimeout(service.SendTimeout).withMaxBody(service.SendBodyLimit()), h.sendMessage))
	mux.Handle("GET /v1/sends/{key}", h.authenticated(auth.ScopeSend, opts(), h.sendStatus))
	// The API keys the person signed in created, in every workspace: listed
	// and revoked by them. A key is created in a workspace (above): POST
	// here answers 400 and says so. The scope admits any credential; the
	// service refuses a key.
	mux.Handle("GET /v1/me/apikeys", h.authenticated(auth.ScopeRead, opts(), h.listMyAPIKeys))
	mux.Handle("POST /v1/me/apikeys", h.authenticated(auth.ScopeRead, opts(), h.createMyAPIKey))
	mux.Handle("DELETE /v1/me/apikeys/{prefix}", h.authenticated(auth.ScopeRead, opts(), h.revokeMyAPIKey))
	// What the caller's mailboxes take up in the index, a database read;
	// the size of the whole database only for an owner signed in.
	mux.Handle("GET /v1/me/storage", h.authenticated(auth.ScopeRead, opts(), h.storage))
	// Whether this server answers MCP over HTTP at /mcp, for a console that
	// shows how to connect a tool with a key: it never shows an address that
	// answers 404.
	mux.Handle("GET /v1/me/mcp", h.authenticated(auth.ScopeRead, opts(), h.mcpAccess))
	mux.Handle("GET /v1/accounts/{id}/sync", h.authenticated(auth.ScopeRead, opts(), h.syncStatus))
	mux.Handle("POST /v1/accounts/{id}/sync", h.authenticated(auth.ScopeWrite, opts(), h.triggerSync))
	// A mailbox's own consent to sync: a team mailbox's, which its owners
	// and admins give or withdraw on the team's behalf, and an operator
	// mailbox's switch. Switching off deletes its index, as withdrawing
	// consent does.
	mux.Handle("PUT /v1/accounts/{id}/sync",
		h.authenticated(auth.ScopeAdmin, opts().withTimeout(150*time.Second), h.setMailboxSync))

	// The event stream has no route timeout: it lasts as long as the client
	// stays. The long poll's is the longest wait it accepts, plus room to
	// answer.
	mux.Handle("GET /v1/events", h.authenticated(auth.ScopeRead, opts().withTimeout(0), h.streamEvents))
	mux.Handle("GET /v1/events/wait",
		h.authenticated(auth.ScopeRead, opts().withTimeout(service.MaxWait+5*time.Second), h.waitForEvents))

	if h.AdminAPI {
		mux.Handle("GET /v1/apikeys", h.authenticated(auth.ScopeAdmin, opts(), h.listAPIKeys))
		mux.Handle("POST /v1/apikeys", h.authenticated(auth.ScopeAdmin, opts(), h.createAPIKey))
		mux.Handle("DELETE /v1/apikeys/{prefix}", h.authenticated(auth.ScopeAdmin, opts(), h.revokeAPIKey))
	}

	// MCP clients probe these before falling back to the header they were
	// configured with. A 404 keeps them on the static bearer token; anything
	// else starts an OAuth discovery they cannot complete.
	mux.Handle("GET /.well-known/oauth-protected-resource", http.NotFoundHandler())
	mux.Handle("GET /.well-known/oauth-authorization-server", http.NotFoundHandler())
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Uptime  int64  `json:"uptime_seconds"`
}

func (h *Handler) healthz(q *request) {
	uptime := int64(0)
	if !h.Started.IsZero() {
		uptime = int64(time.Since(h.Started).Seconds())
	}
	q.write(http.StatusOK, healthResponse{Status: "ok", Version: h.Version, Uptime: uptime})
}

func (h *Handler) listAccounts(q *request) {
	params, err := q.query("workspace")
	if err != nil {
		q.fail(err)
		return
	}
	accounts, err := h.Service.ListAccounts(q.ctx(), q.principal, params["workspace"])
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, accounts)
}

func (h *Handler) getAccount(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	a, err := h.Service.GetAccount(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, a)
}

func (h *Handler) addAccount(q *request) {
	var req service.AddAccountRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	result, err := h.Service.AddAccount(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, result)
}

func (h *Handler) removeAccount(q *request) {
	params, err := q.query("confirm")
	if err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RemoveAccount(q.ctx(), q.principal, q.r.PathValue("id"),
		service.RemoveAccountRequest{Confirm: params["confirm"]}); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

type startOAuthRequest struct {
	Flow string `json:"flow,omitempty"`
}

func (h *Handler) startOAuth(q *request) {
	var req startOAuthRequest
	// The body is optional: the default flow is the common one.
	if err := q.decodeOptional(&req); err != nil {
		q.fail(err)
		return
	}
	flow, err := h.Service.StartOAuth(q.ctx(), q.principal, q.r.PathValue("id"), req.Flow)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, flow)
}

type completeOAuthRequest struct {
	// RedirectURL is the whole address the browser ended on. Taking the URL
	// rather than the code is one paste instead of three, and it carries the
	// state that ties it back to the flow.
	RedirectURL string `json:"redirect_url"`
}

func (h *Handler) completeOAuth(q *request) {
	var req completeOAuthRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	a, err := h.Service.CompleteOAuth(q.ctx(), q.principal, req.RedirectURL)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, a)
}

func (h *Handler) listFolders(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	folders, err := h.Service.ListFolders(q.ctx(), q.principal, q.r.PathValue("id"))
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, folders)
}

func (h *Handler) listAPIKeys(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	keys, err := h.Service.ListAPIKeys(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, keys)
}

func (h *Handler) createAPIKey(q *request) {
	var req service.CreateAPIKeyRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	created, err := h.Service.CreateAPIKey(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, created)
}

func (h *Handler) revokeAPIKey(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.RevokeAPIKey(q.ctx(), q.principal, q.r.PathValue("prefix")); err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusNoContent, nil)
}

func (h *Handler) observe(route, method string, status int, took time.Duration) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	h.Metrics.HTTPDuration.WithLabelValues(route).Observe(took.Seconds())
}

func (h *Handler) metricAuthFailure(reason string) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.AuthFailures.WithLabelValues(reason).Inc()
}
