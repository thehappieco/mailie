package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Applying an issuer's keys (docs/workspaces.md, "Keys an issuer makes",
// "Applying the issuer's records").
//
// An extension that follows an issuer of keys reads its feed and hands each
// page to ApplyIssuedKeys: the page's records and its cursor in one write, so
// that a crash re-applies nothing and loses nothing. Each record applies
// inside a savepoint of its own: one the schema refuses applies again as a
// partial record (only what it narrows), and one that still cannot be written
// stops its key here (the operator's stop) rather than holding up the feed,
// so that no record fails its page.
//
// What a record says is judged here, once: who gave each grant must read the
// mailbox here, the key's terms must be the revision this server accepts
// now, and nothing is offered that an issued key never holds (send, a team's
// mailbox). A grant whose giver this server cannot judge yet waits for the
// giver's document, and is judged in that document's own write. A reset of
// the directory (ResetDirectory) starts the keys again with it, never wider
// than they were.

// IssuedKeysPage is one page of an issuer's key feed, as an extension read
// it.
type IssuedKeysPage struct {
	// Issuer is the directory's issuer, which the registered KeyIssuer
	// names; Generation and Reset are the directory's when the page was
	// read: a page read before a reset never applies (ErrDirectoryMoved).
	Issuer, Generation string
	Reset              int64
	Keys               []IssuedKey
	// Cursor is where the page leaves the feed (its next_after).
	Cursor int64
	// Last is a page that said nothing more was waiting; PublishedThrough,
	// on it only, is up to when the issuer said it published everything.
	Last             bool
	PublishedThrough time.Time
}

// IssuedKey is one record of an issuer's key, as an extension decoded it.
// Times keep the issuer's meaning: a zero ExpiresAt is the earliest (the key
// is past it); a zero RevokedAt beside Revoked is a revocation at a time
// unknown, taken as now.
type IssuedKey struct {
	// ExternalID is the key's id at its issuer; Selector, the public part of
	// its credential (the key's prefix here); Hash, what its issuer's check
	// compares a credential with (KeyIssuer.Verify).
	ExternalID, Selector, Hash string
	// WorkspaceID is the directory's id of the key's workspace; Creator the
	// creator's subject at the issuer.
	WorkspaceID, Creator string
	// Version orders the key's records: only a higher one applies, and any
	// after a reset. 0 for a record whose version did not decode.
	Version   int64
	CreatedAt time.Time
	ExpiresAt time.Time
	// Terms is the key terms revision its creator agreed to, "" for none.
	Terms string
	// Revoked is the record's revocation, present or not; never read from
	// a time.
	Revoked   bool
	RevokedAt time.Time
	// Stop is the reason a revocation stops the key with here
	// (workspace.KeyStopReason); an unknown one is manual.
	Stop workspace.KeyStopReason
	// Grants are what the record lists: for a partial record, only those
	// whose mailbox and use read whole.
	Grants []IssuedGrant
	// Partial is a record not usable whole: of it only what narrows a key
	// held applies (its revocation, an earlier expiry, the grants it no
	// longer lists), and every grant it lists that is not held already is
	// refused (RefusedInvalid).
	Partial bool
}

// IssuedGrant is one use of one mailbox a record gives its key: who gave it,
// by their subject at the issuer, and when.
type IssuedGrant struct {
	AccountID string
	// Use is "read", "act" or "send"; any other is ignored.
	Use     string
	GivenBy string
	GivenAt time.Time
}

// IssuedRefusal is why a grant of an issued key was refused here, in the
// core's words; an extension maps them to its issuer's.
type IssuedRefusal string

// Why a grant of an issued key is refused.
const (
	// RefusedGiverNotReader: whoever gave read does not read the mailbox
	// here, or signs in nobody here.
	RefusedGiverNotReader IssuedRefusal = "giver_not_reader"
	// RefusedMailboxUnknown: no live mailbox of the key's workspace has that
	// id here, or the key's workspace is not held here.
	RefusedMailboxUnknown IssuedRefusal = "mailbox_unknown"
	// RefusedNotOffered: a use, or a workspace, this server never offers an
	// issued key (send, a team).
	RefusedNotOffered IssuedRefusal = "not_offered"
	// RefusedTeamWithheld: a team's, where this edition withholds teams.
	RefusedTeamWithheld IssuedRefusal = "team_withheld"
	// RefusedTermsNotCurrent: the key's terms were not the revision this
	// server accepts when it was first stored; judged once, for its life.
	RefusedTermsNotCurrent IssuedRefusal = "terms_not_current"
	// RefusedInvalid: a grant of a record not usable whole.
	RefusedInvalid IssuedRefusal = "invalid"
)

// IssuedRefusals lists every IssuedRefusal, as migration 0028 records them.
func IssuedRefusals() []IssuedRefusal {
	return []IssuedRefusal{RefusedGiverNotReader, RefusedMailboxUnknown, RefusedNotOffered, RefusedTeamWithheld,
		RefusedTermsNotCurrent, RefusedInvalid}
}

// IssuedKeysApplied is what applying a page did.
type IssuedKeysApplied struct {
	// Cursor is where the feed stands now; AppliedAt, when the page was
	// applied.
	Cursor    int64
	AppliedAt time.Time
	// Stored counts keys stored for the first time, Updated keys whose
	// record changed what is held, Revoked keys revoked, Forgotten records
	// of a selector this server forgot, Ended records of a key never held
	// that arrived ended (only what is left of them kept), Refused grants
	// refused, and Faults records that could not be written even narrowed,
	// whose key this server stopped instead.
	Stored, Updated, Revoked, Forgotten, Ended, Refused, Faults int
	// Narrowed is a page that took something away, or left something owed
	// to the issuer: what an extension tells its issuer at once.
	Narrowed bool
	// PassClosed is the page that ended a reset's pass; Unlisted counts the
	// keys it revoked because the pass did not list them.
	PassClosed bool
	Unlisted   int
}

// Errors of applying an issuer's keys.
var (
	// ErrNotKeyIssuer is a page of an issuer this server mirrors no keys of:
	// conflict.
	ErrNotKeyIssuer = errors.New("service: this server mirrors no keys of that issuer")
	errNotKeyIssuer = E(CodeConflict, "this server mirrors no keys of that issuer", ErrNotKeyIssuer)
)

// maxIssuedID bounds an issued key's id at its issuer, as the schema does.
const maxIssuedID = 64

