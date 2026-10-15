package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// The external directories an extension of the daemon follows (migration
// 0022; docs/workspaces.md, "Following an external directory").
//
// A directory is followed by its issuer. Its row holds the generation of its
// data the documents applied belong to, whether the directory confirmed it,
// how many times it was reset here, how far its feed was read, and up to
// when it says it published every change: one row, so a generation and its
// cursor move together, and go back only with a reset, which never goes
// back (the schema's triggers). Per person it followed, what it last said of
// them (directory_people): the document's version, counted under the reset
// it was applied at, their status, and the moments it disabled them and
// revoked their sessions and credentials, in microseconds, kept as the
// highest ever seen, across resets too. A person's row outlives their
// deletion (gone_at) until an extension releases them, which forgets them
// in the journal as their deletion would have.
//
// These are the transaction's steps; what a document does to a person, a
// workspace and a membership is internal/service's, which runs them inside
// its one write.

// Directory is a directory followed, as directories holds it.
type Directory struct {
	Issuer     string
	Generation string
	Confirmed  bool
	// Reset counts the resets recorded for it here: a new generation, or
	// the directory forgetting where its feed stood.
	Reset int64
	// Cursor is where its feed was read up to.
	Cursor int64
	// PublishedThroughUS is the moment, in unix microseconds, up to which
	// it said it published every change, with no page of its feed left
	// unread; zero when none was trusted since the last reset.
	PublishedThroughUS int64
	// ChangedAt is when the row last changed, in unix seconds.
	ChangedAt int64
}

// DirectoryPerson is what a directory last said of a person
// (directory_people).
type DirectoryPerson struct {
	Issuer string
	UserID string
	// Subject is the person's identifier at the directory.
	Subject string
	// Generation and Version are the document last applied; Version is 0
	// when none was since the last reset, which also counts Reset.
	Generation string
	Version    int64
	Reset      int64
	// Status is "active", "disabled" or "closed".
	Status string
	// The moments, in unix microseconds, it last said it disabled them,
	// revoked their sessions and revoked their credentials: the highest
	// ever seen, 0 for never.
	DisabledAtUS           int64
	SessionsRevokedAtUS    int64
	CredentialsRevokedAtUS int64
	// AppliedAt is when a document of theirs was last applied, and GoneAt
	// when they were deleted here, 0 while they exist; unix seconds.
	AppliedAt int64
	GoneAt    int64
}

// Errors of the directories followed.
var (
	// ErrNoDirectory is a directory never followed here.
	ErrNoDirectory = errors.New("store: no such directory")
	// ErrDirectoryMoved is a write that expected a directory where it no
	// longer is: another generation or reset, a cursor already past, or
	// no row at all.
	ErrDirectoryMoved = errors.New("store: the directory moved on")
	// ErrPersonStillHere is a directory's person released while they still
	// exist.
	ErrPersonStillHere = errors.New("store: the person is still here")
)

const directoryColumns = `issuer, generation, confirmed, reset, cursor, published_through_us, changed_at`

func scanDirectory(row interface{ Scan(...any) error }) (Directory, error) {
	var d Directory
	err := row.Scan(&d.Issuer, &d.Generation, &d.Confirmed, &d.Reset, &d.Cursor, &d.PublishedThroughUS, &d.ChangedAt)
	return d, err
}

// directoryOn reads a directory's row; false when it was never followed.
func directoryOn(ctx context.Context, q querier, issuer string) (Directory, bool, error) {
	d, err := scanDirectory(q.QueryRowContext(ctx, `SELECT `+directoryColumns+` FROM directories WHERE issuer = ?`, issuer))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Directory{}, false, nil
	case err != nil:
		return Directory{}, false, fmt.Errorf("store: read the directory: %w", err)
	}
	return d, true, nil
}

// Directory reads a directory's row; false when it was never followed.
func (s *Store) Directory(ctx context.Context, issuer string) (Directory, bool, error) {
	return directoryOn(ctx, s.r, issuer)
}

