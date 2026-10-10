package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/thehappieco/mailie/internal/store"
)

// Flags are what a grant lets its holder do with a mailbox.
type Flags struct {
	// Read is searching and reading messages, originals and attachments,
	// listing folders, the mailbox's events and storage. Stored, it is the
	// flag; on a mailbox that has a key, the flag reads only beside the
	// person's grant at its current epoch, which Access reports.
	Read bool
	// Act is marking, starring, archiving, moving and trashing messages. It
	// needs Read: an action names messages the actor reads.
	Act bool
	// Send is sending from the mailbox.
	Send bool
	// Manage is the mailbox's card and re-authorizing it. Owners and admins
	// hold it on every mailbox of their workspace by their role, and also
	// remove it and change who holds what on it; it is stored only for
	// members. It never opens the index.
	Manage bool
}

// AllFlags is every flag.
func AllFlags() Flags { return Flags{Read: true, Act: true, Send: true, Manage: true} }

// LinkFlags is what the person who links a mailbox holds on it: read, act
// and send. They manage it by their role: only an owner or an admin links
// one into a team, and a person owns their personal workspace.
func LinkFlags() Flags { return Flags{Read: true, Act: true, Send: true} }

// Any reports whether any flag is set.
func (f Flags) Any() bool { return f.Read || f.Act || f.Send || f.Manage }

// All reports whether every flag is set.
func (f Flags) All() bool { return f == AllFlags() }

// Covers reports whether f holds every flag need holds.
func (f Flags) Covers(need Flags) bool {
	return (f.Read || !need.Read) && (f.Act || !need.Act) && (f.Send || !need.Send) && (f.Manage || !need.Manage)
}

// Without is f with the flags of drop taken away. Dropping read drops act
// too, which needs it.
func (f Flags) Without(drop Flags) Flags {
	out := Flags{Read: f.Read && !drop.Read, Act: f.Act && !drop.Act, Send: f.Send && !drop.Send, Manage: f.Manage && !drop.Manage}
	if !out.Read {
		out.Act = false
	}
	return out
}

func (f Flags) check() error {
	if !f.Any() {
		return ErrNoFlags
	}
	if f.Act && !f.Read {
		return ErrActWithoutRead
	}
	return nil
}

// Grant is one person's grant on one mailbox.
type Grant struct {
	AccountID   string
	WorkspaceID string
	UserID      string
	Flags
	// GrantedBy is "usr_…" (a person signed in, or the linker's own grant),
	// "key:<prefix>" (the operator: the command line goes through the daemon
	// with its instance admin key) or "migration"; empty once the person who
	// granted it is deleted.
	GrantedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Sealed is the person holding a grant at the mailbox key's current
	// epoch (a SealedGrant): on a mailbox that has a key, what reading needs
	// beside the flag. Always false on a mailbox without one, which is read
	// by the flag alone.
	Sealed bool
}

// grantColumns are a Grant's, over mailbox_access aliased g.
var grantColumns = `g.account_id, g.workspace_id, g.user_id, g.read, g.act, g.send, g.manage, g.granted_by,
	g.created_at, g.updated_at, ` + store.CurrentGrantSQL("g.account_id", "g.user_id")

func scanGrant(row rowScanner) (Grant, error) {
	var (
		g                Grant
		created, updated int64
	)
	err := row.Scan(&g.AccountID, &g.WorkspaceID, &g.UserID, &g.Read, &g.Act, &g.Send, &g.Manage, &g.GrantedBy,
		&created, &updated, &g.Sealed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Grant{}, ErrNoGrant
		}
		return Grant{}, fmt.Errorf("workspace: scan grant: %w", err)
	}
	g.CreatedAt, g.UpdatedAt = unix(created), unix(updated)
	return g, nil
}

// Grant reads a person's grant on a mailbox, whether or not their membership
// counts right now, nor they hold the key; ErrNoGrant when there is none.
// What they may do now is Access's.
func (r *Repository) Grant(ctx context.Context, accountID, userID string) (Grant, error) {
	return grantOn(ctx, r.store.Reader(), accountID, userID)
}