// ApplyIssuedKeys applies a page of an issuer's key feed and moves its cursor,
// in one write: a compare and set on the directory's generation and reset
// and on the feed's cursor (conflict, ErrDirectoryMoved, for a page read
// before a reset or behind the feed), the feed's row made at the
// directory's reset when it has none. Each key of the page applies inside
// its own savepoint (docs/workspaces.md, "Applying the issuer's records"),
// and none fails the page: one the schema refuses applies again as a
// partial record, and one that still cannot be written gets the operator's
// stop, logged at error and counted in Faults. Any other failure (the
// database, the context) fails the whole page, which the extension tries
// again. The page that says nothing more is waiting records up to when the
// issuer published everything and, when a reset's pass is open, ends it:
// every key the pass did not list is revoked. Open streams and downloads
// check their access again once it committed.
//
// No principal: the extension that follows the issuer vouches for the page.
// A server whose workspaces are local is conflict (ErrNotFollowing), and an
// issuer this server mirrors no keys of conflict (ErrNotKeyIssuer).
func (s *Service) ApplyIssuedKeys(ctx context.Context, page IssuedKeysPage) (IssuedKeysApplied, error) {
	if err := s.following(); err != nil {
		return IssuedKeysApplied{}, err
	}
	if s.issuer == nil || page.Issuer != s.issuer.Issuer() {
		return IssuedKeysApplied{}, errNotKeyIssuer
	}
	if page.Cursor < 0 || page.Reset < 0 {
		return IssuedKeysApplied{}, E(CodeBadRequest, "a page's cursor and reset are never negative", nil)
	}
	now := s.now()
	var out IssuedKeysApplied
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		out = IssuedKeysApplied{}
		dir, ok, err := store.DirectoryTx(ctx, tx, page.Issuer)
		switch {
		case err != nil:
			return err
		case !ok:
			return errDirectoryGeneration
		case dir.Generation != page.Generation || dir.Reset != page.Reset:
			return errDirectoryMoved
		}
		feed, err := store.EnsureDirectoryKeysTx(ctx, tx, page.Issuer)
		if err != nil {
			return err
		}
		if feed.Reset != dir.Reset || feed.Cursor > page.Cursor {
			return errDirectoryMoved
		}
		a := &keyApplier{s: s, tx: tx, issuer: page.Issuer, now: now, feed: feed}
		for _, k := range page.Keys {
			if err := a.key(ctx, k, &out); err != nil {
				return err
			}
		}
		if page.Last && feed.PassOpen {
			n, err := a.closePass(ctx)
			if err != nil {
				return err
			}
			out.PassClosed, out.Unlisted = true, n
			out.Narrowed = true
		}
		out.Cursor, out.AppliedAt = page.Cursor, now
		return store.SetDirectoryKeysCursorTx(ctx, tx, page.Issuer, feed.Reset, page.Cursor, page.Last,
			max(micros(page.PublishedThrough), 0), now.UnixMicro())
	})
	if err != nil {
		return IssuedKeysApplied{}, fromDirectory(err, "applying the issuer's keys failed")
	}
	s.accessChanged()
	if out.PassClosed {
		s.log.Info("the issuer's keys were listed again after a reset; the keys it did not list are revoked",
			"unlisted", out.Unlisted)
	}
	return out, nil
}

// keyApplier applies records inside one write.
type keyApplier struct {
	s      *Service
	tx     *sql.Tx
	issuer string
	now    time.Time
	feed   store.DirectoryKeys
}

// keyOutcome is what one record did, merged into the page's only once its
// savepoint was released.
type keyOutcome struct {
	stored, updated, revoked, forgotten, ended bool
	refused                                    int
	narrowed                                   bool
}

func (o keyOutcome) into(out *IssuedKeysApplied) {
	for _, c := range []struct {
		on bool
		n  *int
	}{{o.stored, &out.Stored}, {o.updated, &out.Updated}, {o.revoked, &out.Revoked},
		{o.forgotten, &out.Forgotten}, {o.ended, &out.Ended}} {
		if c.on {
			*c.n++
		}
	}
	out.Refused += o.refused
	out.Narrowed = out.Narrowed || o.narrowed
}

// key applies one record in a savepoint; again as a partial record when the
// schema refused it; and, when that is refused too, stops its key here. Only
// a failure that is no refusal of the schema is returned, and fails the
// page.
func (a *keyApplier) key(ctx context.Context, k IssuedKey, out *IssuedKeysApplied) error {
	var got keyOutcome
	err := a.savepoint(ctx, func() error {
		var err error
		got, err = a.apply(ctx, k, false)
		return err
	})
	if err == nil {
		got.into(out)
		return nil
	}
	if !store.IsConstraint(err) {
		return err
	}
	first := err
	err = a.savepoint(ctx, func() error {
		var err error
		got, err = a.apply(ctx, k, true)
		return err
	})
	if err == nil {
		got.narrowed = true
		got.into(out)
		a.s.log.Warn("an issued key's record the schema refused was applied narrowed", "key", k.ExternalID,
			"workspace", k.WorkspaceID, "err", first)
		return nil
	}
	if !store.IsConstraint(err) {
		return err
	}
	out.Faults++
	out.Narrowed = true
	a.s.log.Error("issued key record not applied", "key", k.ExternalID, "workspace", k.WorkspaceID, "err", err)
	err = a.savepoint(ctx, func() error {
		_, err := stopIssuedKeyTx(ctx, a.tx, a.issuer, k.ExternalID, k.Selector, a.now.Unix())
		return err
	})
	if err != nil && !store.IsConstraint(err) {
		return err
	}
	if err != nil {
		a.s.log.Error("an issued key whose record could not be applied could not be stopped either",
			"key", k.ExternalID, "err", err)
	}
	return nil
}

// savepoint runs fn inside a savepoint of the applier's transaction, rolled
// back to on any failure, which it returns.
func (a *keyApplier) savepoint(ctx context.Context, fn func() error) error {
	if _, err := a.tx.ExecContext(ctx, `SAVEPOINT issued_key`); err != nil {
		return fmt.Errorf("service: open a savepoint: %w", err)
	}
	if err := fn(); err != nil {
		if _, rbErr := a.tx.ExecContext(ctx, `ROLLBACK TO issued_key`); rbErr != nil {
			return errors.Join(err, fmt.Errorf("service: roll back to the savepoint: %w", rbErr))
		}
		if _, relErr := a.tx.ExecContext(ctx, `RELEASE issued_key`); relErr != nil {
			return errors.Join(err, fmt.Errorf("service: release the savepoint: %w", relErr))
		}
		return err
	}
	if _, err := a.tx.ExecContext(ctx, `RELEASE issued_key`); err != nil {
		return fmt.Errorf("service: release the savepoint: %w", err)
	}
	return nil
}

// whole reports whether a record is usable whole: every member this server
// stores within its bounds, a version, a creation, and each grant naming a
// mailbox, a use it knows, who gave it and when, once, act only beside read
// (the issuer's own rule for this server's mailboxes). One that is not
// applies as a partial record.
func (k IssuedKey) whole() bool {
	switch {
	case !boundedText(k.ExternalID, maxIssuedID), !boundedText(k.Selector, maxIssuedID),
		!boundedText(k.Hash, 256), !boundedText(k.WorkspaceID, maxIssuedID), !boundedText(k.Creator, 255),
		k.Version < 1, k.CreatedAt.IsZero(), k.CreatedAt.UnixMicro() <= 0:
		return false
	}
	seen := map[grantKey]bool{}
	for _, g := range k.Grants {
		key := grantKey{g.AccountID, g.Use}
		if g.AccountID == "" || !knownUse(g.Use) || !boundedText(g.GivenBy, 255) || g.GivenAt.IsZero() ||
			g.GivenAt.UnixMicro() < 0 || seen[key] {
			return false
		}
		seen[key] = true
	}
	for key := range seen {
		if key.use == useAct && !seen[grantKey{key.account, useRead}] {
			return false
		}
	}
	return true
}

// The uses an issued key's grant names.
const (
	useRead = "read"
	useAct  = "act"
	useSend = "send"
)

func knownUse(use string) bool { return use == useRead || use == useAct || use == useSend }

// grantKey names one use of one mailbox.
type grantKey struct{ account, use string }

// heldKey is an issued key this server holds, as its rows say.
type heldKey struct {
	prefix, externalID, hash, workspaceID, createdBy string
	createdUS, version                               int64
	termsOK                                          bool
	revokedAt, expiresAt, endedAt                    int64
}

func (h heldKey) live(now int64) bool { return h.revokedAt == 0 && h.endedAt == 0 && h.expiresAt > now }

const heldKeyColumns = `k.prefix, i.external_id, k.hash, coalesce(k.workspace_id, ''), k.created_by, i.created_at_us,
	i.version, i.terms_ok, k.revoked_at, k.expires_at, i.ended_at`

