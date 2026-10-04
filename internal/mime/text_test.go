package mime

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thehappieco/mailie/internal/provider"
)

func textPart(mimeType, charset, encoding string) provider.PartInfo {
	info := provider.PartInfo{Path: []int{1}, MIMEType: mimeType, Encoding: encoding}
	if charset != "" {
		info.Params = map[string]string{"charset": charset}
	}
	return info
}

func decode(t *testing.T, info provider.PartInfo, in string, maxBytes int64) Text {
	t.Helper()
	got, err := DecodeText(info, strings.NewReader(in), maxBytes)
	if err != nil {
		t.Fatalf("DecodeText: %v", err)
	}
	if !utf8.ValidString(got.Text) {
		t.Fatalf("DecodeText returned invalid UTF-8: %q", got.Text)
	}
	if int64(len(got.Text)) > maxBytes {
		t.Fatalf("DecodeText returned %d bytes over a cap of %d", len(got.Text), maxBytes)
	}
	return got
}

func TestDeclaredCharsetsAreConvertedToUTF8(t *testing.T) {
	cases := []struct {
		charset, in, want string
	}{
		{"iso-8859-1", "caf\xe9", "café"},
		{"ISO-8859-15", "\xa4 5", "€ 5"},
		{"windows-1252", "\x93quoted\x94 \x80", "“quoted” €"},
		{"koi8-r", "\xf0\xd2\xc9\xd7\xc5\xd4", "Привет"},
		{"shift_jis", "\x82\xb1\x82\xf1\x82\xc9\x82\xbf\x82\xcd", "こんにちは"},
		{"iso-2022-jp", "\x1b$B$3$s$K$A$O\x1b(B", "こんにちは"},
		{"gb2312", "\xc4\xe3\xba\xc3", "你好"},
		{"utf-16", "\xfe\xff\x00h\x00i", "hi"},
		{"utf-16le", "h\x00i\x00", "hi"},
		{`"UTF-8"`, "olá", "olá"},
		{"latin1", "caf\xe9", "café"},
	}
	for _, c := range cases {
		t.Run(c.charset, func(t *testing.T) {
			got := decode(t, textPart("text/plain", c.charset, "8bit"), c.in, 1<<20)
			if got.Text != c.want || got.CharsetFallback || got.Truncated {
				t.Fatalf("got %+v, want %q without fallback", got, c.want)
			}
		})
	}
}

func TestAnUnknownCharsetIsGuessedAndSaysSo(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"8-bit latin text", "Ol\xe1 mundo, a\xe7\xe3o", "Olá mundo, ação"},
		{"UTF-8 text", "Olá mundo", "Olá mundo"},
		{"ASCII text", "hello", "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decode(t, textPart("text/plain", "x-klingon", "8bit"), c.in, 1<<20)
			if got.Text != c.want || !got.CharsetFallback {
				t.Fatalf("got %+v, want %q with fallback", got, c.want)
			}
		})
	}
}

func TestAnUnsupportedCharsetIsGuessedRatherThanBlanked(t *testing.T) {
	// The WHATWG index maps ISO-2022-KR to "replacement", which would turn
	// the whole part into a single U+FFFD.
	got := decode(t, textPart("text/plain", "iso-2022-kr", "7bit"), "hello there", 1<<20)
	if got.Text != "hello there" || !got.CharsetFallback {
		t.Fatalf("got %+v", got)
	}
}

func TestUTF8LabelledAsASCIIOrNothingIsNotAFallback(t *testing.T) {
	for _, cs := range []string{"us-ascii", ""} {
		got := decode(t, textPart("text/plain", cs, "8bit"), "Olá — tudo bem?", 1<<20)
		if got.Text != "Olá — tudo bem?" || got.CharsetFallback {
			t.Fatalf("charset %q: got %+v", cs, got)
		}
	}
}

func TestLatin1LabelledAsUTF8IsReadAsLatin1AndSaysSo(t *testing.T) {
	got := decode(t, textPart("text/plain", "utf-8", "8bit"), "Ol\xe1, a\xe7\xe3o \x80", 1<<20)
	if got.Text != "Olá, ação €" || !got.CharsetFallback {
		t.Fatalf("got %+v", got)
	}
}

func TestDamagedUTF8KeepsItsGoodCharacters(t *testing.T) {
	got := decode(t, textPart("text/plain", "utf-8", "8bit"), "Olá \xff mundo, ação", 1<<20)
	if got.Text != "Olá � mundo, ação" || !got.CharsetFallback {
		t.Fatalf("got %+v", got)
	}
}

