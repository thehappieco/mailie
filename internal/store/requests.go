package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Requests an extension carries for a person (migration 0029; docs/workspaces.md,
// "Acts an extension runs for a person").
//
// A person may ask for a change elsewhere, where an extension of the daemon
// takes the request and carries it here, with no session: the service runs it
// under this server's own rules for that person (service.ActFor). Each such
// request runs once. The write that decides it — the act's own transaction,
// a request whose effect held already included — first reads its record
// (BeginRequestTx: a request that ran is ErrRequestDone, carrying what it did,
// and the write does nothing else), then checks that the person may still act
// and that the mailbox it names is in the workspace it names, then makes the
// act's own checks and change, and last records what it did
// (RecordRequestTx), all in one transaction. A refusal rolls the record back
// with the rest: the extension keeps its own note of a refusal. The record
// holds the person's id, the act's name and counts; never a name, an address
// or what was asked.

// Request is a request an extension carries for a person, as the write that
// decides it is told of it.
type Request struct {
	// Extension is the extension's name, 1 to 64 bytes; ID is the request's
	// id there, its idempotency key: 1 to 64 of [A-Za-z0-9_.-].
	Extension, ID string
	// UserID is the person who asked.
	UserID string
	// Act names the act, from the service's closed list.
	Act string
	// WorkspaceID is the workspace an act on a mailbox is for: the mailbox
	// must be in it. Empty for a person's own act.
	WorkspaceID string
	// FirstIssued is the request's intent's first moment: a narrowing is
	// refused when what it would take away was given at or after its second
	// (ErrSetSince), and a widening is stamped with it (Stamp).
	FirstIssued time.Time
	// KeepUntil is when its record goes.
	KeepUntil time.Time
	// OwnNarrowing is an act that only narrows the person's own settings:
	// a person suspended by an extension may still make it; one disabled here
	// may not.
	OwnNarrowing bool
}

// RequestOutcome is what a request did, as its record keeps it.
type RequestOutcome struct {
	// Changed is false when what it asked held already: nothing was written
	// but its record.
	Changed bool `json:"changed"`
	// Purged is a removal that deleted a mailbox that never connected, for
	// good.
	Purged bool `json:"purged,omitempty"`
	// SessionsEnded counts the live sessions a sign-out everywhere ended.
	SessionsEnded int `json:"sessions_ended,omitempty"`
	// Dismissed counts the notices a dismissal deleted.
	Dismissed int `json:"dismissed,omitempty"`
}

// Errors of a request's record.
var (
	// ErrRequestDone is a request that ran already: its record says what it
	// did (*RequestDone), and nothing runs again.
	ErrRequestDone = errors.New("store: the request ran already")
	// ErrActorInactive is a person who may no longer act: deleted, disabled,
	// or suspended for anything but their own narrowings.
	ErrActorInactive = errors.New("store: the person may no longer act")
	// ErrNotInWorkspace is a mailbox outside the workspace the request names.
	ErrNotInWorkspace = errors.New("store: the mailbox is not in the request's workspace")
	// ErrSetSince is a narrowing whose target was given again at or after
	// the second the request was first asked: it takes nothing away.
	ErrSetSince = errors.New("store: what the request would take away was given again since it was asked")
	// ErrBadRequest is a request whose extension, id, person, act or record's
	// end is out of shape.
	ErrBadRequest = errors.New("store: a request names its extension, its id, its person, its act and its end")
)

// RequestDone is ErrRequestDone with what the request did the first time.
type RequestDone struct {
	Outcome RequestOutcome
}

func (e *RequestDone) Error() string { return ErrRequestDone.Error() }

// Is makes errors.Is(err, ErrRequestDone) find it.
func (e *RequestDone) Is(target error) bool { return target == ErrRequestDone }

// MaxRequestKept bounds how long after it runs a request's record is kept.
const MaxRequestKept = 31 * 24 * time.Hour

// requestIDByte reports whether c may be in a request's id.
func requestIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// Check refuses a request out of shape (ErrBadRequest): its extension 1 to
// 64 bytes, its id 1 to 64 of [A-Za-z0-9_.-], a person, an act of 1 to 64
// bytes, and its record's end after now and at most MaxRequestKept ahead.
func (r *Request) Check(now time.Time) error {
	switch {
	case len(r.Extension) == 0 || len(r.Extension) > 64,
		len(r.ID) == 0 || len(r.ID) > 64,
		r.UserID == "",
		len(r.Act) == 0 || len(r.Act) > 64,
		!r.KeepUntil.After(now),
		r.KeepUntil.After(now.Add(MaxRequestKept)):
		return ErrBadRequest
	}
	for i := 0; i < len(r.ID); i++ {
		if !requestIDByte(r.ID[i]) {
			return ErrBadRequest
		}
	}
	return nil
}

// Since is the second a narrowing compares with: the request's first moment,
// floored to unix seconds; 0, which nothing refuses, for no request or none
// given.
func (r *Request) Since() int64 {
	if r == nil || r.FirstIssued.IsZero() {
		return 0
	}
	return r.FirstIssued.Unix()
}

// Stamp is when a widening a request makes is recorded, in unix seconds:
// its intent's first moment, at most now, so that the order in which people
// asked decides, not the order in which requests ran; now for a widening
// that comes with no request. A write never moves a moment that stands back
// to it (a grant's raised_at, a team's agreement): it keeps the later one,
// which a narrowing asked between the two must still meet.
func (r *Request) Stamp(now int64) int64 {
	if s := r.Since(); s > 0 && s < now {
		return s
	}
	return now
}

// Unbounded is the request with no first moment to compare with: a
// narrowing that takes away what no widening gives again (a removed
// mailbox's kept index, which nothing indexes again while it is removed)
// always runs. Nil for nil.
func (r *Request) Unbounded() *Request {
	if r == nil {
		return nil
	}
	u := *r
	u.FirstIssued = time.Time{}
	return &u
}

// widenedSince reports whether a value a narrowing would take away was given
// at, a time in unix seconds, at or after the request's first second.
func (r *Request) widenedSince(at int64) bool {
	s := r.Since()
	return s > 0 && at >= s
}

// BeginRequestTx is the start of a request's decisive write, inside its
// transaction: a request that ran already is *RequestDone (and the write
// must do nothing else); a person who may no longer act is
// ErrActorInactive — active, and for their own narrowings active or
// suspended by an extension (not disabled here); and a mailbox, when
// accountID names one, outside the request's workspace is
// ErrNotInWorkspace (one gone is the act's to say). A nil request checks
// nothing.
func BeginRequestTx(ctx context.Context, tx *sql.Tx, req *Request, accountID string) error {
	if req == nil {
		return nil
	}
	var outcome string
	err := tx.QueryRowContext(ctx, `SELECT outcome FROM extension_requests WHERE extension = ? AND id = ?`,
		req.Extension, req.ID).Scan(&outcome)
	switch {
	case err == nil:
		var out RequestOutcome
		if err := json.Unmarshal([]byte(outcome), &out); err != nil {
			return fmt.Errorf("store: read the request's record: %w", err)
		}
		return &RequestDone{Outcome: out}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: read the request's record: %w", err)
	}
	if err := RequirePersonActsTx(ctx, tx, req.UserID, req.OwnNarrowing); err != nil {
		return err
	}
	if accountID == "" || req.WorkspaceID == "" {
		return nil
	}
	var workspaceID string
	err = tx.QueryRowContext(ctx, `SELECT workspace_id FROM accounts WHERE id = ?`, accountID).Scan(&workspaceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("store: read the mailbox's workspace: %w", err)
	case workspaceID != req.WorkspaceID:
		return ErrNotInWorkspace
	}
	return nil
}

// RequirePersonActsTx refuses, inside the caller's transaction, a person
// who may no longer act (ErrActorInactive): one deleted or disabled, and one
// suspended by an extension unless ownNarrowing, an act that only narrows
// their own settings.
func RequirePersonActsTx(ctx context.Context, tx *sql.Tx, userID string, ownNarrowing bool) error {
	var (
		status    string
		suspended int64
	)
	err := tx.QueryRowContext(ctx, `SELECT status, suspended_at FROM users WHERE id = ?`, userID).Scan(&status, &suspended)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrActorInactive
	case err != nil:
		return fmt.Errorf("store: read the person's status: %w", err)
	case status == "active" && suspended == 0:
		return nil
	case ownNarrowing && status == "disabled" && suspended > 0:
		return nil
	}
	return ErrActorInactive
}

