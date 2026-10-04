package mime

import (
	"bytes"
	"io"
	stdmime "mime"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message/charset"

	"github.com/thehappieco/mailie/internal/provider"
)

// MaxFilenameBytes caps a filename in UTF-8 bytes. Filesystems allow 255 per
// path component; the margin leaves room for the " (1)" a browser appends to
// a name that already exists in the downloads folder.
const MaxFilenameBytes = 200

// Filename returns the name a part's sender gave it, decoded and made safe
// (see SafeFilename), or "" when it has none or nothing of it survives.
//
// info.Filename is what BODYSTRUCTURE's Content-Disposition "filename" or
// Content-Type "name" said, already RFC 2047 decoded. When it is empty the
// RFC 2231 forms (name*, name*0*, ...) left raw in info.Params are tried.
func Filename(info provider.PartInfo) string {
	if name := SafeFilename(info.Filename); name != "" {
		return name
	}
	return SafeFilename(FilenameFromParams(nil, info.Params))
}

// DownloadName is Filename, or a name made from the part's path and type
// when the sender gave none: "part-1.2.pdf". It is never empty.
func DownloadName(info provider.PartInfo) string {
	if name := Filename(info); name != "" {
		return name
	}
	name := "part"
	if path := info.PathString(); path != "" {
		name += "-" + path
	}
	return name + extensions[normalizeToken(info.MIMEType)]
}

// extensions is a fixed table, not mime.ExtensionsByType: that one reads the
// host's mime.types, and a download's name should not depend on the host.
var extensions = map[string]string{
	"application/gzip":   ".gz",
	"application/json":   ".json",
	"application/pdf":    ".pdf",
	"application/zip":    ".zip",
	"image/gif":          ".gif",
	"image/jpeg":         ".jpg",
	"image/png":          ".png",
	"image/webp":         ".webp",
	"message/rfc822":     ".eml",
	"text/calendar":      ".ics",
	"text/csv":           ".csv",
	"text/html":          ".html",
	"text/plain":         ".txt",
	"text/vcard":         ".vcf",
	"application/ics":    ".ics",
	"application/msword": ".doc",
}

// FilenameFromParams reads a filename from raw MIME parameters: first the
// Content-Disposition's, then the Content-Type's. For each it takes the
// RFC 2231 extended form (filename*=utf-8”..., with continuations
// filename*0*, filename*1, ...) over the plain one, decodes RFC 2047 words in
// the plain one, and converts any declared charset to UTF-8. Parameter names
// are matched case-insensitively. The result is not sanitised.
func FilenameFromParams(disposition, contentType map[string]string) string {
	if name := paramRFC2231(disposition, "filename"); name != "" {
		return name
	}
	return paramRFC2231(contentType, "name")
}