func TestAnHTMLPartWithoutACharsetHonoursItsMetaTag(t *testing.T) {
	in := `<html><head><meta charset="windows-1252"></head><body>Caf` + "\xe9 \x80" + `</body></html>`
	got := decode(t, textPart("text/html", "", "8bit"), in, 1<<20)
	want := `<html><head><meta charset="windows-1252"></head><body>Café €</body></html>`
	if got.Text != want || got.CharsetFallback {
		t.Fatalf("got %+v", got)
	}
}

func TestHTMLIsReturnedAsHTML(t *testing.T) {
	in := `<p onclick="x()">hi</p><script>alert(1)</script><img src="https://tracker.example/p.gif">`
	got := decode(t, textPart("text/html", "utf-8", "7bit"), in, 1<<20)
	if got.Text != in {
		t.Fatalf("markup changed: %q", got.Text)
	}
}

func TestLineEndingsComeOutAsLF(t *testing.T) {
	got := decode(t, textPart("text/plain", "utf-8", "7bit"), "a\r\nb\rc\n\r\n", 1<<20)
	if got.Text != "a\nb\nc\n\n" {
		t.Fatalf("got %q", got.Text)
	}
}

func TestAByteOrderMarkIsDropped(t *testing.T) {
	got := decode(t, textPart("text/plain", "utf-8", "7bit"), "\xef\xbb\xbfhello", 1<<20)
	if got.Text != "hello" {
		t.Fatalf("got %q", got.Text)
	}
}

func TestTruncationNeverSplitsACharacter(t *testing.T) {
	in := strings.Repeat("á", 10) // 20 bytes
	for maxBytes := int64(1); maxBytes < 20; maxBytes++ {
		got := decode(t, textPart("text/plain", "utf-8", "8bit"), in, maxBytes)
		if !got.Truncated {
			t.Fatalf("cap %d: not marked truncated", maxBytes)
		}
		if want := strings.Repeat("á", int(maxBytes/2)); got.Text != want {
			t.Fatalf("cap %d: got %q, want %q", maxBytes, got.Text, want)
		}
		if got.CharsetFallback {
			t.Fatalf("cap %d: a cut character was taken for a charset problem", maxBytes)
		}
	}
}

func TestATextExactlyAtTheCapIsNotTruncated(t *testing.T) {
	got := decode(t, textPart("text/plain", "utf-8", "8bit"), "ação", 6)
	if got.Text != "ação" || got.Truncated {
		t.Fatalf("got %+v", got)
	}
	got = decode(t, textPart("text/plain", "utf-8", "8bit"), "ação!", 6)
	if got.Text != "ação" || !got.Truncated {
		t.Fatalf("one byte over: got %+v", got)
	}
}

func TestTheCapHoldsWhenConversionGrowsTheText(t *testing.T) {
	// Each Windows-1252 byte below becomes three bytes of UTF-8.
	in := strings.Repeat("\x80", 100)
	for _, cs := range []string{"windows-1252", "x-klingon", "us-ascii"} {
		got := decode(t, textPart("text/plain", cs, "8bit"), in, 10)
		if got.Text != "€€€" || !got.Truncated {
			t.Fatalf("charset %q: got %+v", cs, got)
		}
	}
}

func TestTheCapAppliesToTheDecodedText(t *testing.T) {
	// 4 kB of base64 is 3 kB of text: a cap of 3000 holds all of it.
	in := strings.Repeat("YWJj", 1000)
	got := decode(t, textPart("text/plain", "utf-8", "base64"), in, 3000)
	if len(got.Text) != 3000 || got.Truncated {
		t.Fatalf("got %d bytes, truncated=%v", len(got.Text), got.Truncated)
	}
}

func TestANonPositiveCapIsRefused(t *testing.T) {
	for _, maxBytes := range []int64{0, -1} {
		if _, err := DecodeText(textPart("text/plain", "", ""), strings.NewReader("x"), maxBytes); !errors.Is(err, ErrBadLimit) {
			t.Fatalf("cap %d: err = %v", maxBytes, err)
		}
	}
}

func TestAReadErrorIsReportedNotHidden(t *testing.T) {
	for _, cs := range []string{"utf-8", "iso-8859-1", "x-klingon"} {
		r := &failingReader{data: []byte("partial"), err: errDisk}
		if _, err := DecodeText(textPart("text/plain", cs, "8bit"), r, 1<<20); !errors.Is(err, errDisk) {
			t.Fatalf("charset %q: err = %v, want the reader's", cs, err)
		}
	}
}

func TestTheCharsetParameterIsFoundWhateverItsCase(t *testing.T) {
	info := provider.PartInfo{MIMEType: "text/plain", Params: map[string]string{"CharSet": "iso-8859-1"}}
	got := decode(t, info, "caf\xe9", 1<<20)
	if got.Text != "café" || got.CharsetFallback {
		t.Fatalf("got %+v", got)
	}
}
