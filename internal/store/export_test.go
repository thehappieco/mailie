package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// WritePrepares counts what the store's writer prepares, from
// WatchWritePrepares on, of the statements that write a table usage rule 1
// counts (messages, parts, folders, drafts), transaction by transaction.
// SQLite compiles the triggers a statement fires each time it prepares it,
// migration 0030's usage totals among them, so such a statement prepared
// again for every row of a batch compiles them again for every row.
type WritePrepares struct {
	mu sync.Mutex
	// inTx is how many times each one was prepared since the writer's
	// transaction began.
	inTx map[string]int
	// repeated is, for each one prepared more than once in a transaction,
	// the most times it was.
	repeated map[string]int
	total    int
}

// countedWrite is a statement that writes one of the tables rule 1 counts.
var countedWrite = regexp.MustCompile(`(?is)^\s*(?:INSERT(?:\s+OR\s+\w+)?\s+INTO|UPDATE|DELETE\s+FROM)\s+(?:messages|parts|folders|drafts)\b`)

// WatchWritePrepares puts in place of s's writer one on the same database,
// with the same pragmas, whose connection counts what it prepares. It
// implements no ExecerContext or QueryerContext, so database/sql prepares
// every statement it runs, a statement run once included: what SQLite
// compiles is what it prepares.
func WatchWritePrepares(t testing.TB, s *Store) *WritePrepares {
	t.Helper()
	watch := &WritePrepares{inTx: map[string]int{}, repeated: map[string]int{}}
	w := sql.OpenDB(watchedConnector{driver: s.w.Driver(), name: dsn(s.path, true), watch: watch})
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.PingContext(context.Background()); err != nil {
		t.Fatalf("store: connect the watched writer: %v", err)
	}
	if err := s.w.Close(); err != nil {
		t.Fatalf("store: close the writer: %v", err)
	}
	s.w = w
	return watch
}

// Repeated is every statement that writes a counted table and was prepared
// more than once in one transaction, by its first line, with the most times
// it was.
func (w *WritePrepares) Repeated() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.repeated))
	for query, n := range w.repeated {
		first, _, _ := strings.Cut(strings.TrimSpace(query), "\n")
		out[first] = max(out[first], n)
	}
	return out
}

// Total is how many statements that write a counted table were prepared.
func (w *WritePrepares) Total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

func (w *WritePrepares) began() {
	w.mu.Lock()
	defer w.mu.Unlock()
	clear(w.inTx)
}

func (w *WritePrepares) prepared(query string) {
	if !countedWrite.MatchString(query) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.total++
	w.inTx[query]++
	if n := w.inTx[query]; n > 1 {
		w.repeated[query] = max(w.repeated[query], n)
	}
}

type watchedConnector struct {
	driver driver.Driver
	name   string
	watch  *WritePrepares
}

func (c watchedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.name)
	if err != nil {
		return nil, err
	}
	return watchedConn{Conn: conn, watch: c.watch}, nil
}

func (c watchedConnector) Driver() driver.Driver { return c.driver }

// watchedConn is the driver's connection with only Prepare, PrepareContext,
// Begin and BeginTx: database/sql runs every statement through a prepare.
type watchedConn struct {
	driver.Conn
	watch *WritePrepares
}

func (c watchedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.watch.prepared(query)
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c watchedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.watch.began()
	if bc, ok := c.Conn.(driver.ConnBeginTx); ok {
		return bc.BeginTx(ctx, opts)
	}
	//nolint:staticcheck // the driver's own fallback, for a connection without BeginTx
	return c.Conn.Begin()
}
