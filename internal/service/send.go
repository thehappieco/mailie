package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/mime"
	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/store"
	"github.com/thehappieco/mailie/internal/workspace"
)

// Sending mail.
//
// The privacy boundary, which every function here keeps:
//
//   - Mailie sends only when asked, one request per message, and the request
//     is the confirmation: without confirm=true nothing is looked at, and no
//     flag or test relaxes that.
//   - Who may send from a mailbox is decided here, before anything is read,
//     and asked again right before every connection to the submission
//     server. A person sends from a mailbox when they hold the send flag on
//     it, signed in, and only while they allow sending in the console under
//     the current policy (SendConsent): a withdrawal stops every send that
//     has not connected yet. The message goes out under the sender's name,
//     whoever linked the mailbox. A workspace's API key sends from a mailbox
//     when it holds the send flag on it and has the send scope, on a server
//     whose keys may send (MAIL_KEYS_MAY_SEND), while it is live: the key
//     terms its creator agreed to cover it, and no person's consent is
//     asked. Its message goes out under the address alone, and its record
//     names the key. A mailbox of the operator workspace sends only for the
//     operator: an instance key with the send scope.
//   - Nothing of the message is kept. Its text lives in the request, its
//     attachments in the spool (<data>/tmp/send-*) for as long as the send
//     runs, and both are gone when it returns. The provider's Sent folder
//     keeps the message: Gmail and Microsoft file it themselves, and for a
//     generic IMAP account that asks for it Mailie adds exactly the bytes it
//     sent, once. What the send record keeps is in store.Send: ids, state, a
//     classified reason, counts and times — never a subject, an address or a
//     word of the text.
//   - The idempotency key is taken before anything is dialed, and a failure
//     once the message was on the wire is unknown, never retried: Exchange
//     delivers both copies of a retry, and only Gmail recognises the
//     Message-ID. An unknown send becomes sent when the account's Sent folder
//     shows its Message-ID, and stays unknown otherwise.
//   - Nothing is logged about a send but its account, its key, its state and
//     the class of a failure: never an address, a subject, a filename or any
//     of the text. The refused recipients go back to the caller, who wrote
//     them, and nowhere else.

// Limits of a send.
const (
	// MaxRecipients bounds to, cc and bcc together.
	MaxRecipients = 100
	// MaxSubjectBytes is RFC 5322's line limit, which a subject has to fit
	// in before it is encoded.
	MaxSubjectBytes = 998
	// MaxTextBytes bounds the plain-text body.
	MaxTextBytes = 1 << 20
	// MaxAttachments bounds the files one message carries, uploaded and
	// forwarded together.
	MaxAttachments = 100
	// DailySendLimit is how many sends one person may start in a day, all
	// their sessions together. A workspace key has its own
	// (DailyKeySendLimit); an instance key has none.
	DailySendLimit = 200
	// MaxIdempotencyKey bounds the Idempotency-Key a caller sends.
	MaxIdempotencyKey = 128

	// SendTimeout bounds a send request: the in-request retries wait up to
	// 55 s between four attempts, and each connection is bounded on its own.
	// The route carries the same number.
	SendTimeout = 3 * time.Minute
	// attemptTimeout bounds one submission: dial, authenticate, transmit.
	attemptTimeout = 60 * time.Second
	// sentCopyTimeout bounds filing the copy in Sent and marking a replied
	// message answered, which go ahead once the message is sent, whatever
	// the caller does. Within the request's own time where it has any left,
	// but never less than minAfterSend.
	sentCopyTimeout = 2 * time.Minute
	minAfterSend    = 15 * time.Second
	// answerMargin is the time kept to record and answer once the server
	// has spoken.
	answerMargin = 10 * time.Second

	// DefaultSendSpoolBytes is how much the sends in flight may hold in the
	// spool at once — their attachments, and each message as it is written
	// out for the wire — and sendsPerCaller how many one caller may have.
	DefaultSendSpoolBytes = 512 << 20
	sendsPerCaller        = 2
	sendRetryAfter        = 10 * time.Second

	// maxReferences is how many ids a reply's References keeps: the thread's
	// first message and the last twenty, as RFC 5322 suggests trimming it.
	maxReferences = 21
	// maxNameBytes bounds a display name.
	maxNameBytes = 256
	// maxAddressBytes is RFC 5321's limit on a forward path.
	maxAddressBytes = 254
)

// defaultSendRetry is how long to wait before each new attempt when the
// server said to come back later before anything was transmitted.
var defaultSendRetry = []time.Duration{5 * time.Second, 20 * time.Second, 30 * time.Second}

// Send states, as a SendResult and a SendStatus give them.
const (
	SendStateSending = store.SendSending
	SendStateSent    = store.SendSent
	SendStateFailed  = store.SendFailed
	SendStateUnknown = store.SendUnknown
)

// Reasons a send failed or ended unknown. Fixed words, never the server's.
const (
	// Failed: nothing was sent.
	SendReasonRecipientsRefused = "recipients_refused" // Rejected names them
	SendReasonTooLarge          = "too_large"
	SendReasonRefused           = "refused" // the server refused the message for good
	SendReasonAuthFailed        = "auth_failed"
	// The submission server does not let the account sign in the way it
	// signs in: SMTP AUTH turned off for the mailbox (Microsoft 365), or no
	// mechanism Mailie speaks. Authorizing again does not change it; the
	// mailbox's administrator does.
	SendReasonAuthUnsupported = "auth_unsupported"
	SendReasonNeedsReauth     = "needs_reauth"
	SendReasonRateLimited     = "rate_limited"
	SendReasonTemporary       = "temporary"
	// Unreachable: no connection, or none that could be verified and
	// encrypted.
	SendReasonUnreachable = "unreachable"
	SendReasonStopped     = "stopped" // sending was withdrawn, or the account became unusable, before dialing
	// Unknown: the message may have been delivered.
	SendReasonAfterData   = "after_data"
	SendReasonInterrupted = store.ReasonInterrupted
)

// Compose is the message a caller asks to send.
type Compose struct {
	AccountID string    `json:"account_id"`
	To        []Address `json:"to"`
	Cc        []Address `json:"cc,omitempty"`
	Bcc       []Address `json:"bcc,omitempty"`
	Subject   string    `json:"subject"`
	// Text is the plain-text body, UTF-8. There is no HTML composition.
	Text string `json:"text"`
	// InReplyTo is the local id of a message the caller may read: the new
	// one answers it, carries its thread (In-Reply-To, References) and has
	// its subject prefixed "Re: " once.
	InReplyTo int64 `json:"in_reply_to,omitempty"`
	// ForwardOf is the local id of a message the caller may read: the new
	// one forwards it and has its subject prefixed "Fwd: " once.
	ForwardOf int64 `json:"forward_of,omitempty"`
	// ForwardAttachments are parts of messages the caller may read,
	// fetched from the mail server when the message is sent.
	ForwardAttachments []ForwardAttachment `json:"forward_attachments,omitempty"`
	// Confirm must be true: the request is the confirmation that the
	// person asked for this message to be sent.
	Confirm bool `json:"confirm"`
}

