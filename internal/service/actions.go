package service

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/events"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Actions on messages: marking them read or unread, starring them,
// archiving, moving them to a folder, putting them in the trash.
//
// The privacy boundary, which every function here keeps:
//
//   - Mailie changes a mailbox only when asked, one request per action, never
//     on its own and never because a message was opened.
//   - Who may act is decided here, before any connection, and asked again
//     on the connection before every command that changes the mailbox. A
//     person changes a mailbox when they hold the act flag on it — signed
//     in, or with a key they made — and only while they allow actions in the
//     console under the current policy (ActionsConsent): a withdrawal stops
//     the next command, even of an action already running. Who linked the
//     mailbox does not matter: the actor's consent and flag do. A mailbox of
//     the operator workspace is changed only with an instance key. Every
//     action needs the write scope.
//   - Nothing is ever deleted for good: there is no \Deleted and EXPUNGE here
//     but inside the provider's MOVE fallback, which expunges exactly the UIDs
//     it moved and needs UIDPLUS for it. A server without MOVE and without
//     UIDPLUS cannot move safely, and is refused.
//   - Every command runs on the account's interactive connection, the
//     engine's, so acting costs no connection beyond the three an account has.
//   - The index reflects what the server confirmed — the flags it echoed, the
//     UIDs it reported, and which message is at which of several of them,
//     read back rather than taken from COPYUID's order — never what was
//     hoped for, and never announces new mail: a moved row keeps its id and
//     says where it went. A command sent is seen to its answer even when the
//     caller has left, so that what the server did is recorded; one whose
//     answer never came leaves the index to the passes, which the action
//     asks for. A mailbox whose index can no longer follow — one that is not
//     syncing while its index stays, as a team mailbox migration 0011 kept
//     stopped does for its readers — is not changed at all (indexFollows).
//   - Nothing is logged about a message but its account, its id and its
//     folder's id: never a subject, an address or a folder's name.

// Limits of the action routes.
const (
	// MaxActionIDs is how many messages one action may name.
	MaxActionIDs = 100
	// actionTimeout bounds the server work of one action; the routes carry
	// the same number.
	actionTimeout = 30 * time.Second
	// indexTimeout bounds recording in the index what the server has
	// already done, which goes ahead even when the caller has left.
	indexTimeout = 15 * time.Second
)

// Where MoveRequest.To can point besides a folder id.
const (
	MoveToArchive = "archive"
	MoveToInbox   = "inbox"
)

// SetFlagsRequest marks messages read or unread, starred or not. A flag left
// out is left alone.
type SetFlagsRequest struct {
	IDs     []int64 `json:"ids"`
	Seen    *bool   `json:"seen,omitempty"`
	Flagged *bool   `json:"flagged,omitempty"`
}

// MoveRequest moves messages within their account. To is "archive",
// "inbox", or a folder id from the account's folder listing, in decimal.
// The trash has its own request.
type MoveRequest struct {
	IDs []int64 `json:"ids"`
	To  string  `json:"to"`
}

// TrashRequest puts messages in the account's trash folder.
type TrashRequest struct {
	IDs []int64 `json:"ids"`
}

// ActionResult is the messages an action touched, as the index holds them
// after it: moved rows under the ids they had, a message moved back from a
// folder that is not synced under the new id it was given, and one copied
// back into the folder a move only took its label from (Gmail) under the new
// id of the copy. Removed are ids that left the index — archived on Gmail,
// where All Mail is not synced, or moved where their new place is not known
// yet — and that a move back (an undo) still accepts for a few minutes when
// the server said where the message went.
type ActionResult struct {
	Messages []MessageSummary `json:"messages"`
	Removed  []int64          `json:"removed"`
}

// Errors of the action routes.
var (
	errActionsOff = E(CodeConflict,
		"actions are off: you have not allowed them in the console", nil)
	errNotOperator = E(CodeNotAuthorized,
		"a mailbox of the operator workspace is changed only with an instance key", nil)
	errNoSafeMove = E(CodeConflict,
		"this mail server supports neither MOVE nor UIDPLUS, so a move would also delete messages other apps "+
			"marked for deletion; Mailie does not move messages there", nil)
	errUseTrash = E(CodeBadRequest, "to move messages to the trash, use the trash action", nil)
	errBadDest  = E(CodeBadRequest,
		"to must be archive, inbox or the id of a folder of the same account that is synced and can hold messages", nil)
	errNotInInbox = E(CodeBadRequest,
		"on Gmail only a message in the inbox can be archived; this one is not in the inbox", nil)
	errAlreadyTrash = E(CodeConflict,
		"that message is already in the trash; Mailie never deletes permanently", nil)
	errNoTrash = E(CodeConflict,
		"this account has no trash folder, and Mailie never deletes permanently", nil)
	errNoArchive = E(CodeConflict, "this account has no archive folder", nil)
	// Gmail's All Mail can be hidden from IMAP in its settings; archiving
	// is moving there.
	errNoAllMail = E(CodeConflict,
		"Gmail's All Mail is hidden from IMAP; show it in Gmail's settings (Labels) to archive from Mailie", nil)
	errCannotReturn = E(CodeConflict,
		"that message can no longer be moved back from here; find it in your mail app", nil)
	errNotSyncing = E(CodeConflict,
		"this mailbox is not syncing, so Mailie's index could not follow an action; nothing was changed on the mail server", nil)
	// errStoppedSyncing is errNotSyncing once a command has gone out.
	errStoppedSyncing = E(CodeConflict,
		"this mailbox stopped syncing while the action ran: the mail server may have made the change, or part of it, "+
			"which Mailie's index can no longer follow", nil)
)

// SetFlags marks messages read or unread, starred or not, on their mail
// server, and records what the server says they have now.
func (s *Service) SetFlags(ctx context.Context, p Principal, req SetFlagsRequest) (ActionResult, error) {
	if err := s.authorize(p, auth.ScopeWrite); err != nil {
		return ActionResult{}, err
	}
	if req.Seen == nil && req.Flagged == nil {
		return ActionResult{}, E(CodeBadRequest, "nothing to change: send seen, flagged or both", nil)
	}
	a, targets, err := s.actionTargets(ctx, p, req.IDs, false)
	if err != nil {
		return ActionResult{}, err
	}
	var add, del []imap.Flag
	for _, f := range []struct {
		want *bool
		flag imap.Flag
	}{{req.Seen, imap.FlagSeen}, {req.Flagged, imap.FlagFlagged}} {
		switch {
		case f.want == nil:
		case *f.want:
			add = append(add, f.flag)
		default:
			del = append(del, f.flag)
		}
	}
	if err := s.changeFlags(ctx, p, a, targets, add, del); err != nil {
		return ActionResult{}, err
	}
	return s.actionResult(ctx, targets, nil, nil)
}