// GrantTx reads a person's grant inside the caller's transaction: how a Check
// re-reads what the caller holds where it is used.
func GrantTx(ctx context.Context, tx *sql.Tx, accountID, userID string) (Grant, error) {
	return grantOn(ctx, tx, accountID, userID)
}

func grantOn(ctx context.Context, q querier, accountID, userID string) (Grant, error) {
	return scanGrant(q.QueryRowContext(ctx,
		`SELECT `+grantColumns+` FROM mailbox_access g WHERE g.account_id = ? AND g.user_id = ?`, accountID, userID))
}

// ManagesTx is nil when userID manages a mailbox right now, inside the
// caller's transaction: as an active owner or admin of its workspace, active
// on the instance, or holding manage on it as an active member. An empty
// userID is the operator, who manages the operator workspace's mailboxes and
// no other. Otherwise ErrNoGrant, or ErrNoMailbox for a mailbox nobody knows.
//
// It is what a consent attempt is held to when it stores its grant: whoever
// started it must still be someone who may (account.Registry.CheckFlowsWith).
func ManagesTx(ctx context.Context, tx *sql.Tx, accountID, userID string) error {
	mb, err := mailboxTx(ctx, tx, accountID)
	if err != nil {
		return err
	}
	if userID == "" {
		if mb.kind == KindOperator {
			return nil
		}
		return ErrNoGrant
	}
	held, err := accessOn(ctx, tx, userID, []string{accountID})
	if err != nil {
		return err
	}
	if !held[accountID].Manage {
		return ErrNoGrant
	}
	return nil
}

// Access reads what a person may do with each mailbox named, as an active
// member of its workspace active on the instance: read as the one rule says
// (store.ReaderSQL: the flag, and on a mailbox that has a key a grant at its
// current epoch), act from their grant where they read, send from their
// grant, and manage from their grant or from their role, owner or admin,
// which manages every mailbox of the workspace. A mailbox they neither hold a
// grant on nor manage by their role is absent: the caller's own "access" on
// each mailbox they see. One they hold a grant on is present even when every
// flag it reports is off: a member waiting for the key holds read and act and
// reads nothing yet, and still sees the mailbox's card.
func (r *Repository) Access(ctx context.Context, userID string, accountIDs []string) (map[string]Flags, error) {
	return accessOn(ctx, r.store.Reader(), userID, accountIDs)
}

func accessOn(ctx context.Context, q querier, userID string, accountIDs []string) (map[string]Flags, error) {
	out := make(map[string]Flags, len(accountIDs))
	if len(accountIDs) == 0 || userID == "" {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT id, reads, act AND reads, send, manage FROM (
		SELECT a.id, coalesce(`+store.ReaderSQL("g")+`, 0) AS reads, coalesce(g.act, 0) AS act,
		       coalesce(g.send, 0) AS send, coalesce(g.manage, 0) OR m.role IN ('owner', 'admin') AS manage
		  FROM accounts a
		  JOIN workspace_members m ON m.workspace_id = a.workspace_id AND m.user_id = ?1 AND m.status = 'active'
		  JOIN users u ON u.id = m.user_id AND u.status = 'active'
		  LEFT JOIN mailbox_access g ON g.account_id = a.id AND g.user_id = m.user_id
		 WHERE a.id IN (SELECT value FROM json_each(?2))
		   AND (g.account_id IS NOT NULL OR m.role IN ('owner', 'admin')))`,
		userID, string(list))
	if err != nil {
		return nil, fmt.Errorf("workspace: read access: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id string
			f  Flags
		)
		if err := rows.Scan(&id, &f.Read, &f.Act, &f.Send, &f.Manage); err != nil {
			return nil, fmt.Errorf("workspace: read access: %w", err)
		}
		out[id] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read access: %w", err)
	}
	return out, nil
}

// mailbox is what a grant change needs to know of the mailbox.
type mailbox struct {
	id, workspaceID string
	kind            Kind
	// personID is the person of a personal workspace's mailbox.
	personID string
}

func mailboxTx(ctx context.Context, tx *sql.Tx, accountID string) (mailbox, error) {
	var mb mailbox
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.workspace_id, w.kind, coalesce(w.person_id, '')
		FROM accounts a JOIN workspaces w ON w.id = a.workspace_id WHERE a.id = ?`, accountID).
		Scan(&mb.id, &mb.workspaceID, &mb.kind, &mb.personID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return mailbox{}, ErrNoMailbox
	case err != nil:
		return mailbox{}, fmt.Errorf("workspace: read the mailbox: %w", err)
	}
	return mb, nil
}

