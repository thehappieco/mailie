package service

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Following an external directory, for an extension of the daemon
// (internal/app's Options.Extensions, through app.Deps), in a binary that
// runs the daemon under a workspace source that is not the local one
// (workspace.Platform): a service elsewhere says, person by person, whether
// they are active, when it disabled them and revoked their sessions and
// credentials, and which of its workspaces they belong to, with which role
// (docs/workspaces.md, "Following an external directory"). The extension
// reads it; the service applies each document in one write
// (ApplyDirectoryDocument), at a person's first sign-in in the transaction
// that creates them (SignInExternal), and keeps, per directory, its
// generation, whether it confirmed it, its resets and where its feed was
// read up to. No transport calls any of it, and none of it takes a
// principal: the directory decides, and the extension vouches for it.
//
// What a directory says applies without the protections a change made here
// keeps — the last owner, the last reader, a closed team, a person who is not
// active — and in a closed team too. Its end of a membership, or of a person,
// may leave a team mailbox with nobody who reads it, which the journal
// records as stranded (store.MailboxStranded); a membership it ends while
// its generation is not confirmed, and whose end would strand a mailbox, is
// held instead, disabled with its flags and sealed grants, until a document
// lists it again or the generation is confirmed.

// DirectoryStatus is what a directory says of a person.
type DirectoryStatus string

// What a directory says of a person.
const (
	// DirectoryActive is a person who may use what they belong to.
	DirectoryActive DirectoryStatus = "active"
	// DirectoryDisabled is a person the directory disabled: suspended here,
	// their memberships still mirrored.
	DirectoryDisabled DirectoryStatus = "disabled"
	// DirectoryClosed is a person the directory closed: suspended here until
	// a fresh answer confirms it, then forgotten (ForgetExternalPerson).
	DirectoryClosed DirectoryStatus = "closed"
)

// DirectoryWorkspace is a workspace a document says its person belongs to.
type DirectoryWorkspace struct {
	// ID is the directory's id for it: 1 to 64 of [A-Za-z0-9_.-], never
	// "wsp_…" (workspace.ValidExternalID).
	ID string
	// Kind is personal or team, and never changes.
	Kind workspace.Kind
	// Name is a team's; "" for the personal one.
	Name string
	// Role is the person's: owner for their personal workspace.
	Role workspace.Role
	// Closed is a team the directory closed.
	Closed bool
	// Version is the directory's version of the team's name and closure.
	Version int64
}

// DirectoryDocument is what a directory says of a person, as an extension
// read it.
type DirectoryDocument struct {
	// Issuer is the directory's, the identity provider's origin; Subject is
	// its stable id for the person, the identity that signs them in here.
	Issuer, Subject string
	// Generation is the generation of the directory's data the document
	// belongs to, 1 to 64 bytes.
	Generation string
	// Version orders the person's documents: only a higher one than the
	// last applied applies, and any after a reset.
	Version int64
	Status  DirectoryStatus
	// DisabledAt, SessionsRevokedAt and CredentialsRevokedAt are when the
	// directory last disabled the person and revoked their sessions and
	// their credentials; zero for never. Kept, and compared, at
	// microseconds (UnixMicro), each as the highest ever seen.
	DisabledAt, SessionsRevokedAt, CredentialsRevokedAt time.Time
	// Workspaces are the person's personal workspace, first, and the teams
	// they belong to; none when Status is closed.
	Workspaces []DirectoryWorkspace
}

// ApplyOptions tune how a document applies.
type ApplyOptions struct {
	// CredentialsMargin is added to CredentialsRevokedAt when stopping the
	// keys the person created before it, so that a key made by a clock a
	// little behind the directory's is not spared.
	CredentialsMargin time.Duration
}

// Stranding is a mailbox a change left stranded, as the journal recorded it
// (workspace.Stranding).
type Stranding = workspace.Stranding

// DirectoryApplied is what applying a document did.
type DirectoryApplied struct {
	UserID string
	// Applied is false for a document no newer than the last applied, which
	// changed nothing.
	Applied bool
	// Suspended is a person suspended by it; Resumed one it resumed.
	Suspended bool
	Resumed   bool
	// SuspensionKept is an active document that does not know of the
	// disable that suspended the person (its DisabledAt is older than the
	// one kept): the suspension stays.
	SuspensionKept bool
	SessionsEnded  int
	KeysStopped    int
	// Changed are the workspaces whose members, name or closure changed.
	Changed []string
	// Held are the workspaces whose membership was held rather than ended.
	Held []string
	// PersonalPending is a local personal workspace the document could not
	// replace while it holds a mailbox or a live key; ReplacedPersonal is
	// the id of the one it replaced.
	PersonalPending  bool
	ReplacedPersonal string
	// Stranded are the mailboxes its ends of memberships left stranded.
	Stranded []Stranding
}

