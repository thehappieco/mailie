// Package account owns the mailboxes this server knows about: registering
// them, collecting consent, keeping their credentials sealed, and handing the
// rest of the server a provider.Mailbox it can use.
//
// The credentials are the reason this is a package rather than a table. An
// OAuth grant is not a value that sits still: Microsoft rotates the refresh
// token on every use, both providers expire access tokens hourly, and a grant
// can die between one connection and the next. Everything here exists to make
// those transitions survivable — a refreshed token written down before it is
// used, a rejected login retried once with a fresh token, and a genuinely dead
// grant reported as something only a person can fix.
package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/secrets"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// State is where an account stands.
type State string

const (
	// StatePendingAuth is an account created but not yet authorised.
	StatePendingAuth State = "pending_auth"
	// StateActive is an account the daemon is syncing.
	StateActive State = "active"
	// StateNeedsReauth is an account whose grant is gone. Only a person can
	// fix it, so the workers stop rather than retry.
	StateNeedsReauth State = "needs_reauth"
	// StateDisabled is an account switched off deliberately.
	StateDisabled State = "disabled"
	// StateError is an account failing for a reason that may pass.
	StateError State = "error"
)

// PersonInitialDays is how far back the first sync of a person's mailbox
// reaches: the last 90 days, as the privacy policy they consented to says.
// The service refuses anything else from a person, and the sync engine uses
// it for every owned mailbox whatever the row holds.
const PersonInitialDays = 90

// Account is one mailbox this server knows about.
type Account struct {
	ID          string
	Email       string
	DisplayName string
	Provider    provider.Kind
	AuthKind    string // oauth2 | password
	IMAPHost    string
	IMAPPort    int
	SMTPHost    string
	SMTPPort    int
	SMTPTLS     string // implicit | starttls
	LoginUser   string
	Tenant      string

	// WorkspaceID is the workspace the mailbox belongs to, which never
	// changes. Create puts a person's mailbox in their personal workspace,
	// and one an instance key creates in the operator workspace, unless it
	// names another.
	WorkspaceID string
	// OwnerUserID is the person who linked the account: "linked by" in the
	// API and the documents, the column keeps its name. The mailbox syncs
	// under their consent. Empty exactly for a mailbox of the operator
	// workspace, which an instance key created.
	OwnerUserID string
	// OAuthClient is which registration issued the stored grant, "installed"
	// or "web": a refresh token only works with the client it came from.
	OAuthClient string

	SyncTier         string
	SyncTierResolved string
	SaveSentCopy     bool
	// InitialDays is how far back the first sync reaches, in days; zero or
	// less is everything. Only a mailbox nobody owns may have anything but
	// PersonInitialDays: a person's is synced that far back whatever this
	// says.
	InitialDays     int
	FolderOverrides map[string]string

	State       State
	StateReason string

	LastOKAt            time.Time
	LastError           string
	ConsecutiveFailures int
	NextRetryAt         time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IMAPAddr is the address to dial.
func (a Account) IMAPAddr() string { return fmt.Sprintf("%s:%d", a.IMAPHost, a.IMAPPort) }

// UsesOAuth reports whether this account authenticates with a bearer token.
func (a Account) UsesOAuth() bool { return a.AuthKind == "oauth2" }

// Errors the repository reports.
var (
	// ErrNotFound is an account id nobody knows.
	ErrNotFound = errors.New("account: no such account")
	// ErrDuplicate is an address already linked in the same workspace.
	ErrDuplicate = errors.New("account: that address is already connected")
	// ErrNoCredentials is an account with nothing stored to authenticate with.
	ErrNoCredentials = errors.New("account: no stored credentials")
	// ErrUnknownOwner is an owner id no user has.
	ErrUnknownOwner = errors.New("account: no such owner")
	// ErrNoWorkspace is a workspace that does not exist, or one the account
	// cannot go into: a linked mailbox in the operator workspace, one nobody
	// linked anywhere else, or a person with no personal workspace.
	ErrNoWorkspace = errors.New("account: no such workspace for this mailbox")
)

// Repository reads and writes accounts and their sealed credentials.
//
// Every change of an account's state is journaled as account.state in the
// transaction that makes it, and published to the bus once that transaction
// has committed; then whoever registered with OnChange hears about it. That
// is how the sync engine learns an account became usable or stopped being so
// without polling, whichever path changed it: a consent finishing in the
// background, a token source finding its grant dead, a person removing a
// mailbox.
type Repository struct {
	store   *store.Store
	keyring *secrets.Keyring
	now     func() time.Time
	journal *events.Journal

	notifyMu sync.Mutex
	bus      *events.Bus
	onChange []func(accountID string)
}

// NewRepository builds the repository.
func NewRepository(s *store.Store, keyring *secrets.Keyring) *Repository {
	return &Repository{store: s, keyring: keyring, now: s.Now, journal: events.NewJournal(s)}
}

// PublishTo sends the account.state events the repository journals to bus,
// after each commit. Without a bus they are still journaled — replay reads
// the table, not the bus — only nobody attached hears them live.
func (r *Repository) PublishTo(bus *events.Bus) {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.bus = bus
}

// OnChange registers fn to be told, after the change has committed, the id of
// every account that was created, removed, changed state or had its folder
// overrides changed — anything that can decide whether and how it syncs.
//
// fn runs on whichever goroutine made the change: a request, a consent
// finishing in the background, or a sync worker whose token refresh found
// the grant dead. It must not block and must not call back into the
// repository; hand the id to something that acts on it later.
func (r *Repository) OnChange(fn func(accountID string)) {
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	r.onChange = append(r.onChange, fn)
}

// StateChange is the account.state payload: an account moved from Previous
// to State. Reason is the fixed, short state_reason — never a provider's own
// words, never a credential.
type StateChange struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"`
	Previous  string `json:"previous_state"`
	Reason    string `json:"reason,omitempty"`
}