// DirectoryTx is Directory inside the caller's transaction.
func DirectoryTx(ctx context.Context, tx *sql.Tx, issuer string) (Directory, bool, error) {
	return directoryOn(ctx, tx, issuer)
}

// EnsureDirectoryTx reads a directory's row inside the caller's transaction,
// creating it at generation, unconfirmed and never reset, when it was never
// followed: what the first document applied for it does. A row already there
// is returned as it is, whatever its generation, for the caller to compare.
func EnsureDirectoryTx(ctx context.Context, tx *sql.Tx, issuer, generation string, now int64) (Directory, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO directories(issuer, generation, changed_at) VALUES (?, ?, ?)
		ON CONFLICT(issuer) DO NOTHING`, issuer, generation, now); err != nil {
		return Directory{}, fmt.Errorf("store: follow the directory: %w", err)
	}
	d, ok, err := directoryOn(ctx, tx, issuer)
	if err == nil && !ok {
		err = fmt.Errorf("%w: %s", ErrNoDirectory, issuer)
	}
	return d, err
}

// ResetDirectoryTx records, inside the caller's transaction, that a directory
// starts again at generation: a new generation, or the directory forgetting
// where its feed stood (gone), whatever the generation. Its row, created at
// generation when it was never followed (nothing else to reset then), moves
// to generation, unconfirmed, with its cursor and what it published through
// forgotten and its reset counted once more. Every person's document version
// of the directory falls to 0, counted under that reset, so the next
// document of each applies whatever its version; what the directory revoked
// is kept. Every workspace from another source has its version fall to 0 the
// same way, counted under the sum of every directory's resets. The schema
// lets a version fall in no other way (migration 0022).
func ResetDirectoryTx(ctx context.Context, tx *sql.Tx, issuer, generation string, now int64) (Directory, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO directories(issuer, generation, changed_at) VALUES (?, ?, ?)
		ON CONFLICT(issuer) DO NOTHING`, issuer, generation, now)
	if err != nil {
		return Directory{}, fmt.Errorf("store: follow the directory: %w", err)
	}
	created, err := res.RowsAffected()
	if err != nil {
		return Directory{}, fmt.Errorf("store: follow the directory: %w", err)
	}
	if created == 0 {
		steps := []struct {
			what, query string
			args        []any
		}{
			{"reset the directory", `UPDATE directories
				SET generation = ?, confirmed = 0, cursor = 0, published_through_us = 0, reset = reset + 1, changed_at = ?
				WHERE issuer = ?`, []any{generation, now, issuer}},
			{"forget its people's versions", `UPDATE directory_people
				SET version = 0, reset = (SELECT d.reset FROM directories d WHERE d.issuer = directory_people.issuer)
				WHERE issuer = ?`, []any{issuer}},
			{"forget its workspaces' versions", `UPDATE workspaces
				SET source_version = 0, source_reset = (SELECT coalesce(sum(d.reset), 0) FROM directories d)
				WHERE source <> 'local'`, nil},
		}
		for _, step := range steps {
			if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
				return Directory{}, fmt.Errorf("store: %s: %w", step.what, err)
			}
		}
	}
	d, ok, err := directoryOn(ctx, tx, issuer)
	if err == nil && !ok {
		err = fmt.Errorf("%w: %s", ErrNoDirectory, issuer)
	}
	return d, err
}

