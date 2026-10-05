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
	// SourcePlatform is mirrored from the platform, which owns workspaces
	// and memberships for the products it serves; Mailie creates none of its
	// own.
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
	// local source, ErrManagedElsewhere for the platform's.
	Changeable() error
	// PersonCreatedTx runs inside the transaction that creates a person. The
	// local source creates their personal workspace and its membership
	// there; the platform source does nothing, since the platform's document
	// creates it.
	PersonCreatedTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error
}

// Local is the source of the self-hosted edition: everything is created and
// changed here.
func Local() Source { return localSource{} }

// Platform is the source of a server whose workspaces the platform owns. In
// this phase it is a stub: it refuses every local change and creates nothing.
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

// PersonCreatedTx creates nothing: the platform's document will.
func (platformSource) PersonCreatedTx(context.Context, *sql.Tx, string, time.Time) error { return nil }
