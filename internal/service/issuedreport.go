package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/store"
)

// What this server reports of the keys an issuer makes, its own stops of
// them, how long it keeps what it holds of them, and the operator's hold
// (docs/workspaces.md, "Keys an issuer makes").

// IssuedStopCause is why this server stops an issued key on its own, in the
// core's words; an extension maps them to its issuer's.
type IssuedStopCause string

// Why this server stops an issued key on its own.
const (
	// IssuedStopOperator: the operator held it (HoldIssuedKey), or a record
	// of it could not be applied even narrowed.
	IssuedStopOperator IssuedStopCause = "operator"
	// IssuedStopCreator: its creator is stopped here: disabled on this
	// server, or suspended outside the directory's documents.
	IssuedStopCreator IssuedStopCause = "creator"
	// IssuedStopWithdrawn: this edition refuses every issued key
	// (Deps.IssuedKeysRefused); computed, never stored.
	IssuedStopWithdrawn IssuedStopCause = "withdrawn"
)

// IssuedStopCauses lists every IssuedStopCause.
func IssuedStopCauses() []IssuedStopCause {
	return []IssuedStopCause{IssuedStopOperator, IssuedStopCreator, IssuedStopWithdrawn}
}

// MaxIssuedReportEntries bounds each list of an IssuedKeysReport.
const MaxIssuedReportEntries = 1000

// IssuedKeysReport is what this server owes its issuer of the keys it made,
// and where their feed stands: what an extension acknowledges with.
type IssuedKeysReport struct {
	// Generation and Reset are the directory's; Through is where the key
	// feed was applied up to, AppliedAt when (now, before any page of this
	// reset applied); PassOpen is a reset's pass not ended yet.
	Generation string
	Reset      int64
	Through    int64
	AppliedAt  time.Time
	PassOpen   bool
	// Refused are the refusals owed, the oldest first; RefusedMore says more
	// wait behind them.
	Refused     []IssuedRefusalOwed
	RefusedMore bool
	// Waiting are the grants held back until their giver's document, the
	// oldest given first, of keys listed since the last reset; WaitingMore
	// says more wait.
	Waiting     []IssuedWaiting
	WaitingMore bool
	// Suspended are this server's own stops of live keys, one per key and
	// cause; SuspendedMore says more.
	Suspended     []IssuedStop
	SuspendedMore bool
	// Held are the keys held narrower than a reset's pass listed them, one
	// per key; HeldMore says more.
	Held     []IssuedHeld
	HeldMore bool
}

// IssuedRefusalOwed is one refusal owed to the issuer: of a grant, at the
// version of the key's record that listed it. ID is what IssuedKeysTold
// takes once the issuer accepted it.
type IssuedRefusalOwed struct {
	ID         int64
	ExternalID string
	Version    int64
	AccountID  string
	Use        string
	Cause      IssuedRefusal
}

// IssuedWaiting is one grant held back until its giver's document applies.
type IssuedWaiting struct {
	ExternalID, AccountID, Use string
	// GivenBy is the giver's subject at the issuer.
	GivenBy string
	GivenAt time.Time
}

// IssuedStop is one of this server's own stops of a live key.
type IssuedStop struct {
	ExternalID string
	Cause      IssuedStopCause
}

// IssuedHeld is a key this server holds narrower than a reset's pass listed
// it: revoked, an earlier expiry (zero for none), the grants it keeps off.
type IssuedHeld struct {
	ID         int64
	ExternalID string
	Revoked    bool
	ExpiresAt  time.Time
	Removed    []IssuedGrantRef
}

// IssuedGrantRef names one use of one mailbox.
type IssuedGrantRef struct {
	AccountID, Use string
}

