package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// pass is one sync connection's run: its folder schedule and what it knows
// about the server. Everything here happens on the sync loop's goroutine,
// one command at a time, which is what the connection requires.
type pass struct {
	w       *worker
	m       *Manager
	s       provider.Session
	acct    account.Account
	mb      provider.Mailbox
	profile provider.Profile
	caps    provider.Caps
	tier    string

	sched         map[int64]*folderSched
	nextDiscovery time.Time
	// statuses is the last LIST-STATUS answer, by folder name.
	statuses map[string]provider.FolderStatus

	// Signals not yet applied to a folder: the inbox may not be listed yet.
	inboxDirty, inboxExpunge, allDirty bool

	lastProgressAt time.Time
	lastProgress   int
}

// folderSched is what the loop remembers about a folder between passes.
// Positions (UIDs, modseq, cursor) live in the database; this is only when
// to look next.
type folderSched struct {
	nextPass time.Time
	// dirty: something signalled a change; pass even if LIST-STATUS says
	// the counters did not move.
	dirty bool
	// expunge: an expunge was signalled or suspected; run the UID diff.
	expunge  bool
	nextDiff time.Time
	// confirmAt: a diff tombstoned rows; none runs again before this, so a
	// row is deleted only by a diff that much later than the one that first
	// missed it, however many signals arrive in between. That gap is what
	// lets a reassigned UID claim its row, and a search that came back short
	// be contradicted.
	confirmAt time.Time
	// nextFlagSweep: the uidpoll tier reads every flag window then.
	nextFlagSweep time.Time
	// backfilling: the initial sync has batches left.
	backfilling bool
}

func newPass(w *worker, s provider.Session, acct account.Account, mb provider.Mailbox, tier string) *pass {
	return &pass{
		w: w, m: w.m, s: s, acct: acct, mb: mb, caps: s.Caps(), tier: tier,
		// What the server turns out to be counts, not only what the account
		// row says: a Gmail mailbox connected as generic IMAP still has an
		// All Mail that must never be synced.
		profile: mb.Profile().ForServer(s.Caps(), acct.IMAPHost),
		sched:   map[int64]*folderSched{},
	}
}

func (p *pass) now() time.Time { return p.m.opts.Now() }

func (p *pass) schedOf(id int64) *folderSched {
	st := p.sched[id]
	if st == nil {
		st = &folderSched{}
		p.sched[id] = st
	}
	return st
}

// loop runs passes as they fall due until the connection fails or the worker
// stops. It returns only with an error.
func (p *pass) loop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-p.s.Closed():
			// Dropped while nothing was asking: reconnect now, not at the
			// next pass, and take the idle connection down with it. Why it
			// dropped decides how it is logged (failureLog).
			return fmt.Errorf("the sync connection closed: %w", p.s.CloseCause())
		default:
		}
		p.absorb()
		if !p.now().Before(p.nextDiscovery) {
			if err := p.discover(ctx); err != nil {
				return err
			}
		}
		folders, err := p.m.store.Folders(ctx, p.acct.ID)
		if err != nil {
			return err
		}
		p.applySignals(folders)
		due := p.due(folders)
		unchanged, err := p.skipUnchanged(ctx, due)
		if err != nil {
			return err
		}
		ran := false
		for i, f := range due {
			if unchanged[f.ID] {
				continue
			}
			st := p.schedOf(f.ID)
			err := p.folderPass(ctx, f, st)
			switch {
			case err == nil:
				ran = true
			case p.fatal(err):
				return err
			default:
				p.folderFailed(ctx, f, st, err)
			}
			// A signal for the inbox jumps the queue: new mail never waits
			// for a label's pass.
			if i+1 < len(due) && p.inboxJumps() {
				break
			}
		}
		if ran {
			p.w.recordOK(ctx)
		}
		if p.anyBackfilling() || p.pendingSignals() {
			continue
		}
		// The round changed folders' phases and schedules: sleep only if
		// nothing is due now — the next folder waiting for its initial sync,
		// one a resync sent back to the start.
		if folders, err = p.m.store.Folders(ctx, p.acct.ID); err != nil {
			return err
		}
		if len(p.due(folders)) > 0 {
			continue
		}
		p.sleep(ctx, folders)
	}
}

