package events

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Journal is the durable half of the bus: the events table.
type Journal struct {
	store Store
}

// Store is the slice of the database the journal needs. An interface so the
// bus can be tested without one, and so the dependency runs one way.
type Store interface {
	Reader() *sql.DB
	Write(ctx context.Context, fn func(*sql.Tx) error) error
}

// NewJournal builds the journal.
func NewJournal(s Store) *Journal { return &Journal{store: s} }

// Append writes events inside a transaction the caller already owns and fills
// in the sequence numbers the journal assigned.
//
// This is the only way events are created. Taking the transaction as an
// argument is the point: the caller is in the middle of writing the messages
// these events describe, and both must land together or not at all.
func (j *Journal) Append(ctx context.Context, tx *sql.Tx, evs []Event) ([]Event, error) {
	out := make([]Event, 0, len(evs))
	for _, ev := range evs {
		if !ev.Type.Valid() {
			return nil, fmt.Errorf("events: refusing to journal unknown type %q", ev.Type)
		}
		at := ev.At
		if at.IsZero() {
			at = time.Now()
		}
		var seq int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO events(type, account_id, payload_json, created_at) VALUES (?, ?, ?, ?) RETURNING seq`,
			string(ev.Type), ev.AccountID, string(ev.Payload), at.Unix(),
		).Scan(&seq)
		if err != nil {
			return nil, fmt.Errorf("events: append %s: %w", ev.Type, err)
		}
		ev.Seq = seq
		ev.At = at
		out = append(out, ev)
	}
	return out, nil
}

// Since reads up to limit events after seq, oldest first, keeping those the
// filter accepts.
//
// It returns the events it kept and the highest sequence number it examined.
// Those are different numbers whenever the filter rejects something, and the
// caller needs the second one: advancing a cursor only to the last *delivered*
// event would make the next read start inside a run of filtered-out rows and
// return them again forever.
//
// The filter runs only after the rows are read and the cursor closed, so an
// Allow that looks something up holds no connection while it waits for
// another: with a pool of a few readers, several of those at once would
// otherwise take every connection and wait on each other for good.
func (j *Journal) Since(ctx context.Context, seq int64, filter Filter, limit int) (kept []Event, scanned int64, err error) {
	kept, scanned, _, err = j.since(ctx, seq, filter, limit)
	return kept, scanned, err
}

// since is Since, also reporting how many rows it read: fewer than limit
// means it reached the end of the journal.
func (j *Journal) since(ctx context.Context, seq int64, filter Filter, limit int) (kept []Event, scanned int64, n int, err error) {
	if limit <= 0 {
		limit = DefaultReplayLimit
	}
	batch, err := j.read(ctx, seq, limit)
	if err != nil {
		return nil, seq, 0, err
	}
	scanned = seq
	for _, ev := range batch {
		scanned = ev.Seq
		if filter.Match(ev) {
			kept = append(kept, ev)
		}
	}
	return kept, scanned, len(batch), nil
}

// read loads up to limit rows after seq and closes the cursor before
// returning.
func (j *Journal) read(ctx context.Context, seq int64, limit int) ([]Event, error) {
	rows, err := j.store.Reader().QueryContext(ctx,
		`SELECT seq, type, account_id, payload_json, created_at
		   FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, seq, limit)
	if err != nil {
		return nil, fmt.Errorf("events: replay: %w", err)
	}
	//nolint:errcheck // read-only query; closed explicitly below on the normal path
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var (
			ev      Event
			typeStr string
			payload string
			created int64
		)
		if err := rows.Scan(&ev.Seq, &typeStr, &ev.AccountID, &payload, &created); err != nil {
			return nil, fmt.Errorf("events: scan: %w", err)
		}
		ev.Type = Type(typeStr)
		ev.Payload = []byte(payload)
		ev.At = time.Unix(created, 0).UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: replay: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("events: replay: %w", err)
	}
	return out, nil
}

// Latest is the highest sequence number in the journal, or 0 when empty. A
// client that wants only future events starts here.
func (j *Journal) Latest(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := j.store.Reader().QueryRowContext(ctx, `SELECT max(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("events: latest: %w", err)
	}
	return seq.Int64, nil
}

// Earliest is the lowest sequence number still retained, or 0 when empty.
// A cursor below this cannot be honoured: the events it refers to are gone, and
// the caller has to be told to re-read rather than handed a silent gap.
func (j *Journal) Earliest(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := j.store.Reader().QueryRowContext(ctx, `SELECT min(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("events: earliest: %w", err)
	}
	return seq.Int64, nil
}

// Retention bounds the journal.
type Retention struct {
	// MaxAge drops events older than this.
	MaxAge time.Duration
	// MinRows keeps at least this many of the newest events regardless of age,
	// so a quiet week does not leave a client with no cursor to resume from.
	MinRows int
}

// DefaultRetention keeps a week, and always the last ten thousand events.
var DefaultRetention = Retention{MaxAge: 7 * 24 * time.Hour, MinRows: 10_000}

// DefaultReplayLimit bounds one replay batch.
const DefaultReplayLimit = 500

// Prune applies the retention policy.
//
// An event with a webhook delivery still pending is never dropped, whatever its
// age: retention exists to bound disk, and silently discarding the payload a
// retry is about to send would turn a slow sink into lost notifications.
func (j *Journal) Prune(ctx context.Context, now time.Time, r Retention) (int64, error) {
	if r.MaxAge <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-r.MaxAge).Unix()
	minRows := r.MinRows
	if minRows < 0 {
		minRows = 0
	}

	var removed int64
	err := j.store.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM events
			  WHERE created_at < ?
			    AND seq <= COALESCE((SELECT seq FROM events ORDER BY seq DESC LIMIT 1 OFFSET ?), -1)
			    AND seq NOT IN (SELECT event_seq FROM webhook_deliveries WHERE status = 'pending')`,
			cutoff, minRows)
		if err != nil {
			return fmt.Errorf("events: prune: %w", err)
		}
		removed, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("events: prune: %w", err)
		}
		return nil
	})
	return removed, err
}
