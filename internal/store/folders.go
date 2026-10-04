package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
)

// The folder index: what LIST said, what each folder is for, and where the
// sync engine stands in each one. Folder sync states, as stored.
const (
	FolderStateNew      = "new"
	FolderStateInitial  = "initial"
	FolderStateLive     = "live"
	FolderStateResync   = "resync"
	FolderStateError    = "error"
	FolderStateDisabled = "disabled"
)

// ErrNoFolder is a folder id that is not in the index, or not the account's.
var ErrNoFolder = errors.New("store: no such folder")

// Folder is one row of the folder index.
type Folder struct {
	ID          int64
	AccountID   string
	Name        string // exactly as LIST returned it: what SELECT takes
	DisplayName string
	Delim       string
	Attrs       []string
	Role        provider.FolderRole
	RoleSource  string
	Selectable  bool
	// Synced is whether the engine indexes this folder at all.
	Synced bool
	// Passive folders are never synced but hold rows moved there by us.
	Passive bool

	UIDValidity   uint32
	UIDNext       imap.UID
	MaxSeenUID    imap.UID // highest UID ever indexed under UIDValidity; never goes down
	HighestModSeq uint64
	// BackfillFloor is the lowest UID the initial window wanted; the expunge
	// diff ignores everything below it.
	BackfillFloor imap.UID
	// BackfillCursor is the lowest UID the initial sync has fetched so far;
	// zero once it is done.
	BackfillCursor    imap.UID
	LastFullUIDScanAt time.Time
	LastFlagScanAt    time.Time
	ServerCount       uint32
	LocalCount        int // live rows, maintained by triggers
	UnseenCount       int
	SyncState         string
	SyncError         string
	LastSyncedAt      time.Time
	MissingSince      time.Time
	InitialTotal      int
	InitialFetched    int
	// InitialSince is where the folder's initial window starts, fixed when
	// its initial sync started; zero when it has none (everything, or not
	// started).
	InitialSince time.Time
	// ResyncFrom is set while a UIDVALIDITY resync is in progress: what the
	// folder was when it began, ResyncFromLive or ResyncFromInitial.
	ResyncFrom string
}

// What a folder was when its resync began.
const (
	ResyncFromLive    = "live"
	ResyncFromInitial = "initial"
)

const folderColumns = `id, account_id, name, display_name, delim, attrs_json, role, role_source, selectable,
	synced, passive, uidvalidity, uidnext, max_seen_uid, highest_modseq, backfill_floor, backfill_cursor,
	last_full_uid_scan_at, last_flag_scan_at, server_count, local_count, unseen_count, sync_state, sync_error,
	last_synced_at, missing_since, initial_total, initial_fetched, initial_since, resync_from`

func scanFolder(row interface{ Scan(...any) error }) (Folder, error) {
	var (
		f                                               Folder
		attrs, role                                     string
		selectable, synced, passive                     int
		fullScan, flagScan, lastSynced, missing, modseq int64
		initialSince                                    int64
	)
	err := row.Scan(&f.ID, &f.AccountID, &f.Name, &f.DisplayName, &f.Delim, &attrs, &role, &f.RoleSource,
		&selectable, &synced, &passive, &f.UIDValidity, &f.UIDNext, &f.MaxSeenUID, &modseq,
		&f.BackfillFloor, &f.BackfillCursor, &fullScan, &flagScan, &f.ServerCount, &f.LocalCount,
		&f.UnseenCount, &f.SyncState, &f.SyncError, &lastSynced, &missing, &f.InitialTotal, &f.InitialFetched,
		&initialSince, &f.ResyncFrom)
	if err != nil {
		return Folder{}, err
	}
	f.Role = provider.FolderRole(role)
	f.Selectable, f.Synced, f.Passive = selectable != 0, synced != 0, passive != 0
	f.HighestModSeq = uint64(modseq) //nolint:gosec // G115: stored from a uint64 that fits
	f.LastFullUIDScanAt, f.LastFlagScanAt = unixTime(fullScan), unixTime(flagScan)
	f.LastSyncedAt, f.MissingSince = unixTime(lastSynced), unixTime(missing)
	f.InitialSince = unixTime(initialSince)
	if err := json.Unmarshal([]byte(attrs), &f.Attrs); err != nil {
		f.Attrs = nil
	}
	return f, nil
}

