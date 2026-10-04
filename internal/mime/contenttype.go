package mime

import (
	"strings"

	"github.com/thehappieco/mailie/internal/provider"
)

// OctetStream is what every type not known to be passive is served as.
const OctetStream = "application/octet-stream"

// AttachmentContentType is the Content-Type to serve a part's bytes with.
//
// The sender chose the part's type, so it is an input like any other: a
// text/html or image/svg+xml "attachment" served as itself runs its scripts
// with the daemon's origin the moment a browser opens it. Only types a browser
// displays passively — or does not display at all — are kept; everything else
// becomes application/octet-stream. That is an allowlist on purpose: a type
// nobody thought of is downloaded, never rendered. The route still sends the
// part as an attachment, with nosniff and a sandboxing CSP; this is the layer
// that does not rely on them.
//
// A kept text type carries its charset when the charset is a plain token,
// since the bytes are served undecoded.
func AttachmentContentType(info provider.PartInfo) string {
	mt := normalizeToken(info.MIMEType)
	if !validMediaType(mt) || !passive(mt) {
		return OctetStream
	}
	if strings.HasPrefix(mt, "text/") {
		if cs := normalizeToken(Param(info.Params, "charset")); cs != "" && len(cs) <= 40 && isToken(cs) {
			return mt + "; charset=" + cs
		}
	}
	return mt
}

// passiveTypes are displayed without running anything, or not displayed at
// all. Not here, deliberately: text/html, application/xhtml+xml, image/svg+xml,
// text/xml, application/xml and every other XML type (XSLT and SVG run in
// them), JavaScript and ECMAScript under all their names, text/css,
// application/x-shockwave-flash, multipart/* (which some browsers once
// rendered part by part), and text/plain's lookalikes that a browser sniffs.
var passiveTypes = map[string]bool{
	"text/plain":    true,
	"text/csv":      true,
	"text/calendar": true,
	"text/vcard":    true,
	"text/x-vcard":  true,

	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/bmp":  true,
	"image/tiff": true,
	"image/avif": true,
	"image/heic": true,
	"image/heif": true,

	"application/pdf":               true,
	"application/zip":               true,
	"application/gzip":              true,
	"application/x-7z-compressed":   true,
	"application/x-rar-compressed":  true,
	"application/vnd.rar":           true,
	"application/x-tar":             true,
	"application/rtf":               true,
	"application/msword":            true,
	"application/vnd.ms-excel":      true,
	"application/vnd.ms-powerpoint": true,
	"application/ics":               true,

	"message/rfc822": true,
}

// passiveFamilies are prefixes whose every member is passive: office
// documents are opened by another program, and media is played, not run.
var passiveFamilies = []string{
	"application/vnd.openxmlformats-officedocument.",
	"application/vnd.oasis.opendocument.",
	"audio/",
	"video/",
}

func passive(mt string) bool {
	if passiveTypes[mt] {
		return true
	}
	for _, prefix := range passiveFamilies {
		if strings.HasPrefix(mt, prefix) {
			// An XML flavour inside a passive family is still XML.
			return !strings.HasSuffix(mt, "+xml") && !strings.HasSuffix(mt, "/xml")
		}
	}
	return false
}

// validMediaType accepts exactly type "/" subtype, each an RFC 2045 token:
// no parameters, no whitespace, nothing a header could be split on.
func validMediaType(mt string) bool {
	typ, sub, ok := strings.Cut(mt, "/")
	return ok && typ != "" && sub != "" && isToken(typ) && isToken(sub)
}

func isToken(s string) bool {
	for i := range len(s) {
		c := s[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?=`, c) >= 0 {
			return false
		}
	}
	return s != ""
}