// IssuedKeysReport reads, in one read, what this server owes its issuer of
// the keys it made, for an extension that acknowledges the issuer's feed
// with it. No principal; conflict (ErrNotKeyIssuer) for an issuer this
// server mirrors no keys of, and (ErrDirectoryGeneration) for a directory
// not followed.
func (s *Service) IssuedKeysReport(ctx context.Context, issuer string) (IssuedKeysReport, error) {
	if err := s.keyIssuerIs(issuer); err != nil {
		return IssuedKeysReport{}, err
	}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return IssuedKeysReport{}, E(CodeInternal, "reading what is owed to the issuer failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	out, err := s.issuedKeysReportTx(ctx, tx, issuer)
	if err != nil {
		return IssuedKeysReport{}, fromDirectory(err, "reading what is owed to the issuer failed")
	}
	return out, nil
}

// keyIssuerIs refuses an issuer this server mirrors no keys of.
func (s *Service) keyIssuerIs(issuer string) error {
	if err := s.following(); err != nil {
		return err
	}
	if s.issuer == nil || issuer != s.issuer.Issuer() {
		return errNotKeyIssuer
	}
	return nil
}

func (s *Service) issuedKeysReportTx(ctx context.Context, tx *sql.Tx, issuer string) (IssuedKeysReport, error) {
	dir, ok, err := store.DirectoryTx(ctx, tx, issuer)
	switch {
	case err != nil:
		return IssuedKeysReport{}, err
	case !ok:
		return IssuedKeysReport{}, errDirectoryGeneration
	}
	now := s.now()
	out := IssuedKeysReport{
		Generation: dir.Generation, Reset: dir.Reset, AppliedAt: now.UTC(),
		Refused: []IssuedRefusalOwed{}, Waiting: []IssuedWaiting{}, Suspended: []IssuedStop{}, Held: []IssuedHeld{},
	}
	feed, ok, err := store.DirectoryKeysTx(ctx, tx, issuer)
	if err != nil {
		return IssuedKeysReport{}, err
	}
	if ok && feed.Reset == dir.Reset {
		out.Through, out.PassOpen = feed.Cursor, feed.PassOpen
		if feed.AppliedAtUS > 0 {
			out.AppliedAt = time.UnixMicro(feed.AppliedAtUS).UTC()
		}
	}
	const limit = MaxIssuedReportEntries
	if err := scanAll(ctx, tx, `SELECT id, external_id, version, account_id, use, cause FROM issued_key_tell
		WHERE issuer = ? AND kind = 'refusal' ORDER BY id LIMIT ?`, []any{issuer, limit + 1},
		func(rows *sql.Rows) error {
			var r IssuedRefusalOwed
			var cause string
			if err := rows.Scan(&r.ID, &r.ExternalID, &r.Version, &r.AccountID, &r.Use, &cause); err != nil {
				return err
			}
			r.Cause = IssuedRefusal(cause)
			out.Refused = append(out.Refused, r)
			return nil
		}); err != nil {
		return IssuedKeysReport{}, err
	}
	out.Refused, out.RefusedMore = cut(out.Refused, limit)
	if err := scanAll(ctx, tx, `SELECT i.external_id, g.account_id, g.use, g.given_by, g.given_at_us
		  FROM issued_key_grants g JOIN issued_keys i ON i.prefix = g.prefix JOIN api_keys k ON k.prefix = g.prefix
		 WHERE i.issuer = ? AND g.state = 'waiting' AND i.version > 0 AND i.ended_at = 0 AND k.revoked_at = 0
		   AND k.expires_at > ?
		 ORDER BY g.given_at_us, g.prefix, g.account_id, g.use LIMIT ?`, []any{issuer, now.Unix(), limit + 1},
		func(rows *sql.Rows) error {
			var w IssuedWaiting
			var at int64
			if err := rows.Scan(&w.ExternalID, &w.AccountID, &w.Use, &w.GivenBy, &at); err != nil {
				return err
			}
			w.GivenAt = time.UnixMicro(at).UTC()
			out.Waiting = append(out.Waiting, w)
			return nil
		}); err != nil {
		return IssuedKeysReport{}, err
	}
	out.Waiting, out.WaitingMore = cut(out.Waiting, limit)
	stops := `SELECT i.external_id, s.cause FROM issued_key_stops s JOIN issued_keys i ON i.prefix = s.prefix
		  JOIN api_keys k ON k.prefix = s.prefix
		 WHERE i.issuer = ?1 AND i.ended_at = 0 AND k.revoked_at = 0 AND k.expires_at > ?2`
	if s.issuedKeysRefused {
		// Every live key is stopped by the edition, beside the stops kept.
		stops += ` UNION SELECT i.external_id, 'withdrawn' FROM issued_keys i JOIN api_keys k ON k.prefix = i.prefix
		 WHERE i.issuer = ?1 AND i.ended_at = 0 AND k.revoked_at = 0 AND k.expires_at > ?2`
	}
	if err := scanAll(ctx, tx, `SELECT external_id, cause FROM (`+stops+`) ORDER BY external_id, cause LIMIT ?3`,
		[]any{issuer, now.Unix(), limit + 1}, func(rows *sql.Rows) error {
			var st IssuedStop
			var cause string
			if err := rows.Scan(&st.ExternalID, &cause); err != nil {
				return err
			}
			st.Cause = IssuedStopCause(cause)
			out.Suspended = append(out.Suspended, st)
			return nil
		}); err != nil {
		return IssuedKeysReport{}, err
	}
	out.Suspended, out.SuspendedMore = cut(out.Suspended, limit)
	if err := scanAll(ctx, tx, `SELECT id, external_id, revoked, expires_at, removed FROM issued_key_tell
		WHERE issuer = ? AND kind = 'held' ORDER BY id LIMIT ?`, []any{issuer, limit + 1},
		func(rows *sql.Rows) error {
			var (
				h       IssuedHeld
				expires int64
				raw     string
			)
			if err := rows.Scan(&h.ID, &h.ExternalID, &h.Revoked, &expires, &raw); err != nil {
				return err
			}
			if expires > 0 {
				h.ExpiresAt = time.Unix(expires, 0).UTC()
			}
			var removed []heldRemoved
			if err := json.Unmarshal([]byte(raw), &removed); err != nil {
				return fmt.Errorf("a held key's removed grants: %w", err)
			}
			h.Removed = make([]IssuedGrantRef, 0, len(removed))
			for _, r := range removed {
				h.Removed = append(h.Removed, IssuedGrantRef(r))
			}
			out.Held = append(out.Held, h)
			return nil
		}); err != nil {
		return IssuedKeysReport{}, err
	}
	out.Held, out.HeldMore = cut(out.Held, limit)
	return out, nil
}

// cut keeps at most limit of list, saying whether there were more.
func cut[T any](list []T, limit int) ([]T, bool) {
	if len(list) > limit {
		return list[:limit], true
	}
	return list, false
}

// queryer is a transaction or a pool.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanAll runs a query and hands each row to fn.
func scanAll(ctx context.Context, tx queryer, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("service: read the issued keys: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return fmt.Errorf("service: read the issued keys: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("service: read the issued keys: %w", err)
	}
	return nil
}

// IssuedKeysTold forgets what the issuer accepted: the refusals and the keys
// held narrower an IssuedKeysReport named, by their ID. An ID it no longer
// holds is no error.
func (s *Service) IssuedKeysTold(ctx context.Context, issuer string, ids []int64) error {
	if err := s.keyIssuerIs(issuer); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(ids); start += 500 {
			part := ids[start:min(start+500, len(ids))]
			args := make([]any, 0, len(part)+1)
			args = append(args, issuer)
			for _, id := range part {
				args = append(args, id)
			}
			//nolint:gosec // G202: only placeholders are added; every value is bound
			if _, err := tx.ExecContext(ctx, `DELETE FROM issued_key_tell WHERE issuer = ? AND id IN (`+
				strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`)`, args...); err != nil {
				return fmt.Errorf("service: forget what the issuer accepted: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return E(CodeInternal, "forgetting what the issuer accepted failed", err)
	}
	return nil
}

// IssuedKeyUsed is a day an issued key was accepted.
type IssuedKeyUsed struct {
	ExternalID string
	// Day is the UTC day, YYYY-MM-DD.
	Day string
}

// IssuedKeyRefusals is how many uses of an issued key were refused, on one
// day, for one reason, on one mailbox and use (none for a refusal of the key
// itself: revoked, expired, held).
type IssuedKeyRefusals struct {
	ExternalID string
	Day        string
	AccountID  string
	Use        string
	Refused    UseRefusal
	Count      int64
}

// IssuedKeyUses is what an extension heard of its keys' uses (KeyIssuer.Used)
// since it last recorded them: to RecordIssuedKeyUses, the days accepted and
// how many more refusals; from IssuedKeyUsage, each key's last day accepted
// and the refusals of every day kept, in all.
type IssuedKeyUses struct {
	Used    []IssuedKeyUsed
	Refused []IssuedKeyRefusals
}

// RecordIssuedKeyUses adds what an extension counted of its keys' uses to
// what this server keeps of them, for keys it still holds: each key's last
// day accepted (the latest only, never a time) and its refusals per day,
// added to the counts kept. A key it no longer holds is skipped.
func (s *Service) RecordIssuedKeyUses(ctx context.Context, issuer string, uses IssuedKeyUses) error {
	if err := s.keyIssuerIs(issuer); err != nil {
		return err
	}
	if len(uses.Used)+len(uses.Refused) == 0 {
		return nil
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		for _, u := range uses.Used {
			if !validDay(u.Day) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE issued_keys SET used_day = ?
				WHERE issuer = ? AND external_id = ? AND used_day < ?`, u.Day, issuer, u.ExternalID, u.Day); err != nil {
				return fmt.Errorf("service: record an issued key's last use: %w", err)
			}
		}
		for _, r := range uses.Refused {
			if !validDay(r.Day) || r.Count < 1 {
				continue
			}
			account, use := r.AccountID, r.Use
			if r.Refused == UseRevoked || r.Refused == UseExpired || r.Refused == UseHeld {
				account, use = "", ""
			} else if account == "" || !knownUse(use) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO issued_key_uses(prefix, day, account_id, use, refused, count)
				SELECT prefix, ?, ?, ?, ?, ? FROM issued_keys WHERE issuer = ? AND external_id = ?
				ON CONFLICT(prefix, day, account_id, use, refused) DO UPDATE SET count = count + excluded.count,
				  reported = 0`, r.Day, account, use, string(r.Refused), r.Count, issuer, r.ExternalID); err != nil {
				return fmt.Errorf("service: count an issued key's refused uses: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return E(CodeInternal, "recording the issued keys' uses failed", err)
	}
	return nil
}

// validDay reports whether s is a day as YYYY-MM-DD.
func validDay(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// IssuedKeyUsage reads, for an extension's report of its keys' uses, each
// key's last day accepted on or after since, and every refusal counted on a
// day on or after it, in all (the counts are absolute: a report sent twice
// says the same).
func (s *Service) IssuedKeyUsage(ctx context.Context, issuer string, since time.Time) (IssuedKeyUses, error) {
	if err := s.keyIssuerIs(issuer); err != nil {
		return IssuedKeyUses{}, err
	}
	day := since.UTC().Format(time.DateOnly)
	out := IssuedKeyUses{Used: []IssuedKeyUsed{}, Refused: []IssuedKeyRefusals{}}
	tx, err := s.store.ReadTx(ctx)
	if err != nil {
		return IssuedKeyUses{}, E(CodeInternal, "reading the issued keys' uses failed", err)
	}
	//nolint:errcheck // a read-only transaction: rolling back loses nothing
	defer func() { _ = tx.Rollback() }()
	if err := scanAll(ctx, tx, `SELECT external_id, used_day FROM issued_keys
		WHERE issuer = ? AND used_day <> '' AND used_day >= ? ORDER BY external_id`, []any{issuer, day},
		func(rows *sql.Rows) error {
			var u IssuedKeyUsed
			if err := rows.Scan(&u.ExternalID, &u.Day); err != nil {
				return err
			}
			out.Used = append(out.Used, u)
			return nil
		}); err != nil {
		return IssuedKeyUses{}, E(CodeInternal, "reading the issued keys' uses failed", err)
	}
	if err := scanAll(ctx, tx, `SELECT i.external_id, u.day, u.account_id, u.use, u.refused, u.count
		  FROM issued_key_uses u JOIN issued_keys i ON i.prefix = u.prefix
		 WHERE i.issuer = ? AND u.day >= ? ORDER BY u.day, i.external_id, u.account_id, u.use, u.refused`,
		[]any{issuer, day}, func(rows *sql.Rows) error {
			var r IssuedKeyRefusals
			var refused string
			if err := rows.Scan(&r.ExternalID, &r.Day, &r.AccountID, &r.Use, &refused, &r.Count); err != nil {
				return err
			}
			r.Refused = UseRefusal(refused)
			out.Refused = append(out.Refused, r)
			return nil
		}); err != nil {
		return IssuedKeyUses{}, E(CodeInternal, "reading the issued keys' uses failed", err)
	}
	return out, nil
}

// IssuedKeyUsesReported records that the issuer accepted a report carrying
// every refusal counted up to through (a UTC day, YYYY-MM-DD): a day older
// than yesterday may then go before its 30 days (SweepIssuedKeys). A count
// that rose since is reported again.
func (s *Service) IssuedKeyUsesReported(ctx context.Context, issuer, through string) error {
	if err := s.keyIssuerIs(issuer); err != nil {
		return err
	}
	if !validDay(through) {
		return E(CodeBadRequest, "a day is YYYY-MM-DD", nil)
	}
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE issued_key_uses SET reported = 1
			WHERE reported = 0 AND day <= ? AND prefix IN (SELECT prefix FROM issued_keys WHERE issuer = ?)`,
			through, issuer)
		return err
	})
	if err != nil {
		return E(CodeInternal, "recording the issued keys' report failed", err)
	}
	return nil
}

// EndExpiredIssuedKeys records the end of every issued key whose expiry
// passed by now (its end is its expiry), dropping its waiting grants and
// what is owed of it, which marks its workspace for an extension's report;
// and answers the next expiry of a live issued key, zero when none. The
// hourly housekeeping runs it, and so may an extension at that next moment,
// so that an expired key leaves what is reported within seconds.
func (s *Service) EndExpiredIssuedKeys(ctx context.Context, now time.Time) (time.Time, error) {
	if s.store == nil {
		return time.Time{}, nil
	}
	var next int64
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		type expired struct {
			prefix string
			at     int64
		}
		var ended []expired
		if err := scanAll(ctx, tx, `SELECT k.prefix, k.expires_at FROM api_keys k
			JOIN issued_keys i ON i.prefix = k.prefix
			WHERE k.issuer <> '' AND k.revoked_at = 0 AND i.ended_at = 0 AND k.expires_at <= ?`, []any{now.Unix()},
			func(rows *sql.Rows) error {
				var e expired
				if err := rows.Scan(&e.prefix, &e.at); err != nil {
					return err
				}
				ended = append(ended, e)
				return nil
			}); err != nil {
			return err
		}
		for _, e := range ended {
			if err := endIssuedTx(ctx, tx, e.prefix, max(e.at, 1), "expired"); err != nil {
				return err
			}
		}
		return tx.QueryRowContext(ctx, `SELECT coalesce(min(k.expires_at), 0) FROM api_keys k
			JOIN issued_keys i ON i.prefix = k.prefix
			WHERE k.issuer <> '' AND k.revoked_at = 0 AND i.ended_at = 0 AND k.expires_at > ?`, now.Unix()).Scan(&next)
	})
	if err != nil {
		return time.Time{}, E(CodeInternal, "ending the issued keys that expired failed", err)
	}
	return secondsOrZero(next), nil
}

// SweptIssuedKeys is what SweepIssuedKeys deleted.
type SweptIssuedKeys struct {
	// Keys are the records of keys ended longer ago than they are kept;
	// Ends, what was left of keys a year after their creation; Owed, what
	// waited 30 days for the issuer; Uses, the counts of days no longer
	// kept. Failed counts the keys whose deletion failed, tried again at
	// the next sweep.
	Keys, Ends, Owed, Uses, Failed int
}

// Total is everything SweepIssuedKeys deleted.
func (s SweptIssuedKeys) Total() int { return s.Keys + s.Ends + s.Owed + s.Uses }

// SweepIssuedKeys deletes what this server no longer keeps of the keys an
// issuer made, as of now: each key's record 30 days after it ended, one key
// per write (what is left of it stays: its selector, how and when it ended),
// a failure logged and the next one tried; what is left of a key a year
// after its creation; what waited for the issuer for 30 days; and the
// counts of refused uses older than 30 days, or than yesterday once a report
// carried them. The daemon's housekeeping, which passes the moment of its
// next sweep, so that nothing outlives its period waiting for it, and ends
// the keys that expired first (EndExpiredIssuedKeys, at the real now).
func (s *Service) SweepIssuedKeys(ctx context.Context, now time.Time) (SweptIssuedKeys, error) {
	if s.store == nil {
		return SweptIssuedKeys{}, nil
	}
	var out SweptIssuedKeys
	cutoff := now.Add(-store.IssuedKeyRecordRetention).Unix()
	prefixes, err := prefixesOf(ctx, s.store.Reader(), `SELECT prefix FROM issued_keys
		WHERE ended_at <> 0 AND ended_at <= ? ORDER BY ended_at`, cutoff)
	if err != nil {
		return SweptIssuedKeys{}, E(CodeInternal, "reading the issued keys past their retention failed", err)
	}
	for _, p := range prefixes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		err := s.store.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE prefix = ? AND issuer <> ''
				AND EXISTS (SELECT 1 FROM issued_keys i WHERE i.prefix = api_keys.prefix AND i.ended_at <> 0
				            AND i.ended_at <= ?)`, p, cutoff)
			return err
		})
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			out.Failed++
			s.log.Error("deleting an issued key's record past its retention failed; the next one is tried",
				"key", p, "err", err)
			continue
		}
		out.Keys++
	}
	err = s.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if out.Ends, err = execN(ctx, tx, `DELETE FROM issued_key_ends WHERE forget_at <= ?`, now.Unix()); err != nil {
			return err
		}
		if out.Owed, err = execN(ctx, tx, `DELETE FROM issued_key_tell WHERE received_at <= ?`,
			now.Add(-store.IssuedKeyTellRetention).Unix()); err != nil {
			return err
		}
		oldest := now.Add(-store.IssuedKeyUseRetention).UTC().Format(time.DateOnly)
		yesterday := now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
		out.Uses, err = execN(ctx, tx, `DELETE FROM issued_key_uses WHERE day < ? OR (reported = 1 AND day < ?)`,
			oldest, yesterday)
		return err
	})
	if err != nil {
		return out, E(CodeInternal, "sweeping what is left of the issued keys failed", err)
	}
	return out, nil
}

