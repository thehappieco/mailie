package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// copyDatabase copies a stopped daemon's database, its write-ahead log
// included, to dir: a backup to restore.
func copyDatabase(t *testing.T, path, dir string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from, err := os.Open(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		to, err := os.Create(filepath.Join(dir, filepath.Base(path)+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(to, from); err != nil {
			t.Fatal(err)
		}
		_ = from.Close()
		if err := to.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// restoreDatabase puts a copy back where a stopped daemon's database is.
func restoreDatabase(t *testing.T, dir, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	copyDatabase(t, filepath.Join(dir, filepath.Base(path)), filepath.Dir(path))
}

// stateRun is what an extension read of its state at one start.
type stateRun struct {
	got   map[string]string
	keys  []string
	other []string
}

func TestAnExtensionsStateSurvivesARestartAndRollsBackWithTheDatabase(t *testing.T) {
	cfg := localConfig(t)
	run := func(put map[string][]byte, del []string) stateRun {
		t.Helper()
		var out stateRun
		extension := func(ctx context.Context, _ *Router, d Deps) error {
			state, err := d.State("follower")
			if err != nil {
				return err
			}
			out.got = map[string]string{}
			for _, key := range []string{"cursor", "outbox/1"} {
				value, ok, err := state.Get(ctx, key)
				if err != nil {
					return err
				}
				if ok {
					out.got[key] = string(value)
				}
			}
			if out.keys, err = state.Keys(ctx, ""); err != nil {
				return err
			}
			// Another extension's state is its own.
			other, err := d.State("another-one")
			if err != nil {
				return err
			}
			if out.other, err = other.Keys(ctx, ""); err != nil {
				return err
			}
			return state.Write(ctx, put, del)
		}
		d := startDaemon(t, cfg, Options{Extensions: []Extension{extension}})
		d.stop()
		if err := d.wait(); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return out
	}

	first := run(map[string][]byte{"cursor": []byte("1"), "outbox/1": []byte("pending")}, nil)
	if len(first.got) != 0 || len(first.keys) != 0 {
		t.Fatalf("a new database holds state: %v %v", first.got, first.keys)
	}
	backup := t.TempDir()
	copyDatabase(t, cfg.DatabasePath(), backup)

	second := run(map[string][]byte{"cursor": []byte("2")}, []string{"outbox/1"})
	if second.got["cursor"] != "1" || second.got["outbox/1"] != "pending" || len(second.keys) != 2 || len(second.other) != 0 {
		t.Fatalf("after a restart: %v, keys %v, another extension's %v", second.got, second.keys, second.other)
	}
	third := run(nil, nil)
	if third.got["cursor"] != "2" || len(third.keys) != 1 {
		t.Fatalf("after the second restart: %v, keys %v", third.got, third.keys)
	}

	// The database restored from the copy: the state is the copy's, with the
	// data it describes.
	restoreDatabase(t, backup, cfg.DatabasePath())
	restored := run(nil, nil)
	if restored.got["cursor"] != "1" || restored.got["outbox/1"] != "pending" {
		t.Fatalf("after the restore: %v, want the copy's", restored.got)
	}
}

func TestAnExtensionsStateIsTheDaemonsAndNamedWithinBounds(t *testing.T) {
	if _, err := (Deps{}).State("follower"); !errors.Is(err, ErrNoState) {
		t.Errorf("the state of Deps the daemon did not make: %v, want ErrNoState", err)
	}
	cfg := localConfig(t)
	refused := map[string]error{}
	extension := func(ctx context.Context, _ *Router, d Deps) error {
		for _, name := range []string{"", "Follower", "fol lower", "follower/", string(make([]byte, 65))} {
			_, err := d.State(name)
			refused[name] = err
		}
		state, err := d.State("follower_2-b")
		if err != nil {
			return err
		}
		refused["a mebibyte and one"] = state.Write(ctx, map[string][]byte{"k": make([]byte, 1<<20+1)}, nil)
		return nil
	}
	d := startDaemon(t, cfg, Options{Extensions: []Extension{extension}})
	d.stop()
	if err := d.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for name, err := range refused {
		if name == "a mebibyte and one" {
			if err == nil {
				t.Error("a value of a mebibyte and one byte was written")
			}
			continue
		}
		if !errors.Is(err, ErrStateName) {
			t.Errorf("the state named %q: %v, want ErrStateName", name, err)
		}
	}
}