// DirectoryState is a directory followed, as this server keeps it.
type DirectoryState struct {
	Issuer, Generation string
	Confirmed          bool
	// Reset counts the resets recorded for it; Cursor is where its feed was
	// read up to.
	Reset, Cursor int64
	// PublishedThrough is up to when it said it published every change;
	// zero when none was trusted since the last reset.
	PublishedThrough time.Time
	ChangedAt        time.Time
}

// Errors of following a directory, each the cause of the *Error a method
// returns, which an extension tells them apart by (errors.Is).
var (
	// ErrNotFollowing is a directory's call on a server whose workspaces
	// are local: conflict.
	ErrNotFollowing = errors.New("service: this server's workspaces do not come from a directory")
	// ErrDirectoryGeneration is a document, or a confirmation, of another
	// generation than the directory's, or of a directory never followed:
	// conflict. The extension resets the directory first.
	ErrDirectoryGeneration = errors.New("service: that is not the directory's generation")
	// ErrDirectoryMoved is a cursor written against another generation or
	// reset than the directory's, or a document older than the one applied:
	// conflict.
	ErrDirectoryMoved = store.ErrDirectoryMoved
	// ErrKindChanged is a document naming a workspace with another kind than
	// the one it was mirrored with: conflict.
	ErrKindChanged = workspace.ErrKindChanged
	// ErrRepointed is a document naming a workspace of this server's own,
	// or another person's personal workspace: conflict.
	ErrRepointed = workspace.ErrRepointed
	// ErrPersonStillHere is a person released while they exist: conflict.
	ErrPersonStillHere = store.ErrPersonStillHere
	// ErrNotClosed is a person forgotten on a document that does not close
	// them: conflict.
	ErrNotClosed = errors.New("service: the document does not close the person")
	// ErrNoSuchPerson is a subject no identity of the issuer signs in:
	// not_found.
	ErrNoSuchPerson = errors.New("service: no person signs in with that identity")
	// ErrDirectoryNeeded is a person signing in for the first time, under a
	// source that creates no personal workspace, without their document:
	// internal, to try again once it is read.
	ErrDirectoryNeeded = errors.New("service: a new person's document is needed to create them")
)

// directoryNeededRetry is how long a first sign-in without a document waits.
const directoryNeededRetry = 10 * time.Second

var (
	errNotFollowing = E(CodeConflict,
		"this server's workspaces are its own; no directory's documents apply here", ErrNotFollowing)
	errDirectoryGeneration = E(CodeConflict,
		"that is not the generation of the directory's data this server holds; reset the directory first",
		ErrDirectoryGeneration)
	errDirectoryMoved = E(CodeConflict,
		"the directory moved on since: another generation or reset, or a newer document", ErrDirectoryMoved)
	errNoSuchPerson    = E(CodeNotFound, "no person signs in here with that identity", ErrNoSuchPerson)
	errDirectoryNeeded = &Error{
		Code:    CodeInternal,
		Message: "upstream: the directory this server's workspaces come from has not said who this person is yet; try again shortly",
		Retry:   directoryNeededRetry, Err: ErrDirectoryNeeded,
	}
)

// following refuses a directory's change on a server whose workspaces are
// local: only the source that mirrors a directory takes its documents.
func (s *Service) following() error {
	if s.store == nil || s.workspaces == nil || s.workspaces.Source().Name() != workspace.SourcePlatform {
		return errNotFollowing
	}
	return nil
}

// maxGeneration bounds a directory's generation, as the schema does.
const maxGeneration = 64

// checkDocument refuses a document outside its shape, before anything is
// read: bad_request, nothing applied.
func checkDocument(doc DirectoryDocument) error {
	if err := auth.CheckIssuer(doc.Issuer); err != nil {
		return E(CodeBadRequest, "the issuer must be an origin: https://host[:port], or http:// on loopback", err)
	}
	if !boundedText(doc.Subject, auth.MaxSubjectLength) {
		return Ef(CodeBadRequest, nil, "the subject must be 1 to %d bytes of text", auth.MaxSubjectLength)
	}
	if !boundedText(doc.Generation, maxGeneration) {
		return Ef(CodeBadRequest, nil, "a generation is 1 to %d bytes of text", maxGeneration)
	}
	if doc.Version < 1 {
		return E(CodeBadRequest, "a document's version is a positive integer", nil)
	}
	switch doc.Status {
	case DirectoryActive, DirectoryDisabled, DirectoryClosed:
	default:
		return E(CodeBadRequest, "a person's status is active, disabled or closed", nil)
	}
	for _, at := range []time.Time{doc.DisabledAt, doc.SessionsRevokedAt, doc.CredentialsRevokedAt} {
		if micros(at) < 0 {
			return E(CodeBadRequest, "a document's moments are never before 1970", nil)
		}
	}
	return nil
}

