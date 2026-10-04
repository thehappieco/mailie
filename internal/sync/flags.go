package sync

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
)

// indexedFloor is the lowest UID whose flags the index answers for: the
// backfill cursor while the initial sync runs (below it nothing is fetched
// yet, and a flag fetch would only find rows it does not hold), the floor
// once it is done.
func indexedFloor(f store.Folder) imap.UID {
	lo := f.BackfillFloor
	if f.BackfillCursor != 0 {
		lo = f.BackfillCursor
	}
	return max(lo, 1)
}

// syncFlags brings the folder's flags up to date, by the account's tier.
func (p *pass) syncFlags(ctx context.Context, f store.Folder, sel provider.FolderStatus, st *folderSched) error {
	if p.tier == TierCondStore && sel.HighestModSeq > 0 {
		return p.condstoreFlags(ctx, f, sel)
	}
	return p.pollFlags(ctx, f, st)
}

// condstoreFlags asks for what changed since the folder's recorded
// modification sequence. The new sequence is recorded only once the fetch
// has come back whole; a fetch that fails halfway is asked again from the
// old one.
func (p *pass) condstoreFlags(ctx context.Context, f store.Folder, sel provider.FolderStatus) error {
	if f.HighestModSeq != 0 && sel.HighestModSeq <= f.HighestModSeq {
		return nil
	}
	lo := indexedFloor(f)
	mark := p.m.store.ActionMark()
	ups, err := p.s.FetchFlags(ctx, uidRange(lo, 0), f.HighestModSeq)
	if err != nil {
		return err
	}
	ups = flagsWithin(ups, lo, 0)
	return p.applyFlags(ctx, f, ups, sel.HighestModSeq, mark)
}

// pollFlags reads flags in windows of indexed UIDs, newest first: the newest
// window on every pass, all of them on the sweep schedule or when an expunge
// is suspected. An indexed UID missing from a window's answer is a message
// the server no longer has, so the diff runs.
func (p *pass) pollFlags(ctx context.Context, f store.Folder, st *folderSched) error {
	lo := indexedFloor(f)
	local, err := p.m.store.LocalUIDs(ctx, f.ID, f.UIDValidity, lo, 0, false)
	if err != nil || len(local) == 0 {
		return err
	}
	sort.Slice(local, func(i, j int) bool { return local[i] > local[j] })
	now := p.now()
	sweep := st.expunge || !now.Before(st.nextFlagSweep)
	size := p.m.opts.FlagWindow
	for start := 0; start < len(local); start += size {
		if start > 0 && !sweep {
			break
		}
		window := local[start:min(start+size, len(local))]
		top, bottom := window[0], window[len(window)-1]
		hi := top
		if start == 0 {
			hi = 0 // the newest window is open-ended: whatever arrived is in it too
		}
		mark := p.m.store.ActionMark()
		ups, err := p.s.FetchFlags(ctx, uidRange(bottom, hi), 0)
		if err != nil {
			return err
		}
		ups = flagsWithin(ups, bottom, hi)
		present := make(map[imap.UID]bool, len(ups))
		for _, u := range ups {
			present[u.UID] = true
		}
		for _, uid := range window {
			if !present[uid] {
				st.expunge = true
				break
			}
		}
		if err := p.applyFlags(ctx, f, ups, 0, mark); err != nil {
			return err
		}
	}
	if sweep {
		st.nextFlagSweep = now.Add(p.flagSweepInterval(f))
		return p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
			return nil, store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{LastFlagScanAt: &now})
		})
	}
	return nil
}

// applyFlags writes a flag answer and fetches whatever it named that the
// index does not hold: above the high-water mark that is new mail, below it
// a gap the window missed. mark is store.ActionMark as it was before the
// answer was fetched.
func (p *pass) applyFlags(ctx context.Context, f store.Folder, ups []provider.FlagUpdate, modseq, mark uint64) error {
	if len(ups) == 0 && modseq == 0 {
		return nil
	}
	now := p.now()
	var res store.FlagResult
	err := p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		var err error
		res, err = p.m.store.ApplyFlags(ctx, tx, store.FlagBatch{
			AccountID: p.acct.ID, FolderID: f.ID, UIDValidity: f.UIDValidity, Updates: ups, HighestModSeq: modseq,
			ActionMark: mark, Now: now,
		})
		return res.Events, err
	})
	if err != nil {
		return err
	}
	return p.fetchUnknown(ctx, f, res.Unknown)
}

