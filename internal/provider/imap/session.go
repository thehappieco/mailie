// Package imap drives go-imap and go-mail on behalf of the rest of the server.
//
// It is the only package allowed to import either — the linter enforces it —
// because both have edges that are easy to get wrong in ways nothing catches
// until mail goes missing:
//
//   - In go-imap, whether a command addresses UIDs or sequence numbers is
//     decided by the dynamic type of the number set. Passing the wrong one
//     silently operates on different messages.
//   - Unilateral data handlers run on the client's read goroutine. Issuing a
//     command from one deadlocks the connection.
//   - There are no timeout knobs. A hung server is a hung account until
//     something closes the socket.
//   - Any response the decoder does not recognise ends the connection and
//     fails every pending command — which is why this package never enables
//     QRESYNC and never asks for Gmail's X-GM items.
//
// Everything above is contained here, behind provider.Session.
package imap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/thehappieco/mailie/internal/provider"
)

// Operation deadlines. go-imap exposes none of its own: it hard-codes thirty
// seconds for a response and five minutes for a literal, and manages the
// socket deadlines itself, so wrapping the connection would fight it. The only
// way to bound an operation is a timer that closes the client, which is what
// these drive.
const (
	opTimeout    = 60 * time.Second
	fetchTimeout = 10 * time.Minute
	noopTimeout  = 30 * time.Second
	// idleEventBuffer is how many unilateral signals can queue before they
	// start being dropped. Dropping is safe — every signal means the same
	// thing, "look again" — but the consumer must learn it happened.
	idleEventBuffer = 64
)

// session is one authenticated connection.
type session struct {
	client  *imapclient.Client
	caps    provider.Caps
	role    provider.Role
	profile provider.Profile
	spool   string

	mu       sync.Mutex
	selected *provider.FolderStatus
	// selectedReadOnly tracks how the folder was opened. A STORE against a
	// mailbox opened with EXAMINE is refused, so a write re-selects first.
	selectedReadOnly bool

	events   chan provider.IdleEvent
	overflow atomic.Bool

	closeOnce sync.Once
	// closedBy is why this side closed the connection, once it has
	// (closedHere).
	closedMu sync.Mutex
	closedBy error
}

var _ provider.Session = (*session)(nil)

func (s *session) Caps() provider.Caps { return s.caps }
func (s *session) Role() provider.Role { return s.role }

func (s *session) Selected() *provider.FolderStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.selected == nil {
		return nil
	}
	out := *s.selected
	return &out
}

func (s *session) Events() <-chan provider.IdleEvent { return s.events }

func (s *session) Overflowed() bool { return s.overflow.Swap(false) }

// run bounds one command.
//
// The timer closes the client rather than cancelling anything, because there
// is nothing to cancel: a command in flight owns the connection until the
// server answers. Closing is what turns a hung server into an error the caller
// can classify.
//
// A caller that gives up is not a hung server. A context already done sends
// nothing and leaves the connection as it was. One that ends while the
// command runs cannot stop it — go-imap takes no context — so the command
// runs to its tagged response, and a command that completed has had its
// response read to the end: the connection is in step and stays open. That
// matters because the interactive connection is shared, and a person moving
// down a message list cancels a read on every click; closing it each time
// would cost a login per message and count against the provider's limits.
// Only a command that failed after its caller left is taken to have left the
// connection in a state nobody checked, and closed.
func (s *session) run(ctx context.Context, timeout time.Duration, fn func() error) error {
	return s.runCommand(ctx, timeout, false, fn)
}

// runWrite is run for a command that changes the mailbox: STORE, MOVE, COPY,
// UID EXPUNGE. Once one is on the wire the server carries it out whether or
// not anybody still waits, so a caller that left while it ran gets what the
// server answered — the UIDs a MOVE reported, the flags a STORE set — not
// "cancelled": reporting a change that was made as one that was not leaves
// the caller's record of the mailbox wrong, and what the server said is gone
// with the response. A caller already gone before it is sent still sends
// nothing, and the timer still bounds a server that does not answer.
func (s *session) runWrite(ctx context.Context, timeout time.Duration, fn func() error) error {
	return s.runCommand(ctx, timeout, true, fn)
}

