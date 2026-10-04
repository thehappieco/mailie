// Package events is the notification spine: one journal, one live fan-out.
//
// Every state change that a client could care about is written to the events
// table inside the same transaction as the change itself, and only published
// to subscribers after that transaction commits. Two consequences, both
// deliberate:
//
//   - A subscriber never learns about a message it could not then read. The
//     alternative — publish first, commit later — produces exactly that race,
//     and it shows up as an LLM being told about mail that "does not exist".
//
//   - Replay has one implementation. The sequence number in the journal is the
//     SSE event id, the long-poll cursor and the resume point after a restart,
//     so Last-Event-ID and since_cursor are the same query. A separate
//     in-memory ring would add a second source of truth and a seam between the
//     two where events go missing or arrive twice.
//
// The bus itself therefore keeps no history at all. It fans live events out to
// whoever is attached, with a bounded queue per subscriber: a consumer that
// cannot keep up is marked lagged and told to re-read, rather than being
// disconnected or allowed to stall the sync engine that is feeding it.
package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// Type names an event. The vocabulary is closed: a consumer filters on these,
// and inventing one at a call site makes it invisible to every filter.
type Type string

const (
	// TypeMessageNew is mail that arrived. It carries folder_role, first_copy
	// and first_inbox_copy so a consumer can tell a genuinely new message from
	// another copy of one it already knows about.
	TypeMessageNew Type = "message.new"
	// TypeMessageFlags is a flag change: read, starred, answered.
	TypeMessageFlags Type = "message.flags"
	// TypeMessageMoved is a message that changed folder, or gained or lost a
	// Gmail label. Not a deletion: the message still exists somewhere.
	TypeMessageMoved Type = "message.moved"
	// TypeMessageDeleted is the last copy of a message going away.
	TypeMessageDeleted Type = "message.deleted"
	// TypeFolderChanged is a folder's counters, role or sync state moving.
	TypeFolderChanged Type = "folder.changed"
	// TypeAccountState is an account becoming active, needing re-authorisation
	// or failing.
	TypeAccountState Type = "account.state"
	// TypeSendFinished is the outcome of a submission, including the
	// deliberately unresolved "unknown".
	TypeSendFinished Type = "send.finished"
	// TypeSyncProgress is how far an account's initial sync has come, as a
	// percentage. Throttled at the source: it exists to move a progress bar,
	// not to report every batch.
	TypeSyncProgress Type = "sync.progress"
)

// Types lists every event type, for validating a subscription filter.
func Types() []Type {
	return []Type{
		TypeMessageNew, TypeMessageFlags, TypeMessageMoved, TypeMessageDeleted,
		TypeFolderChanged, TypeAccountState, TypeSendFinished, TypeSyncProgress,
	}
}

// Valid reports whether t is a known type.
func (t Type) Valid() bool {
	for _, known := range Types() {
		if t == known {
			return true
		}
	}
	return false
}

// ParseType converts a string, rejecting anything unknown so a filter that
// would silently match nothing is reported instead.
func ParseType(s string) (Type, error) {
	t := Type(s)
	if !t.Valid() {
		return "", fmt.Errorf("events: unknown event type %q", s)
	}
	return t, nil
}

// Event is one journal row.
type Event struct {
	// Seq is assigned by the journal, so it is monotonic across restarts and
	// usable as a durable cursor.
	Seq       int64           `json:"seq"`
	Type      Type            `json:"type"`
	AccountID string          `json:"account_id"`
	At        time.Time       `json:"at"`
	Payload   json.RawMessage `json:"payload"`
}

// New builds an event with its payload marshalled.
func New(t Type, accountID string, at time.Time, payload any) (Event, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("events: marshal %s payload: %w", t, err)
	}
	return Event{Type: t, AccountID: accountID, At: at, Payload: body}, nil
}