// journalState writes account.state inside tx when from and to differ.
func (r *Repository) journalState(ctx context.Context, tx *sql.Tx, id string, from, to State, reason string, at time.Time) ([]events.Event, error) {
	if from == to {
		return nil, nil
	}
	ev, err := events.New(events.TypeAccountState, id, at, StateChange{
		AccountID: id, State: string(to), Previous: string(from), Reason: reason,
	})
	if err != nil {
		return nil, err
	}
	return r.journal.Append(ctx, tx, []events.Event{ev})
}

// committed publishes what a committed transaction journaled and tells every
// OnChange listener about the accounts it touched.
func (r *Repository) committed(evs []events.Event, ids ...string) {
	r.notifyMu.Lock()
	bus := r.bus
	listeners := append([]func(string){}, r.onChange...)
	r.notifyMu.Unlock()
	if bus != nil && len(evs) > 0 {
		bus.Publish(evs...)
	}
	for _, id := range ids {
		for _, fn := range listeners {
			fn(id)
		}
	}
}

// currentStateTx reads an account's state inside tx.
func currentStateTx(ctx context.Context, tx *sql.Tx, id string) (State, error) {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM accounts WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("account: read state: %w", err)
	}
	return State(state), nil
}

// WithClock overrides the clock, for tests.
func (r *Repository) WithClock(now func() time.Time) *Repository {
	r.now = now
	return r
}

const accountColumns = `id, workspace_id, email, display_name, provider, auth_kind, imap_host, imap_port,
	smtp_host, smtp_port, smtp_tls, login_user, oauth_tenant, coalesce(owner_user_id, ''), oauth_client,
	sync_tier, sync_tier_resolved, save_sent_copy, initial_days, folder_overrides, state, state_reason,
	last_ok_at, last_error, consecutive_failures, next_retry_at, created_at, updated_at`

// OAuth clients an account's grant can come from.
const (
	ClientInstalled = "installed"
	ClientWeb       = "web"
)

// Visibility is which accounts a caller may know exist. Built by
// internal/service from the caller, and turned into SQL here, so listing and
// fetching one account apply the rule the same way.
//
// A person sees a mailbox when they are an active member of its workspace
// (an active membership, and active on the instance) and hold a grant on it:
// any grant shows it, and Need narrows to the mailboxes on which they hold
// those flags.
type Visibility struct {
	// All is an instance credential over REST, as the service has it until
	// it moves instance keys to the operator workspace: every account.
	All bool
	// UserID sees the mailboxes of their workspaces they hold a grant on.
	UserID string
	// Unowned also sees the operator workspace's mailboxes: those nobody
	// linked, which an instance key created.
	Unowned bool
	// Need is the flags a person must hold on the mailbox; the zero value
	// is any grant. It narrows only the person's half of the rule.
	Need workspace.Flags
	// Workspace narrows to one workspace's mailboxes; empty is every one.
	Workspace string
}