func (s *session) runCommand(ctx context.Context, timeout time.Duration, write bool, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return wrap(provider.ErrConnClosed, err, "the request was cancelled before it was sent")
	}
	if cause := s.CloseCause(); cause != nil {
		// Nothing can be sent: what the caller learns is why.
		return cause
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return wrap(provider.ErrConnClosed, ctx.Err(), "the request deadline had already passed")
	}

	done := make(chan struct{})
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		select {
		case <-done:
		default:
			// Closing is the only way to interrupt a command in flight:
			// go-imap exposes no deadline, and the connection owns the
			// socket until the server answers.
			timedOut.Store(true)
			s.closedHere(errNoAnswer)
			//nolint:errcheck // the timeout is what gets reported
			_ = s.client.Close()
		}
	})
	defer func() {
		timer.Stop()
		close(done)
	}()

	err := fn()
	ctxErr := ctx.Err()
	if ctxErr == nil {
		if err != nil && timedOut.Load() {
			// What the command got is the close, which go-imap reports as
			// it does the server hanging up.
			return wrap(provider.ErrConnClosed, err, errNoAnswer.Error())
		}
		return err
	}
	// The caller left while the command ran. Unless the timer fired — it
	// has closed the connection, and the command never got its answer — the
	// server answered it.
	answered := err == nil || timer.Stop()
	if err != nil {
		// Failed with nobody waiting for it: whatever it left on the
		// connection is not worth a later caller finding out. A timer that
		// fired has closed it already.
		s.closedHere(wrap(provider.ErrConnClosed, ctxErr, "the request was cancelled"))
		//nolint:errcheck // the command's own failure, or the cancellation, is what gets reported
		_ = s.client.Close()
	}
	if write && answered {
		// A change the server made, or refused, is what the caller learns.
		return err
	}
	// A command that completed leaves the connection as usable as any
	// other; the caller still learns it was cancelled.
	return wrap(provider.ErrConnClosed, ctxErr, "the request was cancelled")
}