// Folders reads an account's folder index, in id order.
func (s *Store) Folders(ctx context.Context, accountID string) ([]Folder, error) {
	return foldersOn(ctx, s.r, accountID)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func foldersOn(ctx context.Context, q querier, accountID string) ([]Folder, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+folderColumns+` FROM folders WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list folders: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list folders: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list folders: %w", err)
	}
	return out, nil
}

// Folder reads one folder of an account.
func (s *Store) Folder(ctx context.Context, accountID string, folderID int64) (Folder, error) {
	return folderOn(ctx, s.r, accountID, folderID)
}

func folderOn(ctx context.Context, q querier, accountID string, folderID int64) (Folder, error) {
	f, err := scanFolder(q.QueryRowContext(ctx,
		`SELECT `+folderColumns+` FROM folders WHERE id = ? AND account_id = ?`, folderID, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return Folder{}, fmt.Errorf("%w: %d", ErrNoFolder, folderID)
	}
	if err != nil {
		return Folder{}, fmt.Errorf("store: read folder: %w", err)
	}
	return f, nil
}

// FolderListSynced reports whether the account's folder list has been
// discovered into the index. Until it has, the folder listing asks the server.
func (s *Store) FolderListSynced(ctx context.Context, accountID string) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM folders WHERE account_id = ? LIMIT 1`, accountID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check the folder index: %w", err)
	}
	return true, nil
}

// FolderDiscovery is one complete, successful LIST of an account's folders.
type FolderDiscovery struct {
	AccountID string
	Folders   []provider.Folder
	Profile   provider.Profile
	// Overrides is the account's folder_overrides: role -> folder name.
	Overrides map[string]string
	Now       time.Time
}

// DiscoveryResult is what a discovery changed.
type DiscoveryResult struct {
	// Folders is the index after the discovery, in id order.
	Folders []Folder
	Added   []int64
	Updated []int64
	// Missing are folders absent from this LIST for the first time. They keep
	// their rows until the next discovery confirms the absence.
	Missing []int64
	// Removed are folders absent twice in a row, now deleted with their rows.
	Removed []int64
	// Unsynced are folders that were synced and no longer are — an override
	// or an attribute now rules them out, or they stopped being selectable.
	// Their rows are deleted and their sync position reset.
	Unsynced []int64
	// Events were journaled in the transaction; publish them after it commits.
	Events []events.Event
}

type resolvedFolder struct {
	provider.Folder
	role   provider.FolderRole
	source string
}

