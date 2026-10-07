package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// What a workspace's API keys hold on its mailboxes (docs/workspaces.md, "API
// keys").
//
// A key belongs to one workspace and acts as no person. It reaches exactly the
// mailboxes of its workspace it holds a row on in key_access, each with its
// own read, act and send: what it holds stands on its own, whoever gave it,
// whatever becomes of them, until an owner or an admin takes it away or the
// key is revoked. Keys never count as readers and never pass read: only a
// person who reads a mailbox gives a key read on it, which the check the
// service hands each write decides. A carried-over key (migration 0012, no
// workspace) keeps the mailboxes it was carried over with and gains none.
//
// The repository keeps the data and what must always hold: a key holds
// mailboxes of its own workspace only (the schema too), act needs read and a
// key of the write scope or more, send a key of the send scope, a row holds
// at least one flag, and a revoked or expired key is given nothing.

// Errors of what a key holds.
var (
	// ErrNoKey is a key that does not exist, or not in this workspace.
	ErrNoKey = errors.New("workspace: no such key")
	// ErrKeyNotLive is giving something to a key that is revoked or expired.
	ErrKeyNotLive = errors.New("workspace: the key is revoked or expired")
	// ErrKeyScope is act for a key below the write scope, or send for one
	// below the send scope.
	ErrKeyScope = errors.New("workspace: the key's scope does not allow that flag")
	// ErrCarriedOver is giving a carried-over key a mailbox or a flag it did
	// not hold: it gains nothing.
	ErrCarriedOver = errors.New("workspace: a key carried over from a person's gains nothing")
	// ErrNoKeyAccess is a key that holds nothing on that mailbox.
	ErrNoKeyAccess = errors.New("workspace: the key holds nothing on that mailbox")
)

// KeyGrant is what a key is given on one mailbox. Manage is never a key's.
type KeyGrant struct {
	AccountID string
	Flags
}