// absorb takes the worker's signals into the pass.
func (p *pass) absorb() {
	s := p.w.takeSignals()
	p.inboxDirty = p.inboxDirty || s.inbox
	p.inboxExpunge = p.inboxExpunge || s.inboxExpunge
	p.allDirty = p.allDirty || s.all
	if s.rediscover {
		p.nextDiscovery = time.Time{}
	}
}

// inboxJumps reports whether a signal for the inbox arrived during this
// round, so the round ends early and the inbox goes first in the next.
func (p *pass) inboxJumps() bool {
	p.absorb()
	return p.inboxDirty
}

func (p *pass) pendingSignals() bool {
	p.absorb()
	return p.inboxDirty || p.allDirty || p.nextDiscovery.IsZero()
}

// applySignals marks folders dirty for what was signalled.
func (p *pass) applySignals(folders []store.Folder) {
	for _, f := range folders {
		if !syncable(f) {
			continue
		}
		st := p.schedOf(f.ID)
		if p.allDirty {
			st.dirty = true
		}
		if f.Role == provider.RoleInbox {
			if p.inboxDirty {
				st.dirty = true
			}
			if p.inboxExpunge {
				st.dirty, st.expunge = true, true
			}
		}
	}
	// Discovery always runs before this, so a signal for an inbox that is
	// not in the index has nothing to apply to.
	p.inboxDirty, p.inboxExpunge, p.allDirty = false, false, false
}

func syncable(f store.Folder) bool {
	return f.Synced && f.Selectable && f.MissingSince.IsZero()
}

// rolePriority orders folders within a round: the inbox, then what a person
// wrote, then everything else, then what they threw away.
func rolePriority(r provider.FolderRole) int {
	switch r {
	case provider.RoleInbox:
		return 0
	case provider.RoleSent:
		return 1
	case provider.RoleDrafts:
		return 2
	case provider.RoleNone, provider.RoleArchive:
		return 3
	case provider.RoleJunk:
		return 4
	case provider.RoleTrash:
		return 5
	}
	return 6
}