func (s *session) ListFolders(ctx context.Context, withStatus bool) ([]provider.Folder, error) {
	var options imap.ListOptions
	// Never SelectSpecialUse: Exchange Online rejects the LIST-EXTENDED
	// selector outright, and servers that have SPECIAL-USE send the
	// attributes without being asked.
	if withStatus && s.caps.ListStatus {
		options.ReturnStatus = &imap.StatusOptions{
			NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
			HighestModSeq: s.caps.CondStore,
		}
		// Once the LIST carries a RETURN option it is an extended LIST, and
		// Dovecot then sends the SPECIAL-USE attributes only when they are
		// asked for too: without this, every role would silently fall back
		// to the name table. A return option, not the selector Exchange
		// rejects — and Exchange advertises neither extension anyway.
		options.ReturnSpecialUse = s.caps.SpecialUse
	}

	var data []*imap.ListData
	err := s.run(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.List("", "*", &options).Collect()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}

	out := make([]provider.Folder, 0, len(data))
	for _, d := range data {
		f := provider.Folder{
			// Stored exactly as it arrived: go-imap has already decoded
			// modified UTF-7 and will re-encode it for SELECT.
			Name:       d.Mailbox,
			Delim:      d.Delim,
			Attrs:      d.Attrs,
			Selectable: true,
		}
		for _, attr := range d.Attrs {
			if attr == imap.MailboxAttrNoSelect || attr == imap.MailboxAttrNonExistent {
				f.Selectable = false
			}
		}
		if d.Status != nil {
			f.Status = statusFromData(d.Status)
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *session) Select(ctx context.Context, name string, readOnly bool, expectUIDValidity uint32) (provider.FolderStatus, error) {
	options := &imap.SelectOptions{ReadOnly: readOnly, CondStore: s.caps.CondStore}

	var data *imap.SelectData
	err := s.run(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.Select(name, options).Wait()
		return err
	})
	if err != nil {
		// A SELECT that fails leaves no folder selected (RFC 3501), and one
		// whose answer nobody waited for cannot be trusted either. Forgetting
		// the old one makes the next write open its folder again rather than
		// assume it is still open.
		s.mu.Lock()
		s.selected = nil
		s.mu.Unlock()
		return provider.FolderStatus{}, classify(err, nil)
	}

	status := provider.FolderStatus{
		Name:           name,
		UIDValidity:    data.UIDValidity,
		UIDNext:        data.UIDNext,
		NumMessages:    data.NumMessages,
		HighestModSeq:  data.HighestModSeq,
		ReadOnly:       readOnly,
		PermanentFlags: data.PermanentFlags,
	}
	s.mu.Lock()
	s.selected = &status
	s.selectedReadOnly = readOnly
	s.mu.Unlock()

	if expectUIDValidity != 0 && data.UIDValidity != expectUIDValidity {
		// Returned with the status filled in: the caller resyncs from here
		// rather than paying for another round trip to learn the same thing.
		return status, fmt.Errorf("%w: was %d, now %d",
			provider.ErrUIDValidityChanged, expectUIDValidity, data.UIDValidity)
	}
	return status, nil
}

// selectForWrite makes sure the folder is open read-write before a command
// that changes it. Gmail answers a STORE against an EXAMINEd mailbox with
// NO [READ-ONLY], which is a confusing way to learn this. The session
// remembers which folder is open and how, so a folder already open
// read-write is not selected again.
//
// The folder is re-opened under the UIDVALIDITY it was open with: the
// caller's UIDs were read under that one, and in a folder renumbered
// meanwhile they would name other messages.
func (s *session) selectForWrite(ctx context.Context, name string) error {
	s.mu.Lock()
	current := s.selected
	readOnly := s.selectedReadOnly
	s.mu.Unlock()

	if current != nil && current.Name == name && !readOnly {
		return nil
	}
	var expect uint32
	if current != nil && current.Name == name {
		expect = current.UIDValidity
	}
	_, err := s.Select(ctx, name, false, expect)
	return err
}

func (s *session) Status(ctx context.Context, name string) (provider.FolderStatus, error) {
	options := &imap.StatusOptions{
		NumMessages: true, NumUnseen: true, UIDNext: true, UIDValidity: true,
		HighestModSeq: s.caps.CondStore,
	}
	var data *imap.StatusData
	err := s.run(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.Status(name, options).Wait()
		return err
	})
	if err != nil {
		return provider.FolderStatus{}, classify(err, nil)
	}
	return *statusFromData(data), nil
}

func (s *session) Create(ctx context.Context, name string) error {
	err := s.run(ctx, opTimeout, func() error {
		return s.client.Create(name, nil).Wait()
	})
	if err != nil {
		return classify(err, nil)
	}
	return nil
}

func statusFromData(d *imap.StatusData) *provider.FolderStatus {
	out := provider.FolderStatus{
		Name:          d.Mailbox,
		UIDValidity:   d.UIDValidity,
		UIDNext:       d.UIDNext,
		HighestModSeq: d.HighestModSeq,
	}
	if d.NumMessages != nil {
		out.NumMessages = *d.NumMessages
	}
	if d.NumUnseen != nil {
		out.NumUnseen = *d.NumUnseen
	}
	return &out
}

func (s *session) UIDs(ctx context.Context, set imap.UIDSet, since time.Time) ([]imap.UID, error) {
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{set}}
	if !since.IsZero() {
		criteria.Since = since
	}
	// ESEARCH only where it exists: Exchange answers BAD to RETURN, and a BAD
	// here would be read as a broken folder.
	var options *imap.SearchOptions
	if s.caps.ESearch {
		options = &imap.SearchOptions{ReturnAll: true}
	}

	var data *imap.SearchData
	err := s.run(ctx, fetchTimeout, func() error {
		var err error
		data, err = s.client.UIDSearch(criteria, options).Wait()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}
	return data.AllUIDs(), nil
}

func (s *session) UIDCount(ctx context.Context, set imap.UIDSet) (uint32, error) {
	if !s.caps.ESearch {
		return 0, fmt.Errorf("%w: ESEARCH is required to count without listing", provider.ErrUnsupported)
	}
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{set}}
	var data *imap.SearchData
	err := s.run(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.UIDSearch(criteria, &imap.SearchOptions{ReturnCount: true}).Wait()
		return err
	})
	if err != nil {
		return 0, classify(err, nil)
	}
	return data.Count, nil
}

