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
// reader, access and directory, and account's visibility. It lives here
// because store imports neither of them; a copy elsewhere is a second rule.
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

// FlagHolderSQL is the condition, over mailbox_access aliased alias, that its
// holder holds the read flag and it counts: they are an active member of the
// mailbox's workspace and active on the instance. On a mailbox without a key
// it is reading (ReaderSQL); on one with a key it is who may be given the
// key, or waits for it (WaitingSQL).
func FlagHolderSQL(alias string) string {
	return `(` + alias + `.read = 1
	  AND EXISTS (SELECT 1 FROM workspace_members reader_m JOIN users reader_u ON reader_u.id = reader_m.user_id
	               WHERE reader_m.workspace_id = ` + alias + `.workspace_id AND reader_m.user_id = ` + alias + `.user_id
	                 AND reader_m.status = 'active' AND reader_u.status = 'active'))`
}

// ReaderSQL is the condition, over mailbox_access aliased alias, that its
// holder reads the mailbox now: the rule above.
func ReaderSQL(alias string) string {
	account, user := alias+".account_id", alias+".user_id"
	return `(` + FlagHolderSQL(alias) + `
	  AND (NOT ` + HasKeySQL(account) + ` OR ` + CurrentGrantSQL(account, user) + `))`
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
