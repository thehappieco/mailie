package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/events"
)

// The send record: the idempotency reservation, the state machine of one
// submission, and the short trail the console shows a person about what they
// sent.
//
// What a row holds is fixed by the privacy policy: the account, the
// idempotency key, a hash of what was composed, the Message-ID, the state, how
// many attempts, a classified reason, how many recipients, where the copy in
// Sent stands, who asked, and when. Never a subject, an address or a word of
// the text; the provider's Sent folder is where the message itself lives.
// Rows go SendRetention after their last change.

// Send states.
const (
	// SendSending is a key taken and a submission not finished. A daemon
	// that stops in this state cannot know how the submission ended, so at
	// the next start every such row becomes unknown (InterruptSends).
	SendSending = "sending"
	// SendSent is a message the submission server accepted.
	SendSent = "sent"
	// SendUnknown is a submission that failed after the message was on the
	// wire: it may have been delivered. Never retried; a copy of the
	// Message-ID found in the account's Sent folder turns it into sent.
	SendUnknown = "unknown"
	// SendFailed is a submission that sent nothing. The key may be used
	// again.
	SendFailed = "failed"
)

// Where the copy in Sent stands.
const (
	// SentCopyNone is a send that files no copy of its own: the provider
	// files it (Gmail, Microsoft), the account asked for none, or nothing
	// was sent.
	SentCopyNone = "n/a"
	// SentCopyPending is a copy Mailie is about to add.
	SentCopyPending = "pending"
	// SentCopyAppended is a copy in the Sent folder: added by Mailie, or
	// found there already.
	SentCopyAppended = "appended"
	// SentCopyFailed is a copy that could not be added. The send itself
	// succeeded.
	SentCopyFailed = "failed"
)

// Reasons a send record gives for how it ended, instead of the server's own
// words, which can quote addresses.
const (
	ReasonInterrupted = "interrupted"
)

// SendRetention is how long a send record is kept after its last change.
const SendRetention = 30 * 24 * time.Hour

// Send is one send record.
type Send struct {
	AccountID   string
	Key         string
	ComposeHash string
	// MessageID is the Message-ID header, without angle brackets.
	MessageID  string
	State      string
	Reason     string
	Attempts   int
	Recipients int
	SentCopy   string
	// CreatedBy is who asked: a person's id, or "key:<prefix>". UserID is the
	// person signed in who asked; empty for a key, which acts as no person.
	CreatedBy string
	UserID    string
	CreatedAt int64
	UpdatedAt int64
	SentAt    int64
}

// Errors of the send record.
var (
	// ErrNoSend is a key the account has no record of.
	ErrNoSend = errors.New("store: no such send")
	// ErrSendQuota is a person, or a key, that has reached its daily number
	// of sends.
	ErrSendQuota = errors.New("store: the daily send limit is reached")
)

// SendReservation takes a key for a submission.
type SendReservation struct {
	AccountID   string
	Key         string
	ComposeHash string
	MessageID   string
	Recipients  int
	CreatedBy   string
	UserID      string
	// DailyLimit is how many sends one sender may start in a day — the
	// person (UserID), or else the key (CreatedBy) —; zero is no limit. It is
	// counted in the transaction that reserves, so two sends at once cannot
	// both take the last one.
	DailyLimit int
}

