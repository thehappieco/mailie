package mime

import (
	"slices"

	"github.com/thehappieco/mailie/internal/provider"
)

// BodyParts picks the parts to show as a message's body: at most one
// text/plain and one text/html, either possibly nil. The pointers are into
// parts.
//
// The starting point is the parts BODYSTRUCTURE marked IsBody — the first
// plain and the first HTML that are not attachments. When those two are not
// siblings, a flagged part's own multipart/alternative sibling is preferred:
// a message that opens with a plain note and then carries a plain+HTML pair
// would otherwise show one text as "plain" and an unrelated one as
// "formatted". When nothing is flagged (a row indexed before the flag
// existed), the first non-attachment part of each kind is used.
func BodyParts(parts []provider.PartInfo) (plain, html *provider.PartInfo) {
	kind := func(i int) string { return normalizeToken(parts[i].MIMEType) }
	flagged := func(want string) int {
		return slices.IndexFunc(parts, func(p provider.PartInfo) bool {
			return p.IsBody && normalizeToken(p.MIMEType) == want
		})
	}
	candidate := func(i int, want string) bool { return !parts[i].IsAttachment && kind(i) == want }

	p, h := flagged("text/plain"), flagged("text/html")
	if p >= 0 && h >= 0 && siblings(parts[p].Path, parts[h].Path) {
		return &parts[p], &parts[h]
	}
	for _, f := range []int{p, h} {
		if f < 0 {
			continue
		}
		want := "text/html"
		if kind(f) == "text/html" {
			want = "text/plain"
		}
		for j := range parts {
			if j != f && candidate(j, want) && siblings(parts[f].Path, parts[j].Path) {
				if want == "text/html" {
					return &parts[f], &parts[j]
				}
				return &parts[j], &parts[f]
			}
		}
	}
	if p < 0 && h < 0 {
		p = slices.IndexFunc(parts, func(x provider.PartInfo) bool {
			return !x.IsAttachment && normalizeToken(x.MIMEType) == "text/plain"
		})
		h = slices.IndexFunc(parts, func(x provider.PartInfo) bool {
			return !x.IsAttachment && normalizeToken(x.MIMEType) == "text/html"
		})
	}
	if p >= 0 {
		plain = &parts[p]
	}
	if h >= 0 {
		html = &parts[h]
	}
	return plain, html
}

// siblings reports whether two part paths have the same parent.
func siblings(a, b []int) bool {
	return len(a) == len(b) && len(a) > 0 && slices.Equal(a[:len(a)-1], b[:len(b)-1])
}