// RecordRequestTx records, last in a request's decisive write, what it did.
// A nil request records nothing.
func RecordRequestTx(ctx context.Context, tx *sql.Tx, req *Request, out RequestOutcome, now time.Time) error {
	if req == nil {
		return nil
	}
	outcome, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("store: encode the request's outcome: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO extension_requests(extension, id, user_id, act, outcome, done_at, keep_until)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, req.Extension, req.ID, req.UserID, req.Act, string(outcome),
		max(now.UnixMicro(), 1), req.KeepUntil.Unix()); err != nil {
		return fmt.Errorf("store: record the request: %w", err)
	}
	return nil
}

// WriteRequest is Write for a request's decisive write: with req, fn runs
// after BeginRequestTx (for the mailbox accountID names, if any), and once fn
// succeeded what outcome then says is recorded (RecordRequestTx), in the same
// transaction. Without req it is Write.
func (s *Store) WriteRequest(ctx context.Context, req *Request, accountID string, fn func(*sql.Tx) error,
	outcome func() RequestOutcome,
) error {
	if req == nil {
		return s.Write(ctx, fn)
	}
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := BeginRequestTx(ctx, tx, req, accountID); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return RecordRequestTx(ctx, tx, req, outcome(), s.now())
	})
}

// RecordNoOp ends a request whose effect held already when its act looked,
// before any write: in one write its record is begun (BeginRequestTx), holds
// is read again, and while it still holds the request is recorded as having
// changed nothing. It reports whether it did; when holds no longer does,
// nothing is written, and the act goes on as usual, once.
func (s *Store) RecordNoOp(ctx context.Context, req *Request, accountID string, holds func(*sql.Tx) (bool, error)) (bool, error) {
	if req == nil {
		return false, nil
	}
	done := false
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if err := BeginRequestTx(ctx, tx, req, accountID); err != nil {
			return err
		}
		ok, err := holds(tx)
		if err != nil || !ok {
			return err
		}
		done = true
		return RecordRequestTx(ctx, tx, req, RequestOutcome{}, s.now())
	})
	if err != nil {
		return false, err
	}
	return done, nil
}

// RequestOutcomeOf reads a request's record: what it did, and whether it ran.
func (s *Store) RequestOutcomeOf(ctx context.Context, extension, id string) (RequestOutcome, bool, error) {
	var outcome string
	err := s.r.QueryRowContext(ctx, `SELECT outcome FROM extension_requests WHERE extension = ? AND id = ?`,
		extension, id).Scan(&outcome)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RequestOutcome{}, false, nil
	case err != nil:
		return RequestOutcome{}, false, fmt.Errorf("store: read the request's record: %w", err)
	}
	var out RequestOutcome
	if err := json.Unmarshal([]byte(outcome), &out); err != nil {
		return RequestOutcome{}, false, fmt.Errorf("store: read the request's record: %w", err)
	}
	return out, true, nil
}

// SweepRequests deletes the records of the requests whose keep_until passed
// at now, and returns how many.
func (s *Store) SweepRequests(ctx context.Context, now time.Time) (int, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM extension_requests WHERE keep_until <= ?`, now.Unix())
		if err != nil {
			return fmt.Errorf("store: sweep the requests' records: %w", err)
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}
