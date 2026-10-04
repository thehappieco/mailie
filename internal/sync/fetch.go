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

// startInitial opens a folder's initial sync: it searches the window, and
// records where the folder stands — where the window starts, fixed from now
// on, UIDVALIDITY, the modification sequence to ask CHANGEDSINCE from, the
// lowest UID the window wants (the floor the expunge diff never looks
// below), the resume cursor, and the highest UID already on the server,
// which is where "new mail" starts.
//
// That mark is UIDNEXT-1, not the highest UID in the window: a message
// appended with an old date sits above the window's last UID, and counting
// from the window would announce it as new mail on the next pass.
func (p *pass) startInitial(ctx context.Context, f store.Folder, sel provider.FolderStatus) (store.Folder, error) {
	window := p.newWindow()
	uids, err := p.s.UIDs(ctx, uidRange(1, 0), window)
	if err != nil {
		return f, err
	}
	uids = sortedUnique(uids)
	floor := sel.UIDNext
	if floor == 0 {
		floor = 1
	}
	var maxSeen imap.UID
	if sel.UIDNext > 0 {
		maxSeen = sel.UIDNext - 1
	}
	var cursor imap.UID
	if n := len(uids); n > 0 {
		floor = uids[0]
		maxSeen = max(maxSeen, uids[n-1])
		cursor = uids[n-1] + 1 // nothing fetched yet
	}
	initial := store.FolderStateInitial
	total, fetched := len(uids), 0
	empty := ""
	now := p.now()
	err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		err := store.UpdateFolderSync(ctx, tx, f.ID, store.FolderSync{
			UIDValidity: &sel.UIDValidity, UIDNext: &sel.UIDNext, HighestModSeq: &sel.HighestModSeq,
			MaxSeenUID: &maxSeen, BackfillFloor: &floor, BackfillCursor: &cursor, ServerCount: &sel.NumMessages,
			SyncState: &initial, SyncError: &empty, InitialTotal: &total, InitialFetched: &fetched,
			InitialSince: &window,
		})
		if err != nil || len(uids) > 0 {
			return nil, err
		}
		// Nothing in the window: the folder is live at once.
		return p.m.store.FinishInitial(ctx, tx, p.acct.ID, f.ID, now)
	})
	if err != nil {
		return f, err
	}
	if len(uids) == 0 {
		p.progress(ctx, true)
	}
	return p.m.store.Folder(ctx, p.acct.ID, f.ID)
}

// backfill fetches up to Options.BackfillSlice batches of the initial window,
// newest first, each in its own transaction that also moves the resume
// cursor, and announces none of it: this is mail the person already has.
// Once the window is exhausted the folder goes live with one
// folder.changed{initial_done}. It reports whether the window is done.
func (p *pass) backfill(ctx context.Context, f store.Folder) (bool, error) {
	o := p.m.opts
	now := p.now()
	remaining := []imap.UID{}
	if f.BackfillCursor > f.BackfillFloor && f.BackfillCursor > 1 {
		found, err := p.s.UIDs(ctx, uidRange(f.BackfillFloor, f.BackfillCursor-1), p.windowOf(f))
		if err != nil {
			return false, err
		}
		remaining = within(sortedUnique(found), f.BackfillFloor, f.BackfillCursor-1)
	}
	// Newest first.
	sort.Slice(remaining, func(i, j int) bool { return remaining[i] > remaining[j] })
	for b := 0; b < o.BackfillSlice && len(remaining) > 0; b++ {
		n := min(o.BatchSize, len(remaining))
		batch := remaining[:n]
		remaining = remaining[n:]
		sums, mark, err := p.fetch(ctx, batch)
		if err != nil {
			return false, err
		}
		lowest := batch[n-1]
		err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
			res, err := p.m.store.ApplySummaries(ctx, tx, store.SummaryBatch{
				AccountID: p.acct.ID, FolderID: f.ID, UIDValidity: f.UIDValidity, Mode: store.ApplyQuiet,
				Summaries: sums, BackfillCursor: &lowest, ActionMark: mark, Now: now,
			})
			return res.Events, err
		})
		if err != nil {
			return false, err
		}
		p.progress(ctx, false)
	}
	if len(remaining) > 0 {
		return false, nil
	}
	err := p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		return p.m.store.FinishInitial(ctx, tx, p.acct.ID, f.ID, now)
	})
	if err != nil {
		return false, err
	}
	p.m.log.Info("folder initial sync finished", "account", p.acct.ID, "folder", f.ID, "role", string(f.Role))
	p.progress(ctx, true)
	return true, nil
}

// fetchNew fetches what arrived above the folder's high-water mark, oldest
// first, and announces it. It reports how many UIDs arrived.
//
// The search is skipped when UIDNEXT has not moved and the count is what it
// was — unless something signalled the folder: an IDLE EXISTS may come
// before UIDNEXT is updated on some servers.
func (p *pass) fetchNew(ctx context.Context, f store.Folder, sel provider.FolderStatus, forced bool) (int, error) {
	mark := f.MaxSeenUID
	if !forced && sel.UIDNext != 0 && sel.UIDNext <= mark+1 && sel.NumMessages == f.ServerCount {
		return 0, nil
	}
	found, err := p.s.UIDs(ctx, uidRange(mark+1, 0), time.Time{})
	if err != nil {
		return 0, err
	}
	arrived := within(sortedUnique(found), mark+1, 0)
	if len(arrived) == 0 {
		return 0, nil
	}
	return len(arrived), p.fetchAndApply(ctx, f, arrived, store.ApplyLive)
}