func scanHeldKey(row interface{ Scan(...any) error }) (heldKey, error) {
	var h heldKey
	err := row.Scan(&h.prefix, &h.externalID, &h.hash, &h.workspaceID, &h.createdBy, &h.createdUS, &h.version,
		&h.termsOK, &h.revokedAt, &h.expiresAt, &h.endedAt)
	return h, err
}

// findHeld reads the key a record names: by its id at the issuer, else by its
// selector.
func (a *keyApplier) findHeld(ctx context.Context, k IssuedKey) (heldKey, bool, error) {
	if k.ExternalID != "" {
		h, err := scanHeldKey(a.tx.QueryRowContext(ctx, `SELECT `+heldKeyColumns+`
			FROM issued_keys i JOIN api_keys k ON k.prefix = i.prefix WHERE i.issuer = ? AND i.external_id = ?`,
			a.issuer, k.ExternalID))
		switch {
		case err == nil:
			return h, true, nil
		case !errors.Is(err, sql.ErrNoRows):
			return heldKey{}, false, fmt.Errorf("service: read an issued key: %w", err)
		}
	}
	if k.Selector == "" {
		return heldKey{}, false, nil
	}
	h, err := scanHeldKey(a.tx.QueryRowContext(ctx, `SELECT `+heldKeyColumns+`
		FROM issued_keys i JOIN api_keys k ON k.prefix = i.prefix WHERE k.prefix = ? AND i.issuer = ?`,
		k.Selector, a.issuer))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return heldKey{}, false, nil
	case err != nil:
		return heldKey{}, false, fmt.Errorf("service: read an issued key: %w", err)
	}
	return h, true, nil
}

// keyStub is what is left of an issued key once its record went
// (issued_key_ends).
type keyStub struct {
	how               string
	endedAt, forgetAt int64
}

func (a *keyApplier) stubOf(ctx context.Context, selector string) (keyStub, bool, error) {
	var st keyStub
	err := a.tx.QueryRowContext(ctx, `SELECT how, ended_at, forget_at FROM issued_key_ends WHERE prefix = ?`, selector).
		Scan(&st.how, &st.endedAt, &st.forgetAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return keyStub{}, false, nil
	case err != nil:
		return keyStub{}, false, fmt.Errorf("service: read what is left of an issued key: %w", err)
	}
	return st, true, nil
}

// revokedAtOf is a record's revocation moment: its own, never later than
// now, and now when it is unknown. Never 0, which is a live key.
func (a *keyApplier) revokedAtOf(k IssuedKey) int64 {
	t := k.RevokedAt
	if t.IsZero() || t.After(a.now) {
		t = a.now
	}
	return max(t.Unix(), 1)
}

// expiresAtOf is a record's expiry, floored to the second; a zero one is the
// earliest, now. Never 0, which is "never expires" for this server's own
// keys.
func (a *keyApplier) expiresAtOf(k IssuedKey) int64 {
	if k.ExpiresAt.IsZero() {
		return max(a.now.Unix(), 1)
	}
	return max(k.ExpiresAt.Unix(), 1)
}

// stopOf is the reason a record's revocation stops its key with here.
func stopOf(k IssuedKey) workspace.KeyStopReason {
	if k.Stop.Valid() && k.Stop != workspace.KeyStopMigration && k.Stop != workspace.KeyStopRestrictionEmptied {
		return k.Stop
	}
	return workspace.KeyStopManual
}

// apply applies one record, as partial when asked, in the order
// docs/workspaces.md gives: a selector forgotten, a key never held that
// arrived ended, a key that cannot be stored here, then a key stored or
// updated.
func (a *keyApplier) apply(ctx context.Context, k IssuedKey, forcePartial bool) (keyOutcome, error) {
	partial := forcePartial || k.Partial || !k.whole()
	nowSec := a.now.Unix()
	expiresAt := a.expiresAtOf(k)
	var revokedAt int64
	if k.Revoked {
		revokedAt = a.revokedAtOf(k)
	}
	ended := k.Revoked || expiresAt <= nowSec
	held, found, err := a.findHeld(ctx, k)
	if err != nil {
		return keyOutcome{}, err
	}
	if !found {
		if k.Selector != "" {
			stub, forgotten, err := a.stubOf(ctx, k.Selector)
			if err != nil {
				return keyOutcome{}, err
			}
			if forgotten {
				return a.forgotten(ctx, k, stub, ended, expiresAt)
			}
		}
		if ended {
			return a.endedNeverHeld(ctx, k, revokedAt, expiresAt)
		}
		if partial {
			return a.refuseListed(ctx, k, RefusedInvalid)
		}
		creator, cause, err := a.storable(ctx, k)
		if err != nil {
			return keyOutcome{}, err
		}
		if cause != "" {
			return a.refuseListed(ctx, k, cause)
		}
		return a.store(ctx, k, creator, expiresAt)
	}
	if !partial {
		same, err := a.sameKey(ctx, held, k)
		if err != nil {
			return keyOutcome{}, err
		}
		partial = !same
	}
	return a.update(ctx, held, k, partial, revokedAt, expiresAt)
}

// sameKey reports whether a whole record names the key held as it was
// stored: its selector, hash, workspace, creator and creation never change.
func (a *keyApplier) sameKey(ctx context.Context, h heldKey, k IssuedKey) (bool, error) {
	if h.prefix != k.Selector || h.externalID != k.ExternalID || h.hash != k.Hash || h.workspaceID != k.WorkspaceID ||
		h.createdUS != k.CreatedAt.UnixMicro() {
		return false, nil
	}
	user, ok, err := auth.IdentityUserTx(ctx, a.tx, a.issuer, k.Creator)
	if err != nil {
		return false, err
	}
	return ok && user == h.createdBy, nil
}

// storable reads whether a new, live, whole record can be stored here: its
// workspace a personal one this server holds from the issuer's source, and
// its creator that workspace's person. It returns the creator's id, or why
// every grant of the record is refused instead.
func (a *keyApplier) storable(ctx context.Context, k IssuedKey) (string, IssuedRefusal, error) {
	var kind, person string
	err := a.tx.QueryRowContext(ctx, `SELECT kind, coalesce(person_id, '') FROM workspaces
		WHERE id = ? AND source <> 'local'`, k.WorkspaceID).Scan(&kind, &person)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", RefusedMailboxUnknown, nil
	case err != nil:
		return "", "", fmt.Errorf("service: read an issued key's workspace: %w", err)
	case kind == string(workspace.KindTeam) && a.s.teamsWithheld:
		return "", RefusedTeamWithheld, nil
	case kind != string(workspace.KindPersonal):
		return "", RefusedNotOffered, nil
	}
	user, ok, err := auth.IdentityUserTx(ctx, a.tx, a.issuer, k.Creator)
	switch {
	case err != nil:
		return "", "", err
	case !ok || user != person:
		return "", RefusedNotOffered, nil
	}
	return user, "", nil
}