// clause is the WHERE fragment for v, over the accounts table, with its
// arguments.
func (v Visibility) clause() (string, []any) {
	where, args := "1", []any(nil)
	if !v.All {
		where = `(EXISTS (SELECT 1 FROM mailbox_access g
		            JOIN workspace_members m ON m.workspace_id = g.workspace_id AND m.user_id = g.user_id
		            JOIN users u ON u.id = g.user_id
		           WHERE g.account_id = accounts.id AND g.user_id = ? AND m.status = 'active' AND u.status = 'active'
		             AND g.read >= ? AND g.act >= ? AND g.send >= ? AND g.manage >= ?)
		      OR (? AND accounts.workspace_id = '` + workspace.OperatorID + `'))`
		args = []any{v.UserID, v.Need.Read, v.Need.Act, v.Need.Send, v.Need.Manage, v.Unowned}
	}
	if v.Workspace != "" {
		where += ` AND accounts.workspace_id = ?`
		args = append(args, v.Workspace)
	}
	return where, args
}

// Create registers an account. It starts in pending_auth: nothing syncs until
// a credential has been stored for it.
//
// A linked mailbox (OwnerUserID) goes into WorkspaceID, or their personal
// workspace when it is empty, and its linker gets every flag on it in the
// same transaction, which also checks they are an active member there. One
// nobody linked goes into the operator workspace.
func (r *Repository) Create(ctx context.Context, a Account) (Account, error) {
	return r.create(ctx, a, nil)
}

// create is Create with a check run first in its transaction; nil checks
// nothing.
func (r *Repository) create(ctx context.Context, a Account, also func(*sql.Tx) error) (Account, error) {
	now := r.now().UTC().Truncate(time.Second)
	a.CreatedAt, a.UpdatedAt = now, now
	if a.State == "" {
		a.State = StatePendingAuth
	}
	if a.InitialDays == 0 {
		a.InitialDays = PersonInitialDays
	}
	if a.FolderOverrides == nil {
		a.FolderOverrides = map[string]string{}
	}
	if a.OAuthClient == "" {
		a.OAuthClient = ClientInstalled
	}
	overrides, err := json.Marshal(a.FolderOverrides)
	if err != nil {
		return Account{}, fmt.Errorf("account: encode folder overrides: %w", err)
	}

	err = r.store.Write(ctx, func(tx *sql.Tx) error {
		if also != nil {
			if err := also(tx); err != nil {
				return err
			}
		}
		if a.WorkspaceID, err = workspaceForTx(ctx, tx, a); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO accounts(id, workspace_id, email, display_name, provider, auth_kind, imap_host, imap_port,
			 smtp_host, smtp_port, smtp_tls, login_user, oauth_tenant, owner_user_id, oauth_client,
			 sync_tier, save_sent_copy, initial_days, folder_overrides, state, state_reason,
			 state_changed_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
			a.ID, a.WorkspaceID, a.Email, a.DisplayName, string(a.Provider), a.AuthKind, a.IMAPHost, a.IMAPPort,
			a.SMTPHost, a.SMTPPort, a.SMTPTLS, a.LoginUser, a.Tenant, nullable(a.OwnerUserID), a.OAuthClient,
			orDefault(a.SyncTier, "auto"), boolToInt(a.SaveSentCopy), a.InitialDays, string(overrides),
			string(a.State), now.Unix(), now.Unix(), now.Unix())
		if store.IsUnique(err) {
			return ErrDuplicate
		}
		if store.IsForeignKey(err) {
			return ErrUnknownOwner
		}
		if err != nil {
			return fmt.Errorf("account: insert: %w", err)
		}
		if a.OwnerUserID == "" {
			return nil
		}
		return workspace.GrantLinkerTx(ctx, tx, a.ID, a.WorkspaceID, a.OwnerUserID, now)
	})
	if err != nil {
		return Account{}, err
	}
	r.committed(nil, a.ID)
	return a, nil
}

// workspaceForTx is the workspace a new account goes into, checked: the one
// it names, or else its linker's personal workspace, or the operator's for a
// mailbox nobody linked. A linked mailbox never goes into the operator
// workspace, and one nobody linked never anywhere else.
func workspaceForTx(ctx context.Context, tx *sql.Tx, a Account) (string, error) {
	switch {
	case a.WorkspaceID == "" && a.OwnerUserID == "":
		return workspace.OperatorID, nil
	case a.WorkspaceID == "":
		w, err := workspace.PersonalOfTx(ctx, tx, a.OwnerUserID)
		if errors.Is(err, workspace.ErrNotFound) {
			return "", fmt.Errorf("%w: the person has no personal workspace", ErrNoWorkspace)
		}
		if err != nil {
			return "", err
		}
		return w.ID, nil
	}
	w, err := workspace.GetTx(ctx, tx, a.WorkspaceID)
	if errors.Is(err, workspace.ErrNotFound) {
		return "", ErrNoWorkspace
	}
	if err != nil {
		return "", err
	}
	if (w.Kind == workspace.KindOperator) != (a.OwnerUserID == "") {
		return "", ErrNoWorkspace
	}
	return w.ID, nil
}

