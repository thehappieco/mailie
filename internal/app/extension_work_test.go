package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thehappieco/mailie/internal/lockfile"
	"github.com/thehappieco/mailie/internal/obs"
	"github.com/thehappieco/mailie/internal/secrets"
)

func TestTheDaemonStopsOnlyOnceItsExtensionsBackgroundWorkHasReturned(t *testing.T) {
	cfg := localConfig(t)
	ended := make(chan struct{})   // the work saw its context end
	release := make(chan struct{}) // the test lets it return
	var returned atomic.Bool
	extension := func(_ context.Context, _ *Router, d Deps) error {
		d.Go("holds-on", func(ctx context.Context) {
			<-ctx.Done()
			close(ended)
			<-release
			returned.Store(true)
		})
		return nil
	}
	d := startDaemon(t, cfg, Options{Extensions: []Extension{extension}})
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo)

	d.stop()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the work's context did not end when the daemon was stopped")
	}
	// The listener is down, but the daemon is not gone: it still holds its
	// data directory, which it lets go of only once everything has closed.
	select {
	case <-d.returned:
		t.Fatalf("Run returned while its extension's work was still running: %v", d.err)
	case <-time.After(300 * time.Millisecond):
	}
	if lock, err := lockfile.Acquire(cfg.LockPath()); err == nil {
		_ = lock.Release()
		t.Fatal("the daemon let go of its data directory while its extension's work was still running")
	}

	letGo()
	if err := d.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !returned.Load() {
		t.Fatal("Run returned before its extension's work did")
	}
}

func TestTheDaemonWaitsForSlowExtensionWorkBeforeClosingTheDatabase(t *testing.T) {
	// Longer than the fifteen seconds the listener is given to shut down:
	// the wait has no bound, as the sync engine's has none.
	const slow = 20 * time.Second
	cfg := localConfig(t)
	used := make(chan error, 1)
	extension := func(_ context.Context, _ *Router, d Deps) error {
		d.Go("slow", func(ctx context.Context) {
			<-ctx.Done()
			time.Sleep(slow)
			// Through the service, which writes to the database: still open.
			_, _, err := d.Service.SweepRemovedMailboxes(context.WithoutCancel(ctx), time.Hour)
			used <- err
		})
		return nil
	}
	d := startDaemon(t, cfg, Options{Extensions: []Extension{extension}})

	stopped := time.Now()
	d.stop()
	if err := d.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if waited := time.Since(stopped); waited < slow {
		t.Fatalf("Run returned %s after it was stopped, before its extension's work could have", waited)
	}
	select {
	case err := <-used:
		if err != nil {
			t.Fatalf("%s after the daemon was stopped, its extension's work could not use the database: %v", slow, err)
		}
	default:
		t.Fatal("Run returned before its extension's work did")
	}
}

func TestAnExtensionsBackgroundWorkStartsOnlyOnceItsRoutesAreMounted(t *testing.T) {
	cfg := localConfig(t)
	var firstDone, secondRan atomic.Bool
	saw := make(chan [2]bool, 1)
	first := func(_ context.Context, r *Router, d Deps) error {
		d.Go("first", func(context.Context) { saw <- [2]bool{firstDone.Load(), secondRan.Load()} })
		r.Handle("GET /extension/first", says("first"))
		firstDone.Store(true)
		return nil
	}
	second := func(_ context.Context, r *Router, _ Deps) error {
		secondRan.Store(true)
		r.Handle("GET /extension/second", says("second"))
		return nil
	}
	runDaemon(t, cfg, Options{Extensions: []Extension{first, second}})
	select {
	case got := <-saw:
		if !got[0] || !got[1] {
			t.Fatalf("the work started before every extension had run: the one that asked for it done %v, the next one run %v",
				got[0], got[1])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the work never started")
	}

	// A daemon that refuses a route, or an extension that fails, starts
	// nothing; nothing asked for before then runs.
	var ran atomic.Bool
	asks := func(_ context.Context, _ *Router, d Deps) error {
		d.Go("never", func(context.Context) { ran.Store(true) })
		return nil
	}
	refused := func(_ context.Context, r *Router, _ Deps) error {
		r.Handle("GET /v1/healthz", says("not the health probe"))
		return nil
	}
	failure := errors.New("the extension could not start")
	failing := func(context.Context, *Router, Deps) error { return failure }
	cfg = localConfig(t) // the daemon above holds its data directory until the test ends
	for name, tc := range map[string]struct {
		next Extension
		want error
	}{
		"a route refused":         {refused, ErrRoute},
		"an extension that fails": {failing, failure},
	} {
		err := Run(t.Context(), cfg, obs.NewLoggerTo(io.Discard, "info", "text"), Options{Extensions: []Extension{asks, tc.next}})
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: Run = %v, want %v", name, err, tc.want)
		}
		// Run waits for every work it started before it returns.
		if ran.Load() {
			t.Fatalf("%s: background work ran though the daemon never started", name)
		}
	}
}