// fetchAndApply fetches summaries for UIDs the index lacks, in batches
// oldest first, and writes each batch in its own transaction. A live batch
// first looks for rows the server moved to a new UID (Exchange reassigns
// them), so a message it already knows is not announced as new.
func (p *pass) fetchAndApply(ctx context.Context, f store.Folder, uids []imap.UID, mode store.ApplyMode) error {
	uids = sortedUnique(uids)
	for len(uids) > 0 {
		n := min(p.m.opts.BatchSize, len(uids))
		batch := uids[:n]
		uids = uids[n:]
		sums, mark, err := p.fetch(ctx, batch)
		if err != nil {
			return err
		}
		if len(sums) == 0 {
			continue
		}
		var reclaim map[imap.UID]int64
		if mode == store.ApplyLive {
			if reclaim, err = p.reclaims(ctx, f, sums); err != nil {
				return err
			}
		}
		now := p.now()
		err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
			res, err := p.m.store.ApplySummaries(ctx, tx, store.SummaryBatch{
				AccountID: p.acct.ID, FolderID: f.ID, UIDValidity: f.UIDValidity, Mode: mode,
				Summaries: sums, Reclaim: reclaim, ActionMark: mark, Now: now,
			})
			return res.Events, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// reclaims confirms which summaries are messages the index holds under an
// older UID in the same folder. A candidate is only a candidate — a genuine
// duplicate looks the same — so the old UID is fetched: an empty answer
// means it is gone and the row follows its message. A row an expunge diff
// already found missing needs no asking.
func (p *pass) reclaims(ctx context.Context, f store.Folder, sums []provider.Summary) (map[imap.UID]int64, error) {
	cands, err := p.m.store.ReclaimCandidates(ctx, f.ID, f.UIDValidity, sums)
	if err != nil || len(cands) == 0 {
		return nil, err
	}
	out := map[imap.UID]int64{}
	taken := map[int64]bool{}
	for _, c := range cands {
		if _, done := out[c.UID]; done || taken[c.RowID] {
			continue
		}
		gone := c.Vanished
		if !gone {
			ups, err := p.s.FetchFlags(ctx, imap.UIDSetNum(c.OldUID), 0)
			if err != nil {
				return nil, err
			}
			gone = true
			for _, u := range ups {
				if u.UID == c.OldUID {
					gone = false
				}
			}
		}
		if gone {
			out[c.UID] = c.RowID
			taken[c.RowID] = true
		}
	}
	return out, nil
}

// fetch reads summaries for a set of UIDs, and says where the history of
// actions stood before it asked (store.ActionMark): flags a person's action
// set after that are newer than the summaries, which must not undo them.
func (p *pass) fetch(ctx context.Context, uids []imap.UID) ([]provider.Summary, uint64, error) {
	want := make(map[imap.UID]bool, len(uids))
	for _, u := range uids {
		want[u] = true
	}
	mark := p.m.store.ActionMark()
	var sums []provider.Summary
	err := p.s.FetchSummaries(ctx, uidSetOf(uids), 0, func(s provider.Summary) error {
		if want[s.UID] {
			sums = append(sums, s)
		}
		return nil
	})
	return sums, mark, err
}

// progress journals sync.progress when the initial sync has moved, at most
// every Options.ProgressEvery unless a folder just finished.
func (p *pass) progress(ctx context.Context, force bool) {
	now := p.now()
	if !force && now.Sub(p.lastProgressAt) < p.m.opts.ProgressEvery {
		return
	}
	sum, err := p.m.store.SyncSummary(ctx, p.acct.ID)
	if err != nil {
		return
	}
	pct := sum.InitialProgress()
	if pct == p.lastProgress && !p.lastProgressAt.IsZero() {
		return
	}
	p.lastProgressAt, p.lastProgress = now, pct
	err = p.m.commit(ctx, p.acct.ID, func(tx *sql.Tx) ([]events.Event, error) {
		ev, err := events.New(events.TypeSyncProgress, p.acct.ID, now, store.SyncProgress{
			AccountID: p.acct.ID, Progress: pct, FoldersSynced: sum.FoldersSynced, FoldersTotal: sum.FoldersTotal,
			Messages: sum.Messages,
		})
		if err != nil {
			return nil, err
		}
		return p.m.journal.Append(ctx, tx, []events.Event{ev})
	})
	if err != nil && ctx.Err() == nil {
		p.m.log.Debug("journaling sync progress failed", "account", p.acct.ID, "err", err)
	}
}

func sortedUnique(uids []imap.UID) []imap.UID {
	out := append([]imap.UID(nil), uids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	w := 0
	for _, u := range out {
		if u == 0 || (w > 0 && out[w-1] == u) {
			continue
		}
		out[w] = u
		w++
	}
	return out[:w]
}