// forgotten applies a record of a selector this server forgot: never stored
// again. While a reset's pass is open, what the stub says is owed when it is
// narrower than the record (revoked; or an end before the record's expiry,
// never as a revocation). Outside a pass, the grants of a record still live
// by this server's clock are refused, so that its issuer drops what will
// never apply here.
func (a *keyApplier) forgotten(ctx context.Context, k IssuedKey, st keyStub, ended bool, expiresAt int64) (keyOutcome, error) {
	out := keyOutcome{forgotten: true}
	if a.feed.PassOpen {
		if k.Revoked || !boundedText(k.ExternalID, maxIssuedID) {
			return out, nil
		}
		var owed heldOwed
		switch {
		case st.how == "revoked":
			owed.revoked = true
		case expiresAt > st.endedAt:
			owed.expiresAt = st.endedAt
		}
		if owed.any() {
			if err := a.owe(ctx, k.ExternalID, "", owed); err != nil {
				return keyOutcome{}, err
			}
			out.narrowed = true
		}
		return out, nil
	}
	if ended {
		return out, nil
	}
	cause := RefusedNotOffered
	var held bool
	if err := a.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE id = ? AND source <> 'local')`,
		k.WorkspaceID).Scan(&held); err != nil {
		return keyOutcome{}, fmt.Errorf("service: read an issued key's workspace: %w", err)
	}
	if !held {
		cause = RefusedMailboxUnknown
	}
	refused, err := a.refuseListed(ctx, k, cause)
	refused.forgotten = true
	return refused, err
}

// endedNeverHeld applies a record that arrived ended, of a key this server
// never held: only what is left of it is kept (its selector, how it ended
// and when, until a year after its creation), never the record; and nothing
// of it at all when its selector or creation did not decode, or its creation
// is in the future, or that year is over. Whatever was owed of it goes.
func (a *keyApplier) endedNeverHeld(ctx context.Context, k IssuedKey, revokedAt, expiresAt int64) (keyOutcome, error) {
	out := keyOutcome{ended: true}
	if k.ExternalID != "" {
		if _, err := a.tx.ExecContext(ctx, `DELETE FROM issued_key_tell
			WHERE issuer = ? AND external_id = ? AND prefix = '' AND kind = 'refusal'`, a.issuer, k.ExternalID); err != nil {
			return keyOutcome{}, fmt.Errorf("service: drop what was owed of an ended key: %w", err)
		}
	}
	if !boundedText(k.Selector, maxIssuedID) || k.CreatedAt.IsZero() || k.CreatedAt.UnixMicro() <= 0 ||
		k.CreatedAt.After(a.now) {
		return out, nil
	}
	how, end := "expired", expiresAt
	if k.Revoked {
		how, end = "revoked", revokedAt
	}
	forget := k.CreatedAt.Unix() + int64(store.IssuedKeyEndRetention/time.Second)
	if forget <= a.now.Unix() {
		return out, nil
	}
	if _, err := a.tx.ExecContext(ctx, `INSERT INTO issued_key_ends(prefix, how, ended_at, forget_at)
		SELECT ?1, ?2, ?3, ?4 WHERE NOT EXISTS (SELECT 1 FROM issued_key_ends e WHERE e.prefix = ?1)`,
		k.Selector, how, end, forget); err != nil {
		return keyOutcome{}, fmt.Errorf("service: keep what is left of an ended key: %w", err)
	}
	out.narrowed = true
	return out, nil
}

// refuseListed refuses every grant of a record whose key is not stored here,
// for cause: owed to the issuer at the record's version, never at 0.
func (a *keyApplier) refuseListed(ctx context.Context, k IssuedKey, cause IssuedRefusal) (keyOutcome, error) {
	var out keyOutcome
	for _, g := range k.Grants {
		if g.AccountID == "" || !knownUse(g.Use) {
			continue
		}
		told, err := a.tellRefusal(ctx, k.ExternalID, "", k.Version, g.AccountID, g.Use, cause)
		if err != nil {
			return keyOutcome{}, err
		}
		if told {
			out.refused++
			out.narrowed = true
		}
	}
	return out, nil
}

// tellRefusal owes the issuer a grant's refusal, at a version of its record
// (never 0, which its issuer refuses), once.
func (a *keyApplier) tellRefusal(ctx context.Context, externalID, prefix string, version int64, accountID, use string,
	cause IssuedRefusal,
) (bool, error) {
	if version < 1 || !boundedText(externalID, maxIssuedID) || accountID == "" || !knownUse(use) {
		return false, nil
	}
	res, err := a.tx.ExecContext(ctx, `INSERT INTO issued_key_tell(issuer, kind, external_id, prefix, version,
		  account_id, use, cause, received_at)
		SELECT ?1, 'refusal', ?2, ?3, ?4, ?5, ?6, ?7, ?8
		 WHERE NOT EXISTS (SELECT 1 FROM issued_key_tell t WHERE t.issuer = ?1 AND t.kind = 'refusal'
		                     AND t.external_id = ?2 AND t.version = ?4 AND t.account_id = ?5 AND t.use = ?6)`,
		a.issuer, externalID, prefix, version, accountID, use, string(cause), a.now.Unix())
	if err != nil {
		return false, fmt.Errorf("service: owe a refusal to the issuer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("service: owe a refusal to the issuer: %w", err)
	}
	return n > 0, nil
}

// store stores a new key: its record, its terms judged once, and then what
// every storing and update does (after).
func (a *keyApplier) store(ctx context.Context, k IssuedKey, creator string, expiresAt int64) (keyOutcome, error) {
	termsOK := k.Terms != "" && k.Terms == a.s.consent.Keys
	if _, err := a.tx.ExecContext(ctx, `INSERT INTO api_keys(prefix, hash, name, scope, created_at, expires_at,
		  workspace_id, terms_version, created_by, issuer)
		VALUES (?, ?, '', 'write', ?, ?, ?, ?, ?, ?)`,
		k.Selector, k.Hash, k.CreatedAt.Unix(), expiresAt, k.WorkspaceID, k.Terms, creator, a.issuer); err != nil {
		return keyOutcome{}, fmt.Errorf("service: store an issued key: %w", err)
	}
	if _, err := a.tx.ExecContext(ctx, `INSERT INTO issued_keys(prefix, issuer, external_id, version, listed,
		  created_at_us, terms_ok) VALUES (?, ?, ?, ?, 1, ?, ?)`,
		k.Selector, a.issuer, k.ExternalID, k.Version, k.CreatedAt.UnixMicro(), termsOK); err != nil {
		return keyOutcome{}, fmt.Errorf("service: store an issued key's record: %w", err)
	}
	h := heldKey{
		prefix: k.Selector, externalID: k.ExternalID, hash: k.Hash, workspaceID: k.WorkspaceID, createdBy: creator,
		createdUS: k.CreatedAt.UnixMicro(), version: k.Version, termsOK: termsOK, expiresAt: expiresAt,
	}
	out, err := a.after(ctx, &h, k, false, expiresAt)
	out.stored = true
	return out, err
}

// update applies a record of a key held: a whole one only above the version
// held (any after a reset), a partial one for what it narrows; an earlier
// expiry; the revocation, once; then what every storing and update does.
func (a *keyApplier) update(ctx context.Context, h heldKey, k IssuedKey, partial bool, revokedAt, expiresAt int64) (keyOutcome, error) {
	var out keyOutcome
	version := h.version
	switch {
	case !partial && h.version > 0 && k.Version <= h.version:
		// Already applied: a record sent again changes nothing.
		return keyOutcome{}, nil
	case !partial:
		version = k.Version
	case k.Version > h.version:
		version = k.Version
	}
	if _, err := a.tx.ExecContext(ctx, `UPDATE issued_keys SET version = ?, listed = 1 WHERE prefix = ?`,
		version, h.prefix); err != nil {
		return keyOutcome{}, fmt.Errorf("service: record an issued key's version: %w", err)
	}
	h.version = version
	out.updated = true
	if expiresAt < h.expiresAt {
		if _, err := a.tx.ExecContext(ctx, `UPDATE api_keys SET expires_at = ? WHERE prefix = ?`,
			expiresAt, h.prefix); err != nil {
			return keyOutcome{}, fmt.Errorf("service: bring an issued key's expiry forward: %w", err)
		}
		h.expiresAt = expiresAt
		out.narrowed = true
	}
	if k.Revoked && h.revokedAt == 0 {
		if err := revokeIssuedTx(ctx, a.tx, h.prefix, stopOf(k), revokedAt, "revoked"); err != nil {
			return keyOutcome{}, err
		}
		h.revokedAt = revokedAt
		if h.endedAt == 0 {
			h.endedAt = revokedAt
		}
		out.revoked, out.narrowed = true, true
	}
	after, err := a.after(ctx, &h, k, partial, expiresAt)
	if err != nil {
		return keyOutcome{}, err
	}
	after.updated = true
	after.revoked = after.revoked || out.revoked
	after.narrowed = after.narrowed || out.narrowed
	return after, nil
}

// after is what every storing and update of a key does once its own record
// applied: the creator's belt (a key created before the directory last
// disabled them or revoked their credentials is revoked), the creator's
// stop where they are stopped here, the key's end when its expiry passed,
// its grants judged while it is live, and then, for a key ended longer ago
// than its record is kept, its record gone (only what is left of it stays),
// or, while a reset's pass is open, what it holds narrower than the record
// owed to the issuer.
func (a *keyApplier) after(ctx context.Context, h *heldKey, k IssuedKey, partial bool, recordExpires int64) (keyOutcome, error) {
	var out keyOutcome
	nowSec := a.now.Unix()
	if h.revokedAt == 0 {
		revoked, err := issuedBeltTx(ctx, a.tx, a.issuer, h.createdBy, h.prefix, nowSec)
		if err != nil {
			return keyOutcome{}, err
		}
		if revoked > 0 {
			h.revokedAt = nowSec
			if h.endedAt == 0 {
				h.endedAt = nowSec
			}
			out.revoked, out.narrowed = true, true
		}
	}
	if h.endedAt == 0 && h.revokedAt == 0 {
		if err := a.creatorStop(ctx, h); err != nil {
			return keyOutcome{}, err
		}
	}
	if h.endedAt == 0 && h.revokedAt == 0 && h.expiresAt <= nowSec {
		if err := endIssuedTx(ctx, a.tx, h.prefix, h.expiresAt, "expired"); err != nil {
			return keyOutcome{}, err
		}
		h.endedAt = h.expiresAt
		out.narrowed = true
	}
	var off []grantKey
	if h.live(nowSec) {
		res, err := a.grants(ctx, *h, k, partial)
		if err != nil {
			return keyOutcome{}, err
		}
		out.refused += res.refused
		out.narrowed = out.narrowed || res.narrowed
		off = res.keptOff
	}
	if h.endedAt != 0 && h.endedAt+int64(store.IssuedKeyRecordRetention/time.Second) <= nowSec {
		// Ended longer ago than its record is kept (a record replayed after
		// this server was restored, say): only what is left of it stays.
		if _, err := a.tx.ExecContext(ctx, `DELETE FROM api_keys WHERE prefix = ?`, h.prefix); err != nil {
			return keyOutcome{}, fmt.Errorf("service: forget an issued key that ended long ago: %w", err)
		}
		out.narrowed = true
		st, ok, err := a.stubOf(ctx, h.prefix)
		if err != nil || !ok {
			return out, err
		}
		gone, err := a.forgotten(ctx, k, st, true, recordExpires)
		out.narrowed = out.narrowed || gone.narrowed
		return out, err
	}
	if a.feed.PassOpen && boundedText(k.ExternalID, maxIssuedID) {
		var owed heldOwed
		if h.revokedAt != 0 && !k.Revoked {
			owed.revoked = true
		}
		if h.expiresAt < recordExpires {
			owed.expiresAt = h.expiresAt
		}
		owed.removed = off
		if owed.any() {
			if err := a.owe(ctx, k.ExternalID, h.prefix, owed); err != nil {
				return keyOutcome{}, err
			}
			out.narrowed = true
		}
	}
	return out, nil
}

// creatorStop gives a key the creator's stop when its creator is stopped
// here: disabled on this server, or suspended while the directory's last
// document of them says they are active (suspended outside its documents).
func (a *keyApplier) creatorStop(ctx context.Context, h *heldKey) error {
	var stopped bool
	err := a.tx.QueryRowContext(ctx, `SELECT (u.status = 'disabled' AND u.suspended_at = 0)
		    OR (u.suspended_at <> 0 AND coalesce((SELECT d.status FROM directory_people d
		                                          WHERE d.issuer = ? AND d.user_id = u.id), 'active') = 'active')
		  FROM users u WHERE u.id = ?`, a.issuer, h.createdBy).Scan(&stopped)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("service: read whether an issued key's creator is stopped here: %w", err)
	case !stopped:
		return nil
	}
	if _, err := a.tx.ExecContext(ctx, `INSERT INTO issued_key_stops(prefix, cause, at) VALUES (?, 'creator', ?)
		ON CONFLICT(prefix, cause) DO NOTHING`, h.prefix, a.now.Unix()); err != nil {
		return fmt.Errorf("service: stop an issued key with its creator: %w", err)
	}
	return nil
}

// grantRow is one grant of an issued key as issued_key_grants holds it.
type grantRow struct {
	givenBy   string
	givenAtUS int64
	state     string
	cause     IssuedRefusal
	// giverID is the giver's person here, known when this write judged it
	// held.
	giverID string
}

// The states of an issued key's grant.
const (
	grantHeld    = "held"
	grantWaiting = "waiting"
	grantRefused = "refused"
	grantRemoved = "removed"
)

// Why a grant is a removed tombstone.
const (
	removedByMailbox = "mailbox_removed"
	removedByReset   = "reset"
)

// grantsResult is what judging a record's grants did.
type grantsResult struct {
	refused  int
	narrowed bool
	// keptOff are the grants the record lists that this server keeps off
	// (refused before, or removed), which a reset's pass owes the issuer.
	keptOff []grantKey
}

// grants applies what a record lists to a live key: every row the record no
// longer lists goes (but a removed tombstone, kept while its mailbox is); a
// grant listed as it was keeps its state, never judged again, but one still
// waiting; a new grant is judged (newGrant), act after read and only with
// it; and what the key holds (key_access) follows the grants held, a row
// with no flag left deleted. A partial record judges nothing new: each new
// grant it lists is refused (RefusedInvalid). Every refusal decided here is
// owed to the issuer at the record's version.
func (a *keyApplier) grants(ctx context.Context, h heldKey, k IssuedKey, partial bool) (grantsResult, error) {
	var out grantsResult
	rows, err := a.grantRows(ctx, h.prefix)
	if err != nil {
		return grantsResult{}, err
	}
	listed := map[grantKey]IssuedGrant{}
	var order []grantKey
	for _, g := range k.Grants {
		key := grantKey{g.AccountID, g.Use}
		if g.AccountID == "" || !knownUse(g.Use) || g.GivenBy == "" {
			continue
		}
		if _, dup := listed[key]; dup {
			continue
		}
		listed[key] = g
		order = append(order, key)
	}
	// Read before act: act follows read on the same mailbox.
	slices.SortStableFunc(order, func(x, y grantKey) int {
		switch {
		case x.use == y.use:
			return 0
		case x.use == useRead:
			return -1
		case y.use == useRead:
			return 1
		}
		return 0
	})
	final := map[grantKey]grantRow{}
	for key, r := range rows {
		if _, ok := listed[key]; !ok && r.state == grantRemoved {
			final[key] = r
		}
	}
	decided := map[grantKey]bool{}
	for _, key := range order {
		g := listed[key]
		r, had := rows[key]
		same := had && r.givenBy == g.GivenBy && r.givenAtUS == g.GivenAt.UnixMicro()
		switch {
		case same && r.state != grantWaiting:
			// Judged once: kept as it was.
			if r.state == grantRefused || r.state == grantRemoved {
				out.keptOff = append(out.keptOff, key)
			}
			final[key] = r
		case same && partial:
			final[key] = r
		case same:
			// Still waiting: judged again, as its giver's document would.
			nr, err := a.judgeWaiting(ctx, h, key, r)
			if err != nil {
				return grantsResult{}, err
			}
			final[key], decided[key] = nr, nr.state != grantWaiting
		case partial:
			final[key] = grantRow{givenBy: g.GivenBy, givenAtUS: g.GivenAt.UnixMicro(), state: grantRefused,
				cause: RefusedInvalid}
			decided[key] = true
		default:
			nr, tomb, err := a.newGrant(ctx, h, key, g)
			if err != nil {
				return grantsResult{}, err
			}
			if tomb {
				out.keptOff = append(out.keptOff, key)
			}
			final[key], decided[key] = nr, true
		}
	}
	// Act only beside read held: otherwise it takes read's state (waiting,
	// refused or removed, with read's cause), or, with no read listed at
	// all, is refused as the record's.
	for key, r := range final {
		if key.use != useAct || r.state != grantHeld {
			continue
		}
		rd, ok := final[grantKey{key.account, useRead}]
		switch {
		case ok && rd.state == grantHeld:
			continue
		case !ok:
			r.state, r.cause = grantRefused, RefusedInvalid
		default:
			r.state, r.cause = rd.state, rd.cause
		}
		if r.state == grantRemoved {
			out.keptOff = append(out.keptOff, key)
		}
		final[key], decided[key] = r, true
	}
	// A waiting act whose read is decided follows it.
	for key, r := range final {
		if key.use != useAct || r.state != grantWaiting {
			continue
		}
		rd, ok := final[grantKey{key.account, useRead}]
		switch {
		case !ok:
			r.state, r.cause = grantRefused, RefusedInvalid
		case rd.state == grantWaiting:
			continue
		default:
			r.state, r.cause = rd.state, rd.cause
		}
		if r.state == grantRemoved {
			out.keptOff = append(out.keptOff, key)
		}
		final[key], decided[key] = r, true
	}
	changed, err := a.writeGrants(ctx, h, rows, final)
	if err != nil {
		return grantsResult{}, err
	}
	out.narrowed = changed
	// Every grant the record lists was listed by this version of it.
	for _, key := range order {
		if _, err := a.tx.ExecContext(ctx, `UPDATE issued_key_grants SET version = ?
			WHERE prefix = ? AND account_id = ? AND use = ? AND version <> ?`,
			h.version, h.prefix, key.account, key.use, h.version); err != nil {
			return grantsResult{}, fmt.Errorf("service: record the version that listed an issued key's grant: %w", err)
		}
	}
	for key, r := range final {
		if !decided[key] || r.state != grantRefused {
			continue
		}
		told, err := a.tellRefusal(ctx, h.externalID, h.prefix, k.Version, key.account, key.use, r.cause)
		if err != nil {
			return grantsResult{}, err
		}
		if told {
			out.refused++
			out.narrowed = true
		}
	}
	return out, nil
}

// grantRows reads an issued key's grants.
func (a *keyApplier) grantRows(ctx context.Context, prefix string) (map[grantKey]grantRow, error) {
	return grantRowsTx(ctx, a.tx, prefix)
}

func grantRowsTx(ctx context.Context, tx *sql.Tx, prefix string) (map[grantKey]grantRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT account_id, use, given_by, given_at_us, state, cause
		FROM issued_key_grants WHERE prefix = ?`, prefix)
	if err != nil {
		return nil, fmt.Errorf("service: read an issued key's grants: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	out := map[grantKey]grantRow{}
	for rows.Next() {
		var (
			key   grantKey
			r     grantRow
			cause string
		)
		if err := rows.Scan(&key.account, &key.use, &r.givenBy, &r.givenAtUS, &r.state, &cause); err != nil {
			return nil, fmt.Errorf("service: read an issued key's grants: %w", err)
		}
		r.cause = IssuedRefusal(cause)
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("service: read an issued key's grants: %w", err)
	}
	return out, nil
}

// newGrant judges a grant a record lists that this server has not judged:
// the key's terms; a use it offers (never send); a live mailbox of the key's
// workspace; while a reset's pass is open, a grant given before what this
// server trusted of the previous generation, which it never judges again (a
// removed tombstone, tomb); then read, by its giver (giverRead); and act,
// held for now and settled beside read by the caller.
func (a *keyApplier) newGrant(ctx context.Context, h heldKey, key grantKey, g IssuedGrant) (grantRow, bool, error) {
	r := grantRow{givenBy: g.GivenBy, givenAtUS: g.GivenAt.UnixMicro()}
	live, err := a.liveIn(ctx, key.account, h.workspaceID)
	if err != nil {
		return grantRow{}, false, err
	}
	switch {
	case !h.termsOK:
		r.state, r.cause = grantRefused, RefusedTermsNotCurrent
	case key.use != useRead && key.use != useAct:
		r.state, r.cause = grantRefused, RefusedNotOffered
	case !live:
		r.state, r.cause = grantRefused, RefusedMailboxUnknown
	case a.feed.PassOpen && r.givenAtUS < a.feed.TrustedThroughUS:
		r.state, r.cause = grantRemoved, removedByReset
		return r, true, nil
	case key.use == useRead:
		state, cause, giver, err := giverReadTx(ctx, a.tx, a.issuer, h, key.account, g.GivenBy)
		if err != nil {
			return grantRow{}, false, err
		}
		r.state, r.cause, r.giverID = state, cause, giver
	default:
		// act: settled beside read.
		r.state = grantHeld
	}
	return r, false, nil
}

// judgeWaiting judges again a grant still waiting: read by its giver, now;
// act stays waiting, settled beside read by the caller.
func (a *keyApplier) judgeWaiting(ctx context.Context, h heldKey, key grantKey, r grantRow) (grantRow, error) {
	if key.use != useRead {
		return r, nil
	}
	state, cause, giver, err := giverReadTx(ctx, a.tx, a.issuer, h, key.account, r.givenBy)
	if err != nil {
		return grantRow{}, err
	}
	r.state, r.cause, r.giverID = state, cause, giver
	return r, nil
}

// liveIn reports whether a mailbox is live in a workspace.
func (a *keyApplier) liveIn(ctx context.Context, accountID, workspaceID string) (bool, error) {
	var live bool
	if err := a.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts
		WHERE id = ? AND workspace_id = ? AND removed_at = 0)`, accountID, workspaceID).Scan(&live); err != nil {
		return false, fmt.Errorf("service: read a mailbox an issued key names: %w", err)
	}
	return live, nil
}

// giverReadTx judges read given by a subject of the issuer: refused when it
// signs in nobody here; waiting while the directory has said nothing of them
// since its last reset, or the key was not listed since then (its version
// 0); then held when they read the mailbox here now, by the one rule, and
// refused otherwise. It returns the giver's id when held.
func giverReadTx(ctx context.Context, tx *sql.Tx, issuer string, h heldKey, accountID, givenBy string,
) (string, IssuedRefusal, string, error) {
	user, ok, err := auth.IdentityUserTx(ctx, tx, issuer, givenBy)
	switch {
	case err != nil:
		return "", "", "", err
	case !ok:
		return grantRefused, RefusedGiverNotReader, "", nil
	}
	var fresh bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM directory_people p JOIN directories d
		  ON d.issuer = p.issuer
		 WHERE p.issuer = ? AND p.user_id = ? AND p.gone_at = 0 AND p.version > 0 AND p.reset = d.reset)`,
		issuer, user).Scan(&fresh); err != nil {
		return "", "", "", fmt.Errorf("service: read whether a giver's document applied: %w", err)
	}
	if !fresh || h.version == 0 {
		return grantWaiting, "", "", nil
	}
	reads, err := workspace.ReadsNowTx(ctx, tx, accountID, user)
	if err != nil {
		return "", "", "", err
	}
	if !reads {
		return grantRefused, RefusedGiverNotReader, "", nil
	}
	return grantHeld, "", user, nil
}

