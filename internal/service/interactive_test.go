package service_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/thehappieco/mailie/internal/provider"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
)

// lendingSync is a sync engine that also owns each account's interactive
// connection, as the real one does: it opens it from the registry and
// records who borrowed it.
type lendingSync struct {
	*fakeSync
	mailbox func(context.Context, string) (provider.Mailbox, error)

	mu   sync.Mutex
	lent []string
}

var _ service.InteractiveRunner = (*lendingSync)(nil)

func (s *lendingSync) Interactive(ctx context.Context, id string, fn func(context.Context, provider.Session) error) error {
	s.mu.Lock()
	s.lent = append(s.lent, id)
	s.mu.Unlock()
	mb, err := s.mailbox(ctx, id)
	if err != nil {
		return err
	}
	sess, err := mb.Open(ctx, provider.RoleInteractive)
	if err != nil {
		return err
	}
	defer func() { _ = sess.Close() }()
	return fn(ctx, sess)
}

func TestALiveFolderListingBorrowsTheEnginesConnection(t *testing.T) {
	// An account holds three connections at most, and the third is the
	// engine's. When the engine can lend it, the live listing uses it instead
	// of opening a fourth beside the engine's sync and idle connections.
	engine := &lendingSync{fakeSync: newFakeSync()}
	f := newFixtureWith(t, fixtureOptions{sync: engine})
	engine.mailbox = f.registry.Mailbox
	f.withIMAPAccount(t, provider.KindIMAP, providertest.RichCaps())
	id := f.accountID(t)

	folders, err := f.svc.ListFolders(t.Context(), admin(), id)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if !slices.ContainsFunc(folders, func(f service.Folder) bool { return f.Name == "INBOX" }) {
		t.Errorf("the listing through the engine's connection has no INBOX: %v", folders)
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if !slices.Equal(engine.lent, []string{id}) {
		t.Errorf("the engine lent its connection for %v, want the one listing of %s", engine.lent, id)
	}
}