// Get reads one account.
func (r *Repository) Get(ctx context.Context, id string) (Account, error) {
	row := r.store.Reader().QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// GetByEmail reads the account linked under an address in one workspace.
// The same address may be linked in several workspaces, each its own
// mailbox; within one it is linked at most once.
func (r *Repository) GetByEmail(ctx context.Context, workspaceID, email string) (Account, error) {
	row := r.store.Reader().QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE workspace_id = ? AND email = ? COLLATE NOCASE`,
		workspaceID, email)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// WorkspaceFor is the workspace Create would put a into, without creating
// anything: for a check made before a slow step, which create repeats in its
// own transaction.
func (r *Repository) WorkspaceFor(ctx context.Context, a Account) (string, error) {
	// A read-only transaction on the reader pool: nothing here writes.
	tx, err := r.store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", fmt.Errorf("account: begin: %w", err)
	}
	//nolint:errcheck // a read-only transaction; nothing to keep or lose
	defer func() { _ = tx.Rollback() }()
	return workspaceForTx(ctx, tx, a)
}

// GetVisible reads one account if v may see it, and reports ErrNotFound
// otherwise: an account somebody else owns does not exist, as far as the
// caller can tell.
func (r *Repository) GetVisible(ctx context.Context, id string, v Visibility) (Account, error) {
	where, args := v.clause()
	row := r.store.Reader().QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = ? AND `+where, append([]any{id}, args...)...)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	return a, err
}

// List returns every account, oldest first.
func (r *Repository) List(ctx context.Context) ([]Account, error) {
	return r.ListVisible(ctx, Visibility{All: true})
}

// LinkedBy returns the accounts a person linked, in every workspace, oldest
// first, whatever their access to them now and whether or not they are still
// active: the mailboxes that sync under their consent.
func (r *Repository) LinkedBy(ctx context.Context, userID string) ([]Account, error) {
	rows, err := r.store.Reader().QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE owner_user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("account: list linked: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list linked: %w", err)
	}
	return out, nil
}

// ListVisible returns the accounts v may see, oldest first. The filter runs in
// SQL rather than over every row in Go, so a member's listing never holds
// other people's accounts even briefly.
func (r *Repository) ListVisible(ctx context.Context, v Visibility) ([]Account, error) {
	where, args := v.clause()
	rows, err := r.store.Reader().QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("account: list: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list: %w", err)
	}
	return out, nil
}

// Delete removes an account and everything that hangs off it.
func (r *Repository) Delete(ctx context.Context, id string) error {
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		n, err := purgeTx(ctx, tx, []string{id}, r.now().Unix())
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.committed(nil, id)
	return nil
}

// attempt is one consent attempt: which account, and its OAuth state.
type attempt struct {
	accountID string
	state     string
}

// deleteOwner removes, in one transaction, every account a person owns — as
// Delete removes one — and every consent attempt they started on an account
// they do not own, and then runs also, where the caller deletes the person. It
// returns the accounts removed and the attempts dropped, which the registry
// then stops waiting on.
func (r *Repository) deleteOwner(ctx context.Context, userID string, before, also func(*sql.Tx) error) ([]string, []attempt, error) {
	var (
		owned    []string
		attempts []attempt
	)
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		if before != nil {
			if err := before(tx); err != nil {
				return err
			}
		}
		var err error
		if owned, err = ownedTx(ctx, tx, userID); err != nil {
			return err
		}
		// An owner may have started consent on an account the CLI created;
		// the attempt is theirs, the account is not.
		if attempts, err = deleteFlowsByTx(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := purgeTx(ctx, tx, owned, r.now().Unix()); err != nil {
			return err
		}
		return also(tx)
	})
	if err != nil {
		return nil, nil, err
	}
	r.committed(nil, owned...)
	return owned, attempts, nil
}

// deleteFlowsBy drops every consent attempt a person started and returns them.
func (r *Repository) deleteFlowsBy(ctx context.Context, userID string) ([]attempt, error) {
	var attempts []attempt
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		attempts, err = deleteFlowsByTx(ctx, tx, userID)
		return err
	})
	return attempts, err
}

func ownedTx(ctx context.Context, tx *sql.Tx, userID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM accounts WHERE owner_user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("account: list owned: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("account: list owned: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list owned: %w", err)
	}
	return ids, nil
}