// ForwardAttachment names a part of a message, as GET
// /v1/messages/{id}/attachments/{path} does.
type ForwardAttachment struct {
	MessageID int64  `json:"message_id"`
	Path      string `json:"path"`
}

// Upload is one file the caller attached. Body is read to its end, into the
// spool, before the next one is asked for.
type Upload struct {
	Filename string
	// ContentType is what the uploader declared. It is not trusted: the
	// type a file goes out with is sniffed from its bytes.
	ContentType string
	Body        io.Reader
}

// UploadSource hands the caller's files over one at a time, calling add for
// each in order and stopping at the first error add returns. Nil is a
// message without uploads.
type UploadSource func(add func(Upload) error) error

// SendRequest is one message to send.
type SendRequest struct {
	Compose Compose
	// IdempotencyKey makes a retried request safe: the same key and the same
	// message is answered from the record instead of sent again. Required
	// from a signed-in person (the console makes one per message); an API
	// key that leaves it out gets one made from the message, the key and the
	// minute.
	IdempotencyKey string
	Attachments    UploadSource
}

// SendResult is how a send ended. A request that reached the submission
// server is answered with one whatever happened there; State says what.
type SendResult struct {
	// State is sent, failed (nothing was sent; the same key may try again)
	// or unknown (the message may have been delivered: check the Sent folder
	// before sending it again).
	State string `json:"state"`
	// MessageID is the Message-ID header the message went out with,
	// without angle brackets.
	MessageID string `json:"message_id"`
	// SentAt is when the server took it, unix seconds.
	SentAt int64 `json:"sent_at,omitempty"`
	// Replayed says this answer is the record of an earlier request with the
	// same key and message: nothing was sent now.
	Replayed bool `json:"replayed"`
	// Reason is why a send failed or is unknown, as a fixed word.
	Reason string `json:"reason,omitempty"`
	// Rejected are the recipients the server refused, as the caller wrote
	// them, when Reason is recipients_refused. Never stored or logged.
	Rejected []string `json:"rejected,omitempty"`
}