func (s *session) FetchSummaries(ctx context.Context, set imap.UIDSet, changedSince uint64, fn func(provider.Summary) error) error {
	if changedSince > 0 && !s.caps.CondStore {
		return fmt.Errorf("%w: CONDSTORE is required for CHANGEDSINCE", provider.ErrUnsupported)
	}
	options := &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, InternalDate: true, RFC822Size: true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
		BodySection:   []*imap.FetchItemBodySection{referencesSection},
		ModSeq:        s.caps.CondStore,
		ChangedSince:  changedSince,
	}

	return s.run(ctx, fetchTimeout, func() error {
		cmd := s.client.Fetch(set, options)
		// Drained whatever happens: abandoning a FETCH half-read leaves the
		// connection out of step with the server and every later command
		// answers the wrong question. The explicit Close below is the one
		// whose error matters.
		//nolint:errcheck // reported by the explicit Close
		defer func() { _ = cmd.Close() }()

		var callbackErr error
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			buf, err := msg.Collect()
			if err != nil {
				return classify(err, nil)
			}
			if callbackErr != nil {
				continue
			}
			if err := fn(summaryFrom(buf)); err != nil {
				callbackErr = err
			}
		}
		if err := cmd.Close(); err != nil {
			return classify(err, nil)
		}
		return callbackErr
	})
}

