package store

// Who reads a mailbox (docs/key-scheme.md section 12.13), as SQL fragments.
//
// One rule decides access, whether a team mailbox syncs and has its consent,
// and the last-reader protection: a person reads a mailbox when they hold
// the read flag on it (mailbox_access.read), as an active member of its
// workspace who is active on the instance, and, on a mailbox that has a key
// (a mailbox_keys row), hold a grant at its current epoch, the highest of its
// rows. A mailbox without a key is read by the flag alone, as before the key
// scheme (section 12.14). Keys never count as readers, and neither does a
// role.
//
// The packages that decide access build their queries from these fragments
// and from nothing else: this package's sync eligibility, workspace's last
// reader, access and directory, and account's visibility. Whether a team is
// closed is a gate beside the rule, never part of it (OpenWorkspaceSQL). A
// removed mailbox (migration 0020) is read by nobody: the rule holds only
// on a live one (LiveSQL), so it has no reader, nobody waits for its key and
// nobody is its last reader while it is kept. It lives here because store
// imports neither of them; a copy elsewhere is a second rule.
//
// Each fragment takes the SQL expressions it is about (an alias's column,
// a bound parameter) and names its own tables reader_*, so it nests inside a
// query whatever that query calls its own.

// HasKeySQL is the condition that the mailbox whose id is the expression
// account has a key: any row of mailbox_keys.
func HasKeySQL(account string) string {
	return `EXISTS (SELECT 1 FROM mailbox_keys reader_k WHERE reader_k.account_id = ` + account + `)`
}

// CurrentEpochSQL is the mailbox's current epoch, as an expression: the
// highest of its mailbox_keys rows, NULL for a mailbox without a key.
func CurrentEpochSQL(account string) string {
	return `(SELECT max(reader_e.epoch) FROM mailbox_keys reader_e WHERE reader_e.account_id = ` + account + `)`
}

// CurrentGrantSQL is the condition that the person whose id is the
// expression user holds a grant on the mailbox account at its current epoch.
// False on a mailbox without a key, which has no epoch.
func CurrentGrantSQL(account, user string) string {
	return `EXISTS (SELECT 1 FROM mailbox_grants reader_g WHERE reader_g.account_id = ` + account + `
		  AND reader_g.user_id = ` + user + ` AND reader_g.epoch = ` + CurrentEpochSQL(account) + `)`
}

// LiveSQL is the condition that the mailbox whose id is the expression
// account is not removed (migration 0020): a removed mailbox is kept, with
// its grants and its key, for a recovery, and is read by nobody meanwhile.
// Every fragment of the rule holds only on a live mailbox. A mailbox nobody
// knows is live: what it is about is for the query to have found.
func LiveSQL(account string) string {
	return `NOT EXISTS (SELECT 1 FROM accounts reader_a WHERE reader_a.id = ` + account + ` AND reader_a.removed_at <> 0)`
}

// FlagHolderSQL is the condition, over mailbox_access aliased alias, that its
// holder holds the read flag and it counts: the mailbox is live (LiveSQL),
// and they are an active member of its workspace and active on the instance
// (neither disabled nor suspended). On a mailbox without a key it is reading
// (ReaderSQL); on one with a key it is who may be given the key, or waits
// for it (WaitingSQL).
func FlagHolderSQL(alias string) string {
	return `(` + LiveSQL(alias+`.account_id`) + ` AND ` + flagHolderSQL(alias) + `)`
}

// flagHolderSQL is FlagHolderSQL whether or not the mailbox is live.
func flagHolderSQL(alias string) string {
	return `(` + alias + `.read = 1
	  AND EXISTS (SELECT 1 FROM workspace_members reader_m JOIN users reader_u ON reader_u.id = reader_m.user_id
	               WHERE reader_m.workspace_id = ` + alias + `.workspace_id AND reader_m.user_id = ` + alias + `.user_id
	                 AND reader_m.status = 'active' AND reader_u.status = 'active'))`
}