func deleteFlowsByTx(ctx context.Context, tx *sql.Tx, userID string) ([]attempt, error) {
	rows, err := tx.QueryContext(ctx,
		`DELETE FROM oauth_pending WHERE owner_user_id = ? RETURNING account_id, state`, userID)
	if err != nil {
		return nil, fmt.Errorf("account: drop the user's pending flows: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var out []attempt
	for rows.Next() {
		var a attempt
		if err := rows.Scan(&a.accountID, &a.state); err != nil {
			return nil, fmt.Errorf("account: drop the user's pending flows: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: drop the user's pending flows: %w", err)
	}
	return out, nil
}

// purgeTx deletes accounts, and everything that refers to them, inside the
// caller's transaction, and reports how many accounts it deleted.
//
// Most of it is the schema's ON DELETE CASCADE: credentials, pending consent,
// folders, messages with their parts, bodies and full-text rows, drafts, sends
// and key restrictions. Three things are not, and go here first:
//
//   - A live key restricted to nothing but these accounts is revoked. Its
//     restriction rows cascade away, and a key with no restriction rows
//     reaches every account: without this, deleting the one mailbox a key was
//     limited to would hand it all of them.
//   - The event journal names accounts without a foreign key, so that event
//     retention cannot cascade into pending webhook deliveries. The accounts'
//     events, and any delivery of them, are deleted explicitly.
//   - A webhook's accounts are a JSON list. These ids come out of it, and a
//     hook left with none — which would read as every account — is deleted.
func purgeTx(ctx context.Context, tx *sql.Tx, ids []string, now int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return 0, fmt.Errorf("account: encode ids: %w", err)
	}
	const in = `(SELECT value FROM json_each(?))`
	steps := []struct {
		what  string
		query string
		args  []any
	}{
		{"revoke the keys limited to them", `UPDATE api_keys SET revoked_at = ?
		   WHERE revoked_at = 0
		     AND EXISTS (SELECT 1 FROM api_key_accounts r
		                  WHERE r.key_prefix = api_keys.prefix AND r.account_id IN ` + in + `)
		     AND NOT EXISTS (SELECT 1 FROM api_key_accounts r
		                      WHERE r.key_prefix = api_keys.prefix AND r.account_id NOT IN ` + in + `)`,
			[]any{now, list, list}},
		{"delete deliveries of their events", `DELETE FROM webhook_deliveries
		   WHERE event_seq IN (SELECT seq FROM events WHERE account_id IN ` + in + `)`,
			[]any{list}},
		{"delete their events", `DELETE FROM events WHERE account_id IN ` + in, []any{list}},
		{"delete the webhooks limited to them", `DELETE FROM webhooks
		   WHERE json_array_length(accounts_json) > 0
		     AND NOT EXISTS (SELECT 1 FROM json_each(webhooks.accounts_json) a WHERE a.value NOT IN ` + in + `)`,
			[]any{list}},
		{"take them out of other webhooks", `UPDATE webhooks
		     SET accounts_json = (SELECT json_group_array(a.value) FROM json_each(webhooks.accounts_json) a
		                           WHERE a.value NOT IN ` + in + `),
		         updated_at = ?
		   WHERE EXISTS (SELECT 1 FROM json_each(webhooks.accounts_json) a WHERE a.value IN ` + in + `)`,
			[]any{list, now, list}},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return 0, fmt.Errorf("account: delete: %s: %w", step.what, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id IN `+in, list)
	if err != nil {
		return 0, fmt.Errorf("account: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("account: delete: %w", err)
	}
	return int(n), nil
}

// SetState records where an account stands and why.
//
// A change of state is journaled as account.state in the same transaction.
func (r *Repository) SetState(ctx context.Context, id string, state State, reason string) error {
	at := r.now()
	now := at.Unix()
	var evs []events.Event
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		previous, err := currentStateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET state = ?, state_reason = ?, state_changed_at = ?, updated_at = ?
			  WHERE id = ?`, string(state), reason, now, now, id); err != nil {
			return fmt.Errorf("account: set state: %w", err)
		}
		evs, err = r.journalState(ctx, tx, id, previous, state, reason, at)
		return err
	})
	if err != nil {
		return err
	}
	if len(evs) > 0 {
		r.committed(evs, id)
	}
	return nil
}

// MarkNeedsReauth stops an account until a person consents again.
func (r *Repository) MarkNeedsReauth(ctx context.Context, id, reason string) error {
	return r.SetState(ctx, id, StateNeedsReauth, reason)
}

// SetResolvedTier records which synchronisation path the server's capabilities
// chose, so an operator can see it without reading logs.
func (r *Repository) SetResolvedTier(ctx context.Context, id, tier string) error {
	now := r.now().Unix()
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE accounts SET sync_tier_resolved = ?, updated_at = ? WHERE id = ?`, tier, now, id)
		if err != nil {
			return fmt.Errorf("account: set resolved tier: %w", err)
		}
		return nil
	})
}

// SetFolderOverrides records which folder plays which role, when the server
// does not say and the name table guesses wrong.
func (r *Repository) SetFolderOverrides(ctx context.Context, id string, overrides map[string]string) error {
	if overrides == nil {
		overrides = map[string]string{}
	}
	encoded, err := json.Marshal(overrides)
	if err != nil {
		return fmt.Errorf("account: encode folder overrides: %w", err)
	}
	now := r.now().Unix()
	err = r.store.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE accounts SET folder_overrides = ?, updated_at = ? WHERE id = ?`,
			string(encoded), now, id)
		if err != nil {
			return fmt.Errorf("account: set folder overrides: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("account: set folder overrides: %w", err)
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The roles the engine resolved from the old overrides are wrong now.
	r.committed(nil, id)
	return nil
}

// SavePassword seals an IMAP password.
func (r *Repository) SavePassword(ctx context.Context, id, password string) error {
	return r.saveCredential(ctx, id, "password", []byte(password))
}

// SaveToken seals an OAuth token.
//
// The whole token is stored, not only the refresh half: the access token is
// usually still good after a restart, and keeping it avoids a refresh — and a
// rotation — every time the daemon comes up.
func (r *Repository) SaveToken(ctx context.Context, id string, token *oauth2.Token) error {
	// The struct does hold secrets; that is the point. It is serialised only
	// to be sealed on the next line, and never written anywhere else.
	//nolint:gosec // G117: the result is encrypted before it reaches disk
	encoded, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("account: encode token: %w", err)
	}
	return r.saveCredential(ctx, id, "oauth_token", encoded)
}

func (r *Repository) saveCredential(ctx context.Context, id, field string, plaintext []byte) error {
	sealed, err := r.keyring.Seal(id, field, plaintext)
	if err != nil {
		return fmt.Errorf("account: seal %s: %w", field, err)
	}
	now := r.now().Unix()
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		return r.putCredential(ctx, tx, id, field, sealed, now)
	})
}

