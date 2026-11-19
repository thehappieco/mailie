package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// What an extension reports of a followed workspace's settings, and of a
// person's own, to the directory its workspaces come from (docs/workspaces.md,
// "What an extension reads, and the freshness of an act"): beside its
// snapshot, a workspace's mailboxes — how each connects and syncs, from fixed
// lists, never its address, a folder's name or a server's error — and its
// members; a person's consents against the texts accepted now, whether they
// have an account key, their last sign-out everywhere, and their notices,
// each named by what the journal kept of its mailbox; and which people
// changed. It decides nothing, and no transport calls it.

// Availability is whether a workspace's mailboxes are used now.
type Availability string

// A workspace's availability, the first that holds.
const (
	// AvailabilityClosed is a closed team: nothing in it is used or
	// changes until it reopens.
	AvailabilityClosed Availability = "closed"
	// AvailabilityWithheld is a team where the edition withholds teams.
	AvailabilityWithheld Availability = "withheld"
	// AvailabilityUsable is every other workspace.
	AvailabilityUsable Availability = "usable"
)

// Availabilities are every availability a report says.
func Availabilities() []Availability {
	return []Availability{AvailabilityClosed, AvailabilityWithheld, AvailabilityUsable}
}

// MailboxReason is why a mailbox needs attention, from a fixed list: never
// the provider's or the server's own words.
type MailboxReason string

// Why a mailbox in error or needing a new authorization does, by its state's
// fixed reason (account.Reason*), compared exactly.
const (
	// ReasonScopeMissing is a grant that does not reach the mailbox.
	ReasonScopeMissing MailboxReason = "scope_missing"
	// ReasonMailboxRefused is the mail server refusing the grant.
	ReasonMailboxRefused MailboxReason = "mailbox_refused"
	// ReasonTokenRejected is the mail server rejecting the access token.
	ReasonTokenRejected MailboxReason = "token_rejected"
	// ReasonUnproved is a grant the mail server could not be reached to
	// prove.
	ReasonUnproved MailboxReason = "unproved"
	// ReasonOther is any other reason, a dead grant's and none included.
	ReasonOther MailboxReason = "other"
)

// MailboxReasons are every reason a report says.
func MailboxReasons() []MailboxReason {
	return []MailboxReason{ReasonScopeMissing, ReasonMailboxRefused, ReasonTokenRejected, ReasonUnproved, ReasonOther}
}

// MailboxStates are every state a report says of a mailbox, as its row has
// them (MailboxSettings.State).
func MailboxStates() []string {
	return []string{string(account.StatePendingAuth), string(account.StateActive), string(account.StateNeedsReauth),
		string(account.StateDisabled), string(account.StateError)}
}

// MailboxProviders are every provider a report presents a mailbox as
// (MailboxSettings.Provider): iCloud is generic IMAP on Apple's servers.
func MailboxProviders() []string {
	return []string{string(provider.KindGmail), string(provider.KindMicrosoft), account.ICloud, string(provider.KindIMAP)}
}

// IndexState is how much of a mailbox is indexed.
type IndexState string

// How much of a mailbox is indexed, as of a report's measure.
const (
	// IndexNone: no folder finished its first sync, and no message is
	// indexed.
	IndexNone IndexState = "none"
	// IndexPartial: some is indexed, not every folder finished.
	IndexPartial IndexState = "partial"
	// IndexComplete: every folder that syncs finished its first sync, and
	// there is at least one.
	IndexComplete IndexState = "complete"
)

// IndexStates are every index state a report says.
func IndexStates() []IndexState { return []IndexState{IndexNone, IndexPartial, IndexComplete} }

// MemberStatus is where a member of a workspace stands.
type MemberStatus string

// Where a member stands, the first that holds.
const (
	// MemberDisabled is the person disabled here, or the membership
	// disabled otherwise than by a hold.
	MemberDisabled MemberStatus = "disabled"
	// MemberSuspended is the person suspended by an extension.
	MemberSuspended MemberStatus = "suspended"
	// MemberHeld is a membership an external directory ended and this
	// server holds (migration 0022).
	MemberHeld MemberStatus = "held"
	// MemberActive is every other member.
	MemberActive MemberStatus = "active"
)

