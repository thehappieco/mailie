package mime

import (
	"testing"

	"github.com/thehappieco/mailie/internal/provider"
)

func part(path []int, mimeType string, isBody, isAttachment bool) provider.PartInfo {
	return provider.PartInfo{Path: path, MIMEType: mimeType, IsBody: isBody, IsAttachment: isAttachment}
}

func pathOf(p *provider.PartInfo) string {
	if p == nil {
		return "-"
	}
	return p.PathString()
}

func TestBodyPartsPreferAnAlternativePair(t *testing.T) {
	cases := []struct {
		name            string
		parts           []provider.PartInfo
		plain, htmlPart string
	}{
		{"plain only", []provider.PartInfo{part([]int{1}, "text/plain", true, false)}, "1", "-"},
		{"html only", []provider.PartInfo{part([]int{1}, "text/html", true, false)}, "-", "1"},
		{"alternative", []provider.PartInfo{
			part([]int{1}, "text/plain", true, false), part([]int{2}, "text/html", true, false),
		}, "1", "2"},
		{"alternative inside mixed, with an attachment", []provider.PartInfo{
			part([]int{1, 1}, "text/plain", true, false), part([]int{1, 2}, "text/html", true, false),
			part([]int{2}, "application/pdf", false, true),
		}, "1.1", "1.2"},
		{"a plain note before an alternative pair", []provider.PartInfo{
			part([]int{1}, "text/plain", true, false),
			part([]int{2, 1}, "text/plain", false, false), part([]int{2, 2}, "text/html", true, false),
		}, "2.1", "2.2"},
		{"an HTML attachment is never the body", []provider.PartInfo{
			part([]int{1}, "text/plain", true, false), part([]int{2}, "text/html", false, true),
		}, "1", "-"},
		{"nothing flagged: first of each kind", []provider.PartInfo{
			part([]int{1}, "image/png", false, true),
			part([]int{2}, "text/plain", false, false), part([]int{3}, "text/html", false, false),
			part([]int{4}, "text/plain", false, false),
		}, "2", "3"},
		{"nothing at all", []provider.PartInfo{part([]int{1}, "application/pdf", false, true)}, "-", "-"},
		{"empty", nil, "-", "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plain, html := BodyParts(c.parts)
			if pathOf(plain) != c.plain || pathOf(html) != c.htmlPart {
				t.Fatalf("BodyParts = (%s, %s), want (%s, %s)", pathOf(plain), pathOf(html), c.plain, c.htmlPart)
			}
		})
	}
}