// boundedText reports whether s is 1 to maxBytes of UTF-8 without control
// characters.
func boundedText(s string, maxBytes int) bool {
	return s != "" && len(s) <= maxBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// micros is a moment in unix microseconds, 0 for the zero time.
func micros(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMicro()
}

// secondsOrZero is the moment of unix seconds, the zero time for 0.
func secondsOrZero(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// fromMicros is the moment of unix microseconds, the zero time for 0.
func fromMicros(us int64) time.Time {
	if us == 0 {
		return time.Time{}
	}
	return time.UnixMicro(us).UTC()
}

// checkWorkspacesTx checks, inside the write and writing nothing, the
// document's workspace part: the person's personal workspace first, as its
// owner, and only one; every id one a directory may send, once; each team's
// role and name; and an id already mirrored keeping its kind, its source
// and, personal, its person. A refusal leaves the document's revocations to
// apply without it (ApplyDirectoryDocument).
func checkWorkspacesTx(ctx context.Context, tx *sql.Tx, userID string, list []DirectoryWorkspace) error {
	if len(list) == 0 || list[0].Kind != workspace.KindPersonal {
		return E(CodeBadRequest, "a document names its person's personal workspace first", nil)
	}
	seen := make(map[string]bool, len(list))
	for i, w := range list {
		if !workspace.ValidExternalID(w.ID) {
			return E(CodeBadRequest, "a workspace id is 1 to 64 of letters, digits, '_', '.' and '-', never wsp_…",
				workspace.ErrInvalidExternalID)
		}
		if seen[w.ID] {
			return E(CodeBadRequest, "a document names each workspace once", nil)
		}
		seen[w.ID] = true
		if w.Version < 0 {
			return E(CodeBadRequest, "a workspace's version is never negative", nil)
		}
		switch w.Kind {
		case workspace.KindPersonal:
			if i != 0 || w.Role != workspace.RoleOwner || w.Closed {
				return E(CodeBadRequest, "a document names one personal workspace, first, its person its owner, never closed", nil)
			}
		case workspace.KindTeam:
			if _, err := workspace.ParseRole(string(w.Role)); err != nil {
				return E(CodeBadRequest, "a role is owner, admin or member", err)
			}
			if _, err := workspace.MirrorName(w.Name); err != nil {
				return E(CodeBadRequest, "a team's name has a character to keep", err)
			}
		default:
			return E(CodeBadRequest, "a workspace is personal or a team", nil)
		}
		personID := ""
		if w.Kind == workspace.KindPersonal {
			personID = userID
		}
		if err := workspace.CheckMirroredTx(ctx, tx, workspace.Mirrored{ID: w.ID, Kind: w.Kind, PersonID: personID}); err != nil {
			return fromDirectory(err, "checking the document's workspaces failed")
		}
	}
	return nil
}

// directoryApply is what applyDirectoryTx did, for the caller to finish once
// its write commits.
type directoryApply struct {
	applied DirectoryApplied
	// invalid is the document's workspace part refused (step 6): its
	// revocations committed, the document not.
	invalid error
	// accounts are the mailboxes whose sync the change may have started or
	// stopped.
	accounts []string
	// stoppedKeys counts the stopped keys gone with a replaced personal
	// workspace.
	stoppedKeys int
}

// ApplyDirectoryDocument applies what a directory says of a person, in one
// write, for an extension of the daemon; no transport calls it, and it takes
// no principal (docs/workspaces.md, "Following an external directory"):
//
//  1. The person is the one the document's identity signs in (not_found
//     otherwise), under the source that mirrors a directory (conflict
//     otherwise).
//  2. The directory is followed at the document's generation from its first
//     document on; a document of another generation is refused (conflict):
//     the extension resets the directory first (ResetDirectory).
//  3. Only a newer document applies: a version no higher than the last
//     applied changes nothing (Applied false). A reset sets every version
//     back to 0.
//  4. What it revoked applies when it rose past what was kept, compared at
//     microseconds: a disable rising, or a status disabled or closed for a
//     person not suspended, suspends them (a person disabled here stays as
//     they are: a disable here stands over a directory); sessions revoked
//     end the sessions started at or before that moment; credentials
//     revoked stop the keys they created before it and the margin
//     (KeyStopAccountRecovered), in every workspace.
//  5. A closed person stops there: their memberships and their deletion
//     wait for a fresh answer that confirms it (ForgetExternalPerson).
//  6. The workspace part is checked before anything of it is written. A
//     refusal — a shape (bad_request), a kind changed (ErrKindChanged) or
//     a workspace repointed (ErrRepointed) — still commits step 4's
//     revocations and the person's status, never the version, and is
//     returned after the commit with what was applied.
//  7. The personal workspace is the directory's: a local one is replaced
//     while it holds no mailbox, live or removed, and no live key
//     (PersonalPending otherwise, and the rest applies).
//  8. Each team is mirrored: created with the directory's id, or renamed,
//     closed or reopened by a higher version than the one it took last.
//  9. Each membership listed is written, held or disabled ones made active
//     again, with what a role change does here; every membership of a team
//     from another source the document omits ends, without a refusal, held
//     instead while the directory's generation is not confirmed and its end
//     would strand a mailbox. What an end strands is journaled.
//  10. An active document resumes a suspended person only when its disable
//     is not older than the one kept: one that does not know of the
//     disable that suspended them keeps the suspension (SuspensionKept).
//  11. What the directory said is recorded: the version, the generation,
//     the status and the highest of each revocation.
func (s *Service) ApplyDirectoryDocument(ctx context.Context, doc DirectoryDocument, opts ApplyOptions) (DirectoryApplied, error) {
	if err := s.following(); err != nil {
		return DirectoryApplied{}, err
	}
	if err := checkDocument(doc); err != nil {
		return DirectoryApplied{}, err
	}
	var res directoryApply
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		userID, ok, err := auth.IdentityUserTx(ctx, tx, doc.Issuer, doc.Subject)
		switch {
		case err != nil:
			return err
		case !ok:
			return errNoSuchPerson
		}
		res, err = s.applyDirectoryTx(ctx, tx, userID, doc, opts, s.now())
		return err
	})
	if err != nil {
		return DirectoryApplied{}, fromDirectory(err, "applying the directory's document failed")
	}
	s.directoryApplied(ctx, res)
	if res.invalid != nil {
		return res.applied, res.invalid
	}
	return res.applied, nil
}

