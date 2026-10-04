package sync

import "time"

// Options tune the engine. The zero value is the production schedule, every
// number below with the reason it is what it is; tests shorten them.
type Options struct {
	// Now is the engine's clock for what it writes: sync windows, pass
	// times. Timers are real ones.
	Now func() time.Time

	// EligibilityInterval is how often the whole list of accounts that may
	// sync is read again, as a backstop for a lifecycle change nobody
	// reported with Reconcile. Thirty seconds at least: this polls the
	// database.
	EligibilityInterval time.Duration
	// RetentionInterval is how often the event journal is pruned.
	RetentionInterval time.Duration
	// DiscoveryInterval is how often the folder list is read again.
	DiscoveryInterval time.Duration

	// InboxInterval is the safety net under IDLE on the condstore tier.
	InboxInterval time.Duration
	// InboxPollInterval is how often the inbox is looked at when nothing
	// notifies: no IDLE, or the uidpoll tier, where a flag change raises no
	// modification sequence to notice.
	InboxPollInterval time.Duration
	// SentInterval is for sent mail and drafts, which a person expects to
	// see soon after writing.
	SentInterval time.Duration
	// TrashInterval is for the trash, which nobody watches closely.
	TrashInterval time.Duration
	// JunkInterval is for spam. A person looking for a message that never
	// reached the inbox looks there first, and with LIST-STATUS a pass over
	// an unchanged folder costs no SELECT, so it is looked at nearly as
	// often as sent mail.
	JunkInterval time.Duration
	// OtherInterval is for every other folder: labels, archives, a
	// person's own folders.
	OtherInterval time.Duration
	// FolderRetry is how long a folder whose pass failed waits before the
	// next one. The other folders do not wait.
	FolderRetry time.Duration

	// InboxDiffInterval and OtherDiffInterval space the UID diffs that find
	// expunged mail when nothing signalled an expunge.
	InboxDiffInterval time.Duration
	OtherDiffInterval time.Duration
	// ConfirmDiffDelay is when a diff that tombstoned something is repeated:
	// a row is deleted only when two consecutive diffs miss it.
	ConfirmDiffDelay time.Duration

	// InboxFlagSweep and OtherFlagSweep space the uidpoll tier's reading of
	// every flag window, not only the newest one.
	InboxFlagSweep time.Duration
	OtherFlagSweep time.Duration

	// IdleRenew overrides the provider profile's IDLE renewal.
	IdleRenew time.Duration
	// IdleDebounce gathers a burst of IDLE notifications into one pass.
	IdleDebounce time.Duration
	// IdleStopTimeout bounds ending an IDLE: a server that never answers
	// DONE gets its connection closed instead.
	IdleStopTimeout time.Duration

	// BatchSize bounds one FETCH and one transaction.
	BatchSize int
	// FlagWindow is how many indexed UIDs one uidpoll flag fetch covers.
	FlagWindow int
	// BackfillSlice is how many initial-sync batches a folder gets before
	// the loop looks at what else is due, so new mail never waits behind a
	// long backfill.
	BackfillSlice int

	// BackoffBase and BackoffMax bound the exponential retry after a dropped
	// connection or a temporary failure.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// BackoffReset is how long without a failure puts the exponent back to
	// zero: Gmail ends an OAuth session about hourly, and that must cost a
	// second, not five minutes.
	BackoffReset time.Duration
	// LongBackoff is the wait after a refusal a retry will not fix soon:
	// credentials refused, IMAP not enabled on the mailbox, a grant that
	// needs a person.
	LongBackoff time.Duration
	// TooManyBackoff is the wait after the provider's connection cap, which
	// counts every client the person runs.
	TooManyBackoff time.Duration
	// RateLimitMin is the least wait after throttling.
	RateLimitMin time.Duration

	// OpenTimeout bounds dialling and authenticating one connection.
	OpenTimeout time.Duration
	// InteractiveIdle closes the interactive connection after this long
	// unused.
	InteractiveIdle time.Duration
	// ProgressEvery throttles sync.progress events per account.
	ProgressEvery time.Duration
	// OKEvery throttles recording a good pass on the account row.
	OKEvery time.Duration
	// ShutdownTimeout bounds waiting for the workers to log out.
	ShutdownTimeout time.Duration
}

func (o Options) withDefaults() Options {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def(&o.EligibilityInterval, 30*time.Second)
	def(&o.RetentionInterval, time.Hour)
	def(&o.DiscoveryInterval, 30*time.Minute)
	def(&o.InboxInterval, 5*time.Minute)
	def(&o.InboxPollInterval, time.Minute)
	def(&o.SentInterval, 5*time.Minute)
	def(&o.TrashInterval, 30*time.Minute)
	def(&o.JunkInterval, 5*time.Minute)
	def(&o.OtherInterval, 15*time.Minute)
	def(&o.FolderRetry, 5*time.Minute)
	def(&o.InboxDiffInterval, 10*time.Minute)
	def(&o.OtherDiffInterval, time.Hour)
	def(&o.ConfirmDiffDelay, 30*time.Second)
	def(&o.InboxFlagSweep, 30*time.Minute)
	def(&o.OtherFlagSweep, 3*time.Hour)
	def(&o.IdleDebounce, 500*time.Millisecond)
	def(&o.IdleStopTimeout, 10*time.Second)
	def(&o.BackoffBase, time.Second)
	def(&o.BackoffMax, 5*time.Minute)
	def(&o.BackoffReset, 10*time.Minute)
	def(&o.LongBackoff, 15*time.Minute)
	def(&o.TooManyBackoff, 5*time.Minute)
	def(&o.RateLimitMin, time.Minute)
	def(&o.OpenTimeout, time.Minute)
	def(&o.InteractiveIdle, 5*time.Minute)
	def(&o.ProgressEvery, 2*time.Second)
	def(&o.OKEvery, time.Minute)
	def(&o.ShutdownTimeout, 15*time.Second)
	if o.BatchSize <= 0 || o.BatchSize > 200 {
		// 200 at most: the plan's bound on one FETCH, and one transaction
		// that holds the single writer for a few milliseconds.
		o.BatchSize = 200
	}
	if o.FlagWindow <= 0 {
		o.FlagWindow = 1000
	}
	if o.BackfillSlice <= 0 {
		o.BackfillSlice = 5
	}
	return o
}