func (s *session) FetchFlags(ctx context.Context, set imap.UIDSet, changedSince uint64) ([]provider.FlagUpdate, error) {
	if changedSince > 0 && !s.caps.CondStore {
		return nil, fmt.Errorf("%w: CONDSTORE is required for CHANGEDSINCE", provider.ErrUnsupported)
	}
	options := &imap.FetchOptions{
		UID: true, Flags: true, ModSeq: s.caps.CondStore, ChangedSince: changedSince,
	}

	var buffers []*imapclient.FetchMessageBuffer
	err := s.run(ctx, fetchTimeout, func() error {
		var err error
		buffers, err = s.client.Fetch(set, options).Collect()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}

	return flagUpdates(buffers), nil
}

// flagUpdates flattens a FETCH response, normalising the flags.
//
// Every path that reads flags goes through here. Normalising in one place and
// not another would mean the same flag compared differently depending on which
// command happened to read it, which shows up as a message whose read state
// flaps on every pass.
func flagUpdates(buffers []*imapclient.FetchMessageBuffer) []provider.FlagUpdate {
	out := make([]provider.FlagUpdate, 0, len(buffers))
	for _, buf := range buffers {
		out = append(out, provider.FlagUpdate{
			UID: buf.UID, ModSeq: buf.ModSeq, Flags: normalizeFlags(buf.Flags),
		})
	}
	return out
}

func (s *session) FetchHeader(ctx context.Context, uid imap.UID) ([]byte, error) {
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	options := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}

	var buffers []*imapclient.FetchMessageBuffer
	err := s.run(ctx, fetchTimeout, func() error {
		var err error
		buffers, err = s.client.Fetch(imap.UIDSetNum(uid), options).Collect()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}
	if len(buffers) == 0 {
		return nil, fmt.Errorf("%w: uid %d", provider.ErrMessageGone, uid)
	}
	return buffers[0].FindBodySection(section), nil
}

func (s *session) FetchPart(ctx context.Context, uid imap.UID, info provider.PartInfo, maxBytes int64) (provider.Part, error) {
	section := &imap.FetchItemBodySection{Part: info.Path, Peek: true}
	return s.fetchSection(ctx, uid, info, section, maxBytes)
}

func (s *session) FetchRaw(ctx context.Context, uid imap.UID, maxBytes int64) (provider.Part, error) {
	section := &imap.FetchItemBodySection{Peek: true}
	info := provider.PartInfo{MIMEType: "message/rfc822"}
	return s.fetchSection(ctx, uid, info, section, maxBytes)
}

// fetchSection reads one body section straight to a temporary file.
//
// Spooling rather than streaming is deliberate. Handing back a reader still
// bound to the connection would keep an IMAP session — and every sync pass for
// that account — busy for as long as the slowest HTTP client took to read a
// thirty megabyte attachment, and a client that simply disappeared would hold
// it until the operation timer fired.
func (s *session) fetchSection(ctx context.Context, uid imap.UID, info provider.PartInfo, section *imap.FetchItemBodySection, maxBytes int64) (provider.Part, error) {
	if err := os.MkdirAll(s.spool, 0o700); err != nil {
		return provider.Part{}, fmt.Errorf("imap: create spool directory: %w", err)
	}
	file, err := os.CreateTemp(s.spool, "part-*")
	if err != nil {
		return provider.Part{}, fmt.Errorf("imap: create spool file: %w", err)
	}
	// Removed on every way out but the one that hands it over, a panic
	// included: a spool file nobody holds is a copy of somebody's mail that
	// stays on disk until the daemon next starts.
	handedOver := false
	defer func() {
		if handedOver {
			return
		}
		//nolint:errcheck // unwinding a failed fetch
		_ = file.Close()
		//nolint:errcheck // unwinding a failed fetch
		_ = os.Remove(file.Name())
	}()

	options := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	var written int64
	var found bool

	err = s.run(ctx, fetchTimeout, func() error {
		cmd := s.client.Fetch(imap.UIDSetNum(uid), options)
		//nolint:errcheck // reported by the explicit Close below
		defer func() { _ = cmd.Close() }()

		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			for {
				item := msg.Next()
				if item == nil {
					break
				}
				data, ok := item.(imapclient.FetchItemDataBodySection)
				if !ok {
					continue
				}
				found = true
				if data.Literal == nil {
					// NIL where the section should be: a part path the
					// server no longer has, a message being expunged, or a
					// server's quirk. Not an empty section — served as one,
					// an original would be an empty .eml — but content the
					// server does not have. The deferred Close reads what is
					// left of the response.
					return fmt.Errorf("%w: the server returned no content (NIL) for section %v of uid %d",
						provider.ErrMessageGone, section.Part, uid)
				}
				limited := io.LimitReader(data.Literal, maxBytes+1)
				n, err := io.Copy(file, limited)
				if err != nil {
					return err
				}
				if n > maxBytes {
					return fmt.Errorf("%w: the section is larger than %d bytes", provider.ErrTooLarge, maxBytes)
				}
				written = n
			}
		}
		return cmd.Close()
	})
	if err != nil {
		return provider.Part{}, classify(err, nil)
	}
	if !found {
		return provider.Part{}, fmt.Errorf("%w: uid %d", provider.ErrMessageGone, uid)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return provider.Part{}, fmt.Errorf("imap: rewind spool file: %w", err)
	}
	handedOver = true
	return provider.Part{Info: info, Body: &spooledFile{File: file}, Size: written}, nil
}

// spooledFile removes itself when the caller is done with it.
type spooledFile struct{ *os.File }

func (f *spooledFile) Close() error {
	name := f.Name()
	err := f.File.Close()
	if rmErr := os.Remove(name); err == nil && rmErr != nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}