// applyDirectoryTx is ApplyDirectoryDocument's write, for a person who
// exists, inside the caller's transaction: its own, or the one a first
// sign-in creates the person in. It opens no write of its own.
func (s *Service) applyDirectoryTx(ctx context.Context, tx *sql.Tx, userID string, doc DirectoryDocument, opts ApplyOptions,
	now time.Time,
) (directoryApply, error) {
	out := directoryApply{applied: DirectoryApplied{UserID: userID}}
	dir, err := store.EnsureDirectoryTx(ctx, tx, doc.Issuer, doc.Generation, now.Unix())
	if err != nil {
		return directoryApply{}, err
	}
	if dir.Generation != doc.Generation {
		return directoryApply{}, errDirectoryGeneration
	}
	kept, _, err := store.DirectoryPersonTx(ctx, tx, doc.Issuer, userID)
	if err != nil {
		return directoryApply{}, err
	}
	if doc.Version <= kept.Version {
		return out, nil
	}
	disabledUS, sessionsUS, credentialsUS := micros(doc.DisabledAt), micros(doc.SessionsRevokedAt),
		micros(doc.CredentialsRevokedAt)

	// 4. What it revoked, when it rose past what was kept.
	suspended, err := suspendedTx(ctx, tx, userID)
	if err != nil {
		return directoryApply{}, err
	}
	if disabledUS > kept.DisabledAtUS || (doc.Status != DirectoryActive && !suspended) {
		done, err := s.users.SuspendTx(ctx, tx, userID)
		if err != nil {
			return directoryApply{}, err
		}
		if done.Suspended {
			out.applied.Suspended = true
			out.applied.SessionsEnded += done.Sessions
			out.applied.KeysStopped += done.Keys
		}
	}
	if sessionsUS > kept.SessionsRevokedAtUS {
		n, err := s.users.RevokeSessionsStartedBeforeTx(ctx, tx, userID, sessionsUS)
		if err != nil {
			return directoryApply{}, err
		}
		out.applied.SessionsEnded += n
	}
	if credentialsUS > kept.CredentialsRevokedAtUS {
		before := fromMicros(credentialsUS).Add(opts.CredentialsMargin)
		n, err := s.keys.RevokeCreatedBeforeTx(ctx, tx, userID, before, workspace.KeyStopAccountRecovered)
		if err != nil {
			return directoryApply{}, err
		}
		out.applied.KeysStopped += n
	}
	person := store.DirectoryPerson{
		Issuer: doc.Issuer, UserID: userID, Subject: doc.Subject, Generation: doc.Generation, Version: kept.Version,
		Status: string(doc.Status), DisabledAtUS: disabledUS, SessionsRevokedAtUS: sessionsUS,
		CredentialsRevokedAtUS: credentialsUS, AppliedAt: now.Unix(),
	}

	// 5. A closed person: the revocations and the suspension only.
	if doc.Status == DirectoryClosed {
		person.Version = doc.Version
		if err := store.PutDirectoryPersonTx(ctx, tx, person); err != nil {
			return directoryApply{}, err
		}
		out.applied.Applied = true
		return out, nil
	}

	// 6. The workspace part, checked before anything of it is written.
	if err := checkWorkspacesTx(ctx, tx, userID, doc.Workspaces); err != nil {
		var refused *Error
		if !errors.As(err, &refused) || refused.Code == CodeInternal {
			return directoryApply{}, err
		}
		if err := store.PutDirectoryPersonTx(ctx, tx, person); err != nil {
			return directoryApply{}, err
		}
		out.invalid = err
		return out, nil
	}

	// 7. The personal workspace.
	personal := doc.Workspaces[0].ID
	had, err := workspace.PersonalOfTx(ctx, tx, userID)
	if err != nil && !errors.Is(err, workspace.ErrNotFound) {
		return directoryApply{}, err
	}
	replaced, pending, stopped, err := workspace.ReplacePersonalTx(ctx, tx, userID, personal, now)
	if err != nil {
		return directoryApply{}, err
	}
	out.applied.PersonalPending, out.applied.ReplacedPersonal, out.stoppedKeys = pending, replaced, stopped
	if !pending && had.ID != personal {
		out.applied.Changed = appendOnce(out.applied.Changed, personal)
	}

	// 8 and 9. The teams and the memberships listed.
	listed := map[string]bool{personal: true}
	for _, w := range doc.Workspaces[1:] {
		listed[w.ID] = true
		created, renamed, closure, err := workspace.MirrorWorkspaceTx(ctx, tx, workspace.Mirrored{
			ID: w.ID, Kind: workspace.KindTeam, Name: w.Name, Closed: w.Closed, Version: w.Version,
		}, now)
		if err != nil {
			return directoryApply{}, err
		}
		out.accounts = append(out.accounts, closure...)
		member, err := workspace.MirrorMemberTx(ctx, tx, w.ID, userID, w.Role, now)
		if err != nil {
			return directoryApply{}, err
		}
		if created || renamed || member {
			out.applied.Changed = appendOnce(out.applied.Changed, w.ID)
		}
	}
	// 9. The memberships it omits.
	teams, err := workspace.MirroredTeamsOfTx(ctx, tx, userID)
	if err != nil {
		return directoryApply{}, err
	}
	for _, id := range teams {
		if listed[id] {
			continue
		}
		ended, err := workspace.EndMembershipTx(ctx, tx, id, userID, !dir.Confirmed, now)
		if err != nil {
			return directoryApply{}, err
		}
		if !ended.Kept {
			out.applied.Changed = appendOnce(out.applied.Changed, id)
		}
		if ended.Held {
			out.applied.Held = append(out.applied.Held, id)
		}
		out.applied.KeysStopped += ended.KeysRevoked
		out.applied.Stranded = append(out.applied.Stranded, ended.Stranded...)
	}
	inChanged, err := workspace.MailboxesInTx(ctx, tx, out.applied.Changed)
	if err != nil {
		return directoryApply{}, err
	}
	out.accounts = append(out.accounts, inChanged...)

	// 10. A resume, only by a document that knows of the disable kept.
	if doc.Status == DirectoryActive {
		if suspended, err = suspendedTx(ctx, tx, userID); err != nil {
			return directoryApply{}, err
		}
		if suspended {
			if disabledUS >= kept.DisabledAtUS {
				if out.applied.Resumed, err = s.users.ResumeTx(ctx, tx, userID); err != nil {
					return directoryApply{}, err
				}
			} else {
				out.applied.SuspensionKept = true
			}
		}
	}

	// 11. What the directory said.
	person.Version = doc.Version
	if err := store.PutDirectoryPersonTx(ctx, tx, person); err != nil {
		return directoryApply{}, err
	}
	out.applied.Applied = true
	return out, nil
}