// ReserveSend takes the key for a submission, before anything is dialed, in
// one writer transaction: a check that read first and wrote later would let
// two requests with the same key both through.
//
// It returns the row as it stands after the call and whether this call took
// it. A key nobody holds, or one whose last submission failed — which sent
// nothing — is taken, and the row starts over as sending with this request's
// hash and Message-ID, and as created now: a key taken again can send a
// message, any message, so it counts in the daily limit of the day it is
// taken, not of the day it first failed. Otherwise a stock of keys failed
// cheaply one day would send past the limit the next. A key in any other
// state is left alone and returned, for the caller to replay or refuse.
func (s *Store) ReserveSend(ctx context.Context, r SendReservation) (Send, bool, error) {
	now := s.now().Unix()
	var (
		out      Send
		reserved bool
	)
	err := s.Write(ctx, func(tx *sql.Tx) error {
		existing, found, err := sendTx(ctx, tx, r.AccountID, r.Key)
		if err != nil {
			return err
		}
		if found && existing.State != SendFailed {
			out = existing
			return nil
		}
		if r.DailyLimit > 0 && (r.UserID != "" || r.CreatedBy != "") {
			column, who := "user_id", r.UserID
			if who == "" {
				column, who = "created_by", r.CreatedBy
			}
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM sends WHERE `+column+` = ? AND created_at > ?`,
				who, now-int64((24*time.Hour).Seconds())).Scan(&n); err != nil {
				return fmt.Errorf("store: count sends: %w", err)
			}
			if n >= r.DailyLimit {
				return ErrSendQuota
			}
		}
		if found {
			_, err = tx.ExecContext(ctx,
				`UPDATE sends SET compose_hash = ?, message_id_hdr = ?, state = 'sending', attempts = 0, error = '',
				        smtp_response = '', sent_copy_state = 'n/a', recipients = ?, created_by = ?, user_id = ?,
				        created_at = ?, updated_at = ?, sent_at = 0
				  WHERE account_id = ? AND idempotency_key = ?`,
				r.ComposeHash, r.MessageID, r.Recipients, r.CreatedBy, r.UserID, now, now, r.AccountID, r.Key)
		} else {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO sends(account_id, idempotency_key, compose_hash, message_id_hdr, state, recipients,
				        created_by, user_id, created_at, updated_at)
				 VALUES (?, ?, ?, ?, 'sending', ?, ?, ?, ?, ?)`,
				r.AccountID, r.Key, r.ComposeHash, r.MessageID, r.Recipients, r.CreatedBy, r.UserID, now, now)
		}
		if err != nil {
			return fmt.Errorf("store: reserve a send: %w", err)
		}
		out, _, err = sendTx(ctx, tx, r.AccountID, r.Key)
		reserved = true
		return err
	})
	if err != nil {
		return Send{}, false, err
	}
	return out, reserved, nil
}

// SendOutcome is how a submission ended.
type SendOutcome struct {
	AccountID string
	Key       string
	// State is sent, failed or unknown.
	State    string
	Reason   string
	Attempts int
	// SentCopy is where the copy in Sent stands once the state is written.
	SentCopy string
}

// FinishSend records how a submission ended and journals send.finished in the
// same transaction; the caller publishes the events once it returns.
//
// Only a row still sending changes. An unknown outcome whose Message-ID the
// index already holds in the account's Sent folder is recorded as sent: the
// provider filed the copy before the failure was even reported.
func (s *Store) FinishSend(ctx context.Context, o SendOutcome) (Send, []events.Event, error) {
	now := s.now()
	var (
		out Send
		evs []events.Event
	)
	err := s.Write(ctx, func(tx *sql.Tx) error {
		current, found, err := sendTx(ctx, tx, o.AccountID, o.Key)
		switch {
		case err != nil:
			return err
		case !found || current.State != SendSending:
			return ErrNoSend
		}
		state, reason, sentAt := o.State, o.Reason, int64(0)
		if state == SendUnknown {
			filed, err := filedInSentTx(ctx, tx, o.AccountID, current.MessageID)
			if err != nil {
				return err
			}
			if filed {
				state, reason = SendSent, ""
			}
		}
		if state == SendSent {
			sentAt = now.Unix()
		}
		copyState := o.SentCopy
		if copyState == "" || state != SendSent {
			copyState = SentCopyNone
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sends SET state = ?, error = ?, attempts = ?, sent_at = ?, sent_copy_state = ?, updated_at = ?
			  WHERE account_id = ? AND idempotency_key = ?`,
			state, reason, o.Attempts, sentAt, copyState, now.Unix(), o.AccountID, o.Key); err != nil {
			return fmt.Errorf("store: finish a send: %w", err)
		}
		if evs, err = journalSendFinished(ctx, s, tx, now, []sendKey{{o.AccountID, o.Key, state, current.UserID,
			keySender(current.CreatedBy)}}); err != nil {
			return err
		}
		out, _, err = sendTx(ctx, tx, o.AccountID, o.Key)
		return err
	})
	if err != nil {
		return Send{}, nil, err
	}
	return out, evs, nil
}

// SetSentCopy records where the copy in Sent of a sent message stands.
func (s *Store) SetSentCopy(ctx context.Context, accountID, key, state string) error {
	now := s.now().Unix()
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sends SET sent_copy_state = ?, updated_at = ? WHERE account_id = ? AND idempotency_key = ?`,
			state, now, accountID, key); err != nil {
			return fmt.Errorf("store: record the sent copy: %w", err)
		}
		return nil
	})
}

// SendOf reads one send record.
func (s *Store) SendOf(ctx context.Context, accountID, key string) (Send, error) {
	out, found, err := sendTx(ctx, s.r, accountID, key)
	if err != nil {
		return Send{}, err
	}
	if !found {
		return Send{}, ErrNoSend
	}
	return out, nil
}