// fetchUnknown fetches UIDs the server has and the index lacks.
//
// Above the high-water mark they arrived after sync was turned on, whatever
// their date: new mail, or old mail put back in the folder, which the person
// has just touched. Below it they are gaps between UIDs the index holds —
// mostly old mail sitting among the window's UIDs: imported newest first, or
// moved, labelled or restored into the folder before sync started. Those are
// kept only when they are inside the folder's initial window, which is all
// the person agreed to have indexed from before: the server is asked which
// are, with the same SINCE the initial sync used.
func (p *pass) fetchUnknown(ctx context.Context, f store.Folder, uids []imap.UID) error {
	if len(uids) == 0 {
		return nil
	}
	// The mark may have moved since f was read.
	cur, err := p.m.store.Folder(ctx, p.acct.ID, f.ID)
	if err != nil {
		return err
	}
	var arrived, gaps []imap.UID
	for _, u := range uids {
		switch {
		case u > cur.MaxSeenUID:
			arrived = append(arrived, u)
		case u >= indexedFloor(cur):
			gaps = append(gaps, u)
		}
	}
	if err := p.fetchAndApply(ctx, cur, arrived, store.ApplyLive); err != nil {
		return err
	}
	if gaps, err = p.inWindow(ctx, cur, gaps); err != nil {
		return err
	}
	return p.fetchAndApply(ctx, cur, gaps, store.ApplyQuiet)
}

// inWindow keeps the UIDs whose messages are inside the folder's initial
// window, by asking the server. A gap outside it is found again by every
// diff and every flag fetch that covers it, and costs this one short search
// each time.
func (p *pass) inWindow(ctx context.Context, f store.Folder, uids []imap.UID) ([]imap.UID, error) {
	window := p.windowOf(f)
	if len(uids) == 0 || window.IsZero() {
		return uids, nil
	}
	found, err := p.s.UIDs(ctx, uidSetOf(uids), window)
	if err != nil {
		return nil, err
	}
	asked := make(map[imap.UID]bool, len(uids))
	for _, u := range uids {
		asked[u] = true
	}
	var out []imap.UID
	for _, u := range sortedUnique(found) {
		if asked[u] {
			out = append(out, u)
		}
	}
	return out, nil
}

func flagsWithin(ups []provider.FlagUpdate, lo, hi imap.UID) []provider.FlagUpdate {
	out := ups[:0:0]
	for _, u := range ups {
		if u.UID >= lo && (hi == 0 || u.UID <= hi) {
			out = append(out, u)
		}
	}
	return out
}

// diff compares every UID the server has above the floor with the index,
// both ways. Rows it misses are tombstoned the first time and deleted the
// second consecutive time; a confirmation diff is scheduled soon after a
// tombstone so a deletion takes about that long, not a whole interval. UIDs
// the server has and the index lacks are fetched.
func (p *pass) diff(ctx context.Context, f store.Folder, st *folderSched) error {
	floor := max(f.BackfillFloor, 1)
	mark := f.MaxSeenUID
	found, err := p.s.UIDs(ctx, uidRange(floor, 0), time.Time{})
	if err != nil {
		return err
	}
	server := within(sortedUnique(found), floor, 0)
	now := p.now()
	var res store.DiffResult
	err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		var err error
		res, err = p.m.store.ApplyUIDDiff(ctx, tx, store.UIDDiff{
			AccountID: p.acct.ID, FolderID: f.ID, UIDValidity: f.UIDValidity, Floor: floor, Upto: mark,
			ServerUIDs: server, Now: now,
		})
		return res.Events, err
	})
	if err != nil {
		return err
	}
	st.expunge = false
	if res.Tombstoned > 0 {
		st.confirmAt = now.Add(p.m.opts.ConfirmDiffDelay)
		st.nextDiff = st.confirmAt
	} else {
		st.confirmAt = time.Time{}
		st.nextDiff = now.Add(p.diffInterval(f))
	}
	return p.fetchUnknown(ctx, f, res.Missing)
}