// writeGrants writes the grants as decided and what the key holds, and
// reports whether anything held was taken away.
func (a *keyApplier) writeGrants(ctx context.Context, h heldKey, before, final map[grantKey]grantRow) (bool, error) {
	return writeGrantsTx(ctx, a.tx, h, before, final, a.now.Unix())
}

func writeGrantsTx(ctx context.Context, tx *sql.Tx, h heldKey, before, final map[grantKey]grantRow, now int64) (bool, error) {
	narrowed := false
	for key, r := range before {
		if _, ok := final[key]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM issued_key_grants WHERE prefix = ? AND account_id = ? AND use = ?`,
			h.prefix, key.account, key.use); err != nil {
			return false, fmt.Errorf("service: drop an issued key's grant: %w", err)
		}
		if r.state == grantHeld {
			narrowed = true
		}
	}
	accounts := map[string]bool{}
	for key := range before {
		accounts[key.account] = true
	}
	for key, r := range final {
		accounts[key.account] = true
		was, had := before[key]
		if had && was == (grantRow{givenBy: r.givenBy, givenAtUS: r.givenAtUS, state: r.state, cause: r.cause}) {
			continue
		}
		if had && was.state == grantHeld && r.state != grantHeld {
			narrowed = true
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE id = ?)`, key.account).
			Scan(&exists); err != nil {
			return false, fmt.Errorf("service: read a mailbox an issued key names: %w", err)
		}
		if !exists {
			// A mailbox this server never had: refused, and owed, with no row.
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO issued_key_grants(prefix, account_id, use, given_by, given_at_us,
			  state, cause, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, (SELECT version FROM issued_keys WHERE prefix = ?))
			ON CONFLICT(prefix, account_id, use) DO UPDATE SET given_by = excluded.given_by,
			  given_at_us = excluded.given_at_us, state = excluded.state, cause = excluded.cause,
			  version = excluded.version`,
			h.prefix, key.account, key.use, r.givenBy, r.givenAtUS, r.state, string(r.cause), h.prefix); err != nil {
			return false, fmt.Errorf("service: record an issued key's grant: %w", err)
		}
	}
	for account := range accounts {
		read := final[grantKey{account, useRead}].state == grantHeld
		act := read && final[grantKey{account, useAct}].state == grantHeld
		if !read {
			n, err := execN(ctx, tx, `DELETE FROM key_access WHERE key_prefix = ? AND account_id = ?`, h.prefix, account)
			if err != nil {
				return false, fmt.Errorf("service: take what an issued key held: %w", err)
			}
			if n > 0 {
				narrowed = true
			}
			continue
		}
		var hadAct int
		err := tx.QueryRowContext(ctx, `SELECT act FROM key_access WHERE key_prefix = ? AND account_id = ?`,
			h.prefix, account).Scan(&hadAct)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			giver := final[grantKey{account, useRead}].giverID
			if _, err := tx.ExecContext(ctx, `INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act,
				  send, granted_by, created_at, updated_at) VALUES (?, ?, ?, 1, ?, 0, ?, ?, ?)`,
				h.prefix, account, h.workspaceID, act, giver, now, now); err != nil {
				return false, fmt.Errorf("service: give an issued key what it holds: %w", err)
			}
		case err != nil:
			return false, fmt.Errorf("service: read what an issued key holds: %w", err)
		case (hadAct == 1) != act:
			if _, err := tx.ExecContext(ctx, `UPDATE key_access SET act = ?, send = 0, updated_at = ?
				WHERE key_prefix = ? AND account_id = ?`, act, now, h.prefix, account); err != nil {
				return false, fmt.Errorf("service: change what an issued key holds: %w", err)
			}
			if !act {
				narrowed = true
			}
		}
	}
	return narrowed, nil
}

// heldOwed is what a key held narrower than a reset's pass listed it is
// owed to the issuer as: revoked, an earlier expiry (0 for none), and the
// grants kept off.
type heldOwed struct {
	revoked   bool
	expiresAt int64
	removed   []grantKey
}

func (o heldOwed) any() bool { return o.revoked || o.expiresAt != 0 || len(o.removed) > 0 }

// heldRemoved is one grant a key is held without, as issued_key_tell keeps
// it.
type heldRemoved struct {
	AccountID string `json:"account_id"`
	Use       string `json:"use"`
}

// maxHeldRemoved bounds the grants one key is owed as kept off.
const maxHeldRemoved = 1000

// owe merges what a key is held narrower than listed into the one row owed
// of it: revoked once revoked, the earliest expiry, the union of what is
// kept off.
func (a *keyApplier) owe(ctx context.Context, externalID, prefix string, o heldOwed) error {
	var (
		id      int64
		revoked bool
		expires int64
		raw     string
	)
	err := a.tx.QueryRowContext(ctx, `SELECT id, revoked, expires_at, removed FROM issued_key_tell
		WHERE issuer = ? AND external_id = ? AND kind = 'held'`, a.issuer, externalID).Scan(&id, &revoked, &expires, &raw)
	found := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		found, raw = false, "[]"
	case err != nil:
		return fmt.Errorf("service: read what is owed of a key held narrower: %w", err)
	}
	var removed []heldRemoved
	if err := json.Unmarshal([]byte(raw), &removed); err != nil {
		removed = nil
	}
	for _, key := range o.removed {
		r := heldRemoved{AccountID: key.account, Use: key.use}
		if !slices.Contains(removed, r) && len(removed) < maxHeldRemoved {
			removed = append(removed, r)
		}
	}
	if removed == nil {
		removed = []heldRemoved{}
	}
	body, err := json.Marshal(removed)
	if err != nil {
		return fmt.Errorf("service: owe a key held narrower: %w", err)
	}
	revoked = revoked || o.revoked
	if o.expiresAt != 0 && (expires == 0 || o.expiresAt < expires) {
		expires = o.expiresAt
	}
	if !found {
		_, err = a.tx.ExecContext(ctx, `INSERT INTO issued_key_tell(issuer, kind, external_id, prefix, revoked,
			  expires_at, removed, received_at) VALUES (?, 'held', ?, ?, ?, ?, ?, ?)`,
			a.issuer, externalID, prefix, revoked, expires, string(body), a.now.Unix())
	} else {
		_, err = a.tx.ExecContext(ctx, `UPDATE issued_key_tell SET revoked = ?, expires_at = ?, removed = ?,
			  prefix = CASE WHEN prefix = '' THEN ? ELSE prefix END WHERE id = ?`,
			revoked, expires, string(body), prefix, id)
	}
	if err != nil {
		return fmt.Errorf("service: owe a key held narrower: %w", err)
	}
	return nil
}

// closePass ends a reset's pass: every key of the issuer it did not list,
// still live, is revoked (manual), ended as unlisted; nothing of them is
// owed, since the issuer no longer has them.
func (a *keyApplier) closePass(ctx context.Context) (int, error) {
	prefixes, err := prefixesOf(ctx, a.tx, `SELECT i.prefix FROM issued_keys i JOIN api_keys k ON k.prefix = i.prefix
		WHERE i.issuer = ? AND i.listed = 0 AND i.ended_at = 0 AND k.revoked_at = 0`, a.issuer)
	if err != nil {
		return 0, err
	}
	for _, p := range prefixes {
		if err := revokeIssuedTx(ctx, a.tx, p, workspace.KeyStopManual, a.now.Unix(), "unlisted"); err != nil {
			return 0, err
		}
	}
	return len(prefixes), nil
}

// revokeIssuedTx revokes an issued key here, for reason, at a moment, and
// ends it (how: revoked, or unlisted by a pass): its grants still waiting
// and the refusals owed of it go; what it held stays with its record, which
// works no more.
func revokeIssuedTx(ctx context.Context, tx *sql.Tx, prefix string, reason workspace.KeyStopReason, at int64, how string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ?, stop_reason = ?
		WHERE prefix = ? AND revoked_at = 0`, at, string(reason), prefix); err != nil {
		return fmt.Errorf("service: revoke an issued key: %w", err)
	}
	return endIssuedTx(ctx, tx, prefix, at, how)
}