// changeFlags adds and removes flags on messages of one account the caller
// may change, on the server and then in the index, as SetFlags does; a reply
// uses it to mark the message it answers.
func (s *Service) changeFlags(ctx context.Context, p Principal, a account.Account, targets []actionTarget,
	add, del []imap.Flag,
) error {
	folders, err := s.actionFolders(ctx, a)
	if err != nil {
		return err
	}
	groups := groupByFolder(targets)
	ctx, cancel := ctxWithDeadline(ctx, actionTimeout)
	defer cancel()
	x := &act{s: s, p: p, a: a}
	err = s.onServer(ctx, a.ID, func(ctx context.Context, sess provider.Session) error {
		profile := provider.ProfileFor(a.Provider).ForServer(sess.Caps(), a.IMAPHost)
		for _, folderID := range sortedKeys(groups) {
			from := folders[folderID]
			rows := groups[folderID]
			if _, err := sess.Select(ctx, from.Name, false, from.UIDValidity); err != nil {
				return err
			}
			err := x.send(ctx, func(ctx context.Context) error {
				set := uidSet(rows)
				echoed := map[imap.UID]provider.FlagUpdate{}
				for _, change := range []struct {
					op    provider.FlagOp
					flags []imap.Flag
				}{{provider.FlagAdd, add}, {provider.FlagDel, del}} {
					if len(change.flags) == 0 {
						continue
					}
					ups, err := sess.StoreFlags(ctx, set, change.op, change.flags, 0)
					if err != nil {
						return err
					}
					for _, u := range ups {
						echoed[u.UID] = u
					}
				}
				// A server may leave out the echo for a message whose flags
				// did not change. What it holds is still the answer, not a
				// guess.
				var missing []imap.UID
				for _, r := range rows {
					if _, ok := echoed[r.UID]; !ok {
						missing = append(missing, r.UID)
					}
				}
				if len(missing) > 0 {
					ups, err := sess.FetchFlags(ctx, uidSetOf(missing), 0)
					if err != nil {
						return err
					}
					for _, u := range ups {
						echoed[u.UID] = u
					}
				}
				updates := make([]provider.FlagUpdate, 0, len(echoed))
				for _, u := range echoed {
					updates = append(updates, u)
				}
				sort.Slice(updates, func(i, j int) bool { return updates[i].UID < updates[j].UID })
				return s.record(ctx, func(ctx context.Context, tx *sql.Tx) ([]events.Event, error) {
					return s.store.ApplyActionFlags(ctx, tx, store.ActionFlags{
						AccountID: a.ID, FolderID: from.ID, UIDValidity: from.UIDValidity, Updates: updates,
						Shared: profile.FlagsSharedAcrossFolders,
					})
				})
			})
			if err != nil {
				return err
			}
			s.log.Info("message flags changed", "account", a.ID, "folder", from.ID, "messages", rowIDs(rows),
				"by", p.Actor())
		}
		return nil
	})
	if err != nil {
		return x.failed(ctx, "changing message flags on the server failed", err)
	}
	s.afterAction(ctx, a.ID)
	return nil
}

// MoveMessages moves messages to another folder of their account: the
// archive, the inbox, or a folder by id. A message already there is left as
// it is. On Gmail, archiving is taking the message out of the inbox, so only
// a message in the inbox can be archived, and the row leaves the index (All
// Mail is not synced); a move back to the inbox from there — the undo of an
// archive — takes the id the archive returned in Removed.
func (s *Service) MoveMessages(ctx context.Context, p Principal, req MoveRequest) (ActionResult, error) {
	if err := s.authorize(p, auth.ScopeWrite); err != nil {
		return ActionResult{}, err
	}
	if strings.TrimSpace(req.To) == "" {
		return ActionResult{}, errBadDest
	}
	a, targets, err := s.actionTargets(ctx, p, req.IDs, true)
	if err != nil {
		return ActionResult{}, err
	}
	folders, err := s.actionFolders(ctx, a)
	if err != nil {
		return ActionResult{}, err
	}
	profile := provider.ProfileFor(a.Provider).ForServer(provider.Caps{}, a.IMAPHost)
	dest, err := moveDestination(req.To, folders, profile)
	if err != nil {
		return ActionResult{}, err
	}
	// All Mail is the archive only on Gmail. A generic server's \All folder
	// (Dovecot's virtual one, say) is a folder like any other.
	gmailArchive := dest.Role == provider.RoleAll && profile.ArchiveRole == provider.RoleAll
	for _, t := range targets {
		if t.left == nil && gmailArchive && t.row.FolderRole != provider.RoleInbox && t.row.FolderID != dest.ID {
			return ActionResult{}, errNotInInbox
		}
		if t.left != nil {
			if _, ok := folders[t.left.FolderID]; !ok || t.left.UID == 0 {
				return ActionResult{}, errCannotReturn
			}
		}
	}
	return s.moveTo(ctx, p, a, folders, dest, targets)
}

// TrashMessages moves messages to their account's trash folder. It is a
// move, not a deletion: the messages stay in the trash for as long as the
// provider keeps them there, and can be moved back.
func (s *Service) TrashMessages(ctx context.Context, p Principal, req TrashRequest) (ActionResult, error) {
	if err := s.authorize(p, auth.ScopeWrite); err != nil {
		return ActionResult{}, err
	}
	a, targets, err := s.actionTargets(ctx, p, req.IDs, false)
	if err != nil {
		return ActionResult{}, err
	}
	folders, err := s.actionFolders(ctx, a)
	if err != nil {
		return ActionResult{}, err
	}
	trash := roleFolder(folders, provider.RoleTrash, false)
	if trash == nil {
		return ActionResult{}, errNoTrash
	}
	for _, t := range targets {
		if t.row.FolderID == trash.ID || t.row.FolderRole == provider.RoleTrash {
			// Not a deletion: Mailie never takes anything out of the trash
			// for good.
			return ActionResult{}, errAlreadyTrash
		}
	}
	return s.moveTo(ctx, p, a, folders, *trash, targets)
}