// InterruptSends turns every send still sending into unknown, and journals
// send.finished for each. It is for the daemon's start, before anything can
// send: a row in that state was left by a daemon that stopped mid-submission,
// and nobody can say whether the server took the message. Never failed,
// which would let the key be used again and the message go out twice. It
// returns how many rows it changed.
func (s *Store) InterruptSends(ctx context.Context) (int, error) {
	now := s.now()
	var changed []sendKey
	err := s.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`UPDATE sends SET state = 'unknown', error = ?, updated_at = ? WHERE state = 'sending'
			 RETURNING account_id, idempotency_key, user_id, created_by`, ReasonInterrupted, now.Unix())
		if err != nil {
			return fmt.Errorf("store: interrupt sends: %w", err)
		}
		//nolint:errcheck // read to the end below
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			k := sendKey{state: SendUnknown}
			var createdBy string
			if err := rows.Scan(&k.accountID, &k.key, &k.userID, &createdBy); err != nil {
				return fmt.Errorf("store: interrupt sends: %w", err)
			}
			k.sentBy = keySender(createdBy)
			changed = append(changed, k)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("store: interrupt sends: %w", err)
		}
		_, err = journalSendFinished(ctx, s, tx, now, changed)
		return err
	})
	if err != nil {
		return 0, err
	}
	return len(changed), nil
}

// SweepSends deletes the send records whose last change is older than
// SendRetention, and the send.finished notices older than that, and reports
// how many of each it deleted.
//
// ahead is how long until the caller sweeps again: a record that would pass
// its retention before then is deleted now, so a sweep once an hour never
// keeps one up to an hour longer than promised. A send still sending is kept
// whatever its age.
//
// The notices are part of the record: each names the mailbox, the key and
// the outcome, and the journal's own retention keeps its newest ten thousand
// events whatever their age, which on a quiet instance is months. So each
// goes SendRetention after it was written, which is never after its record,
// and with any webhook delivery of it.
func (s *Store) SweepSends(ctx context.Context, ahead time.Duration) (records, notices int, err error) {
	cutoff := s.now().Add(ahead - SendRetention).Unix()
	err = s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sends WHERE updated_at < ? AND state <> 'sending'`, cutoff)
		if err != nil {
			return fmt.Errorf("store: sweep sends: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: sweep sends: %w", err)
		}
		records = int(n)
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM webhook_deliveries WHERE event_seq IN (SELECT seq FROM events WHERE type = ? AND created_at < ?)`,
			string(events.TypeSendFinished), cutoff); err != nil {
			return fmt.Errorf("store: sweep send notices: %w", err)
		}
		res, err = tx.ExecContext(ctx, `DELETE FROM events WHERE type = ? AND created_at < ?`,
			string(events.TypeSendFinished), cutoff)
		if err != nil {
			return fmt.Errorf("store: sweep send notices: %w", err)
		}
		if n, err = res.RowsAffected(); err != nil {
			return fmt.Errorf("store: sweep send notices: %w", err)
		}
		notices = int(n)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return records, notices, nil
}

// ForgetSenderTx takes a person out of the send records, inside the
// transaction that deletes them. The records of the mailboxes they linked go
// with those mailboxes; this is for the ones they sent from a mailbox someone
// else linked, which stay with it and no longer say who asked. So do the
// send.finished notices of those sends, which name the sender too.
func ForgetSenderTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE sends SET user_id = '', created_by = '' WHERE user_id = ? OR created_by = ?`, userID, userID); err != nil {
		return fmt.Errorf("store: forget who sent: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET payload_json = json_remove(payload_json, '$.user_id')
		  WHERE type = ? AND json_extract(payload_json, '$.user_id') = ?`,
		string(events.TypeSendFinished), userID); err != nil {
		return fmt.Errorf("store: forget who sent: %w", err)
	}
	return nil
}

