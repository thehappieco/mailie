// Package sync keeps each account's index in step with its mail server.
//
// One Manager per daemon; one worker per account that may sync — the account
// is active and its owner consented, or, for an account nobody owns, the
// operator switched it on (store.SyncEligibleAccounts is the one rule). A
// worker holds at most three connections, each with a fixed job: "sync" runs
// folder passes one after another, "idle" sits in IDLE on the inbox and only
// ever says "look again", and "interactive" is opened on demand for the
// service. Gmail allows fifteen connections across every client the person
// runs, so the budget is the worker's, not the provider's.
//
// The engine stores metadata only: envelope, dates, size, flags and the
// parts of BODYSTRUCTURE. Every write goes through internal/store inside one
// transaction that first checks the account may still be synced, journals
// the events it decides on, and is published to the bus only once it has
// committed. A withdrawal of consent that commits therefore leaves nothing a
// late batch could add, however late its worker hears about it.
//
// Nothing here logs a subject, a sender, a recipient or an address: accounts
// and folders appear by id and role, failures by provider.Class.
package sync

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/store"
)

// Accounts is what the engine reads and records about an account.
// *account.Repository is the implementation.
type Accounts interface {
	Get(ctx context.Context, id string) (account.Account, error)
	SetResolvedTier(ctx context.Context, id, tier string) error
}

// MailboxSource hands out an account's provider mailbox. The daemon passes
// account.Registry.Mailbox, which builds it from the stored credentials; the
// engine asks again for every connection, because a new grant or a refused
// one replaces the mailbox underneath.
type MailboxSource func(ctx context.Context, accountID string) (provider.Mailbox, error)

// Deps is what the engine runs on.
type Deps struct {
	Store     *store.Store
	Accounts  Accounts
	Mailboxes MailboxSource
	// Bus fans committed events out. Nil journals them without publishing.
	Bus *events.Bus
	Log *slog.Logger
}

// Manager runs the workers. It implements service.SyncController.
type Manager struct {
	store     *store.Store
	accounts  Accounts
	mailboxes MailboxSource
	bus       *events.Bus
	journal   *events.Journal
	log       *slog.Logger
	opts      Options

	wake chan struct{}

	mu       sync.Mutex
	running  bool
	closed   bool
	workers  map[string]*worker
	stopping map[string]chan struct{} // done channels of workers told to stop
	pending  map[string]bool          // accounts to reconcile
	slots    map[string]*slot         // interactive connections, by account
	wg       sync.WaitGroup
}

var (
	_ service.SyncController    = (*Manager)(nil)
	_ service.InteractiveRunner = (*Manager)(nil)
)

// New builds the engine. Nothing runs until Run.
func New(d Deps, o Options) *Manager {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	journal := events.NewJournal(d.Store)
	if d.Bus != nil {
		journal = d.Bus.Journal()
	}
	return &Manager{
		store: d.Store, accounts: d.Accounts, mailboxes: d.Mailboxes, bus: d.Bus, journal: journal,
		log: log.With("component", "sync"), opts: o.withDefaults(),
		wake:     make(chan struct{}, 1),
		workers:  map[string]*worker{},
		stopping: map[string]chan struct{}{},
		pending:  map[string]bool{},
		slots:    map[string]*slot{},
	}
}

// Run starts a worker for every account that may sync and keeps the set
// right until ctx ends: on Reconcile, and on a periodic re-read of who is
// eligible. It also prunes the event journal on a schedule. When ctx ends it
// stops every worker — IDLE ended, connections logged out — and returns once
// they have, or once Options.ShutdownTimeout has passed.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.running || m.closed {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	m.reconcileAll(ctx)
	m.prune(ctx)
	eligibility := time.NewTicker(m.opts.EligibilityInterval)
	defer eligibility.Stop()
	retention := time.NewTicker(m.opts.RetentionInterval)
	defer retention.Stop()

	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-m.wake:
			for _, id := range m.takePending() {
				m.reconcile(ctx, id)
			}
		case <-eligibility.C:
			m.reconcileAll(ctx)
		case <-retention.C:
			m.prune(ctx)
		}
	}
}

// Reconcile tells the engine an account's eligibility may have changed. It
// never blocks: the account is queued and Run looks at it.
func (m *Manager) Reconcile(accountID string) {
	if accountID == "" {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.pending[accountID] = true
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Trigger asks for a pass of every synced folder of the account soon, and a
// fresh look at its folder list. It does not wait, and it does not cut short
// a backoff: a person pressing "sync now" must not be able to hammer a
// server that asked to be left alone.
func (m *Manager) Trigger(_ context.Context, accountID string) error {
	w := m.worker(accountID)
	if w == nil {
		return service.ErrSyncNotRunning
	}
	w.trigger()
	return nil
}

func (m *Manager) worker(accountID string) *worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[accountID]
}

func (m *Manager) takePending() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.pending))
	for id := range m.pending {
		ids = append(ids, id)
	}
	m.pending = map[string]bool{}
	return ids
}