// actionTarget is one message an action names: a row of the index, or a row
// a move took out of it a moment ago (only a move may name one of those).
type actionTarget struct {
	row  store.MessageRow
	left *store.LeftRow
}

// actionTargets finds the messages an action names and their account, and
// decides whether the caller may change them.
//
// Every message must be one the caller may read — otherwise the whole
// request is not_found, like a message that does not exist, so ids cannot be
// probed — and all of them must be in one account. A message gone from its
// folder is not_found; one waiting for a resync is a conflict, as for reading.
// Only then is it asked whether the caller may change this account's
// mailbox, and whether they allowed actions.
func (s *Service) actionTargets(ctx context.Context, p Principal, ids []int64, allowLeft bool) (account.Account, []actionTarget, error) {
	if len(ids) == 0 || len(ids) > MaxActionIDs {
		return account.Account{}, nil, Ef(CodeBadRequest, nil, "ids must name between 1 and %d messages", MaxActionIDs)
	}
	if s.store == nil {
		return account.Account{}, nil, errNoMessage
	}
	var (
		targets  []actionTarget
		accounts = map[string]account.Account{}
		seen     = map[int64]bool{}
	)
	visible := func(accountID string) (account.Account, error) {
		if a, ok := accounts[accountID]; ok {
			return a, nil
		}
		if !p.MayAccess(accountID) {
			return account.Account{}, errNoMessage
		}
		a, err := s.accounts.Repo().GetVisible(ctx, accountID, readable(p))
		switch {
		case errors.Is(err, account.ErrNotFound):
			return account.Account{}, errNoMessage
		case err != nil:
			return account.Account{}, E(CodeInternal, "reading the account failed", err)
		}
		accounts[accountID] = a
		return a, nil
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if id < 1 {
			return account.Account{}, nil, errNoMessage
		}
		row, err := s.store.Message(ctx, id)
		if errors.Is(err, store.ErrNoMessage) {
			l, ok := s.store.LeftIndex(id)
			if !ok || !allowLeft {
				return account.Account{}, nil, errNoMessage
			}
			if _, err := visible(l.AccountID); err != nil {
				return account.Account{}, nil, err
			}
			targets = append(targets, actionTarget{left: &l, row: store.MessageRow{ID: l.ID, AccountID: l.AccountID}})
			continue
		}
		if err != nil {
			return account.Account{}, nil, E(CodeInternal, "reading the message failed", err)
		}
		if _, err := visible(row.AccountID); err != nil {
			return account.Account{}, nil, err
		}
		switch {
		case row.Vanished:
			return account.Account{}, nil, errMessageGone
		case row.Stale:
			return account.Account{}, nil, errMessageBusy
		}
		targets = append(targets, actionTarget{row: row})
	}
	if len(accounts) != 1 {
		return account.Account{}, nil, E(CodeBadRequest, "an action's messages must all belong to one account", nil)
	}
	var a account.Account
	for _, only := range accounts {
		a = only
	}
	if err := s.mayAct(ctx, p, a); err != nil {
		return account.Account{}, nil, err
	}
	if err := readyToRead(a); err != nil {
		return account.Account{}, nil, err
	}
	if err := s.indexFollows(ctx, a); err != nil {
		return account.Account{}, nil, err
	}
	return a, targets, nil
}

// indexFollows refuses an action on a mailbox whose index may no longer be
// written: one that is not eligible to sync (store.SyncEligible). Some keep
// their index all the same — a team mailbox migration 0011 kept stopped for
// its readers, or one nobody reads any more that a key still holds — but no
// row of it can change (store.RequireSyncEligibleTx). An action there would
// change the server, fail to record what it did, and leave the index
// contradicting the server with no pass to set it right, so the server is not
// touched.
//
// It is asked when an action is accepted, and again on the connection before
// each command that changes the mailbox (stillMayAct). What is left is the
// moment between that and the recording, which actionFailed answers.
func (s *Service) indexFollows(ctx context.Context, a account.Account) error {
	eligible, err := s.store.SyncEligible(ctx, a.ID)
	switch {
	case err != nil:
		return E(CodeInternal, "reading whether the mailbox syncs failed", err)
	case !eligible:
		return errNotSyncing
	}
	return nil
}

// mayAct decides whether the caller may change this account's mailbox, once
// it is known they may read it.
//
// A person changes a mailbox when they hold the act flag on it and their own
// consent to actions names the current policy: the policy promises Mailie
// changes a mailbox only when the person acting asks. Who linked the mailbox
// does not matter, nor does anybody's role. A workspace key changes a mailbox
// when it holds the act flag on it, with the write scope, while it is live:
// the key terms its creator agreed to cover what it does, and no person's
// consent is asked — but for a person's key the upgrade to workspace keys
// carried over, which acts, as its terms said, only while the person who
// created it allows actions. A mailbox of the operator workspace is changed
// with an instance key, the only credential that sees it. It is asked when an
// action is accepted (actionTargets), and again before each command that
// changes the mailbox (stillMayAct).
func (s *Service) mayAct(ctx context.Context, p Principal, a account.Account) error {
	operator := a.WorkspaceID == workspace.OperatorID
	if p.IsInstance() || operator {
		if p.IsInstance() && operator {
			return nil
		}
		// Unreachable while visibility holds: an instance key sees only
		// the operator's mailboxes, and a person never does.
		return errNotOperator
	}
	held, err := s.grantsOf(ctx, p, a.ID)
	if err != nil {
		return E(CodeInternal, "reading the access to the mailbox failed", err)
	}
	if !held[a.ID].Act {
		return errNoAct
	}
	consenting := p.UserID
	if p.IsWorkspaceKey() {
		if err := s.keyStillLive(ctx, p); err != nil {
			return err
		}
		if !earlierKey(p) && !s.keysActUnderCreator {
			return nil
		}
		consenting = p.CreatedBy
	}
	c, err := s.store.ActionsConsentOf(ctx, consenting)
	switch {
	case errors.Is(err, store.ErrNoSuchUser):
		return errActionsOff
	case err != nil:
		return E(CodeInternal, "reading the actions consent failed", err)
	case c.At == 0 || c.Version != s.consent.Actions:
		return errActionsOff
	}
	return nil
}