// appendOnce appends id unless list has it.
func appendOnce(list []string, id string) []string {
	for _, have := range list {
		if have == id {
			return list
		}
	}
	return append(list, id)
}

// suspendedTx reports whether a person is suspended by an extension now.
func suspendedTx(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var suspended bool
	err := tx.QueryRowContext(ctx, `SELECT suspended_at <> 0 FROM users WHERE id = ?`, userID).Scan(&suspended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, auth.ErrUserNotFound
	case err != nil:
		return false, E(CodeInternal, "reading the person failed", err)
	}
	return suspended, nil
}

// directoryApplied tells, once a document's write committed, whatever answers
// for what it changed: event subscriptions read access again, the sync
// engine starts or stops the mailboxes of the teams it changed and the
// person's own, and a suspended person's consent attempts stop. A failure
// only delays what the engine notices on its own.
func (s *Service) directoryApplied(ctx context.Context, res directoryApply) {
	a := res.applied
	if !a.Applied && res.invalid == nil {
		return
	}
	s.accessChanged()
	if len(a.Stranded) > 0 {
		s.noticesChanged()
	}
	if a.Suspended {
		if err := s.accounts.StopFlowsBy(ctx, a.UserID); err != nil {
			s.log.Warn("a directory suspended a person, but stopping their pending authorisations failed",
				"user", a.UserID, "err", err)
		}
	}
	s.reconcile(res.accounts...)
	if err := s.reconcileTheirs(ctx, a.UserID); err != nil {
		s.log.Warn("listing a person's mailboxes after a directory's document failed; the engine notices on its own",
			"user", a.UserID, "err", err)
	}
	if a.ReplacedPersonal != "" {
		s.log.Info("a directory's document replaced a personal workspace", "user", a.UserID,
			"replaced", a.ReplacedPersonal, "stopped_keys", res.stoppedKeys)
	}
	if a.PersonalPending {
		s.log.Warn("a directory's document could not replace a personal workspace that holds mailboxes or live keys",
			"user", a.UserID)
	}
	if a.SuspensionKept {
		s.log.Warn("an active document did not resume a person: it does not know of the disable that suspended them",
			"user", a.UserID)
	}
	if len(a.Stranded) > 0 {
		s.log.Info("a directory ended memberships that left team mailboxes with nobody who reads them",
			"user", a.UserID, "mailboxes", len(a.Stranded))
	}
}

