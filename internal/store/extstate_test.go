package store

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"
)

// An extension's state listed by prefix (extstate.go): a range of the primary
// key, which lists exactly the keys that begin with the prefix, and reads no
// other.

func TestExtensionStateKeysListsExactlyThePrefixByARangeScan(t *testing.T) {
	f := newSchemaLatest(t)
	ctx := context.Background()
	keys := []string{
		"a", "a/", "a/b", "a/b/c", "a0", "a\x7f", "a\x7fz", "a\xff", "a\xffb", "a\xff\xff", "a\xff\xff\x01", "b",
		"cmd/1", "cmd/2", "cmdx", "cmd0", "é", "é/1", "éa", "ê", "\xff", "\xff\xff", "\xff\xff\x00", "\xffa",
		"日本", "日本/x", "日朝", "z",
	}
	put := map[string][]byte{}
	for _, key := range keys {
		put[key] = []byte("1")
	}
	if err := f.s.WriteExtensionState(ctx, "follower", put, nil); err != nil {
		t.Fatal(err)
	}
	// Another extension's keys are never listed, whatever their prefix.
	if err := f.s.WriteExtensionState(ctx, "other", map[string][]byte{"a/b": nil, "cmd/1": nil}, nil); err != nil {
		t.Fatal(err)
	}
	// What the substring query, which read every key of the extension, listed.
	old := func(prefix string) []string {
		t.Helper()
		got := queryStrings(t, f.s, `SELECT key FROM extension_state
			WHERE extension = 'follower' AND substr(CAST(key AS BLOB), 1, length(CAST(? AS BLOB))) = CAST(? AS BLOB)`,
			prefix, prefix)
		sort.Strings(got)
		return got
	}
	for _, prefix := range []string{
		"", "a", "a/", "a/b", "a\x7f", "a\xff", "a\xff\xff", "b", "c", "cmd/", "cmd", "é", "\xc3", "\xff", "\xff\xff",
		"日", "日本", "zz", "\x00",
	} {
		var want []string
		for _, key := range keys {
			if strings.HasPrefix(key, prefix) {
				want = append(want, key)
			}
		}
		sort.Strings(want)
		got, err := f.s.ExtensionStateKeys(ctx, "follower", prefix)
		if err != nil {
			t.Fatalf("the keys under %q: %v", prefix, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("the keys under %q: %q, want %q", prefix, got, want)
		}
		if o := old(prefix); !slices.Equal(got, o) {
			t.Errorf("the keys under %q: %q, and the old query listed %q", prefix, got, o)
		}
	}
	// A range of the primary key's index, never a scan of the extension's
	// keys.
	plan := strings.Join(queryStrings(t, f.s, `EXPLAIN QUERY PLAN
		SELECT key FROM extension_state WHERE extension = ? AND key >= ? AND key < ? ORDER BY key`,
		"follower", "cmd/", "cmd0"), "; ")
	if !strings.Contains(plan, "key>? AND key<?") {
		t.Errorf("the listing's plan is %q, want a range of the primary key", plan)
	}
}

func TestAPrefixsEndIsTheLeastStringAboveItsKeys(t *testing.T) {
	for prefix, want := range map[string]string{
		"a": "b", "cmd/": "cmd0", "a\xff": "b", "a\xff\xff": "b", "é": "\xc3\xaa", "\x00": "\x01",
	} {
		if got, ok := prefixEnd(prefix); !ok || got != want {
			t.Errorf("the end of %q: %q (%v), want %q", prefix, got, ok, want)
		}
	}
	for _, prefix := range []string{"", "\xff", "\xff\xff"} {
		if got, ok := prefixEnd(prefix); ok {
			t.Errorf("the end of %q: %q, want none", prefix, got)
		}
	}
}