// actionFolders is the account's folder index, by id.
func (s *Service) actionFolders(ctx context.Context, a account.Account) (map[int64]store.Folder, error) {
	list, err := s.store.Folders(ctx, a.ID)
	if err != nil {
		return nil, E(CodeInternal, "reading the folder index failed", err)
	}
	out := make(map[int64]store.Folder, len(list))
	for _, f := range list {
		out[f.ID] = f
	}
	return out, nil
}

// moveDestination resolves what a move names: the archive, the inbox, or a
// folder of the account by id that is synced, can hold messages and is not
// one of the folders a message is not moved into (sent, drafts, and the
// flag views). Gmail's All Mail by id means archiving. The trash has its own
// action, whose tool a client can hold to a confirmation.
func moveDestination(to string, folders map[int64]store.Folder, profile provider.Profile) (store.Folder, error) {
	to = strings.TrimSpace(to)
	switch strings.ToLower(to) {
	case "trash":
		return store.Folder{}, errUseTrash
	case MoveToArchive:
		return archiveTarget(folders, profile)
	case MoveToInbox:
		if f := roleFolder(folders, provider.RoleInbox, true); f != nil {
			return *f, nil
		}
		return store.Folder{}, E(CodeConflict, "this account's inbox is not in the index yet", nil)
	}
	id, err := strconv.ParseInt(to, 10, 64)
	if err != nil || id < 1 {
		return store.Folder{}, errBadDest
	}
	f, ok := folders[id]
	if !ok || !f.Selectable || !f.MissingSince.IsZero() {
		return store.Folder{}, errBadDest
	}
	switch f.Role {
	case provider.RoleTrash:
		return store.Folder{}, errUseTrash
	case provider.RoleSent, provider.RoleDrafts, provider.RoleFlagged, provider.RoleImportant:
		return store.Folder{}, E(CodeBadRequest,
			"messages are not moved into sent, drafts, starred or important", nil)
	case provider.RoleAll:
		if profile.ArchiveRole == provider.RoleAll {
			return f, nil
		}
	}
	if !f.Synced {
		return store.Folder{}, errBadDest
	}
	return f, nil
}

// archiveTarget is where archiving moves a message: Gmail's All Mail, which
// is not synced, or the account's synced archive folder. Of several archive
// folders, the one whose role is surest, then the oldest.
func archiveTarget(folders map[int64]store.Folder, profile provider.Profile) (store.Folder, error) {
	var best *store.Folder
	for _, f := range folders {
		if f.Role != profile.ArchiveRole || !f.Selectable || !f.MissingSince.IsZero() {
			continue
		}
		if f.Role != provider.RoleAll && !f.Synced {
			continue
		}
		if best == nil || provider.RoleSourceRank(f.RoleSource) < provider.RoleSourceRank(best.RoleSource) ||
			(provider.RoleSourceRank(f.RoleSource) == provider.RoleSourceRank(best.RoleSource) && f.ID < best.ID) {
			f := f
			best = &f
		}
	}
	switch {
	case best != nil:
		return *best, nil
	case profile.ArchiveRole == provider.RoleAll:
		return store.Folder{}, errNoAllMail
	default:
		return store.Folder{}, errNoArchive
	}
}

// roleFolder is the account's folder with a role, if it can hold messages.
func roleFolder(folders map[int64]store.Folder, role provider.FolderRole, synced bool) *store.Folder {
	for _, f := range folders {
		if f.Role == role && f.Selectable && f.MissingSince.IsZero() && (f.Synced || !synced) {
			return &f
		}
	}
	return nil
}

// groupKind is how a group of messages goes where a move sends them.
type groupKind int

const (
	// groupRows are indexed rows moved out of their folder.
	groupRows groupKind = iota
	// groupLeft are rows a move took out of the index a moment ago, moved
	// back: the undo of a Gmail archive.
	groupLeft
	// groupBack are rows whose last move only took a label away — the
	// destination already held the message (store.LabelMoveOf) — going back
	// where they came from: the undo of such a move, on Gmail.
	groupBack
)

type groupID struct {
	folder int64
	kind   groupKind
}

// moveGroup is what one move command carries: rows of one folder, or rows a
// move took out of the index that are now in one folder.
type moveGroup struct {
	kind groupKind
	from store.Folder
	// uidvalidity is what the UIDs were read under.
	uidvalidity uint32
	rows        []store.MessageRow
	left        []store.LeftRow
	// copy is decided on the connection, where the server says what it is:
	// COPY instead of MOVE. Out of Gmail's All Mail a COPY adds a label and a
	// MOVE would take the message out of the mailbox; back into a folder a
	// move only took the label of, a COPY adds that label again and a MOVE
	// would take away the one the message had before.
	copy bool
}