// ConfirmDirectoryTx records, inside the caller's transaction, that the
// directory confirmed its generation, when generation is the one its row
// holds, and reports whether that changed anything: confirming twice
// changes nothing. ErrNoDirectory for one never followed, ErrDirectoryMoved
// for another generation.
func ConfirmDirectoryTx(ctx context.Context, tx *sql.Tx, issuer, generation string, now int64) (bool, error) {
	d, ok, err := directoryOn(ctx, tx, issuer)
	switch {
	case err != nil:
		return false, err
	case !ok:
		return false, fmt.Errorf("%w: %s", ErrNoDirectory, issuer)
	case d.Generation != generation:
		return false, fmt.Errorf("%w: it is at another generation", ErrDirectoryMoved)
	case d.Confirmed:
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE directories SET confirmed = 1, changed_at = ? WHERE issuer = ?`,
		now, issuer); err != nil {
		return false, fmt.Errorf("store: confirm the directory's generation: %w", err)
	}
	return true, nil
}

// SetDirectoryCursorTx writes, inside the caller's transaction, where a
// directory's feed was read up to, and, when publishedThroughUS is not zero,
// up to when it published every change, which only rises. It is a compare
// and set: it writes only when the row exists, holds expectGeneration and
// expectReset, and its cursor is not past cursor; ErrDirectoryMoved
// otherwise, so an answer read before a reset never moves the feed. It never
// creates the row.
func SetDirectoryCursorTx(ctx context.Context, tx *sql.Tx, issuer, expectGeneration string, expectReset, cursor, publishedThroughUS, now int64) error {
	if cursor < 0 || publishedThroughUS < 0 {
		return fmt.Errorf("%w: a cursor and a moment are never negative", ErrDirectoryMoved)
	}
	res, err := tx.ExecContext(ctx, `UPDATE directories
		SET cursor = ?, published_through_us = max(published_through_us, ?), changed_at = ?
		WHERE issuer = ? AND generation = ? AND reset = ? AND cursor <= ?`,
		cursor, publishedThroughUS, now, issuer, expectGeneration, expectReset, cursor)
	if err != nil {
		return fmt.Errorf("store: move the directory's cursor: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: move the directory's cursor: %w", err)
	}
	if n == 0 {
		return ErrDirectoryMoved
	}
	return nil
}

const directoryPersonColumns = `issuer, user_id, subject, generation, version, reset, status, disabled_at_us,
	sessions_revoked_at_us, credentials_revoked_at_us, applied_at, gone_at`

func scanDirectoryPerson(row interface{ Scan(...any) error }) (DirectoryPerson, error) {
	var p DirectoryPerson
	err := row.Scan(&p.Issuer, &p.UserID, &p.Subject, &p.Generation, &p.Version, &p.Reset, &p.Status, &p.DisabledAtUS,
		&p.SessionsRevokedAtUS, &p.CredentialsRevokedAtUS, &p.AppliedAt, &p.GoneAt)
	return p, err
}

// DirectoryPersonTx reads, inside the caller's transaction, what a directory
// last said of a person; false when it never said anything.
func DirectoryPersonTx(ctx context.Context, tx *sql.Tx, issuer, userID string) (DirectoryPerson, bool, error) {
	p, err := scanDirectoryPerson(tx.QueryRowContext(ctx, `SELECT `+directoryPersonColumns+`
		FROM directory_people WHERE issuer = ? AND user_id = ?`, issuer, userID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DirectoryPerson{}, false, nil
	case err != nil:
		return DirectoryPerson{}, false, fmt.Errorf("store: read the directory's person: %w", err)
	}
	return p, true, nil
}

// PutDirectoryPersonTx records, inside the caller's transaction, what a
// directory says of a person who exists: p's subject, generation, version,
// status and when it was applied, with each moment it revoked something kept
// as the higher of p's and the one recorded. A new row counts its version
// under its directory's current reset, whatever p.Reset says, and the
// directory must be followed already (EnsureDirectoryTx); a row already
// there keeps its subject (the schema refuses another) and its reset, which
// only ResetDirectoryTx moves. To record what a document revoked without its
// version, pass the version recorded (0 for a new row).
func PutDirectoryPersonTx(ctx context.Context, tx *sql.Tx, p DirectoryPerson) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO directory_people(issuer, user_id, subject, generation, version, reset,
		  status, disabled_at_us, sessions_revoked_at_us, credentials_revoked_at_us, applied_at)
		VALUES (?1, ?2, ?3, ?4, ?5, (SELECT d.reset FROM directories d WHERE d.issuer = ?1), ?6, ?7, ?8, ?9, ?10)
		ON CONFLICT(issuer, user_id) DO UPDATE SET
		  generation = excluded.generation, version = excluded.version, status = excluded.status,
		  disabled_at_us = max(disabled_at_us, excluded.disabled_at_us),
		  sessions_revoked_at_us = max(sessions_revoked_at_us, excluded.sessions_revoked_at_us),
		  credentials_revoked_at_us = max(credentials_revoked_at_us, excluded.credentials_revoked_at_us),
		  applied_at = excluded.applied_at`,
		p.Issuer, p.UserID, p.Subject, p.Generation, p.Version, p.Status, p.DisabledAtUS, p.SessionsRevokedAtUS,
		p.CredentialsRevokedAtUS, p.AppliedAt); err != nil {
		return fmt.Errorf("store: record what the directory says of the person: %w", err)
	}
	return nil
}