// reconcileSendsTx turns the unknown sends whose Message-ID just appeared in
// the account's Sent folder into sent, inside the sync transaction that
// indexed the copy, and returns the events to journal with it.
func reconcileSendsTx(ctx context.Context, tx *sql.Tx, accountID, messageID string, now time.Time) ([]events.Event, error) {
	rows, err := tx.QueryContext(ctx,
		`UPDATE sends SET state = 'sent', error = '', sent_at = ?, updated_at = ?
		  WHERE account_id = ? AND message_id_hdr = ? AND state = 'unknown'
		  RETURNING idempotency_key, user_id, created_by`, now.Unix(), now.Unix(), accountID, messageID)
	if err != nil {
		return nil, fmt.Errorf("store: reconcile sends: %w", err)
	}
	//nolint:errcheck // read to the end below
	defer func() { _ = rows.Close() }()
	var out []events.Event
	for rows.Next() {
		var key, userID, createdBy string
		if err := rows.Scan(&key, &userID, &createdBy); err != nil {
			return nil, fmt.Errorf("store: reconcile sends: %w", err)
		}
		ev, err := events.New(events.TypeSendFinished, accountID, now, SendFinished{
			AccountID: accountID, Key: key, State: SendSent, UserID: userID, SentBy: keySender(createdBy),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reconcile sends: %w", err)
	}
	return out, nil
}

// filedInSentTx reports whether the index holds a live copy of a Message-ID in
// one of the account's Sent folders.
func filedInSentTx(ctx context.Context, tx *sql.Tx, accountID, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id
		  WHERE m.account_id = ? AND m.group_key = ? AND m.message_id = ? AND f.role = 'sent' AND m.vanished_at = 0`,
		accountID, GroupKey(messageID, "", "", 0), messageID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: look for the sent copy: %w", err)
	}
	return n > 0, nil
}

type sendKey struct {
	accountID string
	key       string
	state     string
	// userID is the person who sent it, "" for a key.
	userID string
	// sentBy is "key:<prefix>" for a key's send, "" for a person's.
	sentBy string
}

// keySender is who a send.finished names as having sent it with a key: the
// key ("key:<prefix>"), or "" for a person's send.
func keySender(createdBy string) string {
	if strings.HasPrefix(createdBy, "key:") {
		return createdBy
	}
	return ""
}

// SendsBy lists the send records of one sender ("key:<prefix>"), newest
// first, at most limit: every one, or with workspaceID those from that
// workspace's mailboxes.
func (s *Store) SendsBy(ctx context.Context, createdBy, workspaceID string, limit int) ([]Send, error) {
	rows, err := s.r.QueryContext(ctx,
		`SELECT s.account_id, s.idempotency_key, s.compose_hash, s.message_id_hdr, s.state, s.error, s.attempts,
		        s.recipients, s.sent_copy_state, s.created_by, s.user_id, s.created_at, s.updated_at, s.sent_at
		   FROM sends s JOIN accounts a ON a.id = s.account_id
		  WHERE s.created_by = ?1 AND (?2 = '' OR a.workspace_id = ?2)
		  ORDER BY s.created_at DESC, s.rowid DESC LIMIT ?3`, createdBy, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sends: %w", err)
	}
	//nolint:errcheck // read-only query
	defer func() { _ = rows.Close() }()
	var out []Send
	for rows.Next() {
		var row Send
		if err := rows.Scan(&row.AccountID, &row.Key, &row.ComposeHash, &row.MessageID, &row.State, &row.Reason,
			&row.Attempts, &row.Recipients, &row.SentCopy, &row.CreatedBy, &row.UserID, &row.CreatedAt,
			&row.UpdatedAt, &row.SentAt); err != nil {
			return nil, fmt.Errorf("store: list sends: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sends: %w", err)
	}
	return out, nil
}

func journalSendFinished(ctx context.Context, s *Store, tx *sql.Tx, now time.Time, keys []sendKey) ([]events.Event, error) {
	evs := make([]events.Event, 0, len(keys))
	for _, k := range keys {
		ev, err := events.New(events.TypeSendFinished, k.accountID, now, SendFinished{
			AccountID: k.accountID, Key: k.key, State: k.state, UserID: k.userID, SentBy: k.sentBy,
		})
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return journal(ctx, s, tx, evs)
}

func sendTx(ctx context.Context, q querier, accountID, key string) (Send, bool, error) {
	var out Send
	err := q.QueryRowContext(ctx,
		`SELECT account_id, idempotency_key, compose_hash, message_id_hdr, state, error, attempts, recipients,
		        sent_copy_state, created_by, user_id, created_at, updated_at, sent_at
		   FROM sends WHERE account_id = ? AND idempotency_key = ?`, accountID, key,
	).Scan(&out.AccountID, &out.Key, &out.ComposeHash, &out.MessageID, &out.State, &out.Reason, &out.Attempts,
		&out.Recipients, &out.SentCopy, &out.CreatedBy, &out.UserID, &out.CreatedAt, &out.UpdatedAt, &out.SentAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Send{}, false, nil
	case err != nil:
		return Send{}, false, fmt.Errorf("store: read a send: %w", err)
	}
	return out, true, nil
}