// moveTo moves the targets to dest, one command per source folder, and
// records each folder's outcome in the index as the server confirms it. A
// failure part-way returns the error: the folders moved before it are in the
// index, the one that failed and those after it are not.
func (s *Service) moveTo(ctx context.Context, p Principal, a account.Account, folders map[int64]store.Folder,
	dest store.Folder, targets []actionTarget,
) (ActionResult, error) {
	groups := map[groupID]*moveGroup{}
	group := func(id groupID, from store.Folder, uidvalidity uint32) *moveGroup {
		g := groups[id]
		if g == nil {
			g = &moveGroup{kind: id.kind, from: from, uidvalidity: uidvalidity}
			groups[id] = g
		}
		return g
	}
	for _, t := range targets {
		switch {
		case t.left != nil:
			if t.left.FolderID == dest.ID {
				continue
			}
			g := group(groupID{t.left.FolderID, groupLeft}, folders[t.left.FolderID], t.left.UIDValidity)
			g.left = append(g.left, *t.left)
		case t.row.FolderID == dest.ID:
			// Already there: nothing to send.
		default:
			kind := groupRows
			if l, ok := s.store.LabelMoveOf(t.row.ID); ok && l.ToFolderID == t.row.FolderID && l.FromFolderID == dest.ID {
				kind = groupBack
			}
			from := folders[t.row.FolderID]
			g := group(groupID{t.row.FolderID, kind}, from, from.UIDValidity)
			g.rows = append(g.rows, t.row)
		}
	}
	if len(groups) == 0 {
		return s.actionResult(ctx, targets, nil, nil)
	}
	ids := make([]groupID, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].folder != ids[j].folder {
			return ids[i].folder < ids[j].folder
		}
		return ids[i].kind < ids[j].kind
	})

	var (
		removed  = map[int64]bool{}
		returned = map[int64]int64{}
	)
	ctx, cancel := ctxWithDeadline(ctx, actionTimeout)
	defer cancel()
	x := &act{s: s, p: p, a: a}
	err := s.onServer(ctx, a.ID, func(ctx context.Context, sess provider.Session) error {
		caps := sess.Caps()
		// Keyed on what the server is, not on a folder's role alone: a
		// generic server's \All folder is left with a MOVE like any other.
		gmail := provider.ProfileFor(a.Provider).ForServer(caps, a.IMAPHost).ArchiveRole == provider.RoleAll
		for _, g := range groups {
			g.copy = g.kind == groupBack || (gmail && g.from.Role == provider.RoleAll)
			if !g.copy && !caps.Move && !caps.UIDPlus {
				// Refused before anything is sent: the only expunge left
				// would take other messages marked for deletion with it.
				return errNoSafeMove
			}
		}
		for _, id := range ids {
			g := groups[id]
			var err error
			switch g.kind {
			case groupLeft:
				err = s.moveBack(ctx, sess, x, g, dest, returned)
			case groupBack:
				err = s.copyBack(ctx, sess, x, g, dest, returned)
			default:
				err = s.moveRows(ctx, sess, x, g, dest, removed)
			}
			if err != nil {
				return err
			}
			moved := rowIDs(g.rows)
			for _, l := range g.left {
				moved = append(moved, l.ID)
			}
			s.log.Info("messages moved", "account", a.ID, "from_folder", g.from.ID, "to_folder", dest.ID,
				"messages", moved, "copied", g.copy, "by", p.Actor())
		}
		return nil
	})
	if err != nil {
		return ActionResult{}, x.failed(ctx, "moving messages on the server failed", err)
	}
	s.afterAction(ctx, a.ID)
	return s.actionResult(ctx, targets, removed, returned)
}

// act is one action on an account's mailbox, as it runs on the account's
// interactive connection.
type act struct {
	s *Service
	p Principal
	a account.Account
	// sent is whether a command that changes the mailbox has gone out. After
	// one, even a failure has changed the server, or may have.
	sent bool
}

// send runs the commands that change the mailbox for one folder, and fn
// records what the server answered.
//
// Whether they are sent at all is decided now, not when the action was
// accepted: the caller must still be there, and still allowed to act — a
// withdrawal of consent that committed while the action waited for the
// connection, or between two folders, stops it here. Once sent, they run to
// their answer whatever the caller does, bounded by actionTimeout rather than
// by the caller: the server carries a command out whether or not anybody
// waits, and a person who closes the tab after clicking Archive has still
// archived — the index must say so, and the undo must be able to find it.
func (x *act) send(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := x.s.stillMayAct(ctx, x.p, x.a); err != nil {
		return err
	}
	wire, cancel := context.WithTimeout(context.WithoutCancel(ctx), actionTimeout)
	defer cancel()
	x.sent = true
	return fn(wire)
}

// failed renders an action's failure. Once a command that changes the
// mailbox has gone out, the server has changed, or may have, whatever the
// error: a pass is asked for, so the index learns what it did, and a mailbox
// that stopped syncing before a later folder's commands is not said to be
// unchanged.
func (x *act) failed(ctx context.Context, what string, err error) error {
	if x.sent {
		x.s.afterAction(ctx, x.a.ID)
		if errors.Is(err, errNotSyncing) {
			return errStoppedSyncing
		}
	}
	return x.s.actionFailed(ctx, x.a, what, err)
}

// stillMayAct asks again, on the connection and before a command that
// changes the mailbox, what actionTargets asked when the action was
// accepted: the account is still one the caller may read, and they may still
// change it — the scope, the act flag, their consent or their key — and it
// is still usable, and its index can still follow.
func (s *Service) stillMayAct(ctx context.Context, p Principal, a account.Account) error {
	if err := s.authorize(p, auth.ScopeWrite); err != nil {
		return err
	}
	now, err := s.accounts.Repo().GetVisible(ctx, a.ID, readable(p))
	switch {
	case errors.Is(err, account.ErrNotFound):
		return errNoMessage
	case err != nil:
		return E(CodeInternal, "reading the account failed", err)
	}
	if err := s.mayAct(ctx, p, now); err != nil {
		return err
	}
	if err := readyToRead(now); err != nil {
		return err
	}
	return s.indexFollows(ctx, now)
}

// moveRows moves indexed rows of one folder and has the index follow them.
func (s *Service) moveRows(ctx context.Context, sess provider.Session, x *act, g *moveGroup,
	dest store.Folder, removed map[int64]bool,
) error {
	a := x.a
	keys := make([]string, 0, len(g.rows))
	for _, r := range g.rows {
		keys = append(keys, r.GroupKey)
	}
	// Before the server is asked: a pass may find a message in its new
	// folder before the index has followed it there.
	s.store.ExpectMoves(a.ID, keys)
	if _, err := sess.Select(ctx, g.from.Name, g.copy, g.uidvalidity); err != nil {
		return err
	}
	// Where the destination already has these messages, read before the
	// move: a move that reports one of those UIDs put a message where it
	// was — a Gmail label it had — and took only the source's label away.
	var held map[imap.UID]bool
	if indexed(dest) {
		var err error
		if held, err = s.store.HeldCopies(ctx, dest.ID, dest.UIDValidity, keys); err != nil {
			return E(CodeInternal, "reading the index failed", err)
		}
	}
	return x.send(ctx, func(ctx context.Context) error {
		set := uidSet(g.rows)
		var (
			res provider.MoveResult
			err error
		)
		if g.copy {
			res, err = sess.Copy(ctx, set, dest.Name)
		} else {
			res, err = sess.Move(ctx, set, dest.Name)
		}
		if err != nil {
			if !g.copy && errors.Is(err, provider.ErrConnClosed) {
				// Sent, and no answer: the messages may have moved. The rows
				// stay until the passes know, and if they did leave, they
				// are announced as moved, never as deleted.
				s.store.MayHaveMoved(g.from.ID, g.uidvalidity, rowUIDs(g.rows))
			}
			return err
		}
		destUIDs, destUIDValidity := res.Mapping, res.DestUIDValidity
		// dest is as the index had it before the move was sent: a UID the
		// server reports at or below its mark was the message's already.
		mark := dest.MaxSeenUID
		if destUIDValidity != dest.UIDValidity {
			mark = 0
		}
		switch {
		case destUIDs == nil && indexed(dest):
			// No COPYUID: the server did not say where the messages landed.
			// They are looked up by Message-ID, read-only; a failure here
			// leaves them to the destination's next pass.
			destUIDs, destUIDValidity = s.findMoved(ctx, sess, a, dest, g.rows)
			mark = 0 // found by searching, not reported: no telling
		case destUIDs != nil && !res.Paired():
			// COPYUID of several UIDs says where the messages landed, not
			// which is which. A row placed at another's UID would read that
			// message, and move it the next time it is moved.
			items := make([]landing, 0, len(g.rows))
			for _, r := range g.rows {
				items = append(items, landing{uid: r.UID, identity: r.Identity()})
			}
			destUIDs = s.placeLanded(ctx, sess, a, dest, res, items)
		}
		moves := make([]store.MovedRow, 0, len(g.rows))
		for _, r := range g.rows {
			moves = append(moves, store.MovedRow{ID: r.ID, UID: r.UID, DestUID: destUIDs[r.UID]})
		}
		var result store.ActionMoveResult
		err = s.record(ctx, func(ctx context.Context, tx *sql.Tx) ([]events.Event, error) {
			var err error
			result, err = s.store.ApplyActionMove(ctx, tx, store.ActionMove{
				AccountID: a.ID, FromFolderID: g.from.ID, FromUIDValidity: g.uidvalidity,
				ToFolderID: dest.ID, DestUIDValidity: destUIDValidity, Moves: moves, Held: held, DestMark: mark,
			})
			return result.Events, err
		})
		if err != nil {
			return err
		}
		for _, id := range result.Removed {
			removed[id] = true
		}
		return nil
	})
}

