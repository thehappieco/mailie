package mime

import (
	"bytes"
	"errors"
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
)

func decodeTransfer(t *testing.T, encoding, in string) string {
	t.Helper()
	r, known := NewTransferDecoder(encoding, strings.NewReader(in))
	if !known {
		t.Fatalf("encoding %q reported unknown", encoding)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %q: %v", encoding, err)
	}
	return string(out)
}

func TestQuotedPrintableLinesLongerThanFourKilobytesDecode(t *testing.T) {
	// HTML generators emit one enormous line; the standard library's reader
	// gives up on any line longer than its buffer.
	line := strings.Repeat("<td>ol=C3=A1</td>", 1000) // 17 kB, no line break
	if _, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(line + "\r\n"))); err == nil {
		t.Log("mime/quotedprintable now copes with long lines; the lenient reader is still correct")
	}
	got := decodeTransfer(t, "quoted-printable", line+"\r\n")
	want := strings.Repeat("<td>olá</td>", 1000) + "\r\n"
	if got != want {
		t.Fatalf("decoded %d bytes, want %d", len(got), len(want))
	}
}

func TestQuotedPrintableTakesAnythingItCannotDecodeLiterally(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"escape in either case":           {"caf=E9 caf=e9", "caf\xe9 caf\xe9"},
		"equals escaping nothing":         {"1+1=2 and a=zz", "1+1=2 and a=zz"},
		"soft break":                      {"joi=\r\nned", "joined"},
		"soft break with LF":              {"joi=\nned", "joined"},
		"soft break after whitespace":     {"joi= \t\r\nned", "joined"},
		"equals at the very end":          {"end=", "end"},
		"equals and spaces at the end":    {"end=  ", "end"},
		"bare control characters":         {"a\x00b\x0cc", "a\x00b\x0cc"},
		"trailing whitespace is dropped":  {"line  \t\r\nnext", "line\r\nnext"},
		"inner whitespace stays":          {"a  b\r\n", "a  b\r\n"},
		"whitespace before a soft break":  {"a =\r\nb", "a b"},
		"whitespace at the end":           {"end   ", "end"},
		"lone carriage return":            {"a\rb", "a\rb"},
		"equals then carriage return":     {"a=\rb", "a=\rb"},
		"encoded whitespace is preserved": {"a=20\r\n", "a \r\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decodeTransfer(t, "quoted-printable", c.in); got != c.want {
				t.Fatalf("decode(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBase64IgnoresJunkAndMissingPadding(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"folded lines":           {"SGVs\r\nbG8=\r\n", "Hello"},
		"missing padding":        {"SGVsbG8", "Hello"},
		"junk between quanta":    {"SGVs!!bG8*sIHdv\tcmxk.IQ==", "Hello, world!"},
		"8-bit junk":             {"SGVs\xff\xfebG8=", "Hello"},
		"concatenated chunks":    {"T2zDoQ==IG11bmRv", "Olá mundo"},
		"a lone trailing sextet": {"SGVsbG8hQ", "Hello!"},
		"empty":                  {"", ""},
		"only junk":              {"!!!\r\n", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decodeTransfer(t, "base64", c.in); got != c.want {
				t.Fatalf("decode(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestTransferEncodingNamesAreReadLeniently(t *testing.T) {
	for _, name := range []string{"BASE64", " base64 ", `"base64"`, "base64;"} {
		if got := decodeTransfer(t, name, "SGk="); got != "Hi" {
			t.Fatalf("encoding %q decoded to %q", name, got)
		}
	}
	for _, name := range []string{"", "7bit", "8BIT", "binary"} {
		if got := decodeTransfer(t, name, "=41"); got != "=41" {
			t.Fatalf("identity encoding %q changed the bytes: %q", name, got)
		}
	}
}

func TestAnUnknownTransferEncodingPassesTheBytesThrough(t *testing.T) {
	r, known := NewTransferDecoder("x-uuencode", strings.NewReader("begin 644 x"))
	if known {
		t.Fatal("x-uuencode reported known")
	}
	out, _ := io.ReadAll(r)
	if string(out) != "begin 644 x" {
		t.Fatalf("bytes changed: %q", out)
	}
}

var errDisk = errors.New("disk on fire")

type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestTransferDecodersReportTheReadersOwnError(t *testing.T) {
	for _, enc := range []string{"base64", "quoted-printable"} {
		r, _ := NewTransferDecoder(enc, &failingReader{data: []byte("SGVsbG8="), err: errDisk})
		if _, err := io.ReadAll(r); !errors.Is(err, errDisk) {
			t.Fatalf("%s: err = %v, want the reader's", enc, err)
		}
	}
}

func TestTransferDecodersServeTinyReads(t *testing.T) {
	// A caller reading one byte at a time must see the same bytes.
	for enc, in := range map[string]string{"base64": "T2zDoSBtdW5kbw==", "quoted-printable": "Ol=C3=A1 =\r\nmundo  \r\n"} {
		r, _ := NewTransferDecoder(enc, strings.NewReader(in))
		var out bytes.Buffer
		buf := make([]byte, 1)
		for {
			n, err := r.Read(buf)
			out.Write(buf[:n])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		full := decodeTransfer(t, enc, in)
		if out.String() != full {
			t.Fatalf("%s: byte-at-a-time %q, whole %q", enc, out.String(), full)
		}
	}
}
