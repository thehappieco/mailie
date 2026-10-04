package mime_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/thehappieco/mailie/internal/mime"
	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// The fixtures under testdata are hand-written for this package. Each test
// sees a part the way production does: PartInfo from the BODYSTRUCTURE an
// IMAP server builds for the message, flattened by the provider, and the
// bytes an IMAP server returns for BODY[<path>].

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureParts(raw []byte) []provider.PartInfo {
	return imapprovider.FlattenBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)))
}

func partAt(t *testing.T, parts []provider.PartInfo, path string) provider.PartInfo {
	t.Helper()
	for _, p := range parts {
		if p.PathString() == path {
			return p
		}
	}
	t.Fatalf("no part %s", path)
	return provider.PartInfo{}
}

// imapSection is what go-imap's server answers for BODY[<path>].
func imapSection(raw []byte, path []int) []byte {
	return imapserver.ExtractBodySection(bytes.NewReader(raw), &imap.FetchItemBodySection{Part: path})
}

func TestFixtureBodiesDecodeToWhatTheSenderWrote(t *testing.T) {
	cases := []struct {
		file, path string
		want       string
		fallback   bool
	}{
		{"latin1-qp.eml", "1", "Olá Bruno,\na reunião de amanhã foi remarcada para as 15h; confirme se a sala está livre.\n" +
			"Preço: 10 = 5 + 5  \nAbraços,\nAna\n", false},
		{"alternative.eml", "1", "Olá, mundo — ação ✓\nSegunda linha.\n", false},
		{"alternative.eml", "2", `<html><body><p style="color:red">Olá, mundo — ação ✓</p><script>alert(1)</script></body></html>`, false},
		{"mixed.eml", "1.1", "Café € 5", false},
		{"mixed.eml", "1.2", `<html><head><meta http-equiv="Content-Type" content="text/html; charset=windows-1252"></head><body>Café € 5</body></html>`, false},
		{"mixed.eml", "4", "<script>fetch('/v1/accounts')</script>", false},
		{"unknown-charset.eml", "1", "Olá mundo, ação\n", true},
		{"ascii-labelled-utf8.eml", "1", "Olá — tudo bem?\n", false},
		{"iso-2022-jp.eml", "1", "こんにちは\n", false},
		{"broken-base64.eml", "1", "Hello, world!", false},
		{"broken-base64.eml", "2", "Olá mundo", false},
		{"nested.eml", "1", "See the forwarded message.", false},
		{"lf-only.eml", "1", "Primeira linha continua.\nSegunda linha é UTF-8.\n", false},
	}
	for _, c := range cases {
		t.Run(c.file+"/"+c.path, func(t *testing.T) {
			raw := fixture(t, c.file)
			info := partAt(t, fixtureParts(raw), c.path)
			section, err := mime.Section(raw, info.Path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := mime.DecodeText(info, bytes.NewReader(section), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != c.want || got.CharsetFallback != c.fallback || got.Truncated {
				t.Fatalf("got %q (fallback=%v truncated=%v)\nwant %q (fallback=%v)",
					got.Text, got.CharsetFallback, got.Truncated, c.want, c.fallback)
			}
		})
	}
}

func TestEveryFixturePartDecodesToValidUTF8WithinTheCap(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, name := range names {
		raw := fixture(t, filepath.Base(name))
		for _, info := range fixtureParts(raw) {
			section, err := mime.Section(raw, info.Path)
			if err != nil {
				t.Fatalf("%s %s: %v", name, info.PathString(), err)
			}
			for _, maxBytes := range []int64{1, 2, 3, 7, 64, 1 << 20} {
				got, err := mime.DecodeText(info, bytes.NewReader(section), maxBytes)
				if err != nil {
					t.Fatalf("%s %s cap %d: %v", name, info.PathString(), maxBytes, err)
				}
				if !utf8.ValidString(got.Text) || int64(len(got.Text)) > maxBytes {
					t.Fatalf("%s %s cap %d: %d bytes, valid=%v", name, info.PathString(), maxBytes, len(got.Text), utf8.ValidString(got.Text))
				}
			}
		}
	}
}

func TestSectionAnswersWhatAnIMAPServerWould(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, name := range names {
		raw := fixture(t, filepath.Base(name))
		// Every leaf and every multipart above one. go-imap's server helper
		// is lenient about paths that do not exist (it answers BODY[1.1] of
		// a single-part message with part 1), so only real paths are
		// compared; ErrNoSection for missing ones is asserted separately.
		var paths [][]int
		if filepath.Base(name) == "nested.eml" {
			// The embedded message's own parts, which flattening skips.
			paths = append(paths, []int{2, 1}, []int{2, 2})
		}
		for _, info := range fixtureParts(raw) {
			for n := 1; n <= len(info.Path); n++ {
				paths = append(paths, info.Path[:n])
			}
		}
		for _, path := range paths {
			want := imapSection(raw, path)
			got, err := mime.Section(raw, path)
			if want == nil {
				if !errors.Is(err, mime.ErrNoSection) {
					t.Errorf("%s BODY[%s]: the server has no such section, Section returned %q, %v",
						name, provider.PathString(path), got, err)
				}
				continue
			}
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s BODY[%s]:\n got %q, %v\nwant %q", name, provider.PathString(path), got, err, want)
			}
		}
	}
}

func TestAnEmbeddedMessageIsOneSectionAndItsPartsAreSubsections(t *testing.T) {
	raw := fixture(t, "nested.eml")
	whole, err := mime.Section(raw, []int{2})
	if err != nil || !bytes.HasPrefix(whole, []byte("From: Carla <carla@example.org>\r\n")) {
		t.Fatalf("BODY[2] = %q, %v; want the embedded message with its header", whole, err)
	}
	for path, want := range map[string]string{"2.1": "Original text", "2.2": "<p>Original text</p>"} {
		var p []int
		for _, c := range path {
			if c != '.' {
				p = append(p, int(c-'0'))
			}
		}
		got, err := mime.Section(raw, p)
		if err != nil || string(got) != want {
			t.Fatalf("BODY[%s] = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, missing := range [][]int{{3}, {0}, {1, 1}, {2, 3}, {2, 1, 1}} {
		if _, err := mime.Section(raw, missing); !errors.Is(err, mime.ErrNoSection) {
			t.Fatalf("BODY[%s]: err = %v, want ErrNoSection", provider.PathString(missing), err)
		}
	}
	if got, err := mime.Section(raw, nil); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("BODY[] should be the whole message")
	}
}

func TestFixtureAttachmentsGetSafeNamesAndPassiveTypes(t *testing.T) {
	parts := fixtureParts(fixture(t, "mixed.eml"))
	cases := []struct {
		path, download, contentType string
		attachment                  bool
	}{
		{"2", "Relatório 2026.pdf", "application/pdf", true},
		{"3", "part-3.png", "image/png", false}, // inline, referenced by Content-ID
		{"4", "evil.html", mime.OctetStream, true},
		{"5", "invoicegpj.svg", mime.OctetStream, true},
	}
	for _, c := range cases {
		info := partAt(t, parts, c.path)
		if got := mime.DownloadName(info); got != c.download {
			t.Errorf("part %s: DownloadName = %q, want %q", c.path, got, c.download)
		}
		if got := mime.AttachmentContentType(info); got != c.contentType {
			t.Errorf("part %s: AttachmentContentType = %q, want %q", c.path, got, c.contentType)
		}
		if info.IsAttachment != c.attachment {
			t.Errorf("part %s: IsAttachment = %v, want %v", c.path, info.IsAttachment, c.attachment)
		}
	}
}

func TestAttachmentBytesSurviveTheTransferDecoder(t *testing.T) {
	raw := fixture(t, "mixed.eml")
	info := partAt(t, fixtureParts(raw), "2")
	section, err := mime.Section(raw, info.Path)
	if err != nil {
		t.Fatal(err)
	}
	r, known := mime.NewTransferDecoder(info.Encoding, bytes.NewReader(section))
	if !known {
		t.Fatalf("encoding %q unknown", info.Encoding)
	}
	var got bytes.Buffer
	if _, err := got.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	want := []byte("%PDF-1.4\n%\xe4\xfc\xf6\xdf\n1 0 obj\n<<>>\nendobj\n")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %q, want %q", got.Bytes(), want)
	}
}

func TestFixtureBodyPartsAreTheAlternativePair(t *testing.T) {
	cases := map[string][2]string{
		"mixed.eml":       {"1.1", "1.2"},
		"alternative.eml": {"1", "2"},
		"nested.eml":      {"1", "-"},
		"latin1-qp.eml":   {"1", "-"},
	}
	for name, want := range cases {
		parts := fixtureParts(fixture(t, name))
		plain, html := mime.BodyParts(parts)
		got := [2]string{"-", "-"}
		if plain != nil {
			got[0] = plain.PathString()
		}
		if html != nil {
			got[1] = html.PathString()
		}
		if got != want {
			t.Errorf("%s: BodyParts = %v, want %v", name, got, want)
		}
		if plain != nil && !slices.ContainsFunc(parts, func(p provider.PartInfo) bool { return p.PathString() == plain.PathString() }) {
			t.Errorf("%s: plain part is not one of the message's", name)
		}
	}
}