// grantable refuses a grant change on a personal or the operator workspace's
// mailbox: a personal mailbox is its person's, with a grant that never
// changes, and the operator workspace has no members.
func (mb mailbox) grantable() error {
	switch mb.kind {
	case KindPersonal:
		return ErrPersonal
	case KindOperator:
		return ErrOperator
	}
	return nil
}

// SetGrant sets exactly the flags a person holds on a mailbox, creating the
// grant or replacing it. The person must be an active member of the
// mailbox's workspace (ErrNotMember); flags must hold something (ErrNoFlags),
// act only with read (ErrActWithoutRead), and manage only for a member: an
// owner or an admin manages by their role (ErrManageByRole). Refused on a
// personal or the operator workspace's mailbox, and when it would take read
// from the last person who can read it (ErrLastReader).
//
// Who may grant what is the Check's to decide. Losing manage ends the
// consent attempts the person started on the mailbox, in the same
// transaction. What the keys they gave something to hold stands on its own.
//
// On a mailbox that has a key, read given to a person with an account key
// comes with their grant, which SetGrantSealed takes: SetGrant refuses it
// (ErrSealedGrantNeeded). Taking read deletes the person's grants on the
// mailbox, every epoch's, in the same transaction.
func (r *Repository) SetGrant(ctx context.Context, accountID, userID string, flags Flags, grantedBy string, check Check) (Grant, error) {
	return r.SetGrantSealed(ctx, accountID, userID, flags, nil, grantedBy, check)
}

// SetGrantSealed is SetGrant with the grant that gives read on a mailbox that
// has a key (docs/key-scheme.md sections 9.3 and 12.13): the mailbox's
// private key sealed to the person at its current epoch, written with the
// flag in one transaction. The grant is required when the change adds read
// on such a mailbox for a person with an account key (ErrSealedGrantNeeded),
// and refused otherwise: with a change that does not add read
// (ErrSealedGrantUnwanted: supplying the key, SupplyGrant, is how a member
// who holds the flag gets one), on a mailbox without a key (ErrKeyless), and
// for a person without an account key (ErrNotEnrolled), who is given read by
// the flag alone and waits for the key. It must have a grant's shape
// (keyscheme.ErrShape) at the mailbox's current epoch (ErrEpoch), be sealed,
// by what its browser says, to the person's account public key now
// (ErrSealedToAnother), and the person hold no grant at that epoch yet
// (ErrSealedGrantExists).
//
// That the giver reads the mailbox themself, and has a fresh step-up when
// the recipient is someone else, is the Check's to decide (ReadsNowTx).
func (r *Repository) SetGrantSealed(ctx context.Context, accountID, userID string, flags Flags, sealed *Sealed, grantedBy string,
	check Check,
) (Grant, error) {
	if err := flags.check(); err != nil {
		return Grant{}, err
	}
	if sealed != nil {
		if _, err := grantEpoch(sealed.Grant); err != nil {
			return Grant{}, err
		}
	}
	now := r.now().Unix()
	var out Grant
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := mb.grantable(); err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		m, err := MemberTx(ctx, tx, mb.workspaceID, userID)
		if errors.Is(err, ErrNotMember) || (err == nil && !m.Active()) {
			return ErrNotMember
		}
		if err != nil {
			return err
		}
		if flags.Manage && m.Role != RoleMember {
			return ErrManageByRole
		}
		before, err := GrantTx(ctx, tx, accountID, userID)
		if err != nil && !errors.Is(err, ErrNoGrant) {
			return err
		}
		if before.Read && !flags.Read {
			if err := requireAnotherReaderTx(ctx, tx, userID, []string{accountID}); err != nil {
				return err
			}
		}
		epoch, err := sealedWithReadTx(ctx, tx, accountID, userID, flags.Read && !before.Read, sealed)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage,
			  granted_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id, user_id) DO UPDATE SET read = excluded.read, act = excluded.act, send = excluded.send,
			  manage = excluded.manage, granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
			accountID, mb.workspaceID, userID, flags.Read, flags.Act, flags.Send, flags.Manage, grantedBy, now, now); err != nil {
			return fmt.Errorf("workspace: set grant: %w", err)
		}
		if sealed != nil {
			if err := insertSealedGrantTx(ctx, tx, accountID, mb.workspaceID, userID, epoch, sealed.Grant, grantedBy, now); err != nil {
				return err
			}
		}
		if before.Read && !flags.Read {
			if err := dropSealedGrantsTx(ctx, tx, accountID, userID); err != nil {
				return err
			}
		}
		if before.Manage && !flags.Manage {
			if err := dropAttemptsTx(ctx, tx, userID, accountID); err != nil {
				return err
			}
		}
		out, err = GrantTx(ctx, tx, accountID, userID)
		return err
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