// SyncFolders records a LIST in the folder index, inside the caller's
// transaction.
//
// Roles are resolved as the live listing resolves them (provider.ResolveRole),
// and then made unique: at most one folder per role, archive excepted, which
// is what the index's unique constraint says too. When two folders claim one
// role — a localised name table matching twice, a folder named "Sent" beside
// the one the server marks \Sent — the stronger source keeps it (override,
// then the inbox name, then SPECIAL-USE, then the name table) and a tie goes
// to the name that sorts first, so the answer never depends on LIST order.
//
// A folder absent from the LIST is marked missing the first time and deleted,
// with its rows, only the second consecutive time: a LIST that raced a rename
// must not throw a folder's index away. Deleting a folder announces each of
// its messages as deleted, or as a copy removed when another folder still
// holds it. A folder that was synced and no longer is loses its rows the same
// way at once, and keeps its row. Call this only with a LIST that completed
// without error.
func (s *Store) SyncFolders(ctx context.Context, tx *sql.Tx, d FolderDiscovery) (DiscoveryResult, error) {
	if err := RequireSyncEligibleTx(ctx, tx, d.AccountID); err != nil {
		return DiscoveryResult{}, err
	}
	now := d.Now
	if now.IsZero() {
		now = s.now()
	}

	resolved := resolveUnique(d.Folders, d.Profile, d.Overrides)
	existing, err := foldersOn(ctx, tx, d.AccountID)
	if err != nil {
		return DiscoveryResult{}, err
	}
	byName := make(map[string]Folder, len(existing))
	for _, f := range existing {
		byName[f.Name] = f
	}
	listed := make(map[string]bool, len(resolved))
	claimed := map[provider.FolderRole]bool{}
	for _, r := range resolved {
		listed[r.Name] = true
		if r.role != provider.RoleNone {
			claimed[r.role] = true
		}
	}

	// Roles are unique per account and SQLite checks that per statement, so
	// every role that is about to move is let go of first: a folder losing
	// "sent" to a newly listed one must not collide with it on the way.
	for _, f := range existing {
		var next provider.FolderRole
		if listed[f.Name] {
			next = findResolved(resolved, f.Name).role
		} else if claimed[f.Role] {
			next = provider.RoleNone // missing, and somebody listed has its role now
		} else {
			continue
		}
		if f.Role != next && f.Role != provider.RoleNone {
			if _, err := tx.ExecContext(ctx, `UPDATE folders SET role = '', role_source = '' WHERE id = ?`, f.ID); err != nil {
				return DiscoveryResult{}, fmt.Errorf("store: release a folder role: %w", err)
			}
		}
	}

	var (
		res    DiscoveryResult
		evs    []events.Event
		nowSec = now.Unix()
	)
	for _, r := range resolved {
		attrs := make([]string, 0, len(r.Attrs))
		for _, a := range r.Attrs {
			attrs = append(attrs, string(a))
		}
		attrsJSON, err := json.Marshal(attrs)
		if err != nil {
			return DiscoveryResult{}, fmt.Errorf("store: encode folder attributes: %w", err)
		}
		delim := ""
		if r.Delim != 0 {
			delim = string(r.Delim)
		}
		synced := d.Profile.SyncsFolder(r.Folder, r.role)
		display := displayName(r.Name, r.Delim)

		prev, had := byName[r.Name]
		var id int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO folders(account_id, name, display_name, delim, attrs_json, role, role_source, selectable, synced)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(account_id, name) DO UPDATE SET
			   display_name = excluded.display_name, delim = excluded.delim, attrs_json = excluded.attrs_json,
			   role = excluded.role, role_source = excluded.role_source, selectable = excluded.selectable,
			   synced = excluded.synced, missing_since = 0
			 RETURNING id`,
			d.AccountID, r.Name, display, delim, string(attrsJSON), string(r.role), r.source,
			boolInt(r.Selectable), boolInt(synced)).Scan(&id)
		if err != nil {
			return DiscoveryResult{}, fmt.Errorf("store: record folder: %w", err)
		}
		if had && prev.Synced && prev.Selectable && (!synced || !r.Selectable) {
			f := prev
			f.Role = r.role
			gone, err := s.unsyncFolderTx(ctx, tx, f, now)
			if err != nil {
				return DiscoveryResult{}, err
			}
			evs = append(evs, gone...)
			res.Unsynced = append(res.Unsynced, id)
		}
		change := ""
		switch {
		case !had:
			change = FolderAdded
			res.Added = append(res.Added, id)
		case prev.Role != r.role || prev.Selectable != r.Selectable || prev.Synced != synced:
			change = FolderUpdated
			res.Updated = append(res.Updated, id)
		}
		if change != "" {
			ev, err := events.New(events.TypeFolderChanged, d.AccountID, now, FolderChanged{
				AccountID: d.AccountID, FolderID: id, Name: r.Name, Role: string(r.role), Change: change,
			})
			if err != nil {
				return DiscoveryResult{}, err
			}
			evs = append(evs, ev)
		}
	}

	for _, f := range existing {
		if listed[f.Name] {
			continue
		}
		if f.MissingSince.IsZero() {
			if _, err := tx.ExecContext(ctx, `UPDATE folders SET missing_since = ? WHERE id = ?`, nowSec, f.ID); err != nil {
				return DiscoveryResult{}, fmt.Errorf("store: mark a folder missing: %w", err)
			}
			res.Missing = append(res.Missing, f.ID)
			continue
		}
		gone, err := s.deleteFolderTx(ctx, tx, f, now)
		if err != nil {
			return DiscoveryResult{}, err
		}
		evs = append(evs, gone...)
		res.Removed = append(res.Removed, f.ID)
	}

	if res.Events, err = journal(ctx, s, tx, evs); err != nil {
		return DiscoveryResult{}, err
	}
	if res.Folders, err = foldersOn(ctx, tx, d.AccountID); err != nil {
		return DiscoveryResult{}, err
	}
	return res, nil
}

func findResolved(rs []resolvedFolder, name string) resolvedFolder {
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	return resolvedFolder{}
}

// resolveUnique resolves every folder's role and keeps each unique role on one
// folder: the strongest source, then the name that sorts first.
func resolveUnique(folders []provider.Folder, profile provider.Profile, overrides map[string]string) []resolvedFolder {
	out := make([]resolvedFolder, 0, len(folders))
	seen := map[string]bool{}
	for _, f := range folders {
		if f.Name == "" || seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		role, source := provider.ResolveRole(f, profile, overrides)
		out = append(out, resolvedFolder{Folder: f, role: role, source: source})
	}
	winner := map[provider.FolderRole]int{}
	for i, r := range out {
		if r.role == provider.RoleNone || r.role == provider.RoleArchive {
			continue
		}
		j, taken := winner[r.role]
		if !taken {
			winner[r.role] = i
			continue
		}
		cur := out[j]
		rankI, rankJ := provider.RoleSourceRank(r.source), provider.RoleSourceRank(cur.source)
		if rankI < rankJ || (rankI == rankJ && r.Name < cur.Name) {
			winner[r.role] = i
		}
	}
	for i, r := range out {
		if r.role == provider.RoleNone || r.role == provider.RoleArchive {
			continue
		}
		if winner[r.role] != i {
			out[i].role, out[i].source = provider.RoleNone, ""
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// displayName strips the provider's container prefix, so a person sees "Sent
// Mail" rather than "[Gmail]/Sent Mail".
func displayName(name string, delim rune) string {
	if delim == 0 {
		return name
	}
	if idx := strings.LastIndex(name, string(delim)); idx >= 0 && idx+1 < len(name) {
		return name[idx+1:]
	}
	return name
}

// deleteFolderTx removes a folder and its rows, announcing each message as
// deleted or, when another folder still holds it, as a copy removed.
func (s *Store) deleteFolderTx(ctx context.Context, tx *sql.Tx, f Folder, now time.Time) ([]events.Event, error) {
	rows, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages WHERE folder_id = ?`, f.ID)
	if err != nil {
		return nil, err
	}
	evs, err := s.deleteRowsTx(ctx, tx, f.AccountID, rows, map[int64]Folder{f.ID: f}, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folders WHERE id = ?`, f.ID); err != nil {
		return nil, fmt.Errorf("store: delete folder: %w", err)
	}
	ev, err := events.New(events.TypeFolderChanged, f.AccountID, now, FolderChanged{
		AccountID: f.AccountID, FolderID: f.ID, Name: f.Name, Role: string(f.Role), Change: FolderRemoved,
	})
	if err != nil {
		return nil, err
	}
	return append(evs, ev), nil
}

// unsyncFolderTx forgets what the index holds for a folder that is no longer
// synced: its rows go, each announced as deleted or, when another folder still
// holds the message, as a copy removed, and its sync position is reset, so
// syncing it again starts from scratch. What is stored is the synced folders'
// metadata; rows of a folder nobody keeps up to date would be neither.
func (s *Store) unsyncFolderTx(ctx context.Context, tx *sql.Tx, f Folder, now time.Time) ([]events.Event, error) {
	rows, err := loadRows(ctx, tx, `SELECT `+rowColumns+` FROM messages WHERE folder_id = ?`, f.ID)
	if err != nil {
		return nil, err
	}
	evs, err := s.deleteRowsTx(ctx, tx, f.AccountID, rows, map[int64]Folder{f.ID: f}, now)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE folders SET uidvalidity = 0, uidnext = 0, max_seen_uid = 0, highest_modseq = 0,
		backfill_floor = 0, backfill_cursor = 0, last_full_uid_scan_at = 0, last_flag_scan_at = 0, server_count = 0,
		sync_state = 'new', sync_error = '', last_synced_at = 0, initial_total = 0, initial_fetched = 0,
		initial_since = 0, resync_from = '' WHERE id = ?`, f.ID)
	if err != nil {
		return nil, fmt.Errorf("store: reset an unsynced folder: %w", err)
	}
	return evs, nil
}

