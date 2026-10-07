package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/workspace"
)

// SyncController is the sync engine as the service sees it.
//
// Defined here and implemented by internal/sync, which internal/app wires in:
// the service decides who may ask for what, the engine decides how mail
// moves, and neither imports the other's internals. A nil controller means
// the daemon runs without the engine (tests, and tools that only open the
// database), and every sync operation answers that sync is unavailable.
type SyncController interface {
	// Trigger asks for a pass of every synced folder of the account soon.
	// It never waits for the pass; an account that is not syncing answers
	// ErrSyncNotRunning.
	Trigger(ctx context.Context, accountID string) error
	// Status reports where the account's sync stands, running or not: what
	// is indexed is known either way, and a person whose mailbox stopped
	// still wants to see how much of it is there. ErrSyncNotRunning is for
	// an account the controller knows nothing about, and shows as "off".
	Status(ctx context.Context, accountID string) (SyncStatus, error)
	// Reconcile tells the engine the account's eligibility may have changed
	// — it was added, authorised, consented to, disabled, withdrawn or
	// removed — so it starts or stops the account's worker without waiting
	// for its periodic check.
	//
	// The service calls it on a request's goroutine once the change has
	// committed, so it must not block, and it need not wait for a worker to
	// stop: the engine re-checks eligibility inside every transaction that
	// writes to the index (store.RequireSyncEligibleTx), so a withdrawal
	// that committed leaves nothing for a late batch to store.
	Reconcile(accountID string)
}

// InteractiveRunner is a sync controller that also owns each account's
// interactive connection — the third of the three an account may hold, the
// one for what a person asks of the server now.
//
// An account's connections are budgeted (Gmail allows fifteen across every
// client the person runs), so when the controller has this, the service asks
// it for the connection instead of opening its own: two requests at once then
// queue for the one connection rather than open a fourth. Without it (tests,
// tools with no engine) the service opens a connection per call, as before.
type InteractiveRunner interface {
	// Interactive runs fn on the account's interactive connection, opening
	// it if needed. It works for any account, syncing or not.
	Interactive(ctx context.Context, accountID string, fn func(context.Context, provider.Session) error) error
}

// SyncStatus is one account's sync, as the console and the API show it.
type SyncStatus struct {
	// Running is whether a worker holds the account now.
	Running bool `json:"running"`
	// State is the account-level sync state: "off" (not eligible), "initial",
	// "live", "backoff" or "stopped".
	State string `json:"state"`
	// Tier is the incremental strategy the server's capabilities chose:
	// "condstore" or "uidpoll". Empty until the first connection.
	Tier string `json:"tier,omitempty"`
	// FoldersSynced and FoldersTotal count the folders that are synced at
	// all; FoldersSynced are those whose initial sync has finished.
	FoldersSynced int `json:"folders_synced"`
	FoldersTotal  int `json:"folders_total"`
	// Messages is how many messages are indexed, label copies included.
	Messages int64 `json:"messages"`
	// InitialProgress is 0..100 while the initial sync runs, 100 after.
	InitialProgress int `json:"initial_progress"`
	// LastSyncedAt is when a pass last finished without error.
	LastSyncedAt time.Time `json:"-"`
	// ErrorClass is the short class of the last failure (provider.Class),
	// never the server's text.
	ErrorClass string `json:"error_class,omitempty"`
	// NextRetryAt is when a backing-off worker tries again.
	NextRetryAt time.Time `json:"-"`
}

// ErrSyncNotRunning is what a controller answers for an account it is not
// syncing: not eligible, not consented to, or in backoff before its first
// connection.
var ErrSyncNotRunning = E(CodeConflict, "sync is not running for this account", nil)

// AccountSync is an account's sync as a caller sees it: in every account's
// JSON, and on its own from GET /v1/accounts/{id}/sync.
type AccountSync struct {
	// Enabled is whether the account may sync at all: for a personal
	// mailbox, its person consented in the console; for a team mailbox, an
	// owner or an admin gave the team's consent and someone can read it; for
	// an operator mailbox, the operator switched it on. Sync runs only for an
	// enabled account that is also active.
	Enabled bool `json:"enabled"`
	// Running is whether a worker holds the account now.
	Running bool `json:"running"`
	// State is "off" (not enabled, not active, or no sync engine on this
	// daemon), "initial", "live", "backoff" or "stopped".
	State string `json:"state"`
	// Tier is "condstore" or "uidpoll" once the server has been seen.
	Tier            string `json:"tier,omitempty"`
	FoldersSynced   int    `json:"folders_synced"`
	FoldersTotal    int    `json:"folders_total"`
	Messages        int64  `json:"messages"`
	InitialProgress int    `json:"initial_progress"`
	LastSyncedAt    int64  `json:"last_synced_at,omitempty"`
	// ErrorClass is the short class of the last failure, never the server's
	// words: needs_reauth, auth_failed, connection_closed, rate_limited, …
	ErrorClass  string `json:"error_class,omitempty"`
	NextRetryAt int64  `json:"next_retry_at,omitempty"`
}

