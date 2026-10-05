package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Flags are what a grant lets its holder do with a mailbox.
type Flags struct {
	// Read is searching and reading messages, originals and attachments,
	// listing folders, the mailbox's events and storage.
	Read bool
	// Act is marking, starring, archiving, moving and trashing messages. It
	// needs Read: an action names messages the actor reads.
	Act bool
	// Send is sending from the mailbox.
	Send bool
	// Manage is re-authorizing and removing the mailbox, and seeing and
	// changing who has access to it.
	Manage bool
}

// AllFlags is every flag: what the person who links a mailbox holds on it.
func AllFlags() Flags { return Flags{Read: true, Act: true, Send: true, Manage: true} }

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
	// GrantedBy is "usr_…", "key:<prefix>", "cli" or "migration"; empty once
	// the person who granted it is deleted.
	GrantedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const grantColumns = `g.account_id, g.workspace_id, g.user_id, g.read, g.act, g.send, g.manage, g.granted_by,
	g.created_at, g.updated_at`

func scanGrant(row rowScanner) (Grant, error) {
	var (
		g                Grant
		created, updated int64
	)
	err := row.Scan(&g.AccountID, &g.WorkspaceID, &g.UserID, &g.Read, &g.Act, &g.Send, &g.Manage, &g.GrantedBy,
		&created, &updated)
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
// counts right now; ErrNoGrant when there is none.
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

// activeGrant is the condition, over mailbox_access aliased g, that the
// grant counts: its holder is an active member of the mailbox's workspace and
// active on the instance.
const activeGrant = `EXISTS (SELECT 1 FROM workspace_members m JOIN users u ON u.id = m.user_id
	WHERE m.workspace_id = g.workspace_id AND m.user_id = g.user_id AND m.status = 'active' AND u.status = 'active')`

// ManagesTx is nil when userID manages a mailbox right now, inside the
// caller's transaction: they hold manage on it as an active member of its
// workspace, active on the instance. An empty userID is the operator, who
// manages the operator workspace's mailboxes and no other. Otherwise
// ErrNoGrant, or ErrNoMailbox for a mailbox nobody knows.
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
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access g
		WHERE g.account_id = ? AND g.user_id = ? AND g.manage = 1 AND `+activeGrant, accountID, userID).Scan(&n); err != nil {
		return fmt.Errorf("workspace: read the grant: %w", err)
	}
	if n == 0 {
		return ErrNoGrant
	}
	return nil
}

// Access reads what a person holds on each mailbox named, counting only
// grants whose holder is an active member: the caller's own "access" on each
// mailbox they see. A mailbox they hold nothing on is absent.
func (r *Repository) Access(ctx context.Context, userID string, accountIDs []string) (map[string]Flags, error) {
	out := make(map[string]Flags, len(accountIDs))
	if len(accountIDs) == 0 || userID == "" {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT g.account_id, g.read, g.act, g.send, g.manage
		  FROM mailbox_access g
		 WHERE g.user_id = ? AND g.account_id IN (SELECT value FROM json_each(?)) AND `+activeGrant,
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
	id, workspaceID, linkedBy string
	kind                      Kind
}