// Revoke takes flags away from a person's grant on a mailbox; zero drop takes
// every one. Dropping read drops act too. A grant left with no flag is
// deleted. It returns what is left, the zero Grant when nothing is.
//
// Refused as SetGrant is on personal and operator mailboxes, and when it
// would take read from the last person who can read the mailbox
// (ErrLastReader). Losing manage, which only a member stores, ends the
// consent attempts they started on it. Losing read deletes their grants on
// the mailbox, every epoch's, even where send or manage stay.
func (r *Repository) Revoke(ctx context.Context, accountID, userID string, drop Flags, check Check) (Grant, error) {
	if !drop.Any() {
		drop = AllFlags()
	}
	now := r.now().Unix()
	var out Grant
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := mb.grantable(); err != nil {
			return err
		}
		if err := runCheck(tx, check); err != nil {
			return err
		}
		before, err := GrantTx(ctx, tx, accountID, userID)
		if err != nil {
			return err
		}
		left := before.Without(drop)
		if left == before.Flags {
			out = before
			return nil
		}
		if before.Read && !left.Read {
			if err := requireAnotherReaderTx(ctx, tx, userID, []string{accountID}); err != nil {
				return err
			}
		}
		if left.Any() {
			_, err = tx.ExecContext(ctx, `UPDATE mailbox_access SET read = ?, act = ?, send = ?, manage = ?, updated_at = ?
				WHERE account_id = ? AND user_id = ?`, left.Read, left.Act, left.Send, left.Manage, now, accountID, userID)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_access WHERE account_id = ? AND user_id = ?`, accountID, userID)
		}
		if err != nil {
			return fmt.Errorf("workspace: revoke: %w", err)
		}
		if before.Read && !left.Read {
			if err := dropSealedGrantsTx(ctx, tx, accountID, userID); err != nil {
				return err
			}
		}
		if before.Manage && !left.Manage {
			if err := dropAttemptsTx(ctx, tx, userID, accountID); err != nil {
				return err
			}
		}
		if left.Any() {
			out, err = GrantTx(ctx, tx, accountID, userID)
		}
		return err
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

// GrantLinkTx gives the person who links a mailbox read, act and send on it,
// inside the transaction that creates it (LinkFlags): they manage it by their
// role. They must be an active member of its workspace; who may link where is
// the caller's to have decided.
func GrantLinkTx(ctx context.Context, tx *sql.Tx, accountID, workspaceID, userID string, now time.Time) error {
	m, err := MemberTx(ctx, tx, workspaceID, userID)
	if err != nil {
		return err
	}
	if !m.Active() {
		return ErrNotMember
	}
	at := now.UTC().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage,
		  granted_by, created_at, updated_at) VALUES (?, ?, ?, 1, 1, 1, 0, ?, ?, ?)`,
		accountID, workspaceID, userID, userID, at, at); err != nil {
		return fmt.Errorf("workspace: grant the linker: %w", err)
	}
	return nil
}