// resync follows a folder whose UIDVALIDITY changed — Exchange does this
// unprompted — without deleting and re-inserting: every row is marked stale
// and stays visible, the folder is searched from its oldest indexed message
// (not from the initial window, or everything older would be deleted at the
// end), each summary claims its stale row by content, and only what nothing
// claimed is deleted, and announced, at the end. Known messages keep their
// ids and announce nothing.
//
// What the search finds outside the folder's initial window is there only to
// claim rows the index held — old mail that arrived after sync was turned on
// — and is never stored otherwise: a resync must not widen what is kept.
func (p *pass) resync(ctx context.Context, f store.Folder, sel provider.FolderStatus, st *folderSched) error {
	// What the folder is now. A resync resumed after an interruption finds
	// it "resync" or "error"; BeginResync keeps what the first attempt
	// recorded instead.
	from := store.ResyncFromInitial
	if phaseOf(f) == store.FolderStateLive {
		from = store.ResyncFromLive
	}
	now := p.now()
	var start store.ResyncStart
	err := p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		var err error
		if start, err = p.m.store.BeginResync(ctx, tx, p.acct.ID, f.ID, sel, from); err != nil {
			return nil, err
		}
		if f.LastSyncedAt.IsZero() {
			// Never finished a pass: nothing unclaimed is newer than what the
			// person had, so nothing is announced as new.
			return nil, store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{LastSyncedAt: &now})
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	p.m.log.Info("folder uidvalidity changed; resyncing in place", "account", p.acct.ID, "folder", f.ID,
		"role", string(f.Role))
	if start.Oldest.IsZero() {
		// Nothing indexed to keep: start the folder over.
		state, none := store.FolderStateNew, ""
		st.dirty = true
		return p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
			return nil, store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{SyncState: &state, ResyncFrom: &none})
		})
	}
	uids, claimOnly, err := p.resyncUIDs(ctx, f, start)
	if err != nil {
		return err
	}
	for i := len(uids); i > 0; {
		lo := max(0, i-p.m.opts.BatchSize)
		batch := uids[lo:i]
		i = lo
		sums, mark, err := p.fetch(ctx, batch)
		if err != nil {
			return err
		}
		err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
			res, err := p.m.store.ApplySummaries(ctx, tx, store.SummaryBatch{
				AccountID: p.acct.ID, FolderID: f.ID, UIDValidity: sel.UIDValidity, Mode: store.ApplyResync,
				Summaries: sums, ClaimOnly: claimOnly, ActionMark: mark, Now: now,
			})
			return res.Events, err
		})
		if err != nil {
			return err
		}
	}
	floor := max(sel.UIDNext, 1)
	var mark imap.UID
	if sel.UIDNext > 0 {
		mark = sel.UIDNext - 1
	}
	if n := len(uids); n > 0 {
		floor, mark = uids[0], max(mark, uids[n-1])
	}
	zero := imap.UID(0)
	err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		if err := store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{
			BackfillFloor: &floor, BackfillCursor: &zero, MaxSeenUID: &mark, UIDNext: &sel.UIDNext,
			ServerCount: &sel.NumMessages, HighestModSeq: &sel.HighestModSeq,
		}); err != nil {
			return nil, err
		}
		return p.m.store.FinishResync(ctx, tx, p.acct.ID, f.ID, now)
	})
	if err != nil {
		return err
	}
	st.backfilling = false
	st.nextPass = now.Add(p.interval(f))
	st.nextDiff = now.Add(p.diffInterval(f))
	return nil
}

// resyncUIDs searches a resyncing folder: every UID that may claim a stale
// row, and which of them are outside the folder's initial window and may only
// claim one.
//
// The search starts a day before the oldest stale row. SINCE compares
// calendar dates — in the server's zone or the message's, never more than a
// day from UTC's — so a search from the UTC date of that INTERNALDATE could
// leave the message itself out, and the resync would delete it as gone. A
// folder whose initial sync had not finished is searched over its whole
// window too: this resync is what finishes it.
func (p *pass) resyncUIDs(ctx context.Context, f store.Folder, start store.ResyncStart) ([]imap.UID, map[imap.UID]bool, error) {
	window := p.windowOf(f)
	since := start.Oldest.AddDate(0, 0, -1)
	switch {
	case start.From == store.ResyncFromInitial && window.IsZero():
		since = time.Time{} // the window is everything
	case !window.IsZero() && window.Before(since):
		since = window
	}
	found, err := p.s.UIDs(ctx, uidRange(1, 0), since)
	if err != nil {
		return nil, nil, err
	}
	uids := sortedUnique(found)
	if window.IsZero() || !since.Before(window) {
		return uids, nil, nil
	}
	inside, err := p.s.UIDs(ctx, uidRange(1, 0), window)
	if err != nil {
		return nil, nil, err
	}
	in := make(map[imap.UID]bool, len(inside))
	for _, u := range inside {
		in[u] = true
	}
	claimOnly := map[imap.UID]bool{}
	for _, u := range uids {
		if !in[u] {
			claimOnly[u] = true
		}
	}
	return uids, claimOnly, nil
}