// MarkDirectoryPersonGoneTx records, inside the transaction that deletes a
// person, that they are gone from every directory that said anything of
// them: their rows stay, with their subject, until an extension releases
// them (ReleaseDirectoryPersonTx), and until then their name stays in the
// journal of what was done to mailboxes, which an extension still has to
// report. A person no directory follows has no row, and is forgotten there
// at their deletion.
func MarkDirectoryPersonGoneTx(ctx context.Context, tx *sql.Tx, userID string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE directory_people SET gone_at = ? WHERE user_id = ? AND gone_at = 0`,
		now, userID); err != nil {
		return fmt.Errorf("store: record that the person is gone from their directories: %w", err)
	}
	return nil
}

// ReleaseDirectoryPersonTx forgets, inside the caller's transaction, a person
// deleted here whom a directory followed: their row of that directory goes,
// and, once no directory keeps one, their name in the journal of what was
// done to mailboxes, as the actor and as the person a stranding lost, as
// their deletion does for a person nobody follows. ErrPersonStillHere while
// the person exists. Releasing someone with no row changes nothing.
func ReleaseDirectoryPersonTx(ctx context.Context, tx *sql.Tx, issuer, userID string) error {
	var here bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = ?)`, userID).Scan(&here); err != nil {
		return fmt.Errorf("store: release the directory's person: %w", err)
	}
	if here {
		return ErrPersonStillHere
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM directory_people WHERE issuer = ? AND user_id = ?`,
		issuer, userID); err != nil {
		return fmt.Errorf("store: release the directory's person: %w", err)
	}
	return ForgetPersonInJournalTx(ctx, tx, userID)
}

// ForgetPersonInJournalTx forgets, inside the caller's transaction, a person
// deleted here in the journal of what was done to mailboxes — that they
// acted, and that a stranding lost them — unless a directory still keeps
// their row, until it releases them (ReleaseDirectoryPersonTx).
func ForgetPersonInJournalTx(ctx context.Context, tx *sql.Tx, userID string) error {
	for _, step := range []struct{ what, query string }{
		{"forget who acted on mailboxes", `UPDATE mailbox_events SET actor = '' WHERE actor = ?1
		   AND NOT EXISTS (SELECT 1 FROM directory_people p WHERE p.user_id = ?1)`},
		{"forget whom a mailbox was stranded from", `UPDATE mailbox_events SET lost_user = '' WHERE lost_user = ?1
		   AND NOT EXISTS (SELECT 1 FROM directory_people p WHERE p.user_id = ?1)`},
	} {
		if _, err := tx.ExecContext(ctx, step.query, userID); err != nil {
			return fmt.Errorf("store: %s: %w", step.what, err)
		}
	}
	return nil
}

// DirectoryListed is a person of a directory as an extension lists them: one
// an identity of the directory's issuer signs in now (Held), with or without
// a document applied for them, or one deleted here whose row the directory
// keeps until it releases them (GoneAt).
type DirectoryListed struct {
	Subject string
	UserID  string
	// Held is a person an identity of the issuer links now.
	Held bool
	// GoneAt is when a person no longer here was deleted, unix seconds; 0
	// for one held.
	GoneAt int64
	// Generation, Version and Status are the document last applied for
	// them; empty, 0 and empty when none was.
	Generation string
	Version    int64
	Status     string
}

// DirectoryPeople lists, for an issuer, every person one of its identities
// signs in now, with what its directory last said of them if anything, and
// every person deleted here whose row it keeps, gone: held people first, each
// group by subject.
func (s *Store) DirectoryPeople(ctx context.Context, issuer string) ([]DirectoryListed, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT i.subject, i.user_id, 1, 0, coalesce(p.generation, ''),
		  coalesce(p.version, 0), coalesce(p.status, '')
		  FROM user_identities i LEFT JOIN directory_people p ON p.issuer = i.issuer AND p.user_id = i.user_id
		 WHERE i.issuer = ?1
		UNION ALL
		SELECT p.subject, p.user_id, 0, p.gone_at, p.generation, p.version, p.status
		  FROM directory_people p
		 WHERE p.issuer = ?1 AND p.gone_at <> 0
		 ORDER BY 3 DESC, 1, 2`, issuer)
	if err != nil {
		return nil, fmt.Errorf("store: list the directory's people: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []DirectoryListed
	for rows.Next() {
		var p DirectoryListed
		if err := rows.Scan(&p.Subject, &p.UserID, &p.Held, &p.GoneAt, &p.Generation, &p.Version, &p.Status); err != nil {
			return nil, fmt.Errorf("store: list the directory's people: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the directory's people: %w", err)
	}
	return out, nil
}

// DirectorySubjects maps each of userIDs to their subject at an issuer: the
// identity that signs them in now, or, for a person deleted here, the row
// their directory keeps until it releases them. A person neither names is
// left out.
func (s *Store) DirectorySubjects(ctx context.Context, issuer string, userIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(userIDs))
	if len(userIDs) == 0 {
		return out, nil
	}
	list, err := json.Marshal(userIDs)
	if err != nil {
		return nil, fmt.Errorf("store: encode ids: %w", err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT i.user_id, i.subject FROM user_identities i
		 WHERE i.issuer = ?1 AND i.user_id IN (SELECT value FROM json_each(?2))
		UNION ALL
		SELECT p.user_id, p.subject FROM directory_people p
		 WHERE p.issuer = ?1 AND p.user_id IN (SELECT value FROM json_each(?2))
		   AND NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.issuer = ?1 AND i.user_id = p.user_id)`,
		issuer, string(list))
	if err != nil {
		return nil, fmt.Errorf("store: map people to their subjects: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var userID, subject string
		if err := rows.Scan(&userID, &subject); err != nil {
			return nil, fmt.Errorf("store: map people to their subjects: %w", err)
		}
		out[userID] = subject
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: map people to their subjects: %w", err)
	}
	return out, nil
}

// SweepDirectoryPeopleTx releases, inside the caller's transaction, every
// person deleted here whose directory row was marked gone at or before
// cutoff (unix seconds), as ReleaseDirectoryPersonTx does, and reports how
// many rows it released: an extension that never released them is not kept
// waiting on longer than the journal that names them.
func SweepDirectoryPeopleTx(ctx context.Context, tx *sql.Tx, cutoff int64) (int, error) {
	all, err := goneBeforeTx(ctx, tx, cutoff)
	if err != nil {
		return 0, err
	}
	for _, g := range all {
		if err := ReleaseDirectoryPersonTx(ctx, tx, g.issuer, g.userID); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

type gonePerson struct{ issuer, userID string }

// goneBeforeTx lists the rows of people gone at or before cutoff, every row
// read and the statement finished before anything is released.
func goneBeforeTx(ctx context.Context, tx *sql.Tx, cutoff int64) ([]gonePerson, error) {
	rows, err := tx.QueryContext(ctx, `SELECT issuer, user_id FROM directory_people
		WHERE gone_at <> 0 AND gone_at <= ? ORDER BY gone_at, issuer, user_id`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: list the people gone: %w", err)
	}
	//nolint:errcheck // read to the end below; a close failure changes nothing
	defer func() { _ = rows.Close() }()
	var all []gonePerson
	for rows.Next() {
		var g gonePerson
		if err := rows.Scan(&g.issuer, &g.userID); err != nil {
			return nil, fmt.Errorf("store: list the people gone: %w", err)
		}
		all = append(all, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the people gone: %w", err)
	}
	return all, nil
}
