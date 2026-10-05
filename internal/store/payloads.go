package store

import "github.com/thehappieco/mailie/internal/provider"

// Event payloads the index writes into the journal, in the same transaction as
// the rows they describe. Every payload names its account, so a stream can be
// filtered by ownership without parsing anything else, and none carries a
// credential. They do carry what the person would see in a list — a subject,
// a sender — because that is their own mail, shown to them.
//
// message_id in these payloads is the local row id (messages.id), the id the
// API hands out, not the RFC 5322 Message-ID header.

// Folder changes a FolderChanged event reports.
const (
	FolderAdded       = "added"        // a folder appeared in LIST
	FolderUpdated     = "updated"      // its role, selectability or synced flag changed
	FolderRemoved     = "removed"      // gone from LIST twice in a row, and deleted with its rows
	FolderInitialDone = "initial_done" // its initial sync finished; Count is how many messages it holds
	FolderResyncDone  = "resync_done"  // a UIDVALIDITY resync finished in place
)

// MessageNew is mail that arrived: message.new.
type MessageNew struct {
	AccountID  string `json:"account_id"`
	MessageID  int64  `json:"message_id"`
	FolderID   int64  `json:"folder_id"`
	FolderRole string `json:"folder_role"`
	Subject    string `json:"subject"`
	// From is the first From address, or null when the header had none.
	From         *provider.Address `json:"from"`
	InternalDate int64             `json:"internal_date"`
	// FirstCopy is true when no other folder held this message before; false
	// for an inbox arrival whose Gmail label was synced first.
	FirstCopy bool `json:"first_copy"`
	// FirstInboxCopy is true for an arrival in an inbox that no inbox held
	// before. A consumer waiting for new mail keys on this.
	FirstInboxCopy bool `json:"first_inbox_copy"`
}

// MessageFlags is a flag change on one row: message.flags.
type MessageFlags struct {
	AccountID string   `json:"account_id"`
	MessageID int64    `json:"message_id"`
	FolderID  int64    `json:"folder_id"`
	Flags     []string `json:"flags"`
	Seen      bool     `json:"seen"`
	Flagged   bool     `json:"flagged"`
	Answered  bool     `json:"answered"`
	Draft     bool     `json:"draft"`
	Deleted   bool     `json:"deleted"`
}

// MessageMoved is a copy of a message appearing in a folder or leaving one
// while the message lives on elsewhere: message.moved. Moving a message is a
// copy leaving the source and one appearing in the destination, and on Gmail
// a label added or removed is exactly the same thing.
type MessageMoved struct {
	AccountID  string `json:"account_id"`
	MessageID  int64  `json:"message_id"` // the row that appeared or went away
	FolderID   int64  `json:"folder_id"`  // where it appeared or went away from
	FolderRole string `json:"folder_role"`
	// NewCopy is true when the row appeared in FolderID.
	NewCopy bool `json:"new_copy"`
	// To is FolderID for a new copy, and null for a copy that went away.
	To *int64 `json:"to"`
	// PrimaryID is the row that stands for the message after the change.
	PrimaryID int64 `json:"primary_id"`
}

// ActionMoved is a move a person asked for: message.moved, with the fields
// the engine's copies carry, and where the row came from and went.
//
// A row that follows its message keeps its id: MessageID is the same row
// before and after, FolderID and To are where it is now. A row that left
// the index — the destination is not synced, as Gmail's All Mail is not —
// has To and ToFolderID null and FolderID the folder it left. An action
// never announces message.deleted (nothing was deleted) or message.new.
type ActionMoved struct {
	AccountID  string `json:"account_id"`
	MessageID  int64  `json:"message_id"`
	FolderID   int64  `json:"folder_id"`
	FolderRole string `json:"folder_role"`
	// NewCopy is true only for a row that appeared: a message moved back
	// into a synced folder from one that is not, under a new id.
	NewCopy bool `json:"new_copy"`
	// To is the folder the row is in now, or null when it left the index.
	To *int64 `json:"to"`
	// PrimaryID is the row that stands for the message after the move; 0
	// when no copy of it is left in the index.
	PrimaryID int64 `json:"primary_id"`
	// FromFolderID is the folder it left; ToFolderID is To.
	FromFolderID int64  `json:"from_folder_id"`
	ToFolderID   *int64 `json:"to_folder_id"`
}

// MessageDeleted is the last copy of a message going away: message.deleted.
type MessageDeleted struct {
	AccountID  string `json:"account_id"`
	MessageID  int64  `json:"message_id"`
	FolderID   int64  `json:"folder_id"`
	FolderRole string `json:"folder_role"`
}

// FolderChanged is a folder appearing, changing, going away or finishing a
// sync phase: folder.changed.
type FolderChanged struct {
	AccountID string `json:"account_id"`
	FolderID  int64  `json:"folder_id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Change    string `json:"change"`
	Count     int    `json:"count,omitempty"`
}

// SyncProgress is how far an account's initial sync has come:
// sync.progress. The engine throttles it; it exists to move a progress bar.
type SyncProgress struct {
	AccountID string `json:"account_id"`
	// Progress is 0..100, as SyncSummary.InitialProgress computes it.
	Progress      int   `json:"progress"`
	FoldersSynced int   `json:"folders_synced"`
	FoldersTotal  int   `json:"folders_total"`
	Messages      int64 `json:"messages"`
}

// SendFinished is how a submission ended: send.finished. Key is the send's
// idempotency key, which GET /v1/sends/{key} answers about. It never carries
// a subject, an address or a server's words.
//
// It is one sender's record, not the mailbox's: several people may send from
// one shared mailbox, and the event goes only to whoever may read the record
// it is about (internal/service, the event gate), which UserID decides.
type SendFinished struct {
	AccountID string `json:"account_id"`
	Key       string `json:"key"`
	// State is sent, failed or unknown; a later send.finished with sent
	// follows an unknown that the Sent folder confirmed.
	State string `json:"state"`
	// UserID is the person who sent it, whichever credential they used;
	// absent for an instance key's send, and once that person is deleted
	// (ForgetSenderTx).
	UserID string `json:"user_id,omitempty"`
}
