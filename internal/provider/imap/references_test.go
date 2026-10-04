package imap

import (
	"slices"
	"testing"
)

func TestAMalformedReferencesHeaderStillYieldsItsIDs(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"References: <a@x> <b@x>\r\n\r\n", []string{"a@x", "b@x"}},
		{"References: <a@x>\r\n\t<b@x>\r\n\r\n", []string{"a@x", "b@x"}},
		// Not an id list the RFC recognises; still a thread.
		{"References: a@x <b@x\r\n\r\n", []string{"a@x", "b@x"}},
		{"\r\n", nil},
		{"", nil},
	} {
		if got := parseReferences([]byte(tc.raw)); !slices.Equal(got, tc.want) {
			t.Errorf("parseReferences(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
