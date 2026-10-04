package mime

import (
	"testing"

	"github.com/thehappieco/mailie/internal/provider"
)

func TestActiveContentTypesAreServedAsOctetStream(t *testing.T) {
	for _, mt := range []string{
		"text/html", "TEXT/HTML", " text/html ", "image/svg+xml", "application/xhtml+xml", "text/xml",
		"application/xml", "application/rss+xml", "application/xslt+xml", "application/javascript",
		"text/javascript", "application/x-javascript", "application/ecmascript", "text/ecmascript",
		"application/json", "text/css", "application/x-shockwave-flash", "multipart/mixed",
		"multipart/x-mixed-replace", "message/partial", "application/octet-stream", "text/x-unknown",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml", "video/foo+xml",
		"", "garbage", "text/", "/plain", "text/html; charset=utf-8", "text/plain\r\nX-Injected: 1",
		"image/png extra", "tëxt/plain",
	} {
		info := provider.PartInfo{MIMEType: mt, Params: map[string]string{"charset": "utf-8"}}
		if got := AttachmentContentType(info); got != OctetStream {
			t.Errorf("AttachmentContentType(%q) = %q, want %q", mt, got, OctetStream)
		}
	}
}

func TestPassiveContentTypesKeepTheirType(t *testing.T) {
	cases := []struct {
		mimeType, charset, want string
	}{
		{"application/pdf", "", "application/pdf"},
		{"IMAGE/PNG", "", "image/png"},
		{"image/jpeg", "utf-8", "image/jpeg"},
		{"audio/mpeg", "", "audio/mpeg"},
		{"video/mp4", "", "video/mp4"},
		{"message/rfc822", "", "message/rfc822"},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"text/plain", "ISO-8859-1", "text/plain; charset=iso-8859-1"},
		{"text/csv", `"utf-8"`, "text/csv; charset=utf-8"},
		{"text/plain", "utf-8\r\nX-Injected: 1", "text/plain"},
		{"text/plain", "utf 8", "text/plain"},
		{"text/calendar", "", "text/calendar"},
	}
	for _, c := range cases {
		info := provider.PartInfo{MIMEType: c.mimeType}
		if c.charset != "" {
			info.Params = map[string]string{"charset": c.charset}
		}
		if got := AttachmentContentType(info); got != c.want {
			t.Errorf("AttachmentContentType(%q, %q) = %q, want %q", c.mimeType, c.charset, got, c.want)
		}
	}
}