// fromDirectory maps a failure following a directory onto the transport
// vocabulary, its cause kept for errors.Is.
func fromDirectory(err error, what string) error {
	var already *Error
	switch {
	case errors.As(err, &already):
		return already
	case errors.Is(err, workspace.ErrKindChanged):
		return E(CodeConflict, "a workspace of the document has another kind than the one it was mirrored with", err)
	case errors.Is(err, workspace.ErrRepointed):
		return E(CodeConflict, "a workspace of the document is this server's own, or another person's", err)
	case errors.Is(err, workspace.ErrInvalidExternalID), errors.Is(err, workspace.ErrInvalidName),
		errors.Is(err, workspace.ErrInvalidRole):
		return E(CodeBadRequest, "a workspace of the document is outside its shape", err)
	case errors.Is(err, store.ErrNoDirectory):
		return E(CodeConflict, errDirectoryGeneration.Message, errors.Join(ErrDirectoryGeneration, err))
	case errors.Is(err, store.ErrDirectoryMoved):
		return E(CodeConflict, errDirectoryMoved.Message, err)
	case errors.Is(err, store.ErrPersonStillHere):
		return E(CodeConflict, "that person is still here: a person is released only once deleted", err)
	case errors.Is(err, auth.ErrUserNotFound):
		return E(CodeNotFound, "no such person", err)
	default:
		return E(CodeInternal, what, err)
	}
}

func directoryState(d store.Directory) DirectoryState {
	return DirectoryState{
		Issuer: d.Issuer, Generation: d.Generation, Confirmed: d.Confirmed, Reset: d.Reset, Cursor: d.Cursor,
		PublishedThrough: fromMicros(d.PublishedThroughUS), ChangedAt: time.Unix(d.ChangedAt, 0).UTC(),
	}
}

// Directory reads a directory followed; false when it never was.
func (s *Service) Directory(ctx context.Context, issuer string) (DirectoryState, bool, error) {
	if s.store == nil {
		return DirectoryState{}, false, nil
	}
	d, ok, err := s.store.Directory(ctx, issuer)
	if err != nil {
		return DirectoryState{}, false, E(CodeInternal, "reading the directory failed", err)
	}
	if !ok {
		return DirectoryState{}, false, nil
	}
	return directoryState(d), true, nil
}

// ResetDirectory starts a directory again at generation, in one transaction:
// its generation set, unconfirmed, its cursor and what it published through
// forgotten, its reset counted once more; every person's document version
// of it, and every version of a workspace from another source, set back to
// 0 under that reset, so the next document of each applies whatever its
// version; what it revoked is kept. A directory never followed is created
// at generation, with nothing else to reset. An extension calls it when the
// directory says it forgot where its feed stood, whatever the generation,
// and on a new generation. It returns the directory as it is now.
func (s *Service) ResetDirectory(ctx context.Context, issuer, generation string) (DirectoryState, error) {
	if err := s.following(); err != nil {
		return DirectoryState{}, err
	}
	if err := auth.CheckIssuer(issuer); err != nil {
		return DirectoryState{}, E(CodeBadRequest, "the issuer must be an origin", err)
	}
	if !boundedText(generation, maxGeneration) {
		return DirectoryState{}, Ef(CodeBadRequest, nil, "a generation is 1 to %d bytes of text", maxGeneration)
	}
	var d store.Directory
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		d, err = store.ResetDirectoryTx(ctx, tx, issuer, generation, s.now().Unix())
		return err
	})
	if err != nil {
		return DirectoryState{}, fromDirectory(err, "resetting the directory failed")
	}
	s.log.Info("directory reset", "issuer", issuer, "generation", generation, "reset", d.Reset)
	return directoryState(d), nil
}

// ConfirmDirectoryGeneration records that the directory confirmed its
// generation, when generation is the one held (conflict otherwise, and for a
// directory never followed); confirming twice changes nothing. In the same
// transaction it ends every membership held, one after another, each
// journaling stranded (membership_ended, by the directory) the mailboxes its
// hold kept that nobody else reads, would read once resumed or would read
// but for another hold, so a mailbox two holds kept strands once, naming the
// last. It returns those strandings.
func (s *Service) ConfirmDirectoryGeneration(ctx context.Context, issuer, generation string) ([]Stranding, error) {
	if err := s.following(); err != nil {
		return nil, err
	}
	var (
		out      []Stranding
		accounts []string
		ended    int
	)
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		now := s.now()
		if _, err := store.ConfirmDirectoryTx(ctx, tx, issuer, generation, now.Unix()); err != nil {
			if errors.Is(err, store.ErrDirectoryMoved) || errors.Is(err, store.ErrNoDirectory) {
				return errDirectoryGeneration
			}
			return err
		}
		held, err := workspace.HeldMembershipsTx(ctx, tx)
		if err != nil {
			return err
		}
		var teams []string
		for _, h := range held {
			res, err := workspace.EndMembershipTx(ctx, tx, h.WorkspaceID, h.UserID, false, now)
			if err != nil {
				return err
			}
			out = append(out, res.Stranded...)
			teams = appendOnce(teams, h.WorkspaceID)
		}
		ended = len(held)
		accounts, err = workspace.MailboxesInTx(ctx, tx, teams)
		return err
	})
	if err != nil {
		return nil, fromDirectory(err, "confirming the directory's generation failed")
	}
	if ended > 0 {
		s.accessChanged()
		s.reconcile(accounts...)
		s.log.Info("the directory confirmed its generation: held memberships ended", "issuer", issuer,
			"memberships", ended, "stranded", len(out))
	}
	return out, nil
}