func (r *Repository) putCredential(ctx context.Context, tx *sql.Tx, id, field string, sealed []byte, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO credentials(account_id, field, keyid, ciphertext, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, field) DO UPDATE SET
		   keyid = excluded.keyid, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		id, field, r.keyring.ActiveKeyID(), sealed, now)
	if store.IsForeignKey(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("account: store %s: %w", field, err)
	}
	return nil
}

// Password reads a sealed IMAP password.
func (r *Repository) Password(ctx context.Context, id string) (string, error) {
	plaintext, err := r.credential(ctx, id, "password")
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// Token reads a sealed OAuth token.
func (r *Repository) Token(ctx context.Context, id string) (*oauth2.Token, error) {
	plaintext, err := r.credential(ctx, id, "oauth_token")
	if err != nil {
		return nil, err
	}
	var token oauth2.Token
	if err := json.Unmarshal(plaintext, &token); err != nil {
		return nil, fmt.Errorf("account: decode stored token: %w", err)
	}
	return &token, nil
}

func (r *Repository) credential(ctx context.Context, id, field string) ([]byte, error) {
	var ciphertext []byte
	err := r.store.Reader().QueryRowContext(ctx,
		`SELECT ciphertext FROM credentials WHERE account_id = ? AND field = ?`, id, field).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("account: read %s: %w", field, err)
	}
	plaintext, err := r.keyring.Open(id, field, ciphertext)
	if err != nil {
		// The most likely cause by far is the wrong key in the environment,
		// and saying so saves a long hunt.
		return nil, fmt.Errorf("account: cannot decrypt the stored %s for %s "+
			"(is MAIL_CREDENTIAL_KEY_HEX the key this was sealed with?): %w", field, id, err)
	}
	return plaintext, nil
}

// PendingFlow is a consent flow the daemon is waiting on.
type PendingFlow struct {
	State     string
	AccountID string
	// OwnerUserID is who started the flow; empty for an instance key. A
	// redirect is only accepted from the same caller, and that is checked
	// before its code is exchanged: otherwise anyone could start a flow, send
	// the provider's page to somebody else, and have that person's consent
	// attach their mailbox to an account the sender controls.
	OwnerUserID string
	Flow        FlowKind
	Verifier    string
	RedirectURI string
	DeviceCode  string
	ExpiresAt   time.Time
}