func (r *Repository) directoryMailboxes(ctx context.Context, workspaceID string) ([]MailboxAccess, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT a.id, a.email, a.provider, a.state, a.linked_by, w.kind,
		       a.sync_enabled_at, a.sync_enabled_by, a.sync_consent_version, a.sync_enabled_via = 'migration',
		       (SELECT count(*) FROM mailbox_access g WHERE g.account_id = a.id AND `+store.ReaderSQL("g")+`),
		       coalesce(`+store.CurrentEpochSQL("a.id")+`, 0)
		  FROM accounts a JOIN workspaces w ON w.id = a.workspace_id
		 WHERE a.workspace_id = ?
		 ORDER BY a.created_at, a.rowid`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list the directory: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []MailboxAccess
	for rows.Next() {
		var (
			mb   MailboxAccess
			kind Kind
			at   int64
		)
		if err := rows.Scan(&mb.AccountID, &mb.Email, &mb.Provider, &mb.State, &mb.LinkedBy, &kind,
			&at, &mb.Sync.By, &mb.Sync.Version, &mb.Sync.Migrated, &mb.Readers, &mb.Epoch); err != nil {
			return nil, fmt.Errorf("workspace: list the directory: %w", err)
		}
		mb.Sync.At = unix(at)
		mb.NoReader = kind == KindTeam && mb.Readers == 0
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list the directory: %w", err)
	}
	return out, nil
}

// MailboxAccess is one mailbox of a workspace and who holds what on it: the
// access directory, which shows addresses and grants, never the index.
type MailboxAccess struct {
	AccountID string
	Email     string
	Provider  string
	State     string
	// LinkedBy is who linked the mailbox, for attribution only: "usr_…",
	// "key:<prefix>" or "cli"; empty once that person is deleted, or for a
	// mailbox of the operator linked before anybody recorded it.
	LinkedBy string
	// Sync is the mailbox's own consent to sync: a team mailbox's
	// workspace's, or an operator mailbox's switch. Zero for a personal
	// mailbox, which syncs under its person's.
	Sync Consent
	// Readers counts who reads it now, by the one rule (store.ReaderSQL):
	// active members, active on the instance, holding read, and on a
	// mailbox that has a key a grant at its current epoch.
	Readers int
	// NoReader is a team mailbox nobody can read: it syncs nothing until
	// someone can, and only an owner or an admin who removes it and links it
	// again gets read on it again.
	NoReader bool
	// Epoch is the mailbox key's current epoch; 0 for a mailbox without a
	// key, which is read by the flag alone.
	Epoch int
	// Grants are who holds what on it; each says whether its person holds
	// the key at the current epoch (Grant.Sealed).
	Grants []Grant
	// Keys are the live keys holding something on it: what each holds, who
	// created it and who gave it last.
	Keys []MailboxKey
}

// Consent is a mailbox's own consent to sync, as the directory shows it. A
// zero At is off.
type Consent struct {
	At time.Time
	// By is who gave it: "usr_…", "key:<prefix>" or "cli"; empty once that
	// person is deleted.
	By string
	// Version is the revision of the sync text it was given to; empty for an
	// operator mailbox.
	Version string
	// Migrated is a consent migration 0011 copied from the person who linked
	// the mailbox (By), still bound to them: their withdrawal, or their being
	// disabled or deleted, stops it, until an owner or an admin confirms the
	// team's consent at the current revision.
	Migrated bool
}

// Directory lists a workspace's mailboxes and the grants on each, oldest
// first (as linked, within a second), each mailbox's grants by when they were
// made and then by address, and the live keys holding something on each, by
// when they were given it: what its owners and admins and the operator see.
func (r *Repository) Directory(ctx context.Context, workspaceID string) ([]MailboxAccess, error) {
	if _, err := r.Get(ctx, workspaceID); err != nil {
		return nil, err
	}
	out, err := r.directoryMailboxes(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(out))
	for i, mb := range out {
		index[mb.AccountID] = i
	}

	grants, err := r.store.Reader().QueryContext(ctx, `SELECT `+grantColumns+` FROM mailbox_access g
		  JOIN users u ON u.id = g.user_id
		 WHERE g.workspace_id = ? ORDER BY g.created_at, u.email, g.user_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace: list the directory's grants: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = grants.Close() }()
	for grants.Next() {
		g, err := scanGrant(grants)
		if err != nil {
			return nil, err
		}
		if i, ok := index[g.AccountID]; ok {
			out[i].Grants = append(out[i].Grants, g)
		}
	}
	if err := grants.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list the directory's grants: %w", err)
	}
	keys, err := r.mailboxKeys(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Keys = keys[out[i].AccountID]
	}
	return out, nil
}