// MemberStatuses are every status a report says of a member.
func MemberStatuses() []MemberStatus {
	return []MemberStatus{MemberDisabled, MemberSuspended, MemberHeld, MemberActive}
}

// WorkspaceReport is a followed workspace's snapshot and its settings,
// measured in one read.
type WorkspaceReport struct {
	// MeasuredAt is when the read began; the snapshot's too.
	MeasuredAt time.Time
	// Snapshot is the workspace's snapshot (AccessSnapshot); empty with
	// SnapshotTooLarge past a snapshot's bounds, when the settings are read
	// all the same, each list with its own bound.
	Snapshot         AccessSnapshot
	SnapshotTooLarge bool
	Settings         WorkspaceSettings
}

// WorkspaceSettings is what a workspace's mailboxes and members are.
type WorkspaceSettings struct {
	WorkspaceID  string
	Kind         workspace.Kind
	Availability Availability
	// LinkMinRole is the least role that may link a mailbox here: the
	// owner of a personal workspace, an admin of a team; empty for nobody,
	// in a team closed or withheld.
	LinkMinRole workspace.Role
	// SyncText is the revision of the sync text a team's agreement names
	// now.
	SyncText string
	// Mailboxes are live and removed ones until they are deleted for good,
	// the oldest first; nil with MailboxesOver past SnapshotMaxMailboxes.
	Mailboxes     []MailboxSettings
	MailboxesOver bool
	// Members are every member, by user id; nil with MembersOver past
	// SnapshotMaxPeople.
	Members     []MemberSettings
	MembersOver bool
}

// MailboxSettings is how a mailbox connects and syncs. Never its address, a
// folder's name or a server's error.
type MailboxSettings struct {
	ID string
	// Provider is gmail, microsoft, icloud or imap (MailboxProviders).
	Provider string
	// AuthKind is oauth2 or password.
	AuthKind string
	// State is the row's (MailboxStates); a removed mailbox keeps the one it
	// had.
	State string
	// Reason is why a mailbox in error, disabled or needing a new
	// authorization is; empty in every other state, and on a removed one.
	Reason MailboxReason
	// ConnectedAt is when it first stored something to sign in with; zero
	// when it never did, or when that is not known (an OAuth mailbox
	// connected before migration 0027).
	ConnectedAt time.Time
	// LinkedBy is the person who linked it; empty for anyone else, and once
	// that person is deleted.
	LinkedBy string
	// KeyEpoch is its current key's epoch, 0 for a mailbox without a key.
	KeyEpoch int
	// FilesSentCopy is Mailie itself filing the copy of what is sent from it
	// (filesSentCopy): never for Gmail or Microsoft, nor for a server a
	// login found to be Gmail's, nor while no login said what it is.
	FilesSentCopy bool
	// NoReader: nobody reads it now, or, removed, would read it once it is
	// recovered.
	NoReader bool
	// Team is a team's mailbox, whose sync its owners and admins agree to;
	// otherwise its person's.
	Team bool
	// SyncOn is the consent it syncs under standing and nothing holding it:
	// its person's, or its team's agreement. A team mailbox migration 0011
	// left stopped and bound to its linker is off, with the index it keeps.
	SyncOn bool
	// Held is a personal mailbox's sync turned off by the operator until
	// its person turns it on.
	Held bool
	// Agreement is a team's agreement to its sync, nil while there is none
	// and on a personal mailbox.
	Agreement *SyncAgreementState
	// Paused is its pause, a person's or a recovery's; nil when not paused.
	Paused *SyncPauseState
	// Indexed and LastSyncAt are as of the report's measure: they move
	// without marking the workspace changed.
	Indexed    IndexState
	LastSyncAt time.Time
	// Removed is a mailbox kept until PurgeAfter, removed at RemovedAt;
	// IndexKept, any of its index still there (a folder or a message). All
	// zero on a live one.
	Removed               bool
	RemovedAt, PurgeAfter time.Time
	IndexKept             bool
}