// due lists the folders to pass now, in order. Initial syncs run one folder
// at a time, the inbox first, each newest first; live folders run when their
// timer, a signal or a scheduled diff says so.
func (p *pass) due(folders []store.Folder) []store.Folder {
	now := p.now()
	var live, initial []store.Folder
	for _, f := range folders {
		if !syncable(f) {
			continue
		}
		st := p.schedOf(f.ID)
		if phaseOf(f) != store.FolderStateLive {
			if st.dirty || !now.Before(st.nextPass) {
				initial = append(initial, f)
			}
			continue
		}
		if st.dirty || !now.Before(st.nextPass) || st.diffDue(now) {
			live = append(live, f)
		}
	}
	less := func(list []store.Folder) func(i, j int) bool {
		return func(i, j int) bool {
			a, b := rolePriority(list[i].Role), rolePriority(list[j].Role)
			if a != b {
				return a < b
			}
			return list[i].ID < list[j].ID
		}
	}
	sort.SliceStable(live, less(live))
	sort.SliceStable(initial, less(initial))
	out := live
	if len(initial) > 0 {
		// One initial sync at a time, in priority order; the others wait
		// their turn unless something signalled them. A folder waiting to
		// retry after a failure is not due, so it never holds the rest up.
		first := initial[0]
		out = append(out, first)
		for _, f := range initial[1:] {
			if p.schedOf(f.ID).dirty {
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, less(out))
	return out
}

// diffDue reports whether the UID diff should run now: signalled,
// suspected or scheduled, and not held for a tombstone's confirmation.
func (st *folderSched) diffDue(now time.Time) bool {
	if now.Before(st.confirmAt) {
		return false
	}
	return st.expunge || (!st.nextDiff.IsZero() && !now.Before(st.nextDiff))
}

// phaseOf is where a folder stands, reading through a recorded error: a
// folder whose last pass failed is still new, initial, resyncing or live.
func phaseOf(f store.Folder) string {
	if f.SyncState != store.FolderStateError {
		return f.SyncState
	}
	switch {
	case f.UIDValidity == 0:
		return store.FolderStateNew
	case f.BackfillCursor != 0:
		return store.FolderStateInitial
	}
	// A resync interrupted by the error is found by its stale rows when
	// the folder is passed (resumePhase).
	return store.FolderStateLive
}

func (p *pass) anyBackfilling() bool {
	for _, st := range p.sched {
		if st.backfilling {
			return true
		}
	}
	return false
}

// sleep waits until the next folder falls due, a signal arrives, or the
// worker stops.
func (p *pass) sleep(ctx context.Context, folders []store.Folder) {
	next := p.nextDiscovery
	for _, f := range folders {
		if !syncable(f) {
			continue
		}
		st := p.schedOf(f.ID)
		times := []time.Time{st.nextPass}
		if phaseOf(f) == store.FolderStateLive {
			// Diffs run only on live folders; due never picks another for one.
			times = append(times, st.nextDiff, st.confirmAt)
		}
		for _, t := range times {
			if !t.IsZero() && t.Before(next) {
				next = t
			}
		}
	}
	d := next.Sub(p.now())
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-p.w.signals:
	case <-p.s.Closed():
	}
}

// fatal reports whether an error ends the connection rather than one
// folder's pass.
func (p *pass) fatal(err error) bool {
	select {
	case <-p.s.Closed():
		return true
	default:
	}
	for _, target := range []error{
		context.Canceled, context.DeadlineExceeded, store.ErrNotEligible,
		provider.ErrConnClosed, provider.ErrAuthFailed, provider.ErrNeedsReauth, provider.ErrNotConnected,
		provider.ErrTooManyConnections, provider.ErrRateLimited,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// folderFailed records a folder whose pass failed and moves on: one folder
// that cannot be read never stops the others.
func (p *pass) folderFailed(ctx context.Context, f store.Folder, st *folderSched, err error) {
	class := provider.Class(err)
	p.m.log.Warn("folder sync failed; the other folders go on", "account", p.acct.ID, "folder", f.ID,
		"role", string(f.Role), "tier", p.tier, "class", class, "err", err, "server", provider.ServerReply(err))
	st.dirty, st.backfilling = false, false
	st.nextPass = p.now().Add(p.m.opts.FolderRetry)
	if errors.Is(err, provider.ErrFolderNotFound) {
		p.nextDiscovery = time.Time{}
	}
	state := store.FolderStateError
	werr := p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		return nil, store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{SyncState: &state, SyncError: &class})
	})
	if werr != nil && ctx.Err() == nil && !errors.Is(werr, store.ErrNotEligible) {
		p.m.log.Warn("recording a folder failure failed", "account", p.acct.ID, "folder", f.ID, "err", werr)
	}
}

// discover reads the folder list into the index.
func (p *pass) discover(ctx context.Context) error {
	// Overrides may have changed since the connection was opened.
	acct, err := p.m.accounts.Get(ctx, p.acct.ID)
	if err != nil {
		return err
	}
	p.acct = acct
	listed, err := p.s.ListFolders(ctx, p.caps.ListStatus)
	if err != nil {
		return err
	}
	now := p.now()
	var res store.DiscoveryResult
	err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		var err error
		res, err = p.m.store.SyncFolders(ctx, tx, store.FolderDiscovery{
			AccountID: p.acct.ID, Folders: listed, Profile: p.profile, Overrides: p.acct.FolderOverrides, Now: now,
		})
		return res.Events, err
	})
	if err != nil {
		return err
	}
	p.statuses = map[string]provider.FolderStatus{}
	for _, f := range listed {
		if f.Status != nil {
			p.statuses[f.Name] = *f.Status
		}
	}
	for _, id := range append(res.Removed, res.Unsynced...) {
		delete(p.sched, id)
	}
	if len(res.Removed)+len(res.Unsynced) > 0 {
		// Rows were deleted because a folder went away or stopped being
		// synced; take them out of the write-ahead log too.
		if err := p.m.store.Scrub(ctx); err != nil && ctx.Err() == nil {
			p.m.log.Warn("emptying the write-ahead log after dropping a folder failed; a later checkpoint will",
				"account", p.acct.ID, "err", err)
		}
	}
	p.nextDiscovery = now.Add(p.m.opts.DiscoveryInterval)
	return nil
}