// reconcile starts or stops one account's worker to match whether it may
// sync now.
func (m *Manager) reconcile(ctx context.Context, id string) {
	eligible, err := m.store.SyncEligible(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("checking whether an account may sync failed", "account", id, "err", err)
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	w := m.workers[id]
	switch {
	case eligible && w == nil:
		m.startLocked(ctx, id)
	case eligible:
		// Something about it changed — a new grant, different folder
		// overrides, a state it came back from: look again now rather than
		// at the end of a backoff meant for the old situation.
		w.nudge()
	case w != nil:
		m.stopLocked(id, "no longer eligible")
	}
}

func (m *Manager) reconcileAll(ctx context.Context) {
	ids, err := m.store.SyncEligibleAccounts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("listing the accounts that may sync failed", "err", err)
		}
		return
	}
	eligible := make(map[string]bool, len(ids))
	for _, id := range ids {
		eligible[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	for id := range m.workers {
		if !eligible[id] {
			m.stopLocked(id, "no longer eligible")
		}
	}
	for _, id := range ids {
		if m.workers[id] == nil {
			m.startLocked(ctx, id)
		}
	}
}

// startLocked starts a worker. A worker for the same account still logging
// out is waited for before the new one dials: two sets of connections at once
// would break the budget.
func (m *Manager) startLocked(ctx context.Context, id string) {
	w := newWorker(ctx, m, id, m.stopping[id])
	m.workers[id] = w
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		w.run(w.ctx)
		m.mu.Lock()
		if m.stopping[id] == w.done {
			delete(m.stopping, id)
		}
		if m.workers[id] == w {
			// It ended without being told to — only a context ending does
			// that — so it is not running any more.
			delete(m.workers, id)
		}
		m.mu.Unlock()
	}()
	m.log.Info("account sync started", "account", id)
}

func (m *Manager) stopLocked(id, why string) {
	w := m.workers[id]
	if w == nil {
		return
	}
	delete(m.workers, id)
	w.cancel()
	m.stopping[id] = w.done
	m.log.Info("account sync stopped", "account", id, "why", why)
}

// shutdown stops every worker and waits for them, within a bound.
func (m *Manager) shutdown() {
	m.mu.Lock()
	m.closed = true
	for id := range m.workers {
		m.stopLocked(id, "shutting down")
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(m.opts.ShutdownTimeout):
		m.log.Error("sync workers did not stop in time; leaving them", "timeout", m.opts.ShutdownTimeout)
	}
}

// prune applies the event journal's retention — a week, and always the
// newest ten thousand, never an event a webhook has yet to deliver — and
// then takes what it deleted out of the write-ahead log: the events quote
// subjects and senders.
func (m *Manager) prune(ctx context.Context) {
	removed, err := m.journal.Prune(ctx, m.opts.Now(), events.DefaultRetention)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("pruning the event journal failed", "err", err)
		}
		return
	}
	if removed == 0 {
		return
	}
	m.log.Info("pruned the event journal", "removed", removed)
	if err := m.store.Scrub(ctx); err != nil && ctx.Err() == nil {
		m.log.Warn("emptying the write-ahead log after pruning failed; a later checkpoint will", "err", err)
	}
}

// commit runs fn in one writer transaction that first checks the account
// may still be synced, and publishes the events fn journaled once it has
// committed.
func (m *Manager) commit(ctx context.Context, accountID string, fn func(*sql.Tx) ([]events.Event, error)) error {
	var evs []events.Event
	err := m.store.Write(ctx, func(tx *sql.Tx) error {
		if err := store.RequireSyncEligibleTx(ctx, tx, accountID); err != nil {
			return err
		}
		var err error
		evs, err = fn(tx)
		return err
	})
	if err != nil {
		return err
	}
	m.publish(evs)
	return nil
}

func (m *Manager) publish(evs []events.Event) {
	if m.bus != nil && len(evs) > 0 {
		m.bus.Publish(evs...)
	}
}

// Status reports where an account's sync stands, whether a worker holds it
// or not: what is indexed is known either way.
func (m *Manager) Status(ctx context.Context, accountID string) (service.SyncStatus, error) {
	w := m.worker(accountID)
	sum, err := m.store.SyncSummary(ctx, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.SyncStatus{}, service.ErrSyncNotRunning
	}
	if err != nil {
		return service.SyncStatus{}, err
	}
	folders, err := m.store.Folders(ctx, accountID)
	if err != nil {
		return service.SyncStatus{}, err
	}
	if w == nil && len(folders) == 0 && sum.Messages == 0 {
		return service.SyncStatus{}, service.ErrSyncNotRunning
	}
	st := service.SyncStatus{
		Running:         w != nil,
		Tier:            sum.Tier,
		FoldersSynced:   sum.FoldersSynced,
		FoldersTotal:    sum.FoldersTotal,
		Messages:        sum.Messages,
		InitialProgress: sum.InitialProgress(),
		LastSyncedAt:    sum.LastOKAt,
		NextRetryAt:     sum.NextRetryAt,
	}
	if sum.ConsecutiveFailures > 0 {
		st.ErrorClass = sum.LastError
	}
	switch {
	case w == nil:
		st.State = "stopped"
		st.NextRetryAt = time.Time{}
	case w.backingOff():
		st.State = "backoff"
	case initialPending(folders):
		st.State = "initial"
	default:
		st.State = "live"
	}
	if w != nil {
		if tier := w.currentTier(); tier != "" {
			st.Tier = tier
		}
	}
	return st, nil
}

// initialPending reports whether any synced folder has not finished its
// first sync: never listed as synced yet, or still backfilling, or
// resyncing after a UIDVALIDITY change.
func initialPending(folders []store.Folder) bool {
	if len(folders) == 0 {
		return true
	}
	for _, f := range folders {
		if !f.Synced || !f.Selectable || !f.MissingSince.IsZero() {
			continue
		}
		if f.UIDValidity == 0 || f.BackfillCursor != 0 {
			return true
		}
		switch f.SyncState {
		case store.FolderStateNew, store.FolderStateInitial, store.FolderStateResync:
			return true
		}
	}
	return false
}