// prefixesOf reads the one text column a query returns.
func prefixesOf(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	var out []string
	err := scanAll(ctx, q, query, args, func(rows *sql.Rows) error {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

// execN runs a statement and reports how many rows it changed.
func execN(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("service: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("service: %w", err)
	}
	return int(n), nil
}

// Errors of the operator's hold.
var (
	errOperatorHolds = E(CodeNotAuthorized,
		"only the operator, with an unrestricted instance admin key, holds or releases a key its issuer made", nil)
	errNoIssuedKey = E(CodeNotFound, "this server holds no live key under that selector", nil)
)

// HoldIssuedKey stops a key an issuer made, by its selector, until the
// operator releases it: the operator's (an unrestricted instance admin
// key), for a key that leaked. Refused at once and before every answer, and
// reported to its issuer as stopped here; nothing of the key changes, and
// only ReleaseIssuedKey lifts it, beside any other stop. A selector this
// server holds no live key under is not_found, and holding one held already
// is no error.
func (s *Service) HoldIssuedKey(ctx context.Context, p Principal, selector string) error {
	if err := s.operatorHold(p); err != nil {
		return err
	}
	var held bool
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var live bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issued_keys i JOIN api_keys k
			ON k.prefix = i.prefix WHERE i.prefix = ? AND i.ended_at = 0 AND k.revoked_at = 0)`, selector).
			Scan(&live); err != nil {
			return err
		}
		if !live {
			return errNoIssuedKey
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO issued_key_stops(prefix, cause, at) VALUES (?, 'operator', ?)
			ON CONFLICT(prefix, cause) DO NOTHING`, selector, s.now().Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		held = n > 0
		return err
	})
	var refused *Error
	switch {
	case errors.As(err, &refused):
		return refused
	case err != nil:
		return E(CodeInternal, "holding the key failed", err)
	}
	s.accessChanged()
	s.log.Info("the operator held a key its issuer made", "key", selector, "by", p.Actor(), "changed", held)
	return nil
}