// SetDirectoryCursor records where the directory's feed was read up to and,
// when publishedThrough is not zero, up to when it published every change,
// which only rises. A compare and set: only while the directory is at
// expectGeneration and expectReset, and its cursor not past cursor; conflict
// (ErrDirectoryMoved) otherwise, so an answer read before a reset never
// moves the feed. It never creates the directory: ResetDirectory, or the
// first document, does.
func (s *Service) SetDirectoryCursor(ctx context.Context, issuer, expectGeneration string, expectReset, cursor int64,
	publishedThrough time.Time,
) error {
	if err := s.following(); err != nil {
		return err
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		return store.SetDirectoryCursorTx(ctx, tx, issuer, expectGeneration, expectReset, cursor,
			micros(publishedThrough), s.now().Unix())
	})
	if err != nil {
		return fromDirectory(err, "moving the directory's cursor failed")
	}
	return nil
}

// ForgottenPerson is what forgetting a person deleted.
type ForgottenPerson struct {
	UserID, PersonalWorkspaceID  string
	AccountsRemoved              int
	SessionsDeleted, KeysDeleted int
	Stranded                     []Stranding
}

// ForgetExternalPerson deletes the person a directory closed, on a document
// that confirms it: closed (ErrNotClosed otherwise), of the directory's
// generation and no older than the document applied (conflict otherwise),
// for the person its identity signs in (not_found otherwise). As a deletion
// with force, refused by nothing, the instance's last owner included: their
// personal workspace and its mailboxes, credentials and index, their
// sessions, their identities and everything DeleteUser takes. Each team
// mailbox they alone read, would read once resumed or would read but for a
// hold is journaled stranded (cause forgotten, by the directory) while they
// still exist; their directory row stays, gone, and their name in the
// journal, until the extension releases them (ReleaseDirectoryPerson).
func (s *Service) ForgetExternalPerson(ctx context.Context, doc DirectoryDocument) (ForgottenPerson, error) {
	if err := s.following(); err != nil {
		return ForgottenPerson{}, err
	}
	if err := checkDocument(doc); err != nil {
		return ForgottenPerson{}, err
	}
	if doc.Status != DirectoryClosed {
		return ForgottenPerson{}, E(CodeConflict, "only a document that closes a person forgets them", ErrNotClosed)
	}
	userID, ok, err := s.users.IdentityUser(ctx, doc.Issuer, doc.Subject)
	switch {
	case err != nil:
		return ForgottenPerson{}, E(CodeInternal, "reading the identity failed", err)
	case !ok:
		return ForgottenPerson{}, errNoSuchPerson
	}
	personal := ""
	if w, err := s.workspaces.PersonalOf(ctx, userID); err == nil {
		personal = w.ID
	}
	check := func(tx *sql.Tx) error {
		if linked, ok, err := auth.IdentityUserTx(ctx, tx, doc.Issuer, doc.Subject); err != nil {
			return err
		} else if !ok || linked != userID {
			return errNoSuchPerson
		}
		dir, ok, err := store.DirectoryTx(ctx, tx, doc.Issuer)
		switch {
		case err != nil:
			return err
		case !ok || dir.Generation != doc.Generation:
			return errDirectoryGeneration
		}
		kept, _, err := store.DirectoryPersonTx(ctx, tx, doc.Issuer, userID)
		if err != nil {
			return err
		}
		if doc.Version < kept.Version {
			return errDirectoryMoved
		}
		// Recorded first, so that their row, gone once they are deleted,
		// keeps their name in the journal until the extension releases them.
		return store.PutDirectoryPersonTx(ctx, tx, store.DirectoryPerson{
			Issuer: doc.Issuer, UserID: userID, Subject: doc.Subject, Generation: doc.Generation, Version: doc.Version,
			Status: string(doc.Status), DisabledAtUS: micros(doc.DisabledAt),
			SessionsRevokedAtUS: micros(doc.SessionsRevokedAt), CredentialsRevokedAtUS: micros(doc.CredentialsRevokedAt),
			AppliedAt: s.now().Unix(),
		})
	}
	done, err := s.removePerson(ctx, userID, removeOpts{
		force: true, actor: store.ActorDirectory, cause: store.CauseForgotten, check: check,
	})
	if err != nil {
		return ForgottenPerson{}, fromDirectory(err, "forgetting the person failed")
	}
	s.log.Info("a person a directory closed was forgotten", "user", userID, "accounts", done.accounts,
		"sessions", done.removed.Sessions, "keys", done.removed.Keys, "stranded", len(done.stranded))
	return ForgottenPerson{
		UserID: userID, PersonalWorkspaceID: personal, AccountsRemoved: done.accounts,
		SessionsDeleted: done.removed.Sessions, KeysDeleted: done.removed.Keys, Stranded: done.stranded,
	}, nil
}

