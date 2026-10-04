package mime_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/mime"
)

func TestAnUploadGoesOutAsWhatItsBytesAreNotWhatItsUploaderSaid(t *testing.T) {
	for _, c := range []struct {
		name, declared string
		head           []byte
		want           string
	}{
		{"a pdf", "application/pdf", []byte("%PDF-1.4 x"), "application/pdf"},
		{"a png called text", "text/plain", []byte("\x89PNG\r\n\x1a\n...."), "image/png"},
		{"html called a picture", "image/png", []byte("<!DOCTYPE html><script>alert(1)</script>"), mime.OctetStream},
		{"html called html", "text/html", []byte("<html><body>x</body></html>"), mime.OctetStream},
		{"svg", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), mime.OctetStream},
		{"a docx is a zip", "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			[]byte("PK\x03\x04rest"), "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"a zip called a docx is still a zip only by name", "application/vnd.oasis.opendocument.text",
			[]byte("PK\x03\x04rest"), "application/vnd.oasis.opendocument.text"},
		{"csv is text", "text/csv", []byte("a,b\n1,2\n"), "text/csv; charset=utf-8"},
		{"text said to be a program", "application/x-msdownload", []byte("plain words"), mime.OctetStream},
		{"text said to be nothing in particular", "", []byte("plain words"), "text/plain; charset=utf-8"},
		{"unknown bytes, a passive declared type", "application/msword", []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), "application/msword"},
		{"unknown bytes, a declared type that runs", "application/javascript", []byte("\x00\x01\x02\x03"), mime.OctetStream},
		{"unknown bytes, nothing declared", "", []byte("\x00\x01\x02\x03"), mime.OctetStream},
		{"a header injection in the declared type", "text/plain\r\nX-Evil: 1", []byte("hello"), "text/plain; charset=utf-8"},
		{"empty", "text/plain", nil, "text/plain; charset=utf-8"},
	} {
		if got := mime.UploadContentType(c.declared, c.head, utf8.Valid(c.head)); got != c.want {
			t.Errorf("%s: UploadContentType(%q) = %q, want %q", c.name, c.declared, got, c.want)
		}
	}
}

func TestAnUploadsNameIsMadeSafeAndNeverEmpty(t *testing.T) {
	for _, c := range []struct{ name, contentType, want string }{
		{"report.pdf", "application/pdf", "report.pdf"},
		{"../../etc/passwd", "text/plain", "passwd"},
		{`C:\Users\ana\notes.txt`, "text/plain", "notes.txt"},
		{"invoice\u202egpj.exe", "application/octet-stream", "invoicegpj.exe"},
		{"line\r\nbreak.txt", "text/plain", "line break.txt"},
		{"", "application/pdf", "attachment.pdf"},
		{"...", "text/plain; charset=utf-8", "attachment.txt"},
		{"", mime.OctetStream, "attachment"},
	} {
		if got := mime.UploadFilename(c.name, c.contentType); got != c.want {
			t.Errorf("UploadFilename(%q, %q) = %q, want %q", c.name, c.contentType, got, c.want)
		}
	}
}

func TestTextIsLabelledUTF8OnlyWhenItIsUTF8(t *testing.T) {
	// Excel's CSV export in Windows-1252: "José;São Paulo". Text all the
	// same, and never UTF-8.
	latin1 := []byte("Jos\xe9;S\xe3o Paulo\r\n")
	utf16 := []byte("\xff\xfeJ\x00o\x00s\x00\xe9\x00")
	for _, c := range []struct {
		name, declared string
		file           []byte
		want           string
	}{
		{"a Windows-1252 csv", "text/csv", latin1, "text/csv"},
		{"Latin-1 text said to be nothing", "", []byte("caf\xe9"), "text/plain"},
		{"a UTF-8 csv", "text/csv", []byte("José;São Paulo\r\n"), "text/csv; charset=utf-8"},
		{"a UTF-16 file with its byte order mark", "text/plain", utf16, "text/plain; charset=utf-16le"},
	} {
		v := &mime.UTF8Validator{}
		if _, err := v.Write(c.file); err != nil {
			t.Fatal(err)
		}
		if got := mime.UploadContentType(c.declared, c.file, v.Valid()); got != c.want {
			t.Errorf("%s: UploadContentType = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTheWholeFileDecidesWhetherItIsUTF8NotItsFirstBytes(t *testing.T) {
	// ASCII for longer than the sniff, and Latin-1 after: the names further
	// down a CSV.
	late := append([]byte(strings.Repeat("id;name\r\n", 200)), "7;Jos\xe9\r\n"...)
	valid := []byte(strings.Repeat("Olá, São Paulo! 🙂 ", 300))
	for _, c := range []struct {
		name  string
		file  []byte
		sizes []int // how the file arrives: a character may be split between writes
		want  bool
	}{
		{"UTF-8, whole", valid, []int{len(valid)}, true},
		{"UTF-8, a byte at a time", valid, []int{1}, true},
		{"UTF-8, in odd pieces", valid, []int{7, 3, 1, 512, 2}, true},
		{"Latin-1 after 2 KiB of ASCII", late, []int{512}, false},
		{"cut in the middle of a character", []byte("S\xc3\xa3o \xf0\x9f\x99"), []int{2}, false},
		{"a continuation byte out of place", []byte("ab\x80cd"), []int{1}, false},
		{"nothing", nil, []int{1}, true},
	} {
		v := &mime.UTF8Validator{}
		rest := c.file
		for i := 0; len(rest) > 0; i++ {
			n := min(c.sizes[i%len(c.sizes)], len(rest))
			if _, err := v.Write(rest[:n]); err != nil {
				t.Fatal(err)
			}
			rest = rest[n:]
		}
		if got := v.Valid(); got != c.want {
			t.Errorf("%s: Valid() = %t, want %t", c.name, got, c.want)
		}
	}
	// Only the first bytes were ASCII: the sniff alone would have said UTF-8.
	if head := late[:mime.SniffBytes]; !utf8.Valid(head) {
		t.Fatal("the test file's first bytes are not ASCII; it proves nothing")
	}
}