// findMoved looks messages up in the destination by their Message-ID after a
// move that reported no new UIDs. A UID the index already holds there is
// another message's; of the rest, the newest is the one that just arrived.
// Messages without a Message-ID, or not found, are left out.
func (s *Service) findMoved(ctx context.Context, sess provider.Session, a account.Account, dest store.Folder,
	rows []store.MessageRow,
) (map[imap.UID]imap.UID, uint32) {
	st, err := sess.Select(ctx, dest.Name, true, dest.UIDValidity)
	if err != nil {
		s.log.Info("could not look moved messages up in their new folder; its next pass will index them",
			"account", a.ID, "folder", dest.ID, "class", provider.Class(err))
		return nil, 0
	}
	found := map[imap.UID][]imap.UID{}
	var all []imap.UID
	for _, r := range rows {
		if r.MessageID == "" {
			continue
		}
		uids, err := sess.SearchMessageID(ctx, r.MessageID)
		if err != nil {
			s.log.Info("could not look a moved message up in its new folder; its next pass will index it",
				"account", a.ID, "folder", dest.ID, "message", r.ID, "class", provider.Class(err))
			continue
		}
		found[r.UID] = uids
		all = append(all, uids...)
	}
	held, err := s.store.HeldUIDs(ctx, dest.ID, st.UIDValidity, all)
	if err != nil {
		return nil, 0
	}
	out := map[imap.UID]imap.UID{}
	for _, r := range rows {
		var pick imap.UID
		for _, u := range found[r.UID] {
			if !held[u] && u > pick {
				pick = u
			}
		}
		if pick != 0 {
			held[pick] = true
			out[r.UID] = pick
		}
	}
	return out, st.UIDValidity
}

// landing is a message a COPY or MOVE names: its UID where it is, and what
// tells it apart from the others once it has landed.
type landing struct {
	uid      imap.UID
	identity store.Identity
}

// placeLanded says which message landed at which of the UIDs a COPY or MOVE
// of several reported, which COPYUID does not (provider.MoveResult.Paired):
// it opens the destination read-only on the same connection, reads the
// messages at those UIDs and pairs them with items by identity (pairLanded).
// The answer is source UID to destination UID, like a Mapping, never nil. A
// message it cannot place is left out, and when the reading fails every one
// is: the server moved them all the same, and the caller records each as a
// move to a place the server did not say.
func (s *Service) placeLanded(ctx context.Context, sess provider.Session, a account.Account, dest store.Folder,
	res provider.MoveResult, items []landing,
) map[imap.UID]imap.UID {
	sums, err := readLanded(ctx, sess, dest.Name, res)
	if err != nil {
		s.log.Info("could not read where moved messages landed; their new folder's next pass will index them",
			"account", a.ID, "folder", dest.ID, "class", provider.Class(err))
		return map[imap.UID]imap.UID{}
	}
	placed := pairLanded(res, items, sums)
	moved := 0
	for _, it := range items {
		if _, ok := res.Mapping[it.uid]; ok {
			moved++
		}
	}
	if len(placed) < moved {
		s.log.Info("some moved messages could not be told apart where they landed; their new folder's next pass will index them",
			"account", a.ID, "folder", dest.ID, "moved", moved, "placed", len(placed))
	}
	return placed
}