// SyncAgreementState is a team's agreement to sync a mailbox.
type SyncAgreementState struct {
	// ByUserID is the person who agreed; empty for anyone else (the
	// command line), and once that person is deleted.
	ByUserID string
	At       time.Time
	// Revision is the sync text agreed to.
	Revision string
	// Bound is the agreement migration 0011 copied from the mailbox's
	// linker, not yet confirmed by an owner or an admin.
	Bound bool
}

// SyncPauseState is a mailbox's pause.
type SyncPauseState struct {
	// ByUserID is the person who paused it, or recovered it; empty for
	// anyone else, and once that person is deleted.
	ByUserID string
	At       time.Time
}

// MemberSettings is a member of a workspace.
type MemberSettings struct {
	UserID   string
	Role     workspace.Role
	Status   MemberStatus
	Enrolled bool
}

// WorkspaceReport reads a followed workspace's snapshot and its settings in
// one read-only transaction (store.ReadTx), for an extension; it takes no
// principal. It refuses what AccessSnapshot refuses, but a workspace larger
// than a snapshot holds: that is SnapshotTooLarge, with the settings read
// all the same.
func (s *Service) WorkspaceReport(ctx context.Context, workspaceID string) (WorkspaceReport, error) {
	if err := s.reportedWorkspace(ctx, workspaceID); err != nil {
		return WorkspaceReport{}, err
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return WorkspaceReport{}, E(CodeInternal, "reading the workspace's report failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	out := WorkspaceReport{MeasuredAt: s.now()}
	out.Snapshot, err = s.accessSnapshotTx(ctx, tx, workspaceID, out.MeasuredAt)
	switch {
	case errors.Is(err, workspace.ErrSnapshotTooLarge):
		out.Snapshot, out.SnapshotTooLarge = AccessSnapshot{}, true
	case err != nil:
		return WorkspaceReport{}, err
	}
	read, err := workspace.SettingsTx(ctx, tx, workspaceID)
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return WorkspaceReport{}, errNoSnapshotWorkspace
	case err != nil:
		return WorkspaceReport{}, E(CodeInternal, "reading the workspace's settings failed", err)
	}
	out.Settings = s.presentSettings(workspaceID, read)
	return out, nil
}

// presentSettings is what a workspace's settings say, from what was read.
func (s *Service) presentSettings(workspaceID string, read workspace.Settings) WorkspaceSettings {
	out := WorkspaceSettings{
		WorkspaceID: workspaceID, Kind: read.Kind, Availability: AvailabilityUsable, SyncText: s.consent.Sync,
		MailboxesOver: read.MailboxesOver, MembersOver: read.MembersOver,
	}
	// In PauseSQL's order: a closed team before a team withheld.
	switch {
	case read.Closed:
		out.Availability = AvailabilityClosed
	case s.teamsWithheld && read.Kind == workspace.KindTeam:
		out.Availability = AvailabilityWithheld
	}
	// Who may link (linkInto): a personal workspace's person, its owner,
	// and a team's owners and admins; nobody where it is not usable.
	if out.Availability == AvailabilityUsable {
		switch read.Kind {
		case workspace.KindPersonal:
			out.LinkMinRole = workspace.RoleOwner
		case workspace.KindTeam:
			out.LinkMinRole = workspace.RoleAdmin
		}
	}
	if read.Mailboxes != nil {
		out.Mailboxes = make([]MailboxSettings, 0, len(read.Mailboxes))
		for _, mb := range read.Mailboxes {
			out.Mailboxes = append(out.Mailboxes, presentMailboxSettings(mb))
		}
	}
	if read.Members != nil {
		out.Members = make([]MemberSettings, 0, len(read.Members))
		for _, m := range read.Members {
			out.Members = append(out.Members, MemberSettings{
				UserID: m.UserID, Role: m.Role, Status: memberStatus(m), Enrolled: m.Enrolled,
			})
		}
	}
	return out
}

// presentMailboxSettings is what a mailbox's settings say, from its row.
func presentMailboxSettings(mb workspace.MailboxSettings) MailboxSettings {
	// The fields the account's own rules read: what it is presented as
	// (iCloud), and whether Mailie files its sent copy.
	a := account.Account{
		Provider: provider.Kind(mb.Provider), IMAPHost: mb.IMAPHost, SaveSentCopy: mb.SaveSentCopy,
		ServerKind: mb.ServerKind,
	}
	out := MailboxSettings{
		ID: mb.ID, Provider: a.ProviderName(), AuthKind: mb.AuthKind, State: mb.State,
		ConnectedAt: secondsOrZero(mb.ConnectedAt), LinkedBy: personOrNone(mb.LinkedBy), KeyEpoch: mb.KeyEpoch,
		FilesSentCopy: filesSentCopy(a), NoReader: mb.NoReader, Team: !mb.Personal,
		SyncOn: mb.ConsentStands && !mb.Held, Held: mb.Held, Indexed: indexState(mb),
		LastSyncAt: secondsOrZero(mb.LastOKAt), Removed: mb.RemovedAt != 0,
	}
	if out.Removed {
		out.RemovedAt, out.PurgeAfter = secondsOrZero(mb.RemovedAt), secondsOrZero(mb.PurgeAfter)
		out.IndexKept = mb.IndexKept
	} else {
		out.Reason = mailboxReason(mb.State, mb.StateReason)
	}
	if !mb.Personal && mb.SyncEnabledAt != 0 {
		out.Agreement = &SyncAgreementState{
			ByUserID: personOrNone(mb.SyncEnabledBy), At: secondsOrZero(mb.SyncEnabledAt), Revision: mb.SyncConsentVersion,
			Bound: mb.Bound,
		}
	}
	if mb.SyncPausedAt != 0 {
		out.Paused = &SyncPauseState{ByUserID: personOrNone(mb.SyncPausedBy), At: secondsOrZero(mb.SyncPausedAt)}
	}
	return out
}

// mailboxReason is why a mailbox in error, disabled or needing a new
// authorization is, by its state's fixed reason compared exactly; empty in
// any other state.
func mailboxReason(state, reason string) MailboxReason {
	switch account.State(state) {
	case account.StateError, account.StateDisabled, account.StateNeedsReauth:
	default:
		return ""
	}
	switch reason {
	case account.ReasonScopeMissing:
		return ReasonScopeMissing
	case account.ReasonMailboxRefused:
		return ReasonMailboxRefused
	case account.ReasonTokenRejected:
		return ReasonTokenRejected
	case account.ReasonUnproved:
		return ReasonUnproved
	}
	return ReasonOther
}

// indexState is how much of a mailbox is indexed.
func indexState(mb workspace.MailboxSettings) IndexState {
	switch {
	case mb.FoldersTotal > 0 && mb.FoldersSynced == mb.FoldersTotal:
		return IndexComplete
	case mb.FoldersSynced == 0 && !mb.Messages:
		return IndexNone
	}
	return IndexPartial
}

// memberStatus is where a member stands, the first that holds: a disable
// here stands over a suspension, which it clears.
func memberStatus(m workspace.MemberSettings) MemberStatus {
	switch {
	case m.PersonDisabled && !m.PersonSuspended:
		return MemberDisabled
	case m.PersonSuspended:
		return MemberSuspended
	case m.Held:
		return MemberHeld
	case m.Status != workspace.StatusActive:
		return MemberDisabled
	}
	return MemberActive
}

// personOrNone is an actor when it names a person ("usr_…"), and empty for
// anyone else: a key, the command line, the operator, the daemon.
func personOrNone(actor string) string {
	if strings.HasPrefix(actor, "usr_") {
		return actor
	}
	return ""
}

// PersonSettings is a person's own settings and their newest notices.
type PersonSettings struct {
	UserID     string
	MeasuredAt time.Time
	// Sync, Actions and Send are their consents.
	Sync, Actions, Send ConsentState
	// Texts are the revisions this server accepts now.
	Texts ConsentTexts
	// Enrolled is having an account key.
	Enrolled bool
	// SignedOutEverywhereAt is when they last signed out of every session;
	// zero when never, or before migration 0027.
	SignedOutEverywhereAt time.Time
	// Notices are the newest first; MoreNotices, more than were read.
	Notices     []PersonNoticeState
	MoreNotices bool
}

// ConsentState is one of a person's consents.
type ConsentState struct {
	// At and Revision are when they agreed and to which text; zero and
	// empty while it is off.
	At       time.Time
	Revision string
	// Current is agreeing to the revision accepted now (ConsentTexts). A
	// consent to an earlier one keeps sync on, and holds actions and sends
	// back until they agree again.
	Current bool
	// WithdrawnAt is when they last withdrew it, zero when never, given
	// since, or not known; always zero for sync, which keeps no moment.
	WithdrawnAt time.Time
}

// ConsentTexts are the revisions of the texts this server accepts now: what
// sync stores, actions on messages, sending, and an API key's terms.
type ConsentTexts struct {
	Sync, Actions, Send, Keys string
}

// ConsentTexts are the revisions this server accepts now
// (MAIL_CONSENT_VERSION_*).
func (s *Service) ConsentTexts() ConsentTexts {
	return ConsentTexts{Sync: s.consent.Sync, Actions: s.consent.Actions, Send: s.consent.Send, Keys: s.consent.Keys}
}

// PersonNoticeState is one of a person's notices, named by what the journal
// kept of its mailbox, never its address.
type PersonNoticeState struct {
	// ID is its journal entry's ("mbe_…").
	ID string
	// Kind is the journal's: removed, sync_paused (turned off, its index
	// deleted) or paused (its index kept).
	Kind                   string
	WorkspaceID, AccountID string
	// Team is its workspace being a team now; false once it is gone.
	Team bool
	At   time.Time
	// Label and Hint are what the journal kept: the mailbox's name, unless
	// it says its address, forgotten at its deletion for good, and its
	// address masked.
	Label, Hint string
	// State is where the mailbox stands now: live, removed or deleted.
	State string
	// RecoverableUntil is, for a removal, when the mailbox is (removed) or
	// was (deleted) deleted for good; for a turn-off of a removed one, when
	// it will be. Zero otherwise.
	RecoverableUntil time.Time
	// Cause and Actor are the journal entry's, raw (JournalEntry).
	Cause, Actor string
	// ActorUserID is the person who acted, while they are a member of the
	// notice's workspace (any status); empty otherwise.
	ActorUserID string
}

// Where a notice's mailbox stands, as PersonNoticeState says it.
const (
	NoticeLive    = mailboxLive
	NoticeRemoved = mailboxRemoved
	NoticeDeleted = mailboxDeleted
)

// MaxPersonNotices is the most notices PersonSettings reads.
const MaxPersonNotices = store.MaxPersonNotices

// PersonSettings reads a person's settings and their newest notices, at most
// maxNotices (1 to MaxPersonNotices, bad_request otherwise), in one
// read-only transaction, for an extension; it takes no principal. Nobody is
// not_found. A notice that does not match where its mailbox stands (a
// removal's of a live mailbox, a pause's of one that is not live), which the
// writes that end them never leave, is left out, and logged.
func (s *Service) PersonSettings(ctx context.Context, userID string, maxNotices int) (PersonSettings, error) {
	if maxNotices < 1 || maxNotices > MaxPersonNotices {
		return PersonSettings{}, Ef(CodeBadRequest, nil, "a person's notices are read 1 to %d at a time", MaxPersonNotices)
	}
	if s.store == nil {
		return PersonSettings{}, errNoPersonSettings
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return PersonSettings{}, E(CodeInternal, "reading the person's settings failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	out := PersonSettings{UserID: userID, MeasuredAt: s.now(), Texts: s.ConsentTexts()}
	row, err := store.PersonSettingsTx(ctx, tx, userID, maxNotices)
	switch {
	case errors.Is(err, store.ErrNoSuchUser):
		return PersonSettings{}, errNoPersonSettings
	case err != nil:
		return PersonSettings{}, E(CodeInternal, "reading the person's settings failed", err)
	}
	out.Sync = consentState(row.SyncConsentAt, row.SyncConsentVersion, s.consent.Sync, 0)
	out.Actions = consentState(row.ActionsConsentAt, row.ActionsConsentVersion, s.consent.Actions,
		row.ActionsConsentWithdrawnAt)
	out.Send = consentState(row.SendConsentAt, row.SendConsentVersion, s.consent.Send, row.SendConsentWithdrawnAt)
	out.Enrolled = row.Enrolled
	out.SignedOutEverywhereAt = secondsOrZero(row.SignedOutEverywhereAt)
	out.MoreNotices = row.More
	out.Notices = make([]PersonNoticeState, 0, len(row.Notices))
	for _, n := range row.Notices {
		notice, ok := presentNoticeState(n)
		if !ok {
			s.log.Warn("a notice does not match where its mailbox stands; it is left out of the person's settings",
				"user", userID, "notice", n.Event.ID, "kind", string(n.Event.Kind))
			continue
		}
		out.Notices = append(out.Notices, notice)
	}
	return out, nil
}

var errNoPersonSettings = E(CodeNotFound, "no such person", nil)

// consentState is a consent from its moment and revision, against the
// revision accepted now.
func consentState(at int64, revision, current string, withdrawn int64) ConsentState {
	out := ConsentState{WithdrawnAt: secondsOrZero(withdrawn)}
	if at != 0 {
		out.At, out.Revision, out.Current = secondsOrZero(at), revision, revision == current
	}
	return out
}

// presentNoticeState is a notice with where its mailbox stands now; false
// for one that cannot be where it stands.
func presentNoticeState(n store.PersonNoticeRow) (PersonNoticeState, bool) {
	out := PersonNoticeState{
		ID: n.Event.ID, Kind: string(n.Event.Kind), WorkspaceID: n.Event.WorkspaceID, AccountID: n.Event.AccountID,
		Team: n.Team, At: secondsOrZero(n.Event.At), Label: n.Event.Label, Hint: n.Event.Hint, Cause: n.Event.Cause,
		Actor: n.Event.Actor,
	}
	if n.ActorMember {
		out.ActorUserID = n.Event.Actor
	}
	switch {
	case !n.Kept:
		out.State = NoticeDeleted
	case n.Removed:
		out.State = NoticeRemoved
	default:
		out.State = NoticeLive
	}
	switch n.Event.Kind {
	case store.MailboxRemoved:
		// A recovery withdraws its removal's notices.
		switch out.State {
		case NoticeLive:
			return PersonNoticeState{}, false
		case NoticeRemoved:
			out.RecoverableUntil = secondsOrZero(n.PurgeAfter)
		default:
			// When it was deleted for good, as the journal says, else the
			// end its removal recorded.
			out.RecoverableUntil = secondsOrZero(n.PurgedAt)
			if n.PurgedAt == 0 {
				out.RecoverableUntil = secondsOrZero(n.Event.PurgeAfter)
			}
		}
	case store.MailboxSyncPaused:
		if out.State == NoticeRemoved {
			out.RecoverableUntil = secondsOrZero(n.PurgeAfter)
		}
	case store.MailboxPaused:
		// A removal and a deletion for good end a pause, withdrawing its
		// notices.
		if out.State != NoticeLive {
			return PersonNoticeState{}, false
		}
	default:
		return PersonNoticeState{}, false
	}
	return out, true
}

// PersonChange is a person's mark: the seq of the last change to their own
// settings as a report shows them. Only people an identity signs in are
// marked.
type PersonChange struct {
	UserID string
	Seq    int64
}

// PersonChanges lists the people whose marks are after after, in the order
// of their marks, at most limit (1 to store.MaxPersonChanges, bad_request
// otherwise), and the highest mark there is, read at one moment
// (store.PersonChanges): whom an extension reports again.
func (s *Service) PersonChanges(ctx context.Context, after int64, limit int) ([]PersonChange, int64, error) {
	if s.store == nil {
		return nil, 0, nil
	}
	if limit < 1 || limit > store.MaxPersonChanges {
		return nil, 0, Ef(CodeBadRequest, nil, "a page of changes is 1 to %d people", store.MaxPersonChanges)
	}
	marks, head, err := s.store.PersonChanges(ctx, after, limit)
	if err != nil {
		return nil, 0, E(CodeInternal, "reading the person changes failed", err)
	}
	out := make([]PersonChange, 0, len(marks))
	for _, m := range marks {
		out = append(out, PersonChange(m))
	}
	return out, head, nil
}