// paramRFC2231 reads one parameter in any of its RFC 2231 and RFC 2047
// spellings.
func paramRFC2231(params map[string]string, key string) string {
	if len(params) == 0 {
		return ""
	}
	lower := make(map[string]string, len(params))
	for k, v := range params {
		lower[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if v, ok := lower[key+"*"]; ok {
		cs, value := splitExtended(v)
		return toUTF8(cs, percentDecode(value))
	}
	// Continuations: key*0 or key*0*, key*1 or key*1*, ... Only the first
	// segment carries the charset. A missing index ends the value.
	var (
		raw   bytes.Buffer
		cs    string
		found bool
	)
	const maxSegments = 64
	for i := range maxSegments {
		n := strconv.Itoa(i)
		if v, ok := lower[key+"*"+n+"*"]; ok {
			if i == 0 {
				cs, v = splitExtended(v)
			}
			raw.Write(percentDecode(v))
			found = true
			continue
		}
		if v, ok := lower[key+"*"+n]; ok {
			raw.WriteString(v)
			found = true
			continue
		}
		break
	}
	if found {
		return toUTF8(cs, raw.Bytes())
	}
	return decodeWords(lower[key])
}

// splitExtended splits an RFC 2231 extended value, charset'language'value.
// A value without the two quotes is taken as it is, with no charset.
func splitExtended(v string) (cs, value string) {
	first := strings.IndexByte(v, '\'')
	if first < 0 {
		return "", v
	}
	second := strings.IndexByte(v[first+1:], '\'')
	if second < 0 {
		return "", v
	}
	return v[:first], v[first+1+second+1:]
}

// percentDecode undoes RFC 2231's %XX escapes, leaving a malformed one as it
// is rather than failing the whole name.
func percentDecode(s string) []byte {
	if !strings.Contains(s, "%") {
		return []byte(s)
	}
	if b, err := url.PathUnescape(s); err == nil {
		return []byte(b)
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// toUTF8 converts bytes in a declared charset, guessing as DecodeText does
// when the charset is missing or unknown.
func toUTF8(cs string, b []byte) string {
	label := normalizeToken(cs)
	if label != "" && !isUTF8Family(label) && !isASCIIFamily(label) && !unsupportedCharset(label) {
		if conv, err := charset.Reader(label, bytes.NewReader(b)); err == nil {
			if out, err := io.ReadAll(conv); err == nil {
				return string(bytes.ToValidUTF8(out, replacement))
			}
		}
	}
	return string(bytes.ToValidUTF8(guessUTF8(b, false), replacement))
}

// decodeWords decodes RFC 2047 encoded-words, keeping the input when they are
// malformed or in a charset nobody knows.
func decodeWords(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	dec := stdmime.WordDecoder{CharsetReader: charset.Reader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// SafeFilename makes a sender-chosen name safe to hand to a browser or write
// to a filesystem, or returns "" when nothing usable is left.
//
// It decodes RFC 2047 words, repairs invalid UTF-8, keeps only the last path
// component (senders write "C:\Users\x\report.pdf" and "../../x"), turns
// whitespace and line breaks into single spaces, drops control characters
// and invisible formatting ones — a bidi override is how "gpj.exe" is shown
// as "exe.jpg" — replaces the characters Windows reserves, trims leading dots
// (hidden files, "..") and trailing dots and spaces, defuses Windows device
// names, and cuts the result to MaxFilenameBytes keeping its extension.
// SafeFilename of its own output returns that output unchanged.
func SafeFilename(name string) string {
	name = decodeWords(name)
	if !utf8.ValidString(name) {
		name = string(guessUTF8([]byte(name), false))
	}
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		// The last non-empty component: "dir/" names dir, not "".
		trimmed := strings.TrimRight(name, `/\`+" \t")
		if j := strings.LastIndexAny(trimmed, `/\`); j >= 0 {
			name = trimmed[j+1:]
		} else {
			name = trimmed
		}
	}

	var b strings.Builder
	space := false
	for _, r := range name {
		switch {
		case r == utf8.RuneError:
			r = '_'
		case unicode.IsSpace(r):
			// Tabs and line breaks (a folded header's leftovers) become a
			// space, and a run of spaces becomes one.
			space = b.Len() > 0
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		case strings.ContainsRune(`<>:"|?*/\`, r):
			r = '_'
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	name = strings.TrimLeft(b.String(), ". ")
	name = strings.TrimRight(name, ". ")
	name = defuseDeviceName(name)
	return capFilename(name)
}

// defuseDeviceName prefixes the names Windows maps to devices whatever their
// extension: "con.txt" cannot be created there, and "nul" swallows the file.
func defuseDeviceName(name string) string {
	stem := strings.ToLower(name)
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	switch stem {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return "_" + name
	}
	return name
}

// capFilename cuts a name to MaxFilenameBytes on a character boundary,
// keeping a short extension so the file still opens with the right program.
func capFilename(name string) string {
	if len(name) <= MaxFilenameBytes {
		return name
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
		ext = name[i:]
	}
	stem := name[:len(name)-len(ext)]
	stem = string(dropIncompleteTail([]byte(stem[:MaxFilenameBytes-len(ext)])))
	stem = strings.TrimRight(stem, ". ")
	if stem == "" {
		return strings.TrimLeft(ext, ".")
	}
	return stem + ext
}