func TestBackgroundWorkAskedAfterStartNeverRuns(t *testing.T) {
	cfg := localConfig(t)
	var late atomic.Int32 // runs of work that must never run
	never := func(context.Context) { late.Add(1) }
	kept := make(chan Deps, 1)
	extension := func(_ context.Context, _ *Router, d Deps) error {
		kept <- d
		d.Go("on-time", func(ctx context.Context) {
			<-ctx.Done()
			d.Go("during-shutdown", never)
		})
		return nil
	}
	d := startDaemon(t, cfg, Options{Extensions: []Extension{extension}})
	deps := <-kept

	// From a handler, say, once the daemon answers.
	deps.Go("after-start", never)
	d.stop()
	if err := d.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	deps.Go("after-stop", never)
	if n := late.Load(); n != 0 {
		t.Fatalf("work asked for after the start ran %d times", n)
	}
	for _, name := range []string{"after-start", "during-shutdown", "after-stop"} {
		if line := logLine(d.logs.String(), "work="+name); !strings.Contains(line, "level=ERROR") {
			t.Errorf("dropping %s was not logged as an error: %q", name, line)
		}
	}

	// Deps the daemon did not make run nothing either, and say so.
	logs := &lockedBuffer{}
	Deps{Log: obs.NewLoggerTo(logs, "info", "text")}.Go("not-the-daemons", never)
	if late.Load() != 0 || !strings.Contains(logLine(logs.String(), "work=not-the-daemons"), "level=ERROR") {
		t.Fatalf("Deps the daemon did not make: ran %d, logged %q", late.Load(), logs)
	}
}

// logLine is the first line of logs that contains s, or "".
func logLine(logs, s string) string {
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	return ""
}

func TestAnExtensionsMetricsAreServedBesideTheCores(t *testing.T) {
	cfg := localConfig(t)
	extension := func(_ context.Context, _ *Router, d Deps) error {
		checks := prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "mailserver", Name: "extension_checks_total", Help: "Checks the extension made.",
		})
		if err := d.Metrics.Register(checks); err != nil {
			return err
		}
		checks.Add(3)
		// A name the core serves is refused, never shared.
		taken := prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "mailserver", Name: "http_requests_total", Help: "HTTP requests served.",
		}, []string{"route", "method", "status"})
		if err := d.Metrics.Register(taken); err == nil {
			return errors.New("the extension registered a metric the core serves")
		}
		return nil
	}
	base := runDaemon(t, cfg, Options{Extensions: []Extension{extension}})

	a := fetch(t, http.MethodGet, base+"/metrics")
	if a.status != http.StatusOK {
		t.Fatalf("GET /metrics answered %d", a.status)
	}
	for _, want := range []string{"\nmailserver_extension_checks_total 3\n", "\ngo_goroutines "} {
		if !strings.Contains(a.body, want) {
			t.Errorf("/metrics does not serve %q:\n%s", strings.TrimSpace(want), a.body)
		}
	}
}

func TestAnExtensionOpensOnlyItsOwnSecretsWithTheDaemonsSealer(t *testing.T) {
	cfg := localConfig(t)
	// The daemon's sealer is the keyring of its credential keys, a previous
	// one included.
	previous := bytes.Repeat([]byte{0x2e}, secrets.KeyLen)
	cfg.Credentials.Keys[2] = previous
	sealWith := func(s secrets.Sealer, b secrets.Binding, plaintext string) []byte {
		t.Helper()
		sealed, err := s.Seal(t.Context(), b, []byte(plaintext))
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	daemons, err := NewSealer(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	keyring := func(id uint8, key []byte) secrets.Sealer {
		kr, err := secrets.NewKeyring(id, map[uint8][]byte{id: key})
		if err != nil {
			t.Fatal(err)
		}
		return kr
	}
	own := secrets.ExtensionSecret("ext_fixture")
	envelopes := map[string][]byte{
		"its own":               sealWith(daemons, own, "an extension's secret"),
		"under a previous key":  sealWith(keyring(2, previous), own, "sealed before the key moved"),
		"a credential":          sealWith(daemons, secrets.Credential("ext_fixture", "password"), "hunter2"),
		"another extension's":   sealWith(daemons, secrets.ExtensionSecret("ext_other"), "not this one's"),
		"under a key not given": sealWith(keyring(7, bytes.Repeat([]byte{0x7e}, secrets.KeyLen)), own, "x"),
	}

	type opened struct {
		plaintext string
		err       error
	}
	results := make(chan map[string]opened, 1)
	extension := func(ctx context.Context, _ *Router, d Deps) error {
		got := map[string]opened{}
		for name, envelope := range envelopes {
			p, err := d.OpenSecret(ctx, "ext_fixture", envelope)
			got[name] = opened{string(p), err}
		}
		p, err := d.OpenSecret(ctx, "Ext Fixture", envelopes["its own"])
		got["a ref no key service takes"] = opened{string(p), err}
		results <- got
		return nil
	}
	runDaemon(t, cfg, Options{Extensions: []Extension{extension}})
	got := <-results

	for name, want := range map[string]string{
		"its own":              "an extension's secret",
		"under a previous key": "sealed before the key moved",
	} {
		if got[name].err != nil || got[name].plaintext != want {
			t.Errorf("%s: %q, %v; want %q", name, got[name].plaintext, got[name].err, want)
		}
	}
	for _, name := range []string{"a credential", "another extension's", "under a key not given"} {
		if got[name].plaintext != "" || !secrets.DoesNotOpen(got[name].err) {
			t.Errorf("%s: %q, %v; want an envelope that does not open", name, got[name].plaintext, got[name].err)
		}
	}
	if r := got["a ref no key service takes"]; !errors.Is(r.err, secrets.ErrBinding) || secrets.DoesNotOpen(r.err) {
		t.Errorf("a ref no key service takes: %v; want ErrBinding", r.err)
	}

	// Deps the daemon did not make have no sealer.
	if _, err := (Deps{}).OpenSecret(t.Context(), "ext_fixture", envelopes["its own"]); !errors.Is(err, ErrNoSealer) {
		t.Fatalf("Deps the daemon did not make: %v; want ErrNoSealer", err)
	}
}