// ReleaseIssuedKey lifts the operator's hold of a key an issuer made, by its
// selector, and nothing else: a key whose creator is stopped here stays
// refused. A selector this server holds no key under is not_found; releasing
// one not held is no error.
func (s *Service) ReleaseIssuedKey(ctx context.Context, p Principal, selector string) error {
	if err := s.operatorHold(p); err != nil {
		return err
	}
	var released bool
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM issued_keys WHERE prefix = ?)`, selector).
			Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errNoIssuedKey
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM issued_key_stops WHERE prefix = ? AND cause = 'operator'`, selector)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		released = n > 0
		return err
	})
	var refused *Error
	switch {
	case errors.As(err, &refused):
		return refused
	case err != nil:
		return E(CodeInternal, "releasing the key failed", err)
	}
	s.accessChanged()
	s.log.Info("the operator released a key its issuer made", "key", selector, "by", p.Actor(), "changed", released)
	return nil
}

// operatorHold refuses everyone but the operator, on a server that mirrors
// an issuer's keys or not.
func (s *Service) operatorHold(p Principal) error {
	if err := s.authorize(p, auth.ScopeAdmin); err != nil {
		return err
	}
	if !isOperator(p) {
		return errOperatorHolds
	}
	if s.store == nil {
		return E(CodeInternal, "this service has no database to hold a key in", nil)
	}
	return nil
}

// StopIssuedKey gives a key an issuer made the operator's stop, by its id at
// the issuer or else its selector, for an extension whose record of it
// could not be applied even narrowed: refused and reported stopped until the
// operator releases it (ReleaseIssuedKey) after a look. It reports whether
// this server holds such a key. No principal.
func (s *Service) StopIssuedKey(ctx context.Context, issuer, externalID, selector string) (bool, error) {
	if err := s.keyIssuerIs(issuer); err != nil {
		return false, err
	}
	var stopped bool
	err := s.store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		stopped, err = stopIssuedKeyTx(ctx, tx, issuer, externalID, selector, s.now().Unix())
		return err
	})
	if err != nil {
		return false, E(CodeInternal, "stopping the key failed", err)
	}
	if stopped {
		s.accessChanged()
		s.log.Error("an issued key was stopped: its record could not be applied", "key", externalID)
	}
	return stopped, nil
}