// readLanded reads the messages at the destination UIDs a COPY or MOVE
// reported, opening the destination read-only under the UIDVALIDITY the
// server reported them with.
func readLanded(ctx context.Context, sess provider.Session, name string, res provider.MoveResult,
) (map[imap.UID]provider.Summary, error) {
	if _, err := sess.Select(ctx, name, true, res.DestUIDValidity); err != nil {
		return nil, err
	}
	uids := make([]imap.UID, 0, len(res.Mapping))
	for _, to := range res.Mapping {
		uids = append(uids, to)
	}
	sums := make(map[imap.UID]provider.Summary, len(uids))
	if len(uids) == 0 {
		return sums, nil
	}
	err := sess.FetchSummaries(ctx, uidSetOf(uids), 0, func(sum provider.Summary) error {
		sums[sum.UID] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sums, nil
}

// pairLanded pairs the messages a COPY or MOVE named with the messages read
// at the UIDs it reported (readLanded), by identity (store.Identity). Only
// messages the server reported moving take part, and they are told apart
// only from each other: the messages read are exactly the ones that landed.
// A message is placed when no other message moved with it has its identity
// and exactly one message read has it. Two different messages can share one
// — no Message-ID, or one a sender reuses, in the same second and of the
// same size — and pairing them in any order would be the same guess as
// trusting COPYUID's: neither is placed. Nor is a message whose identity
// none of those read has.
func pairLanded(res provider.MoveResult, items []landing, sums map[imap.UID]provider.Summary) map[imap.UID]imap.UID {
	at := make(map[store.Identity][]imap.UID, len(sums))
	for u, sum := range sums {
		id := store.IdentityOf(sum)
		at[id] = append(at[id], u)
	}
	named := make(map[store.Identity]int, len(items))
	for _, it := range items {
		if _, moved := res.Mapping[it.uid]; moved {
			named[it.identity]++
		}
	}
	out := make(map[imap.UID]imap.UID, len(items))
	for _, it := range items {
		if _, moved := res.Mapping[it.uid]; !moved {
			continue
		}
		if uids := at[it.identity]; named[it.identity] == 1 && len(uids) == 1 {
			out[it.uid] = uids[0]
		}
	}
	return out
}

// returning is a message going back into a synced folder: the id it is
// known by, its UID where it is now, and what tells it apart from the others
// going back with it.
type returning struct {
	id       int64
	uid      imap.UID
	identity store.Identity
}

// moveBack moves rows a move took out of the index a moment ago back into a
// synced folder: the undo of a Gmail archive. The messages are indexed again
// from their summaries, under new ids.
func (s *Service) moveBack(ctx context.Context, sess provider.Session, x *act, g *moveGroup,
	dest store.Folder, returned map[int64]int64,
) error {
	items := make([]returning, 0, len(g.left))
	for _, l := range g.left {
		s.store.ExpectReturn(l)
		items = append(items, returning{id: l.ID, uid: l.UID, identity: l.Identity()})
	}
	if _, err := sess.Select(ctx, g.from.Name, g.copy, g.uidvalidity); err != nil {
		return err
	}
	return x.send(ctx, func(ctx context.Context) error {
		return s.transferBack(ctx, sess, x.a, g, dest, items, returned)
	})
}

// copyBack puts messages back in the folder a move took them out of, when
// that move only took a label away: the destination held them already — on
// Gmail, a label they had — so the undo is a COPY that adds the source's
// label again. A MOVE would take away the label they had before the move.
// The rows stay where they are, as the messages do; the copies are indexed
// under new ids, as a move back from All Mail is.
func (s *Service) copyBack(ctx context.Context, sess provider.Session, x *act, g *moveGroup,
	dest store.Folder, returned map[int64]int64,
) error {
	keys := make([]string, 0, len(g.rows))
	items := make([]returning, 0, len(g.rows))
	for _, r := range g.rows {
		keys = append(keys, r.GroupKey)
		items = append(items, returning{id: r.ID, uid: r.UID, identity: r.Identity()})
	}
	s.store.ExpectMoves(x.a.ID, keys)
	if _, err := sess.Select(ctx, g.from.Name, true, g.uidvalidity); err != nil {
		return err
	}
	return x.send(ctx, func(ctx context.Context) error {
		return s.transferBack(ctx, sess, x.a, g, dest, items, returned)
	})
}

// transferBack sends the COPY or MOVE of moveBack and copyBack, and indexes
// what landed in dest from its summaries, under new ids. Once the command
// has been answered the messages are back on the server; anything that
// fails after that, or a message that cannot be told apart from the others
// where they landed (pairLanded), leaves them to the destination's next
// pass, and their undo is spent (Store.Returned).
func (s *Service) transferBack(ctx context.Context, sess provider.Session, a account.Account, g *moveGroup,
	dest store.Folder, items []returning, returned map[int64]int64,
) error {
	uids := make([]imap.UID, 0, len(items))
	for _, it := range items {
		uids = append(uids, it.uid)
	}
	var (
		res provider.MoveResult
		err error
	)
	if g.copy {
		res, err = sess.Copy(ctx, uidSetOf(uids), dest.Name)
	} else {
		res, err = sess.Move(ctx, uidSetOf(uids), dest.Name)
	}
	if err != nil {
		return err
	}
	all := make([]int64, 0, len(items))
	for _, it := range items {
		all = append(all, it.id)
	}
	if res.Mapping == nil || !indexed(dest) || res.DestUIDValidity != dest.UIDValidity {
		// Back on the server; the destination's next pass indexes them.
		s.store.Returned(all)
		return nil
	}
	sums, err := readLanded(ctx, sess, dest.Name, res)
	if err != nil {
		s.log.Info("could not read where messages moved back landed; their folder's next pass will index them",
			"account", a.ID, "folder", dest.ID, "class", provider.Class(err))
		s.store.Returned(all)
		return nil //nolint:nilerr // moved back already; the next pass indexes them
	}
	placed := res.Mapping
	if !res.Paired() {
		landings := make([]landing, 0, len(items))
		for _, it := range items {
			landings = append(landings, landing{uid: it.uid, identity: it.identity})
		}
		placed = pairLanded(res, landings, sums)
	}
	var unplaced []int64
	defer func() {
		if len(unplaced) > 0 {
			s.log.Info("some messages moved back could not be told apart where they landed; their folder's next pass will index them",
				"account", a.ID, "folder", dest.ID, "messages", unplaced)
		}
		s.store.Returned(unplaced)
	}()
	for _, it := range items {
		sum, ok := sums[placed[it.uid]]
		if !ok {
			unplaced = append(unplaced, it.id)
			continue
		}
		var id int64
		err := s.record(ctx, func(ctx context.Context, tx *sql.Tx) ([]events.Event, error) {
			var (
				evs []events.Event
				err error
			)
			id, evs, err = s.store.ApplyActionReturn(ctx, tx, store.ActionReturn{
				AccountID: a.ID, LeftID: it.id, FromFolderID: g.from.ID, ToFolderID: dest.ID,
				UIDValidity: dest.UIDValidity, Summary: sum,
			})
			return evs, err
		})
		if err != nil {
			return err
		}
		returned[it.id] = id
	}
	return nil
}

// indexed reports whether rows can be placed in a folder: it is synced, and
// has been selected, so the index knows its UIDVALIDITY.
func indexed(f store.Folder) bool {
	return f.Synced && f.Selectable && f.MissingSince.IsZero() && f.UIDValidity != 0
}

// record writes what the server confirmed in one writer transaction and
// publishes the events it journaled once it has committed. It goes ahead
// when the caller has gone: the mailbox has changed either way, and the
// index should say so.
func (s *Service) record(ctx context.Context, fn func(context.Context, *sql.Tx) ([]events.Event, error)) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexTimeout)
	defer cancel()
	var evs []events.Event
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		evs, err = fn(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	if s.bus != nil {
		s.bus.Publish(evs...)
	}
	return nil
}

// afterAction asks for a pass of the account, so the engine confirms what
// the action did. An account the engine is not running is left alone.
func (s *Service) afterAction(ctx context.Context, accountID string) {
	if s.sync == nil {
		return
	}
	if err := s.sync.Trigger(context.WithoutCancel(ctx), accountID); err != nil && !errors.Is(err, ErrSyncNotRunning) {
		s.log.Debug("asking for a pass after an action failed", "account", accountID, "err", err)
	}
}

// actionFailed renders a failure to act. The log gets the account, the
// provider and the failure's class — the server's own words can name a
// folder, and never go there.
func (s *Service) actionFailed(ctx context.Context, a account.Account, what string, err error) error {
	var known *Error
	switch {
	case errors.As(err, &known):
		return err
	case errors.Is(err, store.ErrNotEligible):
		// Only the recording refuses this, after the server answered: the
		// mailbox stopped syncing between the last check and the write.
		return E(CodeConflict, errStoppedSyncing.Message, err)
	case errors.Is(err, store.ErrUIDValidityMismatch), errors.Is(err, provider.ErrUIDValidityChanged):
		return E(CodeConflict, errMessageBusy.Message, err)
	case errors.Is(err, provider.ErrMessageGone):
		return E(CodeNotFound, errMessageGone.Message, err)
	case errors.Is(err, provider.ErrFolderNotFound):
		return E(CodeConflict, "a folder this needs is no longer on the server; the next sync will catch up", err)
	case errors.Is(err, provider.ErrUnsupported):
		return errNoSafeMove
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		s.log.Warn(what, "account", a.ID, "provider", a.ProviderName(), "class", provider.Class(err), "err", err)
	}
	return fromMailbox(err)
}

// actionResult reads the messages an action named back from the index, in
// the order they were named: removed ids in Removed, moved-back and
// copied-back ones under their new ids.
func (s *Service) actionResult(ctx context.Context, targets []actionTarget, removed map[int64]bool,
	returned map[int64]int64,
) (ActionResult, error) {
	out := ActionResult{Messages: []MessageSummary{}, Removed: []int64{}}
	var rows []store.MessageRow
	for _, t := range targets {
		id := t.row.ID
		if t.left != nil {
			id = t.left.ID
		}
		if next, ok := returned[id]; ok {
			id = next
		} else if t.left != nil {
			out.Removed = append(out.Removed, id)
			continue
		}
		if removed[id] {
			out.Removed = append(out.Removed, id)
			continue
		}
		row, err := s.store.Message(ctx, id)
		switch {
		case errors.Is(err, store.ErrNoMessage):
			out.Removed = append(out.Removed, id)
			continue
		case err != nil:
			return ActionResult{}, E(CodeInternal, "reading the messages back failed", err)
		}
		rows = append(rows, row)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	copies, err := s.store.CopyCounts(ctx, ids)
	if err != nil {
		return ActionResult{}, E(CodeInternal, "reading the messages back failed", err)
	}
	for _, r := range rows {
		out.Messages = append(out.Messages, presentSummary(r, copies[r.ID]))
	}
	return out, nil
}

// AccountActions says which of the actions that need a particular folder an
// account can offer.
type AccountActions struct {
	// Archive is whether the account has somewhere to archive to: Gmail's
	// All Mail, or a synced archive folder.
	Archive bool `json:"archive"`
	// Trash is whether it has a trash folder.
	Trash bool `json:"trash"`
	// ArchiveReason says why Archive is missing, when the index can tell:
	// "all_mail_hidden" is a Gmail mailbox whose folder list, read from the
	// server, has no All Mail, because the person turned off Show in IMAP
	// for it in Gmail's settings. Empty whenever Archive is offered, and
	// before any folder list was read.
	ArchiveReason string `json:"archive_reason,omitempty"`
}

// ArchiveReasonAllMailHidden is the one ArchiveReason there is.
const ArchiveReasonAllMailHidden = "all_mail_hidden"

// actionsOf reads which actions an account's folder index allows. An
// account with no index (sync off) offers neither.
func (s *Service) actionsOf(ctx context.Context, a account.Account) AccountActions {
	if s.store == nil {
		return AccountActions{}
	}
	list, err := s.store.Folders(ctx, a.ID)
	if err != nil {
		s.log.Warn("reading an account's folders for its actions failed; offering none", "account", a.ID, "err", err)
		return AccountActions{}
	}
	folders := make(map[int64]store.Folder, len(list))
	for _, f := range list {
		folders[f.ID] = f
	}
	profile := provider.ProfileFor(a.Provider).ForServer(provider.Caps{}, a.IMAPHost)
	_, archiveErr := archiveTarget(folders, profile)
	out := AccountActions{Archive: archiveErr == nil, Trash: roleFolder(folders, provider.RoleTrash, false) != nil}
	if len(folders) > 0 && errors.Is(archiveErr, errNoAllMail) {
		out.ArchiveReason = ArchiveReasonAllMailHidden
	}
	return out
}

func groupByFolder(targets []actionTarget) map[int64][]store.MessageRow {
	out := map[int64][]store.MessageRow{}
	for _, t := range targets {
		out[t.row.FolderID] = append(out[t.row.FolderID], t.row)
	}
	return out
}

func sortedKeys(m map[int64][]store.MessageRow) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func rowUIDs(rows []store.MessageRow) []imap.UID {
	out := make([]imap.UID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.UID)
	}
	return out
}

func rowIDs(rows []store.MessageRow) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func uidSet(rows []store.MessageRow) imap.UIDSet { return uidSetOf(rowUIDs(rows)) }

// uidSetOf compacts UIDs into ranges. Always a UIDSet: the dynamic type of
// the number set is what makes go-imap send a UID command, and a sequence
// set would change other messages.
func uidSetOf(uids []imap.UID) imap.UIDSet {
	sorted := append([]imap.UID(nil), uids...)
	slices.Sort(sorted)
	var set imap.UIDSet
	for _, u := range sorted {
		if n := len(set); n > 0 && (set[n-1].Stop == u || set[n-1].Stop+1 == u) {
			set[n-1].Stop = u
			continue
		}
		set = append(set, imap.UIDRange{Start: u, Stop: u})
	}
	return set
}