func (s *session) StoreFlags(ctx context.Context, set imap.UIDSet, op provider.FlagOp, flags []imap.Flag, unchangedSince uint64) ([]provider.FlagUpdate, error) {
	selected := s.Selected()
	if selected == nil {
		return nil, fmt.Errorf("%w: no folder is selected", provider.ErrFolderNotFound)
	}
	if err := s.selectForWrite(ctx, selected.Name); err != nil {
		return nil, err
	}

	store := &imap.StoreFlags{Op: storeOp(op), Flags: flags}
	var options *imap.StoreOptions
	if unchangedSince > 0 {
		if !s.caps.CondStore {
			return nil, fmt.Errorf("%w: CONDSTORE is required for UNCHANGEDSINCE", provider.ErrUnsupported)
		}
		options = &imap.StoreOptions{UnchangedSince: unchangedSince}
	}

	var buffers []*imapclient.FetchMessageBuffer
	err := s.runWrite(ctx, opTimeout, func() error {
		// STORE answers with a FETCH; not collecting it would leave the
		// response in the pipe.
		var err error
		buffers, err = s.client.Store(set, store, options).Collect()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}

	// The server's echo, not our guess at the outcome: a flag the server
	// refused to set must not appear in the index as set.
	return flagUpdates(buffers), nil
}

func storeOp(op provider.FlagOp) imap.StoreFlagsOp {
	switch op {
	case provider.FlagAdd:
		return imap.StoreFlagsAdd
	case provider.FlagDel:
		return imap.StoreFlagsDel
	default:
		return imap.StoreFlagsSet
	}
}

func (s *session) Move(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	selected := s.Selected()
	if selected == nil {
		return provider.MoveResult{}, fmt.Errorf("%w: no folder is selected", provider.ErrFolderNotFound)
	}
	if !s.caps.Move && !s.caps.UIDPlus {
		// Refused before anything is sent: a COPY followed by a refusal
		// would leave the message in both folders, flagged for deletion in
		// one of them.
		return provider.MoveResult{}, fmt.Errorf(
			"%w: this server has neither MOVE nor UIDPLUS, and a plain EXPUNGE would also delete messages another client flagged",
			provider.ErrUnsupported)
	}
	if err := s.selectForWrite(ctx, selected.Name); err != nil {
		return provider.MoveResult{}, err
	}
	if s.caps.Move {
		return s.moveNative(ctx, set, dest)
	}
	return s.moveByCopy(ctx, set, dest)
}

func (s *session) moveNative(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	var data *imapclient.MoveData
	err := s.runWrite(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.Move(set, dest).Wait()
		return err
	})
	if err != nil {
		return provider.MoveResult{}, classify(err, nil)
	}
	return moveResultFrom(data), nil
}

// moveByCopy is the fallback where MOVE is absent: COPY, STORE \Deleted and
// UID EXPUNGE of exactly the UIDs moved. Move has already made sure there is
// UIDPLUS; a blanket EXPUNGE would remove every message in the folder flagged
// \Deleted, including ones another client flagged and has not expunged yet,
// and losing somebody else's mail to implement "archive" is not a trade worth
// making.
//
// Once the COPY has been made the rest follows whatever the caller does: a
// caller that left, or whose deadline passed, between the steps would leave
// the message in both folders, flagged for deletion in one of them. Each
// step is still bounded by the operation timeout.
func (s *session) moveByCopy(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	result, err := s.copyTo(ctx, set, dest)
	if err != nil {
		return provider.MoveResult{}, err
	}
	rest := context.WithoutCancel(ctx)
	if _, err := s.StoreFlags(rest, set, provider.FlagAdd, []imap.Flag{imap.FlagDeleted}, 0); err != nil {
		return provider.MoveResult{}, err
	}
	err = s.runWrite(rest, opTimeout, func() error { return s.client.UIDExpunge(set).Close() })
	if err != nil {
		return provider.MoveResult{}, classify(err, nil)
	}
	return result, nil
}

func (s *session) Copy(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	if s.Selected() == nil {
		return provider.MoveResult{}, fmt.Errorf("%w: no folder is selected", provider.ErrFolderNotFound)
	}
	// A COPY changes nothing where it copies from, so a folder opened
	// read-only will do.
	return s.copyTo(ctx, set, dest)
}

func (s *session) copyTo(ctx context.Context, set imap.UIDSet, dest string) (provider.MoveResult, error) {
	var data *imap.CopyData
	err := s.runWrite(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.Copy(set, dest).Wait()
		return err
	})
	if err != nil {
		return provider.MoveResult{}, classify(err, nil)
	}
	result := provider.MoveResult{}
	if data != nil {
		result.DestUIDValidity = data.UIDValidity
		result.Mapping = uidMapping(data.SourceUIDs, data.DestUIDs)
	}
	return result, nil
}