// endIssuedTx records that an issued key ended, at a moment and how, unless
// it ended before, and drops what will never be judged or owed of it.
func endIssuedTx(ctx context.Context, tx *sql.Tx, prefix string, at int64, how string) error {
	steps := []struct {
		what, query string
		args        []any
	}{
		{"end an issued key", `UPDATE issued_keys SET ended_at = ?, end_kind = ? WHERE prefix = ? AND ended_at = 0`,
			[]any{at, how, prefix}},
		{"drop an ended key's waiting grants", `DELETE FROM issued_key_grants WHERE prefix = ? AND state = 'waiting'`,
			[]any{prefix}},
		{"drop the refusals owed of an ended key", `DELETE FROM issued_key_tell WHERE prefix = ? AND kind = 'refusal'`,
			[]any{prefix}},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return fmt.Errorf("service: %s: %w", step.what, err)
		}
	}
	return nil
}

// issuedBeltTx revokes, inside the caller's transaction, every live key the
// issuer made for a person (or only the one under prefix, when given) that
// was created before the directory last revoked their credentials
// (account_recovered) or disabled them (creator_suspended), compared at
// microseconds with no margin: whichever of the key and the directory's
// mark arrives first, the other meets it. It reports how many it revoked.
func issuedBeltTx(ctx context.Context, tx *sql.Tx, issuer, userID, prefix string, now int64) (int, error) {
	type stop struct {
		prefix    string
		recovered bool
	}
	var stops []stop
	if err := scanAll(ctx, tx, `SELECT k.prefix, i.created_at_us < p.credentials_revoked_at_us
		  FROM api_keys k JOIN issued_keys i ON i.prefix = k.prefix
		  JOIN directory_people p ON p.issuer = i.issuer AND p.user_id = k.created_by AND p.gone_at = 0
		 WHERE i.issuer = ?1 AND k.created_by = ?2 AND k.revoked_at = 0 AND (?3 = '' OR k.prefix = ?3)
		   AND (i.created_at_us < p.credentials_revoked_at_us OR i.created_at_us < p.disabled_at_us)`,
		[]any{issuer, userID, prefix}, func(rows *sql.Rows) error {
			var s stop
			if err := rows.Scan(&s.prefix, &s.recovered); err != nil {
				return err
			}
			stops = append(stops, s)
			return nil
		}); err != nil {
		return 0, err
	}
	for _, s := range stops {
		reason := workspace.KeyStopCreatorSuspended
		if s.recovered {
			reason = workspace.KeyStopAccountRecovered
		}
		if err := revokeIssuedTx(ctx, tx, s.prefix, reason, now, "revoked"); err != nil {
			return 0, err
		}
	}
	return len(stops), nil
}