func mailboxTx(ctx context.Context, tx *sql.Tx, accountID string) (mailbox, error) {
	var mb mailbox
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.workspace_id, coalesce(a.owner_user_id, ''), w.kind
		FROM accounts a JOIN workspaces w ON w.id = a.workspace_id WHERE a.id = ?`, accountID).
		Scan(&mb.id, &mb.workspaceID, &mb.linkedBy, &mb.kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return mailbox{}, ErrNoMailbox
	case err != nil:
		return mailbox{}, fmt.Errorf("workspace: read the mailbox: %w", err)
	}
	return mb, nil
}

// grantable refuses a grant change on a personal or the operator workspace's
// mailbox, and one to the person the mailbox syncs under.
func (mb mailbox) grantable(userID string) error {
	switch mb.kind {
	case KindPersonal:
		return ErrPersonal
	case KindOperator:
		return ErrOperator
	}
	if userID == mb.linkedBy {
		return ErrLinker
	}
	return nil
}

// SetGrant sets exactly the flags a person holds on a mailbox, creating the
// grant or replacing it. The person must be an active member of the
// mailbox's workspace (ErrNotMember); flags must hold something (ErrNoFlags)
// and act only with read (ErrActWithoutRead). Refused on a personal or the
// operator workspace's mailbox, on the grant of the person it syncs under,
// and when the mailbox would be left without a holder of manage.
//
// Who may grant what (only a holder passes read, act and send on) is the
// Check's to decide. Losing read takes the mailbox out of the restrictions
// of the person's keys, and losing manage ends the consent attempts they
// started on it, in the same transaction.
func (r *Repository) SetGrant(ctx context.Context, accountID, userID string, flags Flags, grantedBy string, check Check) (Grant, error) {
	if err := flags.check(); err != nil {
		return Grant{}, err
	}
	now := r.now().Unix()
	var out Grant
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if err := mb.grantable(userID); err != nil {
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
		before, err := GrantTx(ctx, tx, accountID, userID)
		if err != nil && !errors.Is(err, ErrNoGrant) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage,
			  granted_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id, user_id) DO UPDATE SET read = excluded.read, act = excluded.act, send = excluded.send,
			  manage = excluded.manage, granted_by = excluded.granted_by, updated_at = excluded.updated_at`,
			accountID, mb.workspaceID, userID, flags.Read, flags.Act, flags.Send, flags.Manage, grantedBy, now, now); err != nil {
			return fmt.Errorf("workspace: set grant: %w", err)
		}
		if before.Read && !flags.Read {
			if err := forgetInKeysTx(ctx, tx, userID, []string{accountID}); err != nil {
				return err
			}
		}
		if before.Manage && !flags.Manage {
			if err := dropAttemptsTx(ctx, tx, userID, accountID); err != nil {
				return err
			}
		}
		if err := requireManagerTx(ctx, tx, mb); err != nil {
			return err
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
// Refused as SetGrant is on the mailboxes and the grant it protects; a
// mailbox would be left without a holder of manage. Losing read takes the
// mailbox out of the restrictions of the person's keys, and losing manage
// ends the consent attempts they started on it.
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
		if err := mb.grantable(userID); err != nil {
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
			if err := forgetInKeysTx(ctx, tx, userID, []string{accountID}); err != nil {
				return err
			}
		}
		if before.Manage && !left.Manage {
			if err := dropAttemptsTx(ctx, tx, userID, accountID); err != nil {
				return err
			}
		}
		if err := requireManagerTx(ctx, tx, mb); err != nil {
			return err
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

// requireManagerTx refuses a linked mailbox left with nobody holding manage.
// While the linker rule holds the linker always does; this is its own check
// all the same.
func requireManagerTx(ctx context.Context, tx *sql.Tx, mb mailbox) error {
	if mb.linkedBy == "" {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mailbox_access WHERE account_id = ? AND manage = 1`,
		mb.id).Scan(&n); err != nil {
		return fmt.Errorf("workspace: count managers: %w", err)
	}
	if n == 0 {
		return ErrLastManager
	}
	return nil
}

// GrantLinkerTx gives the person who links a mailbox every flag on it, inside
// the transaction that creates it. They must be an active member of its
// workspace; who may link where is the caller's to have decided.
func GrantLinkerTx(ctx context.Context, tx *sql.Tx, accountID, workspaceID, userID string, now time.Time) error {
	m, err := MemberTx(ctx, tx, workspaceID, userID)
	if err != nil {
		return err
	}
	if !m.Active() {
		return ErrNotMember
	}
	at := now.UTC().Unix()
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_access(account_id, workspace_id, user_id, read, act, send, manage,
		  granted_by, created_at, updated_at) VALUES (?, ?, ?, 1, 1, 1, 1, ?, ?, ?)`,
		accountID, workspaceID, userID, userID, at, at); err != nil {
		return fmt.Errorf("workspace: grant the linker: %w", err)
	}
	return nil
}

// TakeOver makes userID the person a mailbox is linked by, whose consent to
// sync it then syncs under, and returns who it was linked by before. The
// taker must be an active member holding every flag on it
// (ErrNeedsFullGrant); whether they may link there and have agreed to sync is
// the Check's. The previous linker keeps their grant, as an ordinary one. The
// index is kept. Taking over one's own link changes nothing.
func (r *Repository) TakeOver(ctx context.Context, accountID, userID string, check Check) (string, error) {
	now := r.now().Unix()
	var previous string
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		mb, err := mailboxTx(ctx, tx, accountID)
		if err != nil {
			return err
		}
		if mb.kind == KindOperator {
			return ErrOperator
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
		g, err := GrantTx(ctx, tx, accountID, userID)
		if errors.Is(err, ErrNoGrant) || (err == nil && !g.All()) {
			return ErrNeedsFullGrant
		}
		if err != nil {
			return err
		}
		previous = mb.linkedBy
		if previous == userID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET owner_user_id = ?, updated_at = ? WHERE id = ?`,
			userID, now, accountID); err != nil {
			return fmt.Errorf("workspace: take over: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return previous, nil
}

func (r *Repository) directoryMailboxes(ctx context.Context, workspaceID, managedBy string) ([]MailboxAccess, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT a.id, a.email, a.provider, a.state, coalesce(a.owner_user_id, '')
		  FROM accounts a
		 WHERE a.workspace_id = ?1
		   AND (?2 = '' OR EXISTS (SELECT 1 FROM mailbox_access g WHERE g.account_id = a.id AND g.user_id = ?2 AND g.manage = 1))
		 ORDER BY a.created_at, a.id`, workspaceID, managedBy)
	if err != nil {
		return nil, fmt.Errorf("workspace: list the directory: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []MailboxAccess
	for rows.Next() {
		var mb MailboxAccess
		if err := rows.Scan(&mb.AccountID, &mb.Email, &mb.Provider, &mb.State, &mb.LinkedBy); err != nil {
			return nil, fmt.Errorf("workspace: list the directory: %w", err)
		}
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
	// LinkedBy is the person the mailbox syncs under; empty in the operator
	// workspace.
	LinkedBy string
	Grants   []Grant
}

// Directory lists a workspace's mailboxes and their grants, oldest first,
// each mailbox's grants by when they were made and then by address.
// With managedBy, only the mailboxes that person holds manage on.
func (r *Repository) Directory(ctx context.Context, workspaceID, managedBy string) ([]MailboxAccess, error) {
	if _, err := r.Get(ctx, workspaceID); err != nil {
		return nil, err
	}
	out, err := r.directoryMailboxes(ctx, workspaceID, managedBy)
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
	return out, nil
}