// skipUnchanged spares a timer pass over a folder whose counters did not
// move, where the server can say so for every folder in one command
// (LIST-STATUS) and where the counters cover flag changes too (the condstore
// tier's HIGHESTMODSEQ). Signalled folders, initial syncs and due diffs are
// never skipped.
func (p *pass) skipUnchanged(ctx context.Context, due []store.Folder) (map[int64]bool, error) {
	if !p.caps.ListStatus || p.tier != TierCondStore {
		return nil, nil
	}
	now := p.now()
	var candidates []store.Folder
	for _, f := range due {
		st := p.schedOf(f.ID)
		if st.dirty || st.expunge || phaseOf(f) != store.FolderStateLive || f.SyncError != "" || st.diffDue(now) {
			continue
		}
		candidates = append(candidates, f)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	listed, err := p.s.ListFolders(ctx, true)
	if err != nil {
		return nil, err
	}
	statuses := map[string]provider.FolderStatus{}
	for _, f := range listed {
		if f.Status != nil {
			statuses[f.Name] = *f.Status
		}
	}
	p.statuses = statuses
	skip := map[int64]bool{}
	for _, f := range candidates {
		s, ok := statuses[f.Name]
		if !ok || s.UIDValidity != f.UIDValidity || s.UIDNext != f.UIDNext || s.NumMessages != f.ServerCount ||
			s.HighestModSeq == 0 || s.HighestModSeq != f.HighestModSeq {
			continue
		}
		skip[f.ID] = true
		p.schedOf(f.ID).nextPass = now.Add(p.interval(f))
	}
	return skip, nil
}

// interval is how long a folder waits between timer passes.
func (p *pass) interval(f store.Folder) time.Duration {
	o := p.m.opts
	switch f.Role {
	case provider.RoleInbox:
		if p.tier == TierCondStore && p.w.idleUp.Load() {
			return o.InboxInterval
		}
		return o.InboxPollInterval
	case provider.RoleSent, provider.RoleDrafts:
		return o.SentInterval
	case provider.RoleTrash:
		return o.TrashInterval
	case provider.RoleJunk:
		return o.JunkInterval
	}
	return o.OtherInterval
}

func (p *pass) diffInterval(f store.Folder) time.Duration {
	if f.Role == provider.RoleInbox {
		return p.m.opts.InboxDiffInterval
	}
	return p.m.opts.OtherDiffInterval
}

func (p *pass) flagSweepInterval(f store.Folder) time.Duration {
	if f.Role == provider.RoleInbox {
		return p.m.opts.InboxFlagSweep
	}
	return p.m.opts.OtherFlagSweep
}

// initialDays is how far back a folder's initial sync reaches, in days; zero
// or less is everything. A mailbox a person owns gets the 90 days the privacy
// policy they consented to promises, whatever the account row holds: the
// column is the operator's, for mailboxes nobody owns.
func (p *pass) initialDays() int {
	if p.acct.OwnerUserID != "" {
		return account.PersonInitialDays
	}
	return p.acct.InitialDays
}

// newWindow is the start of an initial window opened now, or zero for
// everything.
func (p *pass) newWindow() time.Time {
	days := p.initialDays()
	if days <= 0 {
		return time.Time{}
	}
	return p.now().AddDate(0, 0, -days)
}

// windowOf is the start of the folder's initial window, or zero for
// everything. It was fixed when the folder's initial sync started
// (startInitial), so it never slides with the clock: "the last 90 days" are
// the 90 days before sync was turned on, however long the backfill takes and
// whenever a later pass looks.
func (p *pass) windowOf(f store.Folder) time.Time {
	if !f.InitialSince.IsZero() || p.initialDays() <= 0 {
		return f.InitialSince
	}
	// Never recorded: the folder's initial sync has not started.
	return p.newWindow()
}

// folderPass brings one folder up to date.
func (p *pass) folderPass(ctx context.Context, f store.Folder, st *folderSched) error {
	forced := st.dirty
	st.dirty = false
	prevCount := f.ServerCount
	// What this pass sees is the folder as of its SELECT. Mail that reaches
	// the server after this is for the next pass to find, and to tell from
	// mail moved in by its date (store.ApplySummaries).
	started := p.now()

	sel, err := p.s.Select(ctx, f.Name, true, f.UIDValidity)
	if errors.Is(err, provider.ErrUIDValidityChanged) {
		return p.resync(ctx, f, sel, st)
	}
	if err != nil {
		return err
	}

	phase, err := p.resumePhase(ctx, f)
	if err != nil {
		return err
	}
	switch phase {
	case store.FolderStateResync:
		return p.resync(ctx, f, sel, st)
	case store.FolderStateNew:
		if f, err = p.startInitial(ctx, f, sel); err != nil {
			return err
		}
		phase = f.SyncState
		st.nextDiff = time.Time{}
	}

	added, err := p.fetchNew(ctx, f, sel, forced)
	if err != nil {
		return err
	}
	if f, err = p.m.store.Folder(ctx, p.acct.ID, f.ID); err != nil {
		return err
	}
	if err := p.syncFlags(ctx, f, sel, st); err != nil {
		return err
	}

	now := p.now()
	if phase == store.FolderStateInitial {
		done, err := p.backfill(ctx, f)
		if err != nil {
			return err
		}
		st.backfilling = !done
		if done {
			phase = store.FolderStateLive
			st.nextDiff = now.Add(p.diffInterval(f))
			st.nextFlagSweep = now.Add(p.flagSweepInterval(f))
		}
	} else {
		moved := prevCount != 0 && sel.NumMessages != prevCount+uint32(added) //nolint:gosec // G115: a count of UIDs fits
		if moved {
			st.expunge = true
		}
		if st.diffDue(now) {
			if f, err = p.m.store.Folder(ctx, p.acct.ID, f.ID); err != nil {
				return err
			}
			if err := p.diff(ctx, f, st); err != nil {
				return err
			}
		}
		if st.nextDiff.IsZero() {
			st.nextDiff = now.Add(p.diffInterval(f))
		}
	}

	if err := p.finishPass(ctx, f, sel, phase, started); err != nil {
		return err
	}
	if st.backfilling {
		st.nextPass = now
	} else {
		st.nextPass = now.Add(p.interval(f))
	}
	return nil
}

// resumePhase reads through a recorded error to the phase the folder is
// really in, including a resync the error interrupted.
func (p *pass) resumePhase(ctx context.Context, f store.Folder) (string, error) {
	phase := phaseOf(f)
	if f.SyncState == store.FolderStateError && phase == store.FolderStateLive {
		stale, err := p.m.store.HasStaleRows(ctx, f.ID)
		if err != nil {
			return "", err
		}
		if stale {
			return store.FolderStateResync, nil
		}
	}
	return phase, nil
}

// finishPass records where the folder stands after a pass that started at
// started.
func (p *pass) finishPass(ctx context.Context, f store.Folder, sel provider.FolderStatus, phase string, started time.Time) error {
	u := store.FolderSync{UIDNext: &sel.UIDNext, ServerCount: &sel.NumMessages}
	if phase == store.FolderStateLive {
		u.LastSyncedAt = &started
	}
	if f.SyncState != phase {
		u.SyncState = &phase
	}
	if f.SyncError != "" {
		empty := ""
		u.SyncError = &empty
	}
	return p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		return nil, store.UpdateFolderSync(ctx, tx, f.ID, u)
	})
}