// SendStatus is the record of a send, as GET /v1/sends/{key} gives it.
type SendStatus struct {
	AccountID      string `json:"account_id"`
	IdempotencyKey string `json:"idempotency_key"`
	// State is sending, sent, failed or unknown.
	State     string `json:"state"`
	MessageID string `json:"message_id"`
	Reason    string `json:"reason,omitempty"`
	Attempts  int    `json:"attempts"`
	// Recipients is how many there were; never who.
	Recipients int `json:"recipients"`
	// SentCopy is where the copy Mailie files in Sent stands: "n/a" where
	// the provider files it or the account asked for none, "pending",
	// "appended" or "failed".
	SentCopy  string `json:"sent_copy"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	SentAt    int64  `json:"sent_at,omitempty"`
}

// AccountSend says whether an account can send for this caller.
type AccountSend struct {
	Available bool `json:"available"`
	// Reason says why not: "needs_reauth", "pending_auth", "disabled",
	// "no_smtp", or "not_granted" (the caller holds no send flag on it, or
	// their credential's scope does not send). Whether they allowed sending
	// is theirs to read, from their send consent.
	Reason string `json:"reason,omitempty"`
	// FromName is the name a message from this mailbox carries next to its
	// address: the caller's own name, as in their profile, since a message
	// goes out under the name of whoever sends it. Empty for a key, which
	// sends under the address alone.
	FromName string `json:"from_name,omitempty"`
}

// Errors of the send routes.
var (
	errSendConfirm = E(CodeBadRequest,
		"sending requires confirm=true: the request is the confirmation that the person asked to send this message", nil)
	errSendOff = E(CodeConflict,
		"sending is off: you have not allowed it in the console", nil)
	errNotSendingOperator = E(CodeNotAuthorized,
		"a mailbox of the operator workspace sends only with an instance key", nil)
	errSendKeyRequired = E(CodeBadRequest,
		"an Idempotency-Key header is required: one per message, reused when the same request is retried", nil)
	errSendKeyInvalid = Ef(CodeBadRequest, nil,
		"the Idempotency-Key must be 1 to %d letters, digits, '-', '_', '.' or ':'", MaxIdempotencyKey)
	errSendInProgress = E(CodeConflict, "a send with this idempotency key is in progress", nil)
	errSendUnknown    = E(CodeConflict,
		"the outcome of the send with this idempotency key is unknown: it may have been delivered; "+
			"check the Sent folder before sending it again", nil)
	errSendKeyReused = E(CodeConflict, "this idempotency key was used for a different message", nil)
	errNoSend        = E(CodeNotFound, "no send with that idempotency key", nil)
)

// SendMessage sends one message from an account, when the caller may.
func (s *Service) SendMessage(ctx context.Context, p Principal, req SendRequest) (SendResult, error) {
	c := req.Compose
	if err := s.authorize(p, auth.ScopeSend); err != nil {
		return SendResult{}, err
	}
	if !c.Confirm {
		return SendResult{}, errSendConfirm
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeSend, c.AccountID, needCard)
	if err != nil {
		return SendResult{}, err
	}
	if err := s.maySend(ctx, p, a); err != nil {
		return SendResult{}, err
	}
	if err := readyToSend(a); err != nil {
		return SendResult{}, err
	}
	msg, err := s.checkCompose(ctx, p, c)
	if err != nil {
		return SendResult{}, err
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	switch {
	case key == "" && p.IsSession():
		return SendResult{}, errSendKeyRequired
	case key != "" && !validIdempotencyKey(key):
		return SendResult{}, errSendKeyInvalid
	}
	profile := provider.ProfileFor(a.Provider)
	mailbox, err := s.accounts.Mailbox(ctx, a.ID)
	if err != nil {
		return SendResult{}, fromMailbox(err)
	}

	// The attachments are not there yet, so the reservation starts at the
	// most they may be, and is set to what the send holds once they are.
	hold, err := s.sendSpool.take(downloadCaller(p), sendLimit(profile))
	if err != nil {
		return SendResult{}, err
	}
	defer hold.release()
	spool := &sendSpool{dir: s.spoolDir, limit: sendLimit(profile)}
	// However the send ends, what was spooled for it goes.
	defer spool.remove()
	if err := spool.take(req.Attachments); err != nil {
		return SendResult{}, err
	}
	if n := len(spool.files) + len(c.ForwardAttachments); n > MaxAttachments {
		return SendResult{}, Ef(CodeBadRequest, nil, "a message carries at most %d attachments", MaxAttachments)
	}
	size := msg.estimate(spool)
	if size > profile.SMTPMaxSize {
		return SendResult{}, Ef(CodeBadRequest, nil,
			"the message would be about %d MiB, more than the %d MiB this mailbox's provider accepts",
			size>>20, profile.SMTPMaxSize>>20)
	}
	// What it holds from here: its attachments, the forwarded parts still
	// to come, and the message written out for the wire — each about the
	// estimate at most.
	if err := hold.resize(2 * size); err != nil {
		return SendResult{}, err
	}
	hash, err := composeHash(s.sendHashKey, c, spool)
	if err != nil {
		return SendResult{}, E(CodeInternal, "preparing the message failed", err)
	}
	if key == "" {
		// A caller that retries the same message within the minute — a
		// model whose request timed out, the classic case — is answered
		// from the record rather than sending twice.
		key = autoSendKey(s.sendHashKey, p.Actor(), hash, s.now())
	}

	// Forwarded parts are fetched now, into the spool, on the account's
	// interactive connection: their bytes are part of what is sent, and
	// never kept.
	if err := s.fetchForwarded(ctx, p, msg, spool); err != nil {
		return SendResult{}, err
	}

	if wait, ok := s.pacer.take(a.ID, profile.SMTPMaxPerMinute, s.now()); !ok {
		return SendResult{}, Retryable("this mailbox is sending too fast for its provider; wait a moment", wait, nil)
	}
	messageID, err := newMessageID(a.Email)
	if err != nil {
		s.pacer.giveBack(a.ID)
		return SendResult{}, E(CodeInternal, "preparing the message failed", err)
	}
	limit, limitText := dailySendLimit(p)
	row, reserved, err := s.store.ReserveSend(ctx, store.SendReservation{
		AccountID: a.ID, Key: key, ComposeHash: hash, MessageID: messageID, Recipients: msg.recipients(),
		CreatedBy: p.Actor(), UserID: p.UserID, DailyLimit: limit,
	})
	switch {
	case errors.Is(err, store.ErrSendQuota):
		s.pacer.giveBack(a.ID)
		return SendResult{}, Retryable(limitText, time.Hour, err)
	case err != nil:
		s.pacer.giveBack(a.ID)
		return SendResult{}, E(CodeInternal, "recording the send failed", err)
	case !reserved:
		s.pacer.giveBack(a.ID)
		return replayOrRefuse(p, row, hash)
	}

	out := msg.outgoing(a, s.fromName(ctx, p), messageID, s.now(), spool)
	out.KeepCopy = appendsSentCopy(a)
	res, err := s.submit(ctx, p, a, mailbox, row, out, msg)
	if err != nil {
		return SendResult{}, err
	}
	return res, nil
}

// submit sends a reserved message, retrying what the server said to retry
// before anything was transmitted, and records how it ended.
func (s *Service) submit(ctx context.Context, p Principal, a account.Account, mailbox provider.Mailbox,
	row store.Send, out provider.Outgoing, msg *checkedCompose,
) (SendResult, error) {
	var (
		attempts int
		sent     provider.SendResult
		err      error
		// stop is why the send may no longer go, and dialed whether this
		// attempt connected before it was known.
		stop   error
		dialed bool
	)
	// Asked again right before every connection, not only when the request
	// was accepted: a withdrawal that committed meanwhile — or an account
	// that stopped being usable — stops the send before it connects. The
	// adapter asks, after the send has waited its turn behind the account's
	// others, which can take minutes; asked here, before the wait, a
	// withdrawal during it would let the send through. Called on the
	// attempt's goroutine, so stop is read below without a lock.
	out.BeforeDial = func(ctx context.Context) error {
		// On a context of its own: the attempt's may have run down while
		// the send waited its turn, and a refusal to read the account is
		// not an answer.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexTimeout)
		defer cancel()
		stop = s.stillMaySend(checkCtx, p, a)
		dialed = dialed || stop == nil
		return stop
	}
	for {
		attempts++
		dialed = false
		sent, err = s.attempt(ctx, mailbox, out)
		if stop != nil {
			if !dialed {
				attempts--
			}
			s.finishSend(ctx, row, store.SendOutcome{State: store.SendFailed, Reason: SendReasonStopped, Attempts: attempts})
			return SendResult{}, stop
		}
		if err == nil || !retryableSend(err) || attempts > len(s.sendRetry) {
			break
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < s.sendRetry[attempts-1]+attemptTimeout+answerMargin {
			// Another attempt could outlast the request, and then its
			// outcome would reach nobody.
			break
		}
		s.log.Info("the submission server asked to try again later", "account", a.ID, "send_key", row.Key,
			"attempt", attempts, "class", provider.Class(err))
		wait := time.NewTimer(s.sendRetry[attempts-1])
		select {
		case <-ctx.Done():
			wait.Stop()
		case <-wait.C:
		}
		if ctx.Err() != nil {
			// The caller left, or the route's time ran out, between two
			// attempts. Nothing was transmitted.
			break
		}
	}

	if err != nil {
		state, reason := sendFailure(err)
		finished := s.finishSend(ctx, row, store.SendOutcome{State: state, Reason: reason, Attempts: attempts})
		if finished.State == store.SendSent {
			// Unknown to the submission, but the Sent folder already shows
			// it: the provider took the message.
			s.log.Info("message sent", "account", a.ID, "send_key", row.Key, "attempts", attempts, "reconciled", true)
			return presentSend(finished, false), nil
		}
		if state == store.SendUnknown {
			s.log.Warn("the send's outcome is unknown; it will not be retried", "account", a.ID,
				"provider", a.ProviderName(), "send_key", row.Key, "attempts", attempts, "class", provider.Class(err))
			// The next pass of the Sent folder settles it where the
			// provider files a copy.
			s.afterAction(ctx, a.ID)
		} else {
			s.log.Warn("sending failed; nothing was sent", "account", a.ID, "provider", a.ProviderName(),
				"send_key", row.Key, "attempts", attempts, "reason", reason, "class", provider.Class(err))
		}
		res := presentSend(finished, false)
		var refused *provider.RecipientError
		if errors.As(err, &refused) {
			res.Rejected = msg.rejected(refused)
		}
		return res, nil
	}

	if sent.Copy != nil {
		defer sent.Copy.Discard()
	}
	copyState := store.SentCopyNone
	if out.KeepCopy {
		copyState = store.SentCopyPending
	}
	finished := s.finishSend(ctx, row, store.SendOutcome{
		State: store.SendSent, Attempts: attempts, SentCopy: copyState,
	})
	s.log.Info("message sent", "account", a.ID, "send_key", row.Key, "attempts", attempts)
	s.afterSend(ctx, p, a, row, out, sent, msg, copyState == store.SentCopyPending)
	return presentSend(finished, false), nil
}

// attempt is one submission, on a context of its own: once the server is
// dialed the caller leaving changes nothing about what the server does, and
// the outcome has to be heard to be recorded.
func (s *Service) attempt(ctx context.Context, mailbox provider.Mailbox, out provider.Outgoing) (provider.SendResult, error) {
	wire, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptTimeout)
	defer cancel()
	return mailbox.Sender().Send(wire, out)
}

// finishSend records how a send ended and publishes send.finished. It goes
// ahead when the caller has gone: the record is what a retry of the request
// and the console's status read.
func (s *Service) finishSend(ctx context.Context, row store.Send, o store.SendOutcome) store.Send {
	o.AccountID, o.Key = row.AccountID, row.Key
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexTimeout)
	defer cancel()
	finished, evs, err := s.store.FinishSend(wctx, o)
	if err != nil {
		// The row stays sending, and the next start makes it unknown: never
		// failed, which would let the key send again.
		s.log.Error("recording how a send ended failed", "account", row.AccountID, "send_key", row.Key,
			"state", o.State, "err", err)
		row.State, row.Reason, row.Attempts = o.State, o.Reason, o.Attempts
		return row
	}
	if s.bus != nil {
		s.bus.Publish(evs...)
	}
	return finished
}

// afterSend is what follows a message the server took: the copy in Sent for
// an account whose provider does not file one, the replied message marked
// answered when the sender may act there (markAnswered), and a pass so the
// index sees the copy. None of it changes that the message was sent, so failures are logged
// and the result stands.
func (s *Service) afterSend(ctx context.Context, p Principal, a account.Account, row store.Send,
	out provider.Outgoing, sent provider.SendResult, msg *checkedCompose, fileCopy bool,
) {
	budget := sentCopyTimeout
	if deadline, ok := ctx.Deadline(); ok {
		// Within what is left of the request where it can be, so the answer
		// that the message was sent still reaches the caller.
		budget = min(budget, max(time.Until(deadline)-answerMargin, minAfterSend))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	if fileCopy {
		state := s.fileSentCopy(ctx, a, row, out, sent)
		if err := s.store.SetSentCopy(ctx, a.ID, row.Key, state); err != nil {
			s.log.Warn("recording the copy in Sent failed", "account", a.ID, "send_key", row.Key, "err", err)
		}
	}
	if msg.parent != nil {
		s.markAnswered(ctx, p, msg.parentAccount, *msg.parent)
	}
	s.afterAction(ctx, a.ID)
}

// appendsSentCopy reports whether Mailie files the copy of a sent message in
// the account's Sent folder: only on a generic IMAP account that asks for it.
// Gmail and Microsoft file the copy themselves, and a second one is the
// classic duplicate — whatever the account row says.
func appendsSentCopy(a account.Account) bool {
	return a.Provider == provider.KindIMAP && a.SaveSentCopy
}

// fileSentCopy adds the copy the adapter kept — exactly the bytes sent, with
// the Bcc header the wire never carries — to the account's Sent folder,
// unless the folder already holds the Message-ID: the server filed it after
// all, or an earlier copy made it. It returns where the copy stands. A server
// that turns out to be Gmail's gets nothing.
func (s *Service) fileSentCopy(ctx context.Context, a account.Account, row store.Send, out provider.Outgoing,
	sent provider.SendResult,
) string {
	if sent.Copy == nil {
		s.log.Warn("the message was sent but no copy of it was kept for Sent", "account", a.ID,
			"provider", a.ProviderName(), "send_key", row.Key)
		return store.SentCopyFailed
	}
	state := store.SentCopyFailed
	err := s.onServer(ctx, a.ID, func(ctx context.Context, sess provider.Session) error {
		if provider.IsGmailServer(sess.Caps(), a.IMAPHost) {
			state = store.SentCopyNone
			return nil
		}
		folder, err := s.sentFolder(ctx, a, sess)
		if err != nil {
			return err
		}
		if _, err := sess.Select(ctx, folder, true, 0); err != nil {
			return err
		}
		found, err := sess.SearchMessageID(ctx, out.MessageID)
		if err != nil {
			return err
		}
		if len(found) > 0 {
			state = store.SentCopyAppended
			return nil
		}
		// Opened for each try: a retried APPEND starts from the first byte.
		body, err := sent.Copy.Open()
		if err != nil {
			return err
		}
		//nolint:errcheck // a file only read
		defer func() { _ = body.Close() }()
		if _, err := sess.Append(ctx, folder, body, sent.Copy.Size, []imap.Flag{imap.FlagSeen}, out.Date); err != nil {
			return err
		}
		state = store.SentCopyAppended
		return nil
	})
	if err != nil {
		s.log.Warn("filing the copy in Sent failed; the message was sent", "account", a.ID,
			"provider", a.ProviderName(), "send_key", row.Key, "class", provider.Class(err))
	}
	return state
}

// errNoSentFolder is an account whose Sent folder cannot be found.
var errNoSentFolder = fmt.Errorf("%w: no sent folder", provider.ErrFolderNotFound)

// sentFolder is the name of the account's Sent folder: the index's, when it
// has listed the folders, and otherwise what the server lists now, with the
// roles resolved as the index would.
func (s *Service) sentFolder(ctx context.Context, a account.Account, sess provider.Session) (string, error) {
	if s.store != nil {
		if enabled, err := s.syncEnabled(ctx, a); err == nil && enabled {
			if folders, err := s.actionFolders(ctx, a); err == nil {
				if f := roleFolder(folders, provider.RoleSent, false); f != nil {
					return f.Name, nil
				}
			}
		}
	}
	listed, err := sess.ListFolders(ctx, false)
	if err != nil {
		return "", err
	}
	profile := provider.ProfileFor(a.Provider).ForServer(sess.Caps(), a.IMAPHost)
	for _, f := range listed {
		if role, _ := provider.ResolveRole(f, profile, a.FolderOverrides); role == provider.RoleSent && f.Selectable {
			return f.Name, nil
		}
	}
	return "", errNoSentFolder
}

// markAnswered sets \Answered on the message a reply answers, when the caller
// may change that mailbox (mayAct: their act flag and their own actions
// consent, or a key's) and its index can follow; otherwise nothing, and
// nobody is told: the reply was what was asked for.
func (s *Service) markAnswered(ctx context.Context, p Principal, a account.Account, parent store.MessageRow) {
	if err := s.mayAct(ctx, p, a); err != nil {
		return
	}
	if err := s.indexFollows(ctx, a); err != nil {
		return
	}
	row, err := s.store.Message(ctx, parent.ID)
	if err != nil || row.Vanished || row.Stale || row.Answered {
		return
	}
	if err := s.changeFlags(ctx, p, a, []actionTarget{{row: row}}, []imap.Flag{imap.FlagAnswered}, nil); err != nil {
		s.log.Warn("marking the replied message answered failed", "account", a.ID, "message", row.ID,
			"class", provider.Class(err))
	}
}

// SendStatus reads the record of one of the caller's own sends from an
// account they may send from: whether it was sent, failed, or is still
// unknown. Several people and keys may send from one shared mailbox, and
// each reads only their own records — a key its own, an instance key the
// operator's.
func (s *Service) SendStatus(ctx context.Context, p Principal, accountID, key string) (SendStatus, error) {
	if err := s.authorize(p, auth.ScopeSend); err != nil {
		return SendStatus{}, err
	}
	a, err := s.authorizeAccount(ctx, p, auth.ScopeSend, accountID, needCard)
	if err != nil {
		return SendStatus{}, err
	}
	if err := s.maySendFrom(ctx, p, a); err != nil {
		return SendStatus{}, err
	}
	if key == "" || len(key) > 2*MaxIdempotencyKey {
		return SendStatus{}, errNoSend
	}
	row, err := s.store.SendOf(ctx, a.ID, key)
	switch {
	case errors.Is(err, store.ErrNoSend):
		return SendStatus{}, errNoSend
	case err != nil:
		return SendStatus{}, E(CodeInternal, "reading the send failed", err)
	case !ownsSend(p, row.UserID, row.CreatedBy):
		// Somebody else's send from the same mailbox: not this caller's to
		// read, and answered as a key nobody used.
		return SendStatus{}, errNoSend
	}
	return presentSendStatus(row), nil
}

func presentSendStatus(row store.Send) SendStatus {
	return SendStatus{
		AccountID: row.AccountID, IdempotencyKey: row.Key, State: row.State, MessageID: row.MessageID,
		Reason: row.Reason, Attempts: row.Attempts, Recipients: row.Recipients, SentCopy: row.SentCopy,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, SentAt: row.SentAt,
	}
}

// ownsSend reports whether a send record, by who it names, is the caller's
// own: a person's, signed in; a workspace key's own; the operator's for an
// instance key — the only one that sends from an operator mailbox, the only
// mailboxes it sees.
func ownsSend(p Principal, userID, createdBy string) bool {
	switch {
	case p.IsSession():
		return userID != "" && userID == p.UserID
	case p.IsInstance():
		return userID == ""
	}
	return userID == "" && createdBy == p.Actor()
}

// dailySendLimit is how many sends the caller may start in a day, and the
// words that say so: a person's, all their sessions together; a workspace
// key's own; none for an instance key, the operator's.
func dailySendLimit(p Principal) (int, string) {
	switch {
	case p.IsSession():
		return DailySendLimit, fmt.Sprintf("a person may send at most %d messages a day; try again later", DailySendLimit)
	case p.IsInstance():
		return 0, ""
	}
	return DailyKeySendLimit, fmt.Sprintf("an API key may send at most %d messages a day; try again later",
		DailyKeySendLimit)
}

// SendBodyLimit is the most a send request's body may hold: the largest
// message any provider accepts, the text, and room for the multipart framing.
// At least 40 MiB.
func SendBodyLimit() int64 {
	largest := int64(0)
	for _, kind := range []provider.Kind{provider.KindGmail, provider.KindMicrosoft, provider.KindIMAP} {
		largest = max(largest, provider.ProfileFor(kind).SMTPMaxSize)
	}
	return max(40<<20, largest+MaxTextBytes+4<<20)
}

// maySend decides whether the caller may send from this account, once it is
// known they may see it: the sender rule; for a person their own consent to
// sending under the current policy; for a workspace key that it is still
// live, its key terms covering what it does. Asked when a send is accepted
// and again before every connection (stillMaySend).
func (s *Service) maySend(ctx context.Context, p Principal, a account.Account) error {
	if err := s.maySendFrom(ctx, p, a); err != nil {
		return err
	}
	if p.IsInstance() {
		return nil
	}
	if p.IsWorkspaceKey() {
		return s.keyStillLive(ctx, p)
	}
	c, err := s.store.SendConsentOf(ctx, p.UserID)
	switch {
	case errors.Is(err, store.ErrNoSuchUser):
		return errSendOff
	case err != nil:
		return E(CodeInternal, "reading the send consent failed", err)
	case c.At == 0 || c.Version != s.consent.Send:
		return errSendOff
	}
	return nil
}

// maySendFrom is the sender rule. A person sends from a mailbox when they
// hold the send flag on it — whoever linked it, whatever their role. A
// workspace key sends from a mailbox when it holds the send flag on it, on a
// server whose keys may send; a person's key the upgrade to workspace keys
// carried over never sends. A mailbox of the operator workspace sends with an
// instance key, the only credential that sees it. The send scope is
// authorize's.
func (s *Service) maySendFrom(ctx context.Context, p Principal, a account.Account) error {
	operator := a.WorkspaceID == workspace.OperatorID
	if p.IsInstance() || operator {
		if p.IsInstance() && operator {
			return nil
		}
		// Unreachable while visibility holds.
		return errNotSendingOperator
	}
	if p.IsWorkspaceKey() && (s.keysMayNotSend || earlierKey(p)) {
		return errKeysMayNotSend
	}
	return s.requireFlags(ctx, p, a, needSend)
}

// stillMaySend asks again, right before a connection to the submission
// server, what SendMessage asked when it accepted the send: the account is
// still one the caller may see and use, and they may still send from it.
func (s *Service) stillMaySend(ctx context.Context, p Principal, a account.Account) error {
	if err := s.authorize(p, auth.ScopeSend); err != nil {
		return err
	}
	now, err := s.accounts.Repo().GetVisible(ctx, a.ID, visibility(p))
	switch {
	case errors.Is(err, account.ErrNotFound):
		return E(CodeNotFound, "no such account", err)
	case err != nil:
		return E(CodeInternal, "reading the account failed", err)
	}
	if err := readyToSend(now); err != nil {
		return err
	}
	return s.maySend(ctx, p, now)
}

// readyToSend refuses, before anything is dialed, an account that cannot
// send as it stands.
func readyToSend(a account.Account) error {
	if a.SMTPHost == "" {
		return E(CodeConflict, "this account has no submission server", nil)
	}
	return readyToRead(a)
}

// sendOf says whether an account can send for a caller holding these flags
// on it, their credential's scope applied.
func sendOf(a account.Account, held workspace.Flags) AccountSend {
	switch {
	case a.SMTPHost == "":
		return AccountSend{Reason: "no_smtp"}
	case a.State == account.StateNeedsReauth:
		return AccountSend{Reason: "needs_reauth"}
	case a.State == account.StatePendingAuth:
		return AccountSend{Reason: "pending_auth"}
	case a.State == account.StateDisabled:
		return AccountSend{Reason: "disabled"}
	case !held.Send:
		return AccountSend{Reason: "not_granted"}
	}
	return AccountSend{Available: true}
}

// fromName is the name a message goes out under: the sender's own name from
// their profile. Not the linker's — on a shared mailbox the recipient should
// see who wrote — and not the account's display name, which is a label a
// person gave the mailbox in the console ("gmail", "work") and was never
// meant for recipients. A key, which acts as no person, a name that cannot be
// read, or one that could not go in a header sends under the address alone.
func (s *Service) fromName(ctx context.Context, p Principal) string {
	if !p.IsSession() || p.UserID == "" || s.users == nil {
		return ""
	}
	user, err := s.users.Get(ctx, p.UserID)
	if err != nil {
		s.log.Warn("reading the sender's name failed; sending under the address alone", "user", p.UserID, "err", err)
		return ""
	}
	name := strings.TrimSpace(user.Name)
	if name == "" || strings.ContainsFunc(name, unicode.IsControl) {
		return ""
	}
	return name
}

// replayOrRefuse answers a request whose key another request holds. A key
// another sender holds on the same mailbox is refused as a key reused for
// another message, never replayed: the answer would be their send.
func replayOrRefuse(p Principal, row store.Send, hash string) (SendResult, error) {
	switch {
	case !ownsSend(p, row.UserID, row.CreatedBy), row.ComposeHash != hash:
		return SendResult{}, errSendKeyReused
	case row.State == store.SendSent:
		return presentSend(row, true), nil
	case row.State == store.SendUnknown:
		return SendResult{}, errSendUnknown
	default:
		return SendResult{}, errSendInProgress
	}
}

func presentSend(row store.Send, replayed bool) SendResult {
	return SendResult{
		State: row.State, MessageID: row.MessageID, SentAt: row.SentAt, Replayed: replayed, Reason: row.Reason,
	}
}

// retryableSend reports whether a failed attempt may be tried again: the
// server said later, or could not be reached, and nothing was transmitted.
func retryableSend(err error) bool {
	if errors.Is(err, provider.ErrOutcomeUnknown) || errors.Is(err, provider.ErrAfterData) {
		// The message was on the wire: whatever the server said, it is not
		// sent again by itself.
		return false
	}
	return errors.Is(err, provider.ErrRateLimited) || errors.Is(err, provider.ErrTemporary) ||
		errors.Is(err, provider.ErrTooManyConnections) || errors.Is(err, provider.ErrConnClosed)
}

// sendFailure classifies a submission that did not succeed.
func sendFailure(err error) (state, reason string) {
	var refused *provider.RecipientError
	switch {
	case errors.Is(err, provider.ErrOutcomeUnknown):
		return store.SendUnknown, SendReasonAfterData
	case errors.As(err, &refused):
		return store.SendFailed, SendReasonRecipientsRefused
	case errors.Is(err, provider.ErrTooLarge):
		return store.SendFailed, SendReasonTooLarge
	case errors.Is(err, provider.ErrNeedsReauth):
		return store.SendFailed, SendReasonNeedsReauth
	case errors.Is(err, provider.ErrAuthFailed):
		return store.SendFailed, SendReasonAuthFailed
	case errors.Is(err, provider.ErrAuthUnsupported):
		return store.SendFailed, SendReasonAuthUnsupported
	case errors.Is(err, provider.ErrInsecure):
		return store.SendFailed, SendReasonUnreachable
	case errors.Is(err, provider.ErrRateLimited), errors.Is(err, provider.ErrTooManyConnections):
		return store.SendFailed, SendReasonRateLimited
	case errors.Is(err, provider.ErrConnClosed):
		return store.SendFailed, SendReasonUnreachable
	case errors.Is(err, provider.ErrTerminal), errors.Is(err, provider.ErrUnsupported):
		return store.SendFailed, SendReasonRefused
	}
	return store.SendFailed, SendReasonTemporary
}

// checkedCompose is a Compose that passed every check, with what its checks
// looked up.
type checkedCompose struct {
	to, cc, bcc []provider.Address
	// given maps each recipient address, lowercased, to how the caller
	// wrote it.
	given   map[string]string
	subject string
	text    string
	// parent is the message a reply answers, in parentAccount.
	parent        *store.MessageRow
	parentAccount account.Account
	forwards      []forwardPart
}

type forwardPart struct {
	account account.Account
	row     store.MessageRow
	info    provider.PartInfo
	// file is where it was spooled, once fetched.
	file *spooledFile
}

// checkCompose validates a compose and looks up what it refers to.
func (s *Service) checkCompose(ctx context.Context, p Principal, c Compose) (*checkedCompose, error) {
	out := &checkedCompose{given: map[string]string{}}
	var err error
	if out.to, err = s.checkAddresses("to", c.To, out.given); err != nil {
		return nil, err
	}
	if out.cc, err = s.checkAddresses("cc", c.Cc, out.given); err != nil {
		return nil, err
	}
	if out.bcc, err = s.checkAddresses("bcc", c.Bcc, out.given); err != nil {
		return nil, err
	}
	if n := len(out.to) + len(out.cc) + len(out.bcc); n < 1 || n > MaxRecipients {
		return nil, Ef(CodeBadRequest, nil, "a message has between 1 and %d recipients in to, cc and bcc", MaxRecipients)
	}
	if strings.ContainsAny(c.Subject, "\r\n") {
		return nil, E(CodeBadRequest, "the subject cannot contain a line break", nil)
	}
	if !utf8.ValidString(c.Subject) || hasControl(c.Subject) {
		return nil, E(CodeBadRequest, "the subject must be UTF-8 text without control characters", nil)
	}
	if len(c.Text) > MaxTextBytes {
		return nil, Ef(CodeBadRequest, nil, "the text is longer than %d KiB", MaxTextBytes>>10)
	}
	if !utf8.ValidString(c.Text) {
		return nil, E(CodeBadRequest, "the text must be UTF-8", nil)
	}
	out.text = c.Text
	if c.InReplyTo != 0 && c.ForwardOf != 0 {
		return nil, E(CodeBadRequest, "a message answers another or forwards it, not both", nil)
	}

	subject := strings.TrimSpace(c.Subject)
	switch {
	case c.InReplyTo != 0:
		a, row, err := s.readableMessage(ctx, p, c.InReplyTo)
		if err != nil {
			return nil, err
		}
		out.parent, out.parentAccount = &row, a
		subject = prefixOnce(orSubject(subject, row.Subject), "Re: ", "re:")
	case c.ForwardOf != 0:
		_, row, err := s.readableMessage(ctx, p, c.ForwardOf)
		if err != nil {
			return nil, err
		}
		subject = prefixOnce(orSubject(subject, row.Subject), "Fwd: ", "fwd:", "fw:")
	}
	if len(subject) > MaxSubjectBytes {
		return nil, Ef(CodeBadRequest, nil, "the subject is longer than %d bytes", MaxSubjectBytes)
	}
	out.subject = subject

	for i, fa := range c.ForwardAttachments {
		a, row, info, err := s.attachmentPart(ctx, p, fa.MessageID, fa.Path)
		if err != nil {
			var known *Error
			if errors.As(err, &known) && known.Code == CodeBadRequest {
				return nil, Ef(CodeBadRequest, err, "forward_attachments[%d]: %s", i, known.Message)
			}
			return nil, err
		}
		if info.Size > attachmentCap(a) {
			return nil, Ef(CodeBadRequest, nil, "forward_attachments[%d] is larger than %d MiB",
				i, attachmentCap(a)>>20)
		}
		out.forwards = append(out.forwards, forwardPart{account: a, row: row, info: info})
	}
	return out, nil
}

// checkAddresses validates one of to, cc and bcc. An address is a bare
// addr-spec, as net/mail reads it; a name is text without line breaks or
// control characters. Errors name the position, never the address: they can
// reach a log.
func (s *Service) checkAddresses(field string, in []Address, given map[string]string) ([]provider.Address, error) {
	out := make([]provider.Address, 0, len(in))
	for i, a := range in {
		email := strings.TrimSpace(a.Email)
		parsed, err := mail.ParseAddress(email)
		if email == "" || len(email) > maxAddressBytes || err != nil || parsed.Name != "" || parsed.Address != email {
			return nil, Ef(CodeBadRequest, nil, "%s[%d] is not a valid email address", field, i)
		}
		name := strings.TrimSpace(a.Name)
		if len(name) > maxNameBytes || !utf8.ValidString(name) || hasControl(name) {
			return nil, Ef(CodeBadRequest, nil, "%s[%d] has a name that is too long or holds control characters",
				field, i)
		}
		given[strings.ToLower(email)] = email
		out = append(out, provider.Address{Name: name, Email: email})
	}
	return out, nil
}

// hasControl reports whether s holds a control character: a line break, a
// tab, NUL, DEL. None belongs in a header.
func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// orSubject is the caller's subject, or the original's when the caller left
// it empty, with any line break of the original's flattened: it came from the
// index, not from the caller.
func orSubject(given, original string) string {
	if given != "" {
		return given
	}
	return strings.Join(strings.FieldsFunc(original, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
}

// prefixOnce puts prefix in front of a subject that does not already start
// with one of the forms in already (compared without case).
func prefixOnce(subject, prefix string, already ...string) string {
	lower := strings.ToLower(strings.TrimSpace(subject))
	for _, a := range already {
		if strings.HasPrefix(lower, a) {
			return subject
		}
	}
	return prefix + subject
}

// recipients is how many distinct recipients the message has.
func (m *checkedCompose) recipients() int { return len(m.given) }

// rejected are the recipients a server refused, as the caller wrote them.
// Anything the server named that the caller did not write is left out.
func (m *checkedCompose) rejected(err *provider.RecipientError) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range err.Rejected {
		key := strings.ToLower(strings.TrimSpace(r.Address))
		if given, ok := m.given[key]; ok && !seen[key] {
			seen[key] = true
			out = append(out, given)
		}
	}
	return out
}

// estimate is roughly how large the message will be once built: the text as
// quoted-printable mostly leaves it, each attachment in base64 with its line
// breaks, a forwarded part as the server holds it (already encoded), and room
// for headers. The adapter checks the real size before dialing; this refuses
// the obvious cases before anything is reserved.
func (m *checkedCompose) estimate(spool *sendSpool) int64 {
	size := int64(len(m.text))*21/20 + 8<<10
	for _, f := range spool.files {
		size += f.size*4/3*78/76 + 1<<10
	}
	for _, f := range m.forwards {
		size += f.info.Size + 1<<10
	}
	return size
}

// outgoing builds what the adapter sends. From is the account's address and
// its owner's name (fromName), never anything the caller wrote.
func (m *checkedCompose) outgoing(a account.Account, fromName, messageID string, now time.Time, spool *sendSpool) provider.Outgoing {
	out := provider.Outgoing{
		From:      provider.Address{Name: fromName, Email: a.Email},
		To:        m.to,
		Cc:        m.cc,
		Bcc:       m.bcc,
		Subject:   m.subject,
		TextBody:  m.text,
		MessageID: messageID,
		Date:      now,
	}
	if m.parent != nil {
		out.InReplyTo, out.References = threadOf(*m.parent)
	}
	for _, f := range spool.files {
		out.Attachments = append(out.Attachments, f.attachment())
	}
	for _, f := range m.forwards {
		if f.file != nil {
			out.Attachments = append(out.Attachments, f.file.attachment())
		}
	}
	return out
}

// threadOf is what a reply to row carries: In-Reply-To, its Message-ID, and
// References, its References and its Message-ID, trimmed to the first and the
// last twenty. Every id is bare; the adapter adds the brackets, once.
func threadOf(row store.MessageRow) (inReplyTo string, references []string) {
	parent := cleanID(row.MessageID)
	for _, ref := range row.References {
		for _, id := range strings.Fields(ref) {
			if id = cleanID(id); id != "" {
				references = append(references, id)
			}
		}
	}
	if parent != "" {
		references = append(references, parent)
	}
	if len(references) > maxReferences {
		references = append([]string{references[0]}, references[len(references)-(maxReferences-1):]...)
	}
	return parent, references
}

// cleanID is a message id as it can go in a header: bare, without the
// brackets or whitespace an index row could carry, and without anything that
// would let it break out of one.
func cleanID(id string) string {
	id = store.BareID(id)
	if id == "" || strings.ContainsAny(id, "<> \t\r\n") || hasControl(id) {
		return ""
	}
	return id
}

// fetchForwarded fetches the forwarded parts into the spool, decoded, on
// their accounts' interactive connections.
func (s *Service) fetchForwarded(ctx context.Context, p Principal, m *checkedCompose, spool *sendSpool) error {
	for i := range m.forwards {
		f := &m.forwards[i]
		d, err := s.fetchAttachment(ctx, p, f.account, f.row, f.info)
		if err != nil {
			return err
		}
		file, err := spool.add(Upload{Filename: mime.DownloadName(f.info), ContentType: d.ContentType, Body: d.Body})
		//nolint:errcheck // closing removes the provider's spooled section
		_ = d.Body.Close()
		if err != nil {
			return err
		}
		f.file = file
	}
	return nil
}

// composeHash is the identity of a message for its idempotency key: every
// field of the compose, and each attachment by name, type, size and SHA-256.
//
// An HMAC under key, not a plain hash. The record keeps it, and a caller
// without an Idempotency-Key gets a key made from it, which goes in the log
// and back through GET /v1/sends: a plain SHA-256 of a deterministic encoding
// would let whoever holds the database, a backup or the logs confirm a guess
// of a message — a one-word reply, a templated notice — offline. Replays and
// conflicts still match, under the same key.
func composeHash(key []byte, c Compose, spool *sendSpool) (string, error) {
	type attachment struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		SHA256      string `json:"sha256"`
	}
	canonical := struct {
		AccountID          string              `json:"account_id"`
		To                 []Address           `json:"to"`
		Cc                 []Address           `json:"cc"`
		Bcc                []Address           `json:"bcc"`
		Subject            string              `json:"subject"`
		Text               string              `json:"text"`
		InReplyTo          int64               `json:"in_reply_to"`
		ForwardOf          int64               `json:"forward_of"`
		ForwardAttachments []ForwardAttachment `json:"forward_attachments"`
		Attachments        []attachment        `json:"attachments"`
	}{
		AccountID: c.AccountID, To: trimAddresses(c.To), Cc: trimAddresses(c.Cc), Bcc: trimAddresses(c.Bcc),
		Subject: c.Subject, Text: c.Text, InReplyTo: c.InReplyTo, ForwardOf: c.ForwardOf,
		ForwardAttachments: c.ForwardAttachments,
	}
	for _, f := range spool.files {
		canonical.Attachments = append(canonical.Attachments, attachment{
			Filename: f.filename, ContentType: f.contentType, Size: f.size, SHA256: f.sha256,
		})
	}
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// autoSendKey is the idempotency key of a send that came without one: the
// message, its sender and the minute, keyed as the compose hash is. The
// sender is in it because the record is per mailbox and the key holds the
// row: two tools whose keys send the same alert from one mailbox in the same
// minute each send theirs, where one key made of the message alone would
// refuse the second as a key reused — sending nothing, and telling it that
// another sender had just sent exactly that.
func autoSendKey(key []byte, actor, hash string, now time.Time) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("auto-send-key\x00" + actor + "\x00" + hash))
	return hex.EncodeToString(mac.Sum(nil)) + "/" + strconv.FormatInt(now.Unix()/60, 10)
}

func trimAddresses(in []Address) []Address {
	out := make([]Address, 0, len(in))
	for _, a := range in {
		out = append(out, Address{Name: strings.TrimSpace(a.Name), Email: strings.TrimSpace(a.Email)})
	}
	return out
}

// validIdempotencyKey accepts what a client should send: a UUID, or anything
// of the same alphabet.
func validIdempotencyKey(key string) bool {
	if len(key) > MaxIdempotencyKey {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ':':
		default:
			return false
		}
	}
	return key != ""
}

// newMessageID is a Message-ID for a message from address: a random UUID at
// the sender's domain, bare.
func newMessageID(address string) (string, error) {
	_, domain, ok := strings.Cut(address, "@")
	if !ok || domain == "" {
		return "", errors.New("service: the account's address has no domain")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:] + "@" + strings.ToLower(domain), nil
}

// sendLimit is the most a send's attachments may hold in the spool: the
// provider's submission limit, which their encoding only makes harder to
// fit.
func sendLimit(profile provider.Profile) int64 { return profile.SMTPMaxSize }

// sendSpool holds a send's attachments on disk for as long as the send runs.
type sendSpool struct {
	dir   string
	limit int64
	total int64
	// files are the caller's uploads, in order.
	files []*spooledFile
	// created is every file written, uploads and forwarded parts: what
	// remove deletes.
	created []*spooledFile
}

type spooledFile struct {
	path        string
	filename    string
	contentType string
	size        int64
	sha256      string
}

func (f *spooledFile) attachment() provider.OutgoingAttachment {
	path := f.path
	return provider.OutgoingAttachment{
		Filename: f.filename, ContentType: f.contentType,
		// The spool file this send made, never a name from outside.
		Open: func() (io.ReadCloser, error) { return os.Open(path) }, //nolint:gosec // G304: see above
	}
}

// take spools every upload the source hands over.
func (sp *sendSpool) take(src UploadSource) error {
	if src == nil {
		return nil
	}
	return src(func(u Upload) error {
		f, err := sp.add(u)
		if err != nil {
			return err
		}
		sp.files = append(sp.files, f)
		if len(sp.files) > MaxAttachments {
			return Ef(CodeBadRequest, nil, "a message carries at most %d attachments", MaxAttachments)
		}
		return nil
	})
}

// add writes one file to the spool, 0600, hashing it on the way, sniffing
// its type from its first bytes and learning whether it is UTF-8 text all
// the way through.
func (sp *sendSpool) add(u Upload) (*spooledFile, error) {
	dir := sp.dir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, E(CodeInternal, "preparing the attachments failed", err)
	}
	file, err := os.CreateTemp(dir, "send-*")
	if err != nil {
		return nil, E(CodeInternal, "preparing the attachments failed", err)
	}
	f := &spooledFile{path: file.Name()}
	sp.created = append(sp.created, f)

	src := &sourceReader{r: u.Body}
	var head bytes.Buffer
	var text mime.UTF8Validator
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, h, &headWriter{buf: &head}, &text),
		io.LimitReader(src, sp.limit-sp.total+1))
	if cerr := file.Close(); err == nil && cerr != nil {
		err = cerr
	}
	switch {
	case src.err != nil:
		var known *Error
		if errors.As(src.err, &known) {
			return nil, src.err
		}
		return nil, E(CodeBadRequest, "reading the attachments failed", src.err)
	case err != nil:
		return nil, E(CodeInternal, "preparing the attachments failed", err)
	}
	sp.total += n
	if sp.total > sp.limit {
		return nil, Ef(CodeBadRequest, nil, "the attachments are larger than the %d MiB this mailbox's provider accepts",
			sp.limit>>20)
	}
	f.size = n
	f.sha256 = hex.EncodeToString(h.Sum(nil))
	f.contentType = mime.UploadContentType(u.ContentType, head.Bytes(), text.Valid())
	f.filename = mime.UploadFilename(u.Filename, f.contentType)
	return f, nil
}

// remove deletes everything spooled for the send.
func (sp *sendSpool) remove() {
	for _, f := range sp.created {
		//nolint:errcheck // a file already gone is what was wanted
		_ = os.Remove(f.path)
	}
}

// sourceReader remembers an error of the reader it wraps, so a failure to
// read the caller's upload is told from a failure to write the spool.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// headWriter keeps the first bytes written through it, for sniffing.
type headWriter struct{ buf *bytes.Buffer }

func (w *headWriter) Write(p []byte) (int, error) {
	if room := mime.SniffBytes - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// sendPacer keeps each account's submissions under its provider's rate, over
// the last minute.
type sendPacer struct {
	mu     sync.Mutex
	recent map[string][]time.Time
}

func newSendPacer() *sendPacer { return &sendPacer{recent: map[string][]time.Time{}} }

// take admits one send for the account, or says how long to wait.
func (p *sendPacer) take(accountID string, perMinute int, now time.Time) (time.Duration, bool) {
	if perMinute <= 0 {
		return 0, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.recent[accountID][:0]
	for _, t := range p.recent[accountID] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= perMinute {
		p.recent[accountID] = kept
		return max(kept[0].Add(time.Minute).Sub(now), time.Second), false
	}
	p.recent[accountID] = append(kept, now)
	return 0, true
}

// giveBack returns the last send taken for the account: it was answered from
// the record, or refused, and sent nothing.
func (p *sendPacer) giveBack(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if list := p.recent[accountID]; len(list) > 0 {
		p.recent[accountID] = list[:len(list)-1]
	}
}