// ReleaseDirectoryPerson forgets a person deleted here whom a directory
// followed: their row of that directory goes and, once no directory keeps
// one, their name in the journal of what was done to mailboxes, as the actor
// and as the person a stranding lost. Refused (conflict, ErrPersonStillHere)
// while they exist. Releasing someone with no row changes nothing.
func (s *Service) ReleaseDirectoryPerson(ctx context.Context, issuer, userID string) error {
	if s.store == nil {
		return errNotFollowing
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		return store.ReleaseDirectoryPersonTx(ctx, tx, issuer, userID)
	})
	if err != nil {
		return fromDirectory(err, "releasing the person failed")
	}
	return nil
}

// DirectoryPerson is a person of a directory as an extension lists them.
type DirectoryPerson struct {
	Subject, UserID string
	// Held is a person an identity of the issuer signs in now.
	Held bool
	// Gone is a person deleted here, at GoneAt, whose row the directory
	// keeps until it releases them.
	Gone   bool
	GoneAt time.Time
	// Generation, Version and Status are the document last applied for
	// them; empty, 0 and empty when none was.
	Generation string
	Version    int64
	Status     DirectoryStatus
}

// DirectoryPeople lists, for an issuer, every person one of its identities
// signs in now, with or without a document applied, and every person
// deleted here whose row it keeps.
func (s *Service) DirectoryPeople(ctx context.Context, issuer string) ([]DirectoryPerson, error) {
	if s.store == nil {
		return nil, nil
	}
	listed, err := s.store.DirectoryPeople(ctx, issuer)
	if err != nil {
		return nil, E(CodeInternal, "listing the directory's people failed", err)
	}
	out := make([]DirectoryPerson, 0, len(listed))
	for _, p := range listed {
		out = append(out, DirectoryPerson{
			Subject: p.Subject, UserID: p.UserID, Held: p.Held, Gone: p.GoneAt != 0, GoneAt: secondsOrZero(p.GoneAt),
			Generation: p.Generation, Version: p.Version, Status: DirectoryStatus(p.Status),
		})
	}
	return out, nil
}

// DirectorySubjects maps people to their subjects at an issuer: through the
// identity that signs them in, or, for a person deleted here, the row the
// directory keeps until it releases them. A person neither names is left
// out.
func (s *Service) DirectorySubjects(ctx context.Context, issuer string, userIDs []string) (map[string]string, error) {
	if s.store == nil {
		return map[string]string{}, nil
	}
	out, err := s.store.DirectorySubjects(ctx, issuer, userIDs)
	if err != nil {
		return nil, E(CodeInternal, "mapping people to their subjects failed", err)
	}
	return out, nil
}

// IdentityUser reads the person an identity (issuer, subject) signs in;
// false when it signs in nobody.
func (s *Service) IdentityUser(ctx context.Context, issuer, subject string) (string, bool, error) {
	userID, ok, err := s.users.IdentityUser(ctx, issuer, subject)
	if err != nil {
		return "", false, E(CodeInternal, "reading the identity failed", err)
	}
	return userID, ok, nil
}

// SweepDirectory releases the people deleted here whose directory rows were
// marked gone longer ago than the journal keeps an entry
// (store.MailboxJournalRetention), as ReleaseDirectoryPerson does, and
// reports how many: an extension that never released them keeps their name
// no longer than the journal that names them. The daemon's housekeeping.
func (s *Service) SweepDirectory(ctx context.Context, now time.Time) (int, error) {
	if s.store == nil {
		return 0, nil
	}
	var n int
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = store.SweepDirectoryPeopleTx(ctx, tx, now.Add(-store.MailboxJournalRetention).Unix())
		return err
	})
	if err != nil {
		return 0, E(CodeInternal, "sweeping the directory's people failed", err)
	}
	return n, nil
}

// PersonalBlock is a local personal workspace a directory's document would
// not replace, and what it holds (workspace.PersonalBlock).
type PersonalBlock = workspace.PersonalBlock

// PersonalReplacementBlocks lists the local personal workspaces of people who
// sign in through an identity provider that a directory's document would not
// replace while they hold mailboxes, live or removed, or live keys: what a
// daemon that follows a directory says at start.
func (s *Service) PersonalReplacementBlocks(ctx context.Context) ([]PersonalBlock, error) {
	if s.workspaces == nil {
		return nil, nil
	}
	out, err := s.workspaces.PersonalReplacementBlocks(ctx)
	if err != nil {
		return nil, E(CodeInternal, "reading what keeps personal workspaces failed", err)
	}
	return out, nil
}