func (s *session) SearchMessageID(ctx context.Context, messageID string) ([]imap.UID, error) {
	id := strings.Trim(strings.TrimSpace(messageID), "<>")
	if id == "" {
		return nil, nil
	}
	// HEADER matches a substring of the field, so the brackets are part of
	// what is searched for: "a@b" alone would also find "<xa@b>".
	criteria := &imap.SearchCriteria{Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: "<" + id + ">"}}}
	var options *imap.SearchOptions
	if s.caps.ESearch {
		options = &imap.SearchOptions{ReturnAll: true}
	}
	var data *imap.SearchData
	err := s.run(ctx, opTimeout, func() error {
		var err error
		data, err = s.client.UIDSearch(criteria, options).Wait()
		return err
	})
	if err != nil {
		return nil, classify(err, nil)
	}
	return data.AllUIDs(), nil
}

func moveResultFrom(data *imapclient.MoveData) provider.MoveResult {
	if data == nil {
		return provider.MoveResult{}
	}
	out := provider.MoveResult{DestUIDValidity: data.UIDValidity}
	// MoveData carries number sets rather than UID sets, so this is only
	// meaningful when the server actually answered with COPYUID.
	source, sourceOK := data.SourceUIDs.(imap.UIDSet)
	dest, destOK := data.DestUIDs.(imap.UIDSet)
	if sourceOK && destOK {
		out.Mapping = uidMapping(source, dest)
	}
	return out
}

// uidMapping pairs source and destination UIDs positionally, which is how
// COPYUID defines them — but only as go-imap hands them over: each set parsed
// into sorted, merged ranges ("11,10" is read as 10:11), so of several UIDs
// the pairing is the ascending one, not necessarily the server's. The sets
// are right; the pairing is exact only for one UID (MoveResult.Paired).
func uidMapping(source, dest imap.UIDSet) map[imap.UID]imap.UID {
	from, okFrom := source.Nums()
	to, okTo := dest.Nums()
	if !okFrom || !okTo || len(from) != len(to) {
		return nil
	}
	out := make(map[imap.UID]imap.UID, len(from))
	for i := range from {
		out[from[i]] = to[i]
	}
	return out
}

func (s *session) Append(ctx context.Context, folder string, r io.Reader, size int64, flags []imap.Flag, t time.Time) (provider.AppendResult, error) {
	if s.caps.AppendLimit > 0 && size > s.caps.AppendLimit {
		return provider.AppendResult{}, fmt.Errorf("%w: %d bytes, the server accepts %d",
			provider.ErrTooLarge, size, s.caps.AppendLimit)
	}
	options := &imap.AppendOptions{Flags: flags, Time: t}

	var data *imap.AppendData
	err := s.run(ctx, fetchTimeout, func() error {
		cmd := s.client.Append(folder, size, options)
		if _, err := io.Copy(cmd, r); err != nil {
			//nolint:errcheck // the copy error is the one to report
			_ = cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		var err error
		data, err = cmd.Wait()
		return err
	})
	if err != nil {
		return provider.AppendResult{}, classify(err, nil)
	}
	if data == nil {
		return provider.AppendResult{}, nil
	}
	return provider.AppendResult{UID: data.UID, UIDValidity: data.UIDValidity}, nil
}

func (s *session) Idle(ctx context.Context) (provider.IdleHandle, error) {
	if s.role != provider.RoleIdle {
		return nil, fmt.Errorf("imap: only the %s session may idle, not %s", provider.RoleIdle, s.role)
	}
	if !s.caps.Idle {
		return nil, fmt.Errorf("%w: the server does not advertise IDLE", provider.ErrUnsupported)
	}
	cmd, err := s.client.Idle()
	if err != nil {
		return nil, classify(err, nil)
	}
	return &idleHandle{cmd: cmd}, nil
}

type idleHandle struct {
	cmd  *imapclient.IdleCommand
	once sync.Once
	err  error
}

func (h *idleHandle) Stop() error {
	h.once.Do(func() {
		if err := h.cmd.Close(); err != nil {
			h.err = classify(err, nil)
			return
		}
		if err := h.cmd.Wait(); err != nil {
			h.err = classify(err, nil)
		}
	})
	return h.err
}

func (s *session) Noop(ctx context.Context) error {
	err := s.run(ctx, noopTimeout, func() error { return s.client.Noop().Wait() })
	if err != nil {
		return classify(err, nil)
	}
	return nil
}

func (s *session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closedHere(wrap(provider.ErrConnClosed, net.ErrClosed, "the connection was closed on this side"))
		// A polite logout, briefly: a server that will not answer must not
		// hold up a shutdown.
		if !isClosed(s.client) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				// A courtesy. A server that will not answer must not hold up
				// a shutdown, which is what the timeout below is for.
				//nolint:errcheck // best-effort politeness
				_ = s.client.Logout().Wait()
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
		err = s.client.Close()
		if err != nil && errors.Is(err, net.ErrClosed) {
			err = nil
		}
		close(s.events)
	})
	return err
}