// ReaderSQL is the condition, over mailbox_access aliased alias, that its
// holder reads the mailbox now: the rule above.
func ReaderSQL(alias string) string {
	return `(` + LiveSQL(alias+`.account_id`) + ` AND ` + KeptReaderSQL(alias) + `)`
}

// KeptReaderSQL is the condition, over mailbox_access aliased alias, that its
// holder would read a removed mailbox once it is recovered: the rule above,
// on the mailbox as if it were live, from the flags and the grants it keeps.
// Nobody reads a removed mailbox, so this decides nothing: it only says, to
// whoever may recover one, who would read it again.
func KeptReaderSQL(alias string) string {
	account, user := alias+".account_id", alias+".user_id"
	return `(` + flagHolderSQL(alias) + `
	  AND (NOT ` + HasKeySQL(account) + ` OR ` + CurrentGrantSQL(account, user) + `))`
}

// SuspendedReaderSQL is the condition, over mailbox_access aliased alias,
// that its holder would read the mailbox once resumed: they are suspended by
// an extension of the daemon (users.suspended_at), which keeps their grants,
// and are otherwise a reader by the rule above, an active member of the
// live mailbox's workspace holding the read flag and, on a mailbox that has
// a key, a grant at its current epoch. They read nothing now, and count for
// nothing anywhere the rule decides: this is only what keeps a mailbox they
// alone would read from being called one nobody can ever read again.
func SuspendedReaderSQL(alias string) string {
	return `(` + LiveSQL(alias+`.account_id`) + ` AND ` + KeptSuspendedReaderSQL(alias) + `)`
}

// KeptSuspendedReaderSQL is SuspendedReaderSQL on a removed mailbox as if it
// were live: who would read it once it is recovered and they are resumed.
// Like KeptReaderSQL, it decides nothing but that a removed team mailbox
// waits for them rather than being one nobody could read again.
func KeptSuspendedReaderSQL(alias string) string {
	account, user := alias+".account_id", alias+".user_id"
	return `(` + alias + `.read = 1
	  AND EXISTS (SELECT 1 FROM workspace_members reader_m JOIN users reader_u ON reader_u.id = reader_m.user_id
	               WHERE reader_m.workspace_id = ` + alias + `.workspace_id AND reader_m.user_id = ` + alias + `.user_id
	                 AND reader_m.status = 'active' AND reader_u.suspended_at <> 0)
	  AND (NOT ` + HasKeySQL(account) + ` OR ` + CurrentGrantSQL(account, user) + `))`
}

// OpenWorkspaceSQL is the condition that the workspace whose id is the
// expression workspace is not a closed team (migration 0019): closing a team
// is a gate on using its mailboxes, never part of who reads them. It is
// added beside the reading rule wherever a mailbox is reached — a person's
// and a key's visibility, a person's access, what a key holds — and where it
// syncs, so a closed team's mailboxes reach nobody and index nothing, while
// ReaderSQL and the protections it decides go on as they were: reopened, the
// team is as it was. A workspace nobody knows is open: what it is about is
// for the query to have found.
func OpenWorkspaceSQL(workspace string) string {
	return `NOT EXISTS (SELECT 1 FROM workspaces reader_w WHERE reader_w.id = ` + workspace + ` AND reader_w.closed_at <> 0)`
}

// WaitingSQL is the condition, over mailbox_access aliased alias, that its
// holder waits for the key: they hold the read flag and it counts, on a
// mailbox that has a key, without a grant at its current epoch. They see the
// mailbox's card and read nothing of it until a reader supplies the key
// (section 12.13), or, for their own personal mailbox, they write it a new
// one (section 12.12).
func WaitingSQL(alias string) string {
	account, user := alias+".account_id", alias+".user_id"
	return `(` + FlagHolderSQL(alias) + `
	  AND ` + HasKeySQL(account) + ` AND NOT ` + CurrentGrantSQL(account, user) + `)`
}