const pendingColumns = `state, account_id, coalesce(owner_user_id, ''), flow, pkce_verifier, redirect_uri,
	device_code, expires_at`

// SaveFlow records a flow in progress.
func (r *Repository) SaveFlow(ctx context.Context, f PendingFlow) error {
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO oauth_pending(state, account_id, owner_user_id, flow, pkce_verifier, redirect_uri,
			 device_code, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			f.State, f.AccountID, nullable(f.OwnerUserID), string(f.Flow), f.Verifier, f.RedirectURI,
			f.DeviceCode, f.ExpiresAt.Unix())
		if err != nil {
			return fmt.Errorf("account: record pending flow: %w", err)
		}
		return nil
	})
}

// TakeFlow reads a pending flow and removes it, so a redirect cannot be
// replayed. It is for the daemon's own listeners, which caught the redirect
// themselves and so have no caller to match; a redirect handed in over the
// API goes through TakeOwnedFlow.
func (r *Repository) TakeFlow(ctx context.Context, state string) (PendingFlow, error) {
	return r.takeFlow(ctx, `DELETE FROM oauth_pending WHERE state = ?
		RETURNING `+pendingColumns, state)
}

// TakeOwnedFlow is TakeFlow for the caller who started the flow. A flow
// somebody else started is reported as not found and left exactly where it
// was: not consumed, so the person it belongs to can still finish it, and its
// code never exchanged.
func (r *Repository) TakeOwnedFlow(ctx context.Context, state, ownerUserID string) (PendingFlow, error) {
	return r.takeFlow(ctx, `DELETE FROM oauth_pending WHERE state = ? AND coalesce(owner_user_id, '') = ?
		RETURNING `+pendingColumns, state, ownerUserID)
}

func (r *Repository) takeFlow(ctx context.Context, query string, args ...any) (PendingFlow, error) {
	var f PendingFlow
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		f, err = scanFlow(tx.QueryRowContext(ctx, query, args...))
		return err
	})
	if err != nil {
		return PendingFlow{}, err
	}
	if r.now().After(f.ExpiresAt) {
		return f, fmt.Errorf("%w at %s; start again", ErrFlowExpired, f.ExpiresAt.Format(time.RFC3339))
	}
	return f, nil
}

// OwnedFlow reads a pending flow the caller started, without consuming it.
func (r *Repository) OwnedFlow(ctx context.Context, state, ownerUserID string) (PendingFlow, error) {
	f, err := scanFlow(r.store.Reader().QueryRowContext(ctx,
		`SELECT `+pendingColumns+` FROM oauth_pending WHERE state = ? AND coalesce(owner_user_id, '') = ?`,
		state, ownerUserID))
	if err != nil {
		return PendingFlow{}, err
	}
	if r.now().After(f.ExpiresAt) {
		return f, fmt.Errorf("%w at %s; start again", ErrFlowExpired, f.ExpiresAt.Format(time.RFC3339))
	}
	return f, nil
}

func scanFlow(row scanner) (PendingFlow, error) {
	var (
		f       PendingFlow
		flow    string
		expires int64
	)
	err := row.Scan(&f.State, &f.AccountID, &f.OwnerUserID, &flow, &f.Verifier, &f.RedirectURI,
		&f.DeviceCode, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingFlow{}, ErrNotFound
	}
	if err != nil {
		return PendingFlow{}, fmt.Errorf("account: read pending flow: %w", err)
	}
	f.Flow = FlowKind(flow)
	f.ExpiresAt = time.Unix(expires, 0).UTC()
	return f, nil
}

