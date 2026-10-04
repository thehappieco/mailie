package mime

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/provider"
)

var (
	fuzzEncodings = []string{"", "7bit", "quoted-printable", "base64", "x-uuencode"}
	fuzzCharsets  = []string{
		"", "utf-8", "us-ascii", "iso-8859-1", "windows-1252", "koi8-r", "shift_jis", "euc-jp",
		"iso-2022-jp", "gb18030", "gbk", "big5", "euc-kr", "utf-16", "utf-16le", "utf-16be",
		"x-klingon", "iso-2022-kr", "utf-7",
	}
)

// FuzzDecodeTextNeverFailsAndStaysWithinTheCap feeds arbitrary bytes, under
// every transfer encoding and a spread of charsets, to the decoder a message
// body goes through. Whatever arrives, it must not fail (the reader here
// never does), must return valid UTF-8 no longer than the cap with LF line
// endings, and must hand back UTF-8 input untouched. Along the way the two
// transfer decoders must invert the standard library's encoders exactly.
func FuzzDecodeTextNeverFailsAndStaysWithinTheCap(f *testing.F) {
	f.Add([]byte("Ol=E1 =\r\nmundo  \r\n"), uint8(2), uint8(3), uint16(100))
	f.Add([]byte("SGVsbG8s\r\n IHdvcmxk!!\r\n\tIQ"), uint8(3), uint8(1), uint16(5))
	f.Add([]byte("\x1b$B$3$s$K$A$O\x1b(B"), uint8(0), uint8(8), uint16(4))
	f.Add([]byte("<meta charset=koi8-r>\xf0\xd2\xc9"), uint8(1), uint8(0), uint16(4096))
	f.Add([]byte("\xfe\xff\x00h\x00i\xd8\x00"), uint8(0), uint8(13), uint16(3))
	f.Add([]byte("caf\xe9 \x80\r\n\r\xef\xbb\xbf"), uint8(1), uint8(2), uint16(1))
	f.Fuzz(func(t *testing.T, data []byte, enc, cs uint8, capSeed uint16) {
		mimeType := "text/plain"
		if enc&0x80 != 0 {
			mimeType = "text/html"
		}
		info := provider.PartInfo{
			Path:     []int{1},
			MIMEType: mimeType,
			Encoding: fuzzEncodings[int(enc&0x7f)%len(fuzzEncodings)],
			Params:   map[string]string{"charset": fuzzCharsets[int(cs)%len(fuzzCharsets)]},
		}
		maxBytes := int64(capSeed%4096) + 1

		got, err := DecodeText(info, bytes.NewReader(data), maxBytes)
		if err != nil {
			t.Fatalf("DecodeText(%+v): %v", info, err)
		}
		if !utf8.ValidString(got.Text) {
			t.Fatalf("invalid UTF-8 out: %q", got.Text)
		}
		if int64(len(got.Text)) > maxBytes {
			t.Fatalf("%d bytes out over a cap of %d", len(got.Text), maxBytes)
		}
		if strings.ContainsRune(got.Text, '\r') || strings.HasPrefix(got.Text, "\uFEFF") {
			t.Fatalf("CR or BOM left in: %q", got.Text)
		}
		identity := info.Encoding == "" || info.Encoding == "7bit"
		plainUTF8 := info.Params["charset"] == "utf-8" && utf8.Valid(data) &&
			!bytes.ContainsRune(data, '\r') && !bytes.HasPrefix(data, []byte("\uFEFF"))
		if identity && plainUTF8 && int64(len(data)) <= maxBytes {
			if got.Text != string(data) || got.Truncated || got.CharsetFallback {
				t.Fatalf("UTF-8 input changed: %q -> %+v", data, got)
			}
		}

		roundTrip(t, "base64", []byte(base64.StdEncoding.EncodeToString(data)), data)
		var qp bytes.Buffer
		w := quotedprintable.NewWriter(&qp)
		w.Binary = true
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		roundTrip(t, "quoted-printable", qp.Bytes(), data)
	})
}

func roundTrip(t *testing.T, encoding string, encoded, want []byte) {
	t.Helper()
	r, _ := NewTransferDecoder(encoding, bytes.NewReader(encoded))
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("%s: %v", encoding, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s round trip: %q -> %q -> %q", encoding, want, encoded, got)
	}
}

// FuzzSafeFilenameIsSafeAndStable holds SafeFilename to its promise for any
// input, including input that went through the RFC 2231 and RFC 2047
// decoders first: valid UTF-8, within the cap, no path separator, no control
// or invisible formatting character, no leading dot, no trailing dot or
// space — and a second pass changes nothing.
func FuzzSafeFilenameIsSafeAndStable(f *testing.F) {
	for _, seed := range []string{
		"../../etc/passwd", `C:\x\y.pdf`, "invoice\u202egpj.exe", "=?UTF-8?Q?Relat=C3=B3rio.pdf?=",
		"utf-8''%E2%82%AC%20rates.pdf", "caf\xe9.txt", "CON.txt", ". .", strings.Repeat("é", 150) + ".pdf",
		"=?UTF-8?B?Li4vLi4vZXZpbC5zaA==?=", "a\x00b\r\n c",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		check := func(label, out string) {
			if !utf8.ValidString(out) || len(out) > MaxFilenameBytes {
				t.Fatalf("%s(%q) = %q: invalid or too long", label, in, out)
			}
			if strings.ContainsAny(out, `/\<>:"|?*`) {
				t.Fatalf("%s(%q) = %q: reserved character", label, in, out)
			}
			if strings.HasPrefix(out, ".") || strings.HasPrefix(out, " ") ||
				strings.HasSuffix(out, ".") || strings.HasSuffix(out, " ") {
				t.Fatalf("%s(%q) = %q: leading dot or space, or trailing dot or space", label, in, out)
			}
			for _, r := range out {
				if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
					t.Fatalf("%s(%q) = %q: character %U left in", label, in, out, r)
				}
			}
			if again := SafeFilename(out); again != out {
				t.Fatalf("%s(%q) = %q, and again = %q", label, in, out, again)
			}
		}
		check("SafeFilename", SafeFilename(in))
		check("Filename", Filename(provider.PartInfo{Path: []int{1}, Params: map[string]string{
			"name*": in, "name*0*": in, "name*1": in,
		}}))
		check("FilenameFromParams", SafeFilename(FilenameFromParams(map[string]string{"filename*0*": in, "filename*1*": in}, nil)))
		if name := DownloadName(provider.PartInfo{Filename: in, MIMEType: "application/pdf"}); name == "" {
			t.Fatalf("DownloadName(%q) is empty", in)
		}
	})
}
