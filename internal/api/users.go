package api

import (
	"net/http"
	"strings"

	"github.com/thehappieco/mailie/internal/service"
)

// The console's routes: signing in and up, the signed-in person's own account,
// and invites; and the operator's, closing a person's account. Every rule
// about who may do what is in internal/service; what lives here is the rate
// limiting, which is about the transport — how often one address may knock —
// rather than about the product.

func (h *Handler) signIn(q *request) {
	var req service.SignInRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	// Both limits, before the hash: the address, and the account being
	// guessed at.
	if !q.allowSignIn(emailSubject(req.Email)) {
		return
	}
	session, err := h.Service.SignIn(q.ctx(), req, q.r.UserAgent())
	if err != nil {
		if service.CodeOf(err) == service.CodeUnauthorized {
			h.metricAuthFailure("password")
		}
		q.fail(err)
		return
	}
	q.write(http.StatusOK, session)
}

func (h *Handler) signUp(q *request) {
	var req service.SignUpRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	if !q.allowSignIn(emailSubject(req.Email)) {
		return
	}
	session, err := h.Service.SignUp(q.ctx(), req, q.r.UserAgent())
	if err != nil {
		q.fail(err)
		return
	}
	q.write(http.StatusCreated, session)
}

func (h *Handler) me(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	me, err := h.Service.Me(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, me)
}

func (h *Handler) signOut(q *request) {
	var req service.SignOutRequest
	// The body is optional: signing out of this browser is the common case.
	if err := q.decodeOptional(&req); err != nil {
		q.fail(err)
		return
	}
	if err := h.Service.SignOut(q.ctx(), q.principal, req); err != nil {
		q.fail(err)
		return
	}
	// Written without the usual re-check: the session this request came in
	// on has just ended, which is what was asked for.
	q.write(http.StatusNoContent, nil)
}

func (h *Handler) changePassword(q *request) {
	var req service.PasswordRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	// Proving the current password is a guess at it like any sign-in, so it
	// gets the same per-account budget. A key never reaches the hash: the
	// service refuses it first.
	if q.principal.IsSession() && !q.allowSignIn("user:"+q.principal.UserID) {
		return
	}
	session, err := h.Service.ChangePassword(q.ctx(), q.principal, req, q.r.UserAgent())
	if err != nil {
		q.fail(err)
		return
	}
	// Every session the person had, this one included, was just ended; the
	// new token in the reply is what proves the caller now.
	q.write(http.StatusOK, session)
}

func (h *Handler) updateProfile(q *request) {
	var req service.ProfileRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	user, err := h.Service.UpdateProfile(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, user)
}

func (h *Handler) createInvite(q *request) {
	var req service.InviteRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	invite, err := h.Service.CreateInvite(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusCreated, invite)
}

func (h *Handler) disableUser(q *request) {
	var req service.CloseUserRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	disabled, err := h.Service.DisableUser(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, disabled)
}

func (h *Handler) deleteUser(q *request) {
	var req service.CloseUserRequest
	if err := q.decode(&req); err != nil {
		q.fail(err)
		return
	}
	deleted, err := h.Service.DeleteUser(q.ctx(), q.principal, req)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, deleted)
}

func (h *Handler) providers(q *request) {
	if _, err := q.query(); err != nil {
		q.fail(err)
		return
	}
	providers, err := h.Service.Providers(q.ctx(), q.principal)
	if err != nil {
		q.fail(err)
		return
	}
	q.finish(http.StatusOK, providers)
}

// emailSubject is the per-account key for the sign-in limit. Folded the way
// the lookup folds, or at least as coarsely: if " Ana@X.com" and "ana@x.com"
// reach the same account, they must also spend from the same bucket, or
// padding an address would buy five more guesses each time.
func emailSubject(email string) string {
	return "email:" + strings.ToLower(strings.TrimSpace(email))
}

// allowSignIn applies the sign-in limits, answering 429 with a Retry-After
// when they bite. The reply is the same whether the address or the subject ran
// out: saying which would tell a caller whether spreading the guesses across
// more addresses is worth it.
func (q *request) allowSignIn(subject string) bool {
	ok, retry := q.h.SignInLimits.Allow(q.r, subject)
	if ok {
		return true
	}
	q.h.metricAuthFailure("rate_limited")
	q.fail(service.Retryable("too many attempts; wait before trying again", retry, nil))
	return false
}