func (s *session) Closed() <-chan struct{} { return s.client.Closed() }

// errNoAnswer is a connection the operation timer closed.
var errNoAnswer = wrap(provider.ErrConnClosed, errors.New("imap: the operation timer closed the connection"),
	"the server did not answer in time")

// closedHere records that this side is closing the connection, and why,
// unless it already has. A command failing and Closed firing after it are
// then not the server hanging up, though go-imap reports them the same way.
func (s *session) closedHere(cause error) {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()
	if s.closedBy == nil {
		s.closedBy = cause
	}
}

// CloseCause is what this side recorded closing the connection for, or else
// what go-imap's reader stopped on. go-imap hands that out only from Close,
// which on a client whose reader has stopped closes nothing more and returns
// it; nothing at all is the server hanging up between responses.
func (s *session) CloseCause() error {
	if !isClosed(s.client) {
		return nil
	}
	s.closedMu.Lock()
	own := s.closedBy
	s.closedMu.Unlock()
	if own != nil {
		return own
	}
	err := s.client.Close()
	if err == nil || errors.Is(err, net.ErrClosed) {
		return wrap(provider.ErrServerEnded, io.EOF, "the server closed the connection")
	}
	if cause := classify(err, nil); errors.Is(cause, provider.ErrConnClosed) {
		return cause
	}
	// A read cut short in a response reads as a command that failed; the
	// connection is gone all the same.
	return wrap(provider.ErrConnClosed, err, "the connection failed")
}

// referencesSection is BODY.PEEK[HEADER.FIELDS (REFERENCES)]: the one
// threading header ENVELOPE leaves out. PEEK, because a summary fetch must
// never mark anything read.
var referencesSection = &imap.FetchItemBodySection{
	Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"References"}, Peek: true,
}

// summaryFrom flattens what a FETCH returned.
func summaryFrom(buf *imapclient.FetchMessageBuffer) provider.Summary {
	out := provider.Summary{
		UID:          buf.UID,
		ModSeq:       buf.ModSeq,
		Flags:        normalizeFlags(buf.Flags),
		Envelope:     buf.Envelope,
		InternalDate: buf.InternalDate,
		Size:         buf.RFC822Size,
		References:   parseReferences(buf.FindBodySection(referencesSection)),
	}
	if buf.BodyStructure != nil {
		out.Parts = FlattenBodyStructure(buf.BodyStructure)
	}
	return out
}

// parseReferences reads the ids out of a References header block, without
// their angle brackets. A header the strict parser refuses — they exist in
// the wild — falls back to whitespace-separated tokens, because a thread with
// one odd id is still a thread.
func parseReferences(raw []byte) []string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil
	}
	header := gomail.Header{Header: message.Header{Header: h}}
	if ids, err := header.MsgIDList("References"); err == nil {
		return ids
	}
	var ids []string
	for _, tok := range strings.Fields(header.Get("References")) {
		if id := strings.Trim(tok, "<>"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// normalizeFlags lowercases flags for comparison.
//
// Flags are case-insensitive per the RFC and servers disagree in practice:
// the in-process test server lowercases them, Gmail does not. Comparing raw
// strings would make a flag look changed on every pass.
func normalizeFlags(flags []imap.Flag) []imap.Flag {
	if len(flags) == 0 {
		return nil
	}
	out := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		out = append(out, imap.Flag(strings.ToLower(string(f))))
	}
	return out
}
