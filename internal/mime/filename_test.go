package mime

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/provider"
)

func TestFilenamesCannotNameAPath(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":           "passwd",
		`C:\Users\bob\Desktop\a.pdf`: "a.pdf",
		`..\..\windows\system.ini`:   "system.ini",
		"reports/2026/":              "2026",
		"/":                          "",
		"..":                         "",
		"...":                        "",
		". . .":                      "",
		".bashrc":                    "bashrc",
		"report.pdf.":                "report.pdf",
		"  spaced  out  .txt  ":      "spaced out .txt",
		"dir/..":                     "",
		"=?UTF-8?Q?..=2F..=2Fx.sh?=": "x.sh",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilenamesLoseControlAndInvisibleCharacters(t *testing.T) {
	cases := map[string]string{
		"invoice\u202egpj.exe":     "invoicegpj.exe", // right-to-left override
		"a\u200bb\u200fc.txt":      "abc.txt",        // zero-width space, RTL mark
		"\ufeffbom.txt":            "bom.txt",
		"nul\x00byte.txt":          "nulbyte.txt",
		"esc\x1b[31mred.txt":       "esc[31mred.txt",
		"del\x7f.txt":              "del.txt",
		"c1\u009b\u0080.txt":       "c1.txt",
		"nel\u0085line.txt":        "nel line.txt",
		"folded\r\n name.txt":      "folded name.txt",
		"tab\tseparated.txt":       "tab separated.txt",
		"line\u2028sep\u2029a.txt": "line sep a.txt",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowsReservedCharactersAndNamesAreDefused(t *testing.T) {
	cases := map[string]string{
		`a<b>c:d"e|f?g*h.txt`: "a_b_c_d_e_f_g_h.txt",
		"CON.txt":             "_CON.txt",
		"nul":                 "_nul",
		"com1.tar.gz":         "_com1.tar.gz",
		"console.txt":         "console.txt",
		"Meeting 10:30.ics":   "Meeting 10_30.ics",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLongFilenamesKeepTheirExtension(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("é", 300) + ".pdf",
		strings.Repeat("a", 1000) + ".docx",
		strings.Repeat("日本", 200),
		strings.Repeat("x", 250) + "." + strings.Repeat("y", 40), // too long to be an extension
	} {
		got := SafeFilename(in)
		if len(got) > MaxFilenameBytes || !utf8.ValidString(got) || got == "" {
			t.Fatalf("SafeFilename(%d bytes) = %d bytes, valid=%v", len(in), len(got), utf8.ValidString(got))
		}
		if ext := in[strings.LastIndexByte(in, '.')+1:]; len(ext) < 16 && strings.Contains(in, ".") && !strings.HasSuffix(got, "."+ext) {
			t.Fatalf("extension %q lost: %q", ext, got)
		}
	}
}

func TestEncodedWordFilenamesDecode(t *testing.T) {
	cases := map[string]string{
		"=?UTF-8?Q?Relat=C3=B3rio_2026.pdf?=":       "Relatório 2026.pdf",
		"=?iso-8859-1?B?Y2Fm6S50eHQ=?=":             "café.txt",
		"=?UTF-8?B?5pel5pys6KqeLnBkZg==?=":          "日本語.pdf",
		"=?x-klingon?Q?abc?=.txt":                   "=_x-klingon_Q_abc_=.txt",
		"prefix =?UTF-8?Q?=C3=A9?= suffix.txt":      "prefix é suffix.txt",
		"=?UTF-8?Q?a?= =?UTF-8?Q?b?=.txt":           "ab.txt",
		"=?UTF-8?Q?=3D=3Futf-8=3Fq=3Fx=3F=3D?=.txt": "=_utf-8_q_x_=.txt",
	}
	for in, want := range cases {
		if got := SafeFilename(in); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAFilenameInInvalidUTF8IsRepaired(t *testing.T) {
	if got := SafeFilename("caf\xe9.txt"); got != "café.txt" {
		t.Fatalf("got %q", got)
	}
	if got := SafeFilename("olá\xff.txt"); got != "olá_.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestRFC2231FilenamesDecodeInTheirCharset(t *testing.T) {
	cases := []struct {
		name        string
		disposition map[string]string
		contentType map[string]string
		want        string
	}{
		{"extended UTF-8", map[string]string{"filename*": "utf-8''%E2%82%AC%20rates.pdf"}, nil, "€ rates.pdf"},
		{"extended Latin-1", map[string]string{"filename*": "iso-8859-1'pt'caf%E9.txt"}, nil, "café.txt"},
		{"continuations", nil, map[string]string{
			"name*0*": "utf-8''%E2%82%AC", "name*1": " rates", "name*2*": "%2Ecsv",
		}, "€ rates.csv"},
		{"continuation gap ends the value", nil, map[string]string{"name*0": "a", "name*2": "c"}, "a"},
		{"extended beats plain", map[string]string{"filename": "plain.txt", "filename*": "utf-8''ext.txt"}, nil, "ext.txt"},
		{"disposition beats content type", map[string]string{"filename": "d.txt"}, map[string]string{"name": "c.txt"}, "d.txt"},
		{"content type when no disposition name", map[string]string{}, map[string]string{"NAME": "c.txt"}, "c.txt"},
		{"malformed percent kept", map[string]string{"filename*": "utf-8''100%.txt"}, nil, "100%.txt"},
		{"no quotes", map[string]string{"filename*": "plain%20name.txt"}, nil, "plain name.txt"},
		{"unknown charset guessed", map[string]string{"filename*": "x-klingon''caf%E9.txt"}, nil, "café.txt"},
		{"encoded word in plain form", map[string]string{"filename": "=?UTF-8?Q?=C3=A9.txt?="}, nil, "é.txt"},
		{"nothing", nil, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FilenameFromParams(c.disposition, c.contentType); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestFilenameFallsBackToRFC2231ContentTypeParameters(t *testing.T) {
	// go-imap's Filename() reads only the plain "filename" and "name", so a
	// part named only in RFC 2231 form reaches us with an empty Filename.
	info := provider.PartInfo{
		Path: []int{2}, MIMEType: "text/csv",
		Params: map[string]string{"name*": "utf-8''%C3%A9t%C3%A9.csv"},
	}
	if got := Filename(info); got != "été.csv" {
		t.Fatalf("got %q", got)
	}
	info.Filename = "../given.csv"
	if got := Filename(info); got != "given.csv" {
		t.Fatalf("the given name should win and be made safe: %q", got)
	}
}

func TestDownloadNameIsNeverEmpty(t *testing.T) {
	cases := []struct {
		info provider.PartInfo
		want string
	}{
		{provider.PartInfo{Path: []int{1, 2}, MIMEType: "application/pdf"}, "part-1.2.pdf"},
		{provider.PartInfo{Path: []int{3}, MIMEType: "application/x-unheard-of"}, "part-3"},
		{provider.PartInfo{Path: []int{2}, MIMEType: "IMAGE/PNG", Filename: ".."}, "part-2.png"},
		{provider.PartInfo{MIMEType: "message/rfc822"}, "part.eml"},
		{provider.PartInfo{Path: []int{4}, MIMEType: "text/plain", Filename: "notes.txt"}, "notes.txt"},
	}
	for _, c := range cases {
		if got := DownloadName(c.info); got != c.want {
			t.Errorf("DownloadName(%+v) = %q, want %q", c.info, got, c.want)
		}
	}
}

func TestSafeFilenameOfItsOwnOutputChangesNothing(t *testing.T) {
	for _, in := range []string{
		"../../etc/passwd", "CON.txt", strings.Repeat("é", 300) + ".pdf", " .x. ", "a\u202eb", "=?UTF-8?Q?=3D=3F?=",
		"a:b", "日本語.pdf", "caf\xe9", ". .pdf",
	} {
		once := SafeFilename(in)
		if twice := SafeFilename(once); twice != once {
			t.Errorf("SafeFilename(%q) = %q, but SafeFilename of that = %q", in, once, twice)
		}
	}
}