// uidRange is lo:hi; hi zero is "*".
func uidRange(lo, hi imap.UID) imap.UIDSet {
	return imap.UIDSet{imap.UIDRange{Start: lo, Stop: hi}}
}

// uidSetOf compacts sorted-or-not UIDs into ranges: a literal list of two
// hundred sparse UIDs would still be a short command, and a contiguous run
// is one range.
func uidSetOf(uids []imap.UID) imap.UIDSet {
	if len(uids) == 0 {
		return nil
	}
	sorted := append([]imap.UID(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var set imap.UIDSet
	start, prev := sorted[0], sorted[0]
	for _, u := range sorted[1:] {
		if u == prev {
			continue
		}
		if u == prev+1 {
			prev = u
			continue
		}
		set = append(set, imap.UIDRange{Start: start, Stop: prev})
		start, prev = u, u
	}
	return append(set, imap.UIDRange{Start: start, Stop: prev})
}

// within keeps the UIDs in [lo, hi] (hi zero: no upper bound). Servers answer
// "n:*" with the highest UID even when it is below n — "*" is the largest
// UID in the mailbox and ranges are unordered — so every range ending in "*"
// is filtered before it is believed.
func within(uids []imap.UID, lo, hi imap.UID) []imap.UID {
	out := uids[:0:0]
	for _, u := range uids {
		if u >= lo && (hi == 0 || u <= hi) {
			out = append(out, u)
		}
	}
	return out
}