// FolderSync updates a folder's sync position. Nil fields are left alone.
type FolderSync struct {
	UIDValidity   *uint32
	UIDNext       *imap.UID
	HighestModSeq *uint64
	// MaxSeenUID only ever raises the stored mark: max(stored, this).
	MaxSeenUID        *imap.UID
	BackfillFloor     *imap.UID
	BackfillCursor    *imap.UID
	LastFullUIDScanAt *time.Time
	LastFlagScanAt    *time.Time
	LastSyncedAt      *time.Time
	ServerCount       *uint32
	SyncState         *string
	SyncError         *string
	InitialTotal      *int
	InitialFetched    *int
	// InitialSince records where the initial window starts; the zero time
	// stores "everything".
	InitialSince *time.Time
	// ResyncFrom sets or clears ("") the resync in progress.
	ResyncFrom *string
}

// UpdateFolderSync writes a folder's sync position inside the caller's
// transaction. A folder that is gone is not an error: the update simply
// touches nothing.
func UpdateFolderSync(ctx context.Context, tx *sql.Tx, folderID int64, u FolderSync) error {
	var (
		sets []string
		args []any
	)
	add := func(set string, v any) {
		sets = append(sets, set)
		args = append(args, v)
	}
	if u.UIDValidity != nil {
		add("uidvalidity = ?", *u.UIDValidity)
	}
	if u.UIDNext != nil {
		add("uidnext = ?", uint32(*u.UIDNext))
	}
	if u.HighestModSeq != nil {
		add("highest_modseq = ?", int64(*u.HighestModSeq)) //nolint:gosec // G115: modseqs fit in 63 bits
	}
	if u.MaxSeenUID != nil {
		add("max_seen_uid = max(max_seen_uid, ?)", uint32(*u.MaxSeenUID))
	}
	if u.BackfillFloor != nil {
		add("backfill_floor = ?", uint32(*u.BackfillFloor))
	}
	if u.BackfillCursor != nil {
		add("backfill_cursor = ?", uint32(*u.BackfillCursor))
	}
	if u.LastFullUIDScanAt != nil {
		add("last_full_uid_scan_at = ?", unixOrZero(*u.LastFullUIDScanAt))
	}
	if u.LastFlagScanAt != nil {
		add("last_flag_scan_at = ?", unixOrZero(*u.LastFlagScanAt))
	}
	if u.LastSyncedAt != nil {
		add("last_synced_at = ?", unixOrZero(*u.LastSyncedAt))
	}
	if u.ServerCount != nil {
		add("server_count = ?", *u.ServerCount)
	}
	if u.SyncState != nil {
		add("sync_state = ?", *u.SyncState)
	}
	if u.SyncError != nil {
		add("sync_error = ?", *u.SyncError)
	}
	if u.InitialTotal != nil {
		add("initial_total = ?", *u.InitialTotal)
	}
	if u.InitialFetched != nil {
		add("initial_fetched = ?", *u.InitialFetched)
	}
	if u.InitialSince != nil {
		add("initial_since = ?", unixOrZero(*u.InitialSince))
	}
	if u.ResyncFrom != nil {
		add("resync_from = ?", *u.ResyncFrom)
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, folderID)
	//nolint:gosec // G202: the column list is fixed above; every value is bound
	if _, err := tx.ExecContext(ctx, `UPDATE folders SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return fmt.Errorf("store: update folder sync position: %w", err)
	}
	return nil
}

// FinishInitial closes a folder's initial sync: the backfill cursor goes to
// zero, the folder goes live, and one folder.changed{initial_done} carries
// how many messages it holds — the only announcement an initial sync makes.
func (s *Store) FinishInitial(ctx context.Context, tx *sql.Tx, accountID string, folderID int64, now time.Time) ([]events.Event, error) {
	if err := RequireSyncEligibleTx(ctx, tx, accountID); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = s.now()
	}
	zero := imap.UID(0)
	live := FolderStateLive
	if err := UpdateFolderSync(ctx, tx, folderID, FolderSync{
		BackfillCursor: &zero, SyncState: &live, LastSyncedAt: &now,
	}); err != nil {
		return nil, err
	}
	if err := completeInitialCount(ctx, tx, folderID); err != nil {
		return nil, err
	}
	f, err := folderOn(ctx, tx, accountID, folderID)
	if err != nil {
		return nil, err
	}
	ev, err := events.New(events.TypeFolderChanged, accountID, now, FolderChanged{
		AccountID: accountID, FolderID: f.ID, Name: f.Name, Role: string(f.Role),
		Change: FolderInitialDone, Count: f.LocalCount,
	})
	if err != nil {
		return nil, err
	}
	return journal(ctx, s, tx, []events.Event{ev})
}

// completeInitialCount marks a finished folder's initial window as wholly
// fetched. Messages the window found can vanish before their batch is fetched,
// and a finished folder must not hold the account's percentage below what it
// is while other folders are still at work.
func completeInitialCount(ctx context.Context, tx *sql.Tx, folderID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE folders SET initial_fetched = initial_total WHERE id = ?`, folderID); err != nil {
		return fmt.Errorf("store: close the initial window: %w", err)
	}
	return nil
}

// journal appends events inside tx and returns them with their sequence
// numbers.
func journal(ctx context.Context, s *Store, tx *sql.Tx, evs []events.Event) ([]events.Event, error) {
	if len(evs) == 0 {
		return nil, nil
	}
	return events.NewJournal(s).Append(ctx, tx, evs)
}

func unixTime(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