// judgeWaitingOfTx judges, inside the write that applied a person's
// document, every grant still waiting that they gave, on the keys of the
// issuer listed since its directory's last reset and still live: read held
// when they read the mailbox here now, refused otherwise; the act beside
// each read judged with it, whoever gave it. A refusal is owed to the
// issuer at the key's version. It reports how many grants it settled.
func judgeWaitingOfTx(ctx context.Context, tx *sql.Tx, issuer, subject string, now int64) (int, error) {
	prefixes, err := prefixesOf(ctx, tx, `SELECT DISTINCT g.prefix FROM issued_key_grants g
		  JOIN issued_keys i ON i.prefix = g.prefix JOIN api_keys k ON k.prefix = g.prefix
		 WHERE g.state = 'waiting' AND g.use = 'read' AND g.given_by = ? AND i.issuer = ? AND i.version > 0
		   AND i.ended_at = 0 AND k.revoked_at = 0 AND k.expires_at > ?`, subject, issuer, now)
	if err != nil {
		return 0, err
	}
	settled := 0
	for _, prefix := range prefixes {
		h, err := scanHeldKey(tx.QueryRowContext(ctx, `SELECT `+heldKeyColumns+`
			FROM issued_keys i JOIN api_keys k ON k.prefix = i.prefix WHERE i.prefix = ?`, prefix))
		if err != nil {
			return 0, fmt.Errorf("service: read an issued key: %w", err)
		}
		before, err := grantRowsTx(ctx, tx, prefix)
		if err != nil {
			return 0, err
		}
		final := make(map[grantKey]grantRow, len(before))
		decided := map[grantKey]bool{}
		for key, r := range before {
			final[key] = r
		}
		for key, r := range before {
			if key.use != useRead || r.state != grantWaiting || r.givenBy != subject {
				continue
			}
			state, cause, giver, err := giverReadTx(ctx, tx, issuer, h, key.account, r.givenBy)
			if err != nil {
				return 0, err
			}
			if state == grantWaiting {
				continue
			}
			r.state, r.cause, r.giverID = state, cause, giver
			final[key], decided[key] = r, true
			act := grantKey{key.account, useAct}
			if ar, ok := final[act]; ok && ar.state == grantWaiting {
				ar.state, ar.cause = state, cause
				final[act], decided[act] = ar, true
			}
		}
		if len(decided) == 0 {
			continue
		}
		if _, err := writeGrantsTx(ctx, tx, h, before, final, now); err != nil {
			return 0, err
		}
		for key, r := range final {
			if !decided[key] {
				continue
			}
			settled++
			if r.state != grantRefused || h.version < 1 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO issued_key_tell(issuer, kind, external_id, prefix, version,
				  account_id, use, cause, received_at)
				SELECT ?1, 'refusal', ?2, ?3, ?4, ?5, ?6, ?7, ?8
				 WHERE NOT EXISTS (SELECT 1 FROM issued_key_tell t WHERE t.issuer = ?1 AND t.kind = 'refusal'
				                     AND t.external_id = ?2 AND t.version = ?4 AND t.account_id = ?5 AND t.use = ?6)`,
				issuer, h.externalID, h.prefix, h.version, key.account, key.use, string(r.cause), now); err != nil {
				return 0, fmt.Errorf("service: owe a refusal to the issuer: %w", err)
			}
		}
	}
	return settled, nil
}

// stopIssuedKeyTx gives an issued key, named by its id at the issuer or else
// by its selector, the operator's stop, and reports whether it holds one.
func stopIssuedKeyTx(ctx context.Context, tx *sql.Tx, issuer, externalID, selector string, now int64) (bool, error) {
	var prefix string
	err := tx.QueryRowContext(ctx, `SELECT prefix FROM issued_keys WHERE issuer = ?1
		AND ((?2 <> '' AND external_id = ?2) OR (?2 = '' AND ?3 <> '' AND prefix = ?3))`,
		issuer, externalID, selector).Scan(&prefix)
	if errors.Is(err, sql.ErrNoRows) && externalID != "" && selector != "" {
		err = tx.QueryRowContext(ctx, `SELECT prefix FROM issued_keys WHERE issuer = ? AND prefix = ?`,
			issuer, selector).Scan(&prefix)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("service: read an issued key to stop: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO issued_key_stops(prefix, cause, at) VALUES (?, 'operator', ?)
		ON CONFLICT(prefix, cause) DO NOTHING`, prefix, now); err != nil {
		return false, fmt.Errorf("service: stop an issued key: %w", err)
	}
	return true, nil
}
