package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Where workspaces and their memberships come from.
const (
	// SourceLocal is created and changed here, from the console and the
	// command line: the self-hosted edition.
	SourceLocal = "local"
	// SourcePlatform is mirrored from an external directory, which owns
	// workspaces and memberships; this server creates none of its own. The
	// name is what the schema stores (migration 0008).
	SourcePlatform = "platform"
)

// Source is where workspaces and their memberships come from.
//
// Only a binary that embeds the daemon picks one (app.Options); the default,
// and the only one the open edition has, is Local. Whatever the source,
// linking mailboxes, grants, keys and consents stay Mailie's, and so does the
// operator workspace.
type Source interface {
	// Name is SourceLocal or SourcePlatform.
	Name() string
	// Changeable reports whether workspaces and memberships may be created
	// and changed here, from the console and the command line: nil for the
	// local source, ErrManagedElsewhere for the one mirroring a directory.
	Changeable() error
	// PersonCreatedTx runs inside the transaction that creates a person. The
	// local source creates their personal workspace and its membership
	// there; the other source does nothing, since the directory's document,
	// applied in that same transaction, does (the service's
	// SignInExternal).
	PersonCreatedTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error
}

// Local is the source of the self-hosted edition: everything is created and
// changed here.
func Local() Source { return localSource{} }

// Platform is the source of a server that mirrors an external directory,
// which an extension of the daemon follows (docs/workspaces.md, "Following an
// external directory"): workspaces and memberships are never created or
// changed here (Changeable is ErrManagedElsewhere), a person created here
// gets no workspace from it (PersonCreatedTx creates nothing), and the
// directory's documents, applied by the service through this package's
// mirror (mirror.go), create and change them, the person's personal
// workspace included. RepairPersonal makes nothing under it.
func Platform() Source { return platformSource{} }

type localSource struct{}

func (localSource) Name() string      { return SourceLocal }
func (localSource) Changeable() error { return nil }

// PersonCreatedTx creates the person's personal workspace, with them as its
// one member, an active owner, as old as the person.
func (localSource) PersonCreatedTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error {
	id, err := newID()
	if err != nil {
		return err
	}
	at := now.UTC().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workspaces(id, kind, source, name, person_id, created_at, updated_at)
		 VALUES (?, 'personal', 'local', '', ?, ?, ?)`, id, userID, at, at); err != nil {
		return fmt.Errorf("workspace: create the personal workspace: %w", err)
	}
	return insertMemberTx(ctx, tx, id, userID, RoleOwner, now)
}

type platformSource struct{}

func (platformSource) Name() string      { return SourcePlatform }
func (platformSource) Changeable() error { return ErrManagedElsewhere }

// PersonCreatedTx creates nothing: the directory's document does.
func (platformSource) PersonCreatedTx(context.Context, *sql.Tx, string, time.Time) error { return nil }