// syncOff is the state of an account nothing is syncing.
const syncOff = "off"

// presentSync renders a controller's status.
func presentSync(enabled bool, st SyncStatus) AccountSync {
	out := AccountSync{
		Enabled: enabled, Running: st.Running, State: st.State, Tier: st.Tier,
		FoldersSynced: st.FoldersSynced, FoldersTotal: st.FoldersTotal, Messages: st.Messages,
		InitialProgress: st.InitialProgress, ErrorClass: st.ErrorClass,
	}
	if out.State == "" {
		out.State = syncOff
	}
	if !st.LastSyncedAt.IsZero() {
		out.LastSyncedAt = st.LastSyncedAt.Unix()
	}
	if !st.NextRetryAt.IsZero() {
		out.NextRetryAt = st.NextRetryAt.Unix()
	}
	return out
}

// syncOf asks the engine where an account's sync stands. An account the
// engine is not running, or a daemon without an engine, is off — which is an
// answer, not a failure: listing accounts must not break because one of them
// is not syncing.
func (s *Service) syncOf(ctx context.Context, id string, enabled bool) (AccountSync, error) {
	if s.sync == nil {
		return AccountSync{Enabled: enabled, State: syncOff}, nil
	}
	st, err := s.sync.Status(ctx, id)
	switch {
	case errors.Is(err, ErrSyncNotRunning):
		return AccountSync{Enabled: enabled, State: syncOff}, nil
	case err != nil:
		return AccountSync{}, E(CodeInternal, "reading the account's sync status failed", err)
	}
	return presentSync(enabled, st), nil
}

// SyncStatus reports where an account's sync stands.
func (s *Service) SyncStatus(ctx context.Context, p Principal, accountID string) (AccountSync, error) {
	a, err := s.authorizeAccount(ctx, p, auth.ScopeRead, accountID, needCard)
	if err != nil {
		return AccountSync{}, err
	}
	enabled, err := s.syncEnabled(ctx, a)
	if err != nil {
		return AccountSync{}, err
	}
	return s.syncOf(ctx, a.ID, enabled)
}

// TriggerSync asks for a pass over every synced folder of the account soon,
// and returns without waiting for it: anybody who may read it, with the
// write scope.
func (s *Service) TriggerSync(ctx context.Context, p Principal, accountID string) error {
	a, err := s.authorizeAccount(ctx, p, auth.ScopeWrite, accountID, needRead)
	if err != nil {
		return err
	}
	switch a.State {
	case account.StateNeedsReauth:
		return needsReauth(fmt.Errorf("%w: %s", provider.ErrNeedsReauth, a.StateReason))
	case account.StatePendingAuth:
		return E(CodeConflict, "this account has not been authorised yet", nil)
	}
	enabled, err := s.syncEnabled(ctx, a)
	if err != nil {
		return err
	}
	if !enabled {
		return errSyncOff(a)
	}
	if s.sync == nil {
		return ErrSyncUnavailable
	}
	if err := s.sync.Trigger(ctx, a.ID); err != nil {
		var known *Error
		if errors.As(err, &known) {
			return err
		}
		return E(CodeInternal, "asking for a sync failed", err)
	}
	return nil
}

// ErrSyncUnavailable is a daemon running without the sync engine.
var ErrSyncUnavailable = E(CodeConflict, "sync is not available on this server", nil)

// errSyncOff is an account that may not sync: for a personal mailbox, one
// whose person has not consented; for a team mailbox, one its owners and
// admins have not turned on; for an operator mailbox, one the operator has
// not switched on.
func errSyncOff(a account.Account) error {
	switch {
	case a.WorkspaceID == workspace.OperatorID:
		return E(CodeConflict, "sync is off for this account; an instance administrator has to switch it on", nil)
	case a.OwnerUserID == "":
		return E(CodeConflict, "sync is off for this team mailbox; an owner or an admin of the team has to turn it on", nil)
	}
	return E(CodeConflict, "sync is off: its person has not turned it on in the console", nil)
}