// DeleteFlows drops every flow an account has in progress, when a new one
// replaces them.
func (r *Repository) DeleteFlows(ctx context.Context, accountID string) error {
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_pending WHERE account_id = ?`, accountID); err != nil {
			return fmt.Errorf("account: drop pending flows: %w", err)
		}
		return nil
	})
}

// SweepFlows removes expired flows and returns the accounts they belonged
// to, which the caller records as failed: an attempt that simply ran out of
// time is the most common way consent fails, and a web flow has nothing else
// waiting on it that would notice.
func (r *Repository) SweepFlows(ctx context.Context) ([]string, error) {
	var accounts []string
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`DELETE FROM oauth_pending WHERE expires_at < ? RETURNING account_id`, r.now().Unix())
		if err != nil {
			return fmt.Errorf("account: sweep pending flows: %w", err)
		}
		//nolint:errcheck // read to the end below; a close failure changes nothing
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("account: sweep pending flows: %w", err)
			}
			accounts = append(accounts, id)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("account: sweep pending flows: %w", err)
		}
		return nil
	})
	return accounts, err
}

// SaveGrant stores the token a consent produced, which client issued it, and
// that the account is now active — in one transaction, so the account can
// never hold a token together with the name of a client that cannot refresh
// it.
func (r *Repository) SaveGrant(ctx context.Context, id string, token *oauth2.Token, client string) error {
	return r.saveGrant(ctx, id, token, client, nil)
}

// saveGrant is SaveGrant with a check run first in its transaction; nil
// checks nothing.
func (r *Repository) saveGrant(ctx context.Context, id string, token *oauth2.Token, client string, also func(*sql.Tx) error) error {
	//nolint:gosec // G117: the result is encrypted before it reaches disk
	encoded, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("account: encode token: %w", err)
	}
	sealed, err := r.keyring.Seal(id, "oauth_token", encoded)
	if err != nil {
		return fmt.Errorf("account: seal oauth_token: %w", err)
	}
	at := r.now()
	now := at.Unix()
	var evs []events.Event
	err = r.store.Write(ctx, func(tx *sql.Tx) error {
		if also != nil {
			if err := also(tx); err != nil {
				return err
			}
		}
		previous, err := currentStateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := r.putCredential(ctx, tx, id, "oauth_token", sealed, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET oauth_client = ?, state = ?, state_reason = '', state_changed_at = ?, updated_at = ?
			  WHERE id = ?`, client, string(StateActive), now, now, id); err != nil {
			return fmt.Errorf("account: record grant: %w", err)
		}
		evs, err = r.journalState(ctx, tx, id, previous, StateActive, "", at)
		return err
	})
	if err != nil {
		return err
	}
	// Told even when the account was already active: the grant under it is
	// new, and a worker holding a connection opened with the old one should
	// know.
	r.committed(evs, id)
	return nil
}

// Transition moves an account to a new state, but only from one of the
// states given, and reports whether it moved. Each consent path changes only
// what is its own: a failed re-consent must not stop an account that works,
// and a failure arriving late must not undo a success that got there first.
func (r *Repository) Transition(ctx context.Context, id string, from []State, to State, reason string) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	at := r.now()
	now := at.Unix()
	var (
		moved bool
		evs   []events.Event
	)
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		// Read, then write, in one immediate transaction: nothing can change
		// the state in between, and the event names where it really came from.
		previous, err := currentStateTx(ctx, tx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		allowed := false
		for _, s := range from {
			if s == previous {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE accounts SET state = ?, state_reason = ?, state_changed_at = ?, updated_at = ?
			  WHERE id = ?`, string(to), reason, now, now, id); err != nil {
			return fmt.Errorf("account: change state: %w", err)
		}
		moved = true
		evs, err = r.journalState(ctx, tx, id, previous, to, reason, at)
		return err
	})
	if err != nil {
		return false, err
	}
	if len(evs) > 0 {
		r.committed(evs, id)
	}
	return moved, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(row scanner) (Account, error) {
	var (
		a                                   Account
		kind, overrides                     string
		lastOK, nextRetry, created, updated int64
		saveSent                            int
	)
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Email, &a.DisplayName, &kind, &a.AuthKind, &a.IMAPHost, &a.IMAPPort,
		&a.SMTPHost, &a.SMTPPort, &a.SMTPTLS, &a.LoginUser, &a.Tenant, &a.OwnerUserID, &a.OAuthClient,
		&a.SyncTier, &a.SyncTierResolved,
		&saveSent, &a.InitialDays, &overrides, &a.State, &a.StateReason,
		&lastOK, &a.LastError, &a.ConsecutiveFailures, &nextRetry, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Account{}, err
		}
		return Account{}, fmt.Errorf("account: scan: %w", err)
	}
	a.Provider = provider.Kind(kind)
	a.SaveSentCopy = saveSent != 0
	a.LastOKAt = unixOrZero(lastOK)
	a.NextRetryAt = unixOrZero(nextRetry)
	a.CreatedAt = unixOrZero(created)
	a.UpdatedAt = unixOrZero(updated)
	a.FolderOverrides = map[string]string{}
	if strings.TrimSpace(overrides) != "" {
		if err := json.Unmarshal([]byte(overrides), &a.FolderOverrides); err != nil {
			return Account{}, fmt.Errorf("account: decode folder overrides: %w", err)
		}
	}
	return a, nil
}

func unixOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullable stores an empty string as NULL, for the optional foreign keys.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// both runs two checks in order, either of which may be nil.
func both(first, second func(*sql.Tx) error) func(*sql.Tx) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	return func(tx *sql.Tx) error {
		if err := first(tx); err != nil {
			return err
		}
		return second(tx)
	}
}