// KeyAccess is what a key holds on one mailbox.
type KeyAccess struct {
	KeyPrefix   string
	AccountID   string
	WorkspaceID string
	Flags
	// GrantedBy is who set it last: "usr_…" or "migration"; empty once that
	// person is deleted.
	GrantedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// KeyRef is what a check needs of the key a write is about.
type KeyRef struct {
	Prefix string
	// WorkspaceID is the key's workspace; empty for a carried-over key.
	WorkspaceID string
	// Scope is read, write or send.
	Scope     string
	CreatedBy string
	Revoked   bool
	ExpiresAt time.Time
}

// CarriedOver reports whether the key is one migration 0012 carried over
// from a person's, with no workspace of its own.
func (k KeyRef) CarriedOver() bool { return k.WorkspaceID == "" }

// Live reports whether the key works at the given time.
func (k KeyRef) Live(now time.Time) bool {
	return !k.Revoked && (k.ExpiresAt.IsZero() || now.Before(k.ExpiresAt))
}

// KeyCheck is the service's rule for a write about what a key holds on a
// mailbox, run first inside the write's transaction: the key, the mailbox's
// workspace, and what the key held there before (zero for nothing).
type KeyCheck func(tx *sql.Tx, key KeyRef, mailboxWorkspace string, before Flags) error

// scopeAllows reports whether a key of the given scope may hold the flags.
func scopeAllows(scope string, f Flags) bool {
	if f.Act && scope != "write" && scope != "send" {
		return false
	}
	if f.Send && scope != "send" {
		return false
	}
	return true
}

// checkKeyFlags is what any flags a key is given must be.
func checkKeyFlags(f Flags) error {
	switch {
	case f.Manage:
		return ErrManageByRole
	case !f.Read && !f.Act && !f.Send:
		return ErrNoFlags
	case f.Act && !f.Read:
		return ErrActWithoutRead
	}
	return nil
}

// MailboxWorkspaceTx reads, inside the caller's transaction, the workspace a
// mailbox belongs to; ErrNoMailbox for a mailbox nobody knows.
func MailboxWorkspaceTx(ctx context.Context, tx *sql.Tx, accountID string) (string, error) {
	mb, err := mailboxTx(ctx, tx, accountID)
	if err != nil {
		return "", err
	}
	return mb.workspaceID, nil
}

// KeyRefTx reads a key inside the caller's transaction; ErrNoKey when there
// is none, or for an operator key, which holds no mailbox here.
func KeyRefTx(ctx context.Context, tx *sql.Tx, prefix string) (KeyRef, error) {
	return keyRefOn(ctx, tx, prefix)
}

func keyRefOn(ctx context.Context, q querier, prefix string) (KeyRef, error) {
	var (
		k                KeyRef
		revoked, expires int64
	)
	err := q.QueryRowContext(ctx, `SELECT prefix, coalesce(workspace_id, ''), scope, created_by, revoked_at, expires_at
		FROM api_keys WHERE prefix = ? AND (workspace_id IS NULL OR workspace_id <> ?)`, prefix, OperatorID).
		Scan(&k.Prefix, &k.WorkspaceID, &k.Scope, &k.CreatedBy, &revoked, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return KeyRef{}, ErrNoKey
	case err != nil:
		return KeyRef{}, fmt.Errorf("workspace: read the key: %w", err)
	}
	k.Revoked, k.ExpiresAt = revoked != 0, unix(expires)
	return k, nil
}

// PutKeyAccessTx gives a key what grant names on one mailbox of its
// workspace, inside the transaction that issues the key: the flags must be
// valid for its scope, and the mailbox in its workspace (ErrNoMailbox). Who
// may give what is the caller's to have decided.
func PutKeyAccessTx(ctx context.Context, tx *sql.Tx, prefix, workspaceID string, grant KeyGrant, grantedBy string, now time.Time) error {
	if err := checkKeyFlags(grant.Flags); err != nil {
		return err
	}
	mb, err := mailboxTx(ctx, tx, grant.AccountID)
	if err != nil {
		return err
	}
	if mb.workspaceID != workspaceID {
		return ErrNoMailbox
	}
	return writeKeyAccessTx(ctx, tx, prefix, mb.workspaceID, grant, grantedBy, now, false)
}

// writeKeyAccessTx inserts what a key holds on a mailbox, or with exists
// replaces it: an update, never an upsert, whose insert would run the
// schema's insert triggers on a row that is only changing.
func writeKeyAccessTx(ctx context.Context, tx *sql.Tx, prefix, workspaceID string, grant KeyGrant, grantedBy string,
	now time.Time, exists bool,
) error {
	at := now.UTC().Unix()
	var err error
	if exists {
		_, err = tx.ExecContext(ctx, `UPDATE key_access SET read = ?, act = ?, send = ?, granted_by = ?, updated_at = ?
			WHERE key_prefix = ? AND account_id = ?`,
			grant.Read, grant.Act, grant.Send, grantedBy, at, prefix, grant.AccountID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO key_access(key_prefix, account_id, workspace_id, read, act, send,
			  granted_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			prefix, grant.AccountID, workspaceID, grant.Read, grant.Act, grant.Send, grantedBy, at, at)
	}
	if err != nil {
		return fmt.Errorf("workspace: set what the key holds: %w", err)
	}
	return nil
}

// keyAccessTx reads what a key holds on a mailbox, the zero KeyAccess and
// ErrNoKeyAccess when nothing.
func keyAccessTx(ctx context.Context, q querier, prefix, accountID string) (KeyAccess, error) {
	var (
		a                KeyAccess
		created, updated int64
	)
	err := q.QueryRowContext(ctx, `SELECT key_prefix, account_id, workspace_id, read, act, send, granted_by,
		       created_at, updated_at
		  FROM key_access WHERE key_prefix = ? AND account_id = ?`, prefix, accountID).
		Scan(&a.KeyPrefix, &a.AccountID, &a.WorkspaceID, &a.Read, &a.Act, &a.Send, &a.GrantedBy, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return KeyAccess{}, ErrNoKeyAccess
	case err != nil:
		return KeyAccess{}, fmt.Errorf("workspace: read what the key holds: %w", err)
	}
	a.CreatedAt, a.UpdatedAt = unix(created), unix(updated)
	return a, nil
}

// SetKeyAccess sets exactly what a key holds on a mailbox, creating the row
// or replacing it, and returns it. The key must exist outside the operator
// workspace (ErrNoKey) and work (ErrKeyNotLive); the mailbox must be of its
// workspace (ErrNoMailbox); the flags must hold read, act or send
// (ErrNoFlags), act only with read (ErrActWithoutRead), never manage
// (ErrManageByRole), and only what the key's scope allows (ErrKeyScope). A
// carried-over key may only lose flags (ErrCarriedOver). Who may give what
// is check's to decide; it runs first.
func (r *Repository) SetKeyAccess(ctx context.Context, prefix, accountID string, flags Flags, grantedBy string, check KeyCheck) (KeyAccess, error) {
	if err := checkKeyFlags(flags); err != nil {
		return KeyAccess{}, err
	}
	now := r.now()
	var out KeyAccess
	err := r.store.Write(ctx, func(tx *sql.Tx) error {
		k, mb, before, err := keyTargetTx(ctx, tx, prefix, accountID)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(tx, k, mb.workspaceID, before.Flags); err != nil {
				return err
			}
		}
		if !k.Live(now) {
			return ErrKeyNotLive
		}
		if !scopeAllows(k.Scope, flags) {
			return ErrKeyScope
		}
		if k.CarriedOver() {
			if before.AccountID == "" || !before.Covers(flags) {
				return ErrCarriedOver
			}
		} else if mb.workspaceID != k.WorkspaceID {
			return ErrNoMailbox
		}
		if err := writeKeyAccessTx(ctx, tx, k.Prefix, mb.workspaceID, KeyGrant{AccountID: accountID, Flags: flags},
			grantedBy, now, before.AccountID != ""); err != nil {
			return err
		}
		out, err = keyAccessTx(ctx, tx, k.Prefix, accountID)
		return err
	})
	if err != nil {
		return KeyAccess{}, err
	}
	return out, nil
}

// DropKeyAccess takes a mailbox out of a key: everything it held there. A
// carried-over key left with none is revoked (the schema's trigger). It is
// ErrNoKeyAccess when the key held nothing there. check runs first.
func (r *Repository) DropKeyAccess(ctx context.Context, prefix, accountID string, check KeyCheck) error {
	return r.store.Write(ctx, func(tx *sql.Tx) error {
		k, mb, before, err := keyTargetTx(ctx, tx, prefix, accountID)
		if err != nil {
			return err
		}
		if check != nil {
			if err := check(tx, k, mb.workspaceID, before.Flags); err != nil {
				return err
			}
		}
		if before.AccountID == "" {
			return ErrNoKeyAccess
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM key_access WHERE key_prefix = ? AND account_id = ?`,
			k.Prefix, accountID); err != nil {
			return fmt.Errorf("workspace: take the mailbox out of the key: %w", err)
		}
		return nil
	})
}

// keyTargetTx reads the key, the mailbox and what the key holds on it, for a
// write about them: ErrNoKey, ErrNoMailbox; the zero KeyAccess when the key
// holds nothing there.
func keyTargetTx(ctx context.Context, tx *sql.Tx, prefix, accountID string) (KeyRef, mailbox, KeyAccess, error) {
	k, err := KeyRefTx(ctx, tx, prefix)
	if err != nil {
		return KeyRef{}, mailbox{}, KeyAccess{}, err
	}
	mb, err := mailboxTx(ctx, tx, accountID)
	if err != nil {
		return KeyRef{}, mailbox{}, KeyAccess{}, err
	}
	before, err := keyAccessTx(ctx, tx, k.Prefix, accountID)
	if err != nil && !errors.Is(err, ErrNoKeyAccess) {
		return KeyRef{}, mailbox{}, KeyAccess{}, err
	}
	return k, mb, before, nil
}

// KeyAccessOf reads what a key holds on each mailbox named: a mailbox it holds
// nothing on is absent. Whether the key still works is its authentication's
// and its re-check's, as for a session.
func (r *Repository) KeyAccessOf(ctx context.Context, prefix string, accountIDs []string) (map[string]Flags, error) {
	out := make(map[string]Flags, len(accountIDs))
	if len(accountIDs) == 0 || prefix == "" {
		return out, nil
	}
	list, err := json.Marshal(accountIDs)
	if err != nil {
		return nil, fmt.Errorf("workspace: encode ids: %w", err)
	}
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT x.account_id, x.read, x.act, x.send
		  FROM key_access x
		 WHERE x.key_prefix = ? AND x.account_id IN (SELECT value FROM json_each(?))`, prefix, string(list))
	if err != nil {
		return nil, fmt.Errorf("workspace: read what the key holds: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id string
			f  Flags
		)
		if err := rows.Scan(&id, &f.Read, &f.Act, &f.Send); err != nil {
			return nil, fmt.Errorf("workspace: read what the key holds: %w", err)
		}
		out[id] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: read what the key holds: %w", err)
	}
	return out, nil
}

// KeyWorkspaces lists the workspaces a key reaches: its own, or, for a
// carried-over key, those of the mailboxes it still holds.
func (r *Repository) KeyWorkspaces(ctx context.Context, prefix string) ([]string, error) {
	return listIDs(ctx, r.store.Reader(), `SELECT workspace_id FROM api_keys
		 WHERE prefix = ?1 AND workspace_id IS NOT NULL
		UNION
		SELECT x.workspace_id FROM key_access x JOIN api_keys k ON k.prefix = x.key_prefix
		 WHERE x.key_prefix = ?1 AND k.workspace_id IS NULL
		ORDER BY 1`, prefix)
}

// revokeCreatedInTx revokes the keys a person created in a workspace, when
// they leave it: a key of the workspace is revoked; a carried-over key loses
// the workspace's mailboxes, and is revoked with its last (the schema's
// trigger). It returns how many keys of the workspace it revoked.
func revokeCreatedInTx(ctx context.Context, tx *sql.Tx, workspaceID, userID string, now int64) (int, error) {
	res, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ?
		WHERE workspace_id = ? AND created_by = ? AND revoked_at = 0`, now, workspaceID, userID)
	if err != nil {
		return 0, fmt.Errorf("workspace: revoke the keys the person created: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("workspace: revoke the keys the person created: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_access
		WHERE workspace_id = ? AND key_prefix IN (SELECT prefix FROM api_keys WHERE workspace_id IS NULL AND created_by = ?)`,
		workspaceID, userID); err != nil {
		return 0, fmt.Errorf("workspace: take the workspace out of the person's carried-over keys: %w", err)
	}
	return int(n), nil
}

// MailboxKey is a key holding something on a mailbox, as the access directory
// lists it: live keys only.
type MailboxKey struct {
	Prefix string
	Name   string
	Scope  string
	Flags
	// CreatedBy is the person who created the key; empty once they are
	// deleted.
	CreatedBy string
	GrantedBy string
	UpdatedAt time.Time
	// CarriedOver is a key migration 0012 carried over, with no workspace of
	// its own.
	CarriedOver bool
}

// mailboxKeys lists the live keys holding something on each mailbox of a
// workspace, by mailbox, oldest first.
func (r *Repository) mailboxKeys(ctx context.Context, workspaceID string) (map[string][]MailboxKey, error) {
	rows, err := r.store.Reader().QueryContext(ctx, `SELECT x.account_id, k.prefix, k.name, k.scope, x.read, x.act,
		       x.send, k.created_by, x.granted_by, x.updated_at, k.workspace_id IS NULL
		  FROM key_access x JOIN api_keys k ON k.prefix = x.key_prefix
		 WHERE x.workspace_id = ? AND k.revoked_at = 0 AND (k.expires_at = 0 OR k.expires_at > ?)
		 ORDER BY x.created_at, k.created_at, k.prefix`, workspaceID, r.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("workspace: list the keys on the workspace's mailboxes: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	out := map[string][]MailboxKey{}
	for rows.Next() {
		var (
			accountID string
			k         MailboxKey
			updated   int64
		)
		if err := rows.Scan(&accountID, &k.Prefix, &k.Name, &k.Scope, &k.Read, &k.Act, &k.Send, &k.CreatedBy,
			&k.GrantedBy, &updated, &k.CarriedOver); err != nil {
			return nil, fmt.Errorf("workspace: list the keys on the workspace's mailboxes: %w", err)
		}
		k.UpdatedAt = unix(updated)
		out[accountID] = append(out[accountID], k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workspace: list the keys on the workspace's mailboxes: %w", err)
	}
	return out, nil
}
