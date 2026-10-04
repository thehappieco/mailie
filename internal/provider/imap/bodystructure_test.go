package imap_test

import (
	"testing"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
	imapprovider "github.com/thehappieco/mailie/internal/provider/imap"
)

// text builds a text leaf.
func text(subtype, encoding string, size uint32, params map[string]string) *goimap.BodyStructureSinglePart {
	return &goimap.BodyStructureSinglePart{
		Type: "text", Subtype: subtype, Encoding: encoding, Size: size, Params: params,
		Text: &goimap.BodyStructureText{},
	}
}

func file(mediaType, subtype, filename, disposition, contentID string, size uint32) *goimap.BodyStructureSinglePart {
	part := &goimap.BodyStructureSinglePart{
		Type: mediaType, Subtype: subtype, Encoding: "base64", Size: size, ID: contentID,
	}
	if disposition != "" || filename != "" {
		part.Extended = &goimap.BodyStructureSinglePartExt{
			Disposition: &goimap.BodyStructureDisposition{
				Value:  disposition,
				Params: map[string]string{"filename": filename},
			},
		}
	}
	return part
}

func TestASinglePartMessageIsSectionOne(t *testing.T) {
	// The walk gives the root an empty path; IMAP addresses it as BODY[1], and
	// fetching BODY[] instead returns the headers too.
	parts := imapprovider.FlattenBodyStructure(text("plain", "7bit", 42, nil))

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	if got := parts[0].PathString(); got != "1" {
		t.Fatalf("path = %q, want 1", got)
	}
	if !parts[0].IsBody {
		t.Error("the only text part is the body")
	}
	if parts[0].IsAttachment {
		t.Error("a bare text/plain message is not an attachment")
	}
}

func TestANestedMessageYieldsIMAPPathsThatCanBeFetched(t *testing.T) {
	// multipart/mixed( multipart/alternative( text/plain, text/html ), application/pdf )
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []goimap.BodyStructure{
			&goimap.BodyStructureMultiPart{
				Subtype: "alternative",
				Children: []goimap.BodyStructure{
					text("plain", "quoted-printable", 100, map[string]string{"charset": "utf-8"}),
					text("html", "base64", 200, map[string]string{"charset": "utf-8"}),
				},
			},
			file("application", "pdf", "fatura.pdf", "attachment", "", 50000),
		},
	}

	parts := imapprovider.FlattenBodyStructure(structure)
	var paths []string
	for _, p := range parts {
		paths = append(paths, p.PathString())
	}
	want := []string{"1.1", "1.2", "2"}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("paths = %v, want %v", paths, want)
		}
	}
}

func TestTheFirstTextPartOfEachKindIsTheBody(t *testing.T) {
	// A later text/plain is usually a signature or a quoted reply. Treating it
	// as the body shows the wrong text.
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []goimap.BodyStructure{
			text("plain", "7bit", 100, nil),
			text("html", "7bit", 200, nil),
			text("plain", "7bit", 20, nil),
		},
	}

	parts := imapprovider.FlattenBodyStructure(structure)
	if !parts[0].IsBody || !parts[1].IsBody {
		t.Fatal("the first text/plain and the first text/html are the body")
	}
	if parts[2].IsBody {
		t.Fatal("a second text/plain is not the body")
	}
}

func TestAnAttachmentIsRecognisedByDispositionOrByBeingAFile(t *testing.T) {
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []goimap.BodyStructure{
			text("plain", "7bit", 100, nil),
			// The explicit case.
			file("application", "pdf", "fatura.pdf", "attachment", "", 1000),
			// No disposition at all, which plenty of mail does.
			file("image", "jpeg", "", "", "", 2000),
		},
	}

	parts := imapprovider.FlattenBodyStructure(structure)
	if parts[0].IsAttachment {
		t.Error("the text body is not an attachment")
	}
	if !parts[1].IsAttachment {
		t.Error("a part disposed as an attachment is one")
	}
	if !parts[2].IsAttachment {
		t.Error("a non-text part with no disposition is still a file to a person")
	}
}

func TestAnInlineImageReferencedFromTheBodyIsNotAnAttachment(t *testing.T) {
	// A logo the HTML pulls in with cid: should not appear in the attachment
	// list beside the invoice the person actually attached.
	structure := &goimap.BodyStructureMultiPart{
		Subtype: "related",
		Children: []goimap.BodyStructure{
			text("html", "7bit", 300, nil),
			file("image", "png", "logo.png", "inline", "<logo@example.com>", 4000),
		},
	}

	parts := imapprovider.FlattenBodyStructure(structure)
	if parts[1].IsAttachment {
		t.Error("an inline part with a Content-ID is referenced from the body")
	}
	if parts[1].ContentID != "logo@example.com" {
		t.Errorf("ContentID = %q, want the angle brackets stripped", parts[1].ContentID)
	}
}

func TestAForwardedMessageIsOneAttachmentNotItsContents(t *testing.T) {
	// Descending into message/rfc822 would flatten its parts into the
	// parent's list, where their paths mean something else entirely.
	inner := &goimap.BodyStructureSinglePart{
		Type: "message", Subtype: "rfc822", Encoding: "7bit", Size: 5000,
		MessageRFC822: &goimap.BodyStructureMessageRFC822{
			BodyStructure: &goimap.BodyStructureMultiPart{
				Subtype:  "alternative",
				Children: []goimap.BodyStructure{text("plain", "7bit", 10, nil)},
			},
		},
	}
	structure := &goimap.BodyStructureMultiPart{
		Subtype:  "mixed",
		Children: []goimap.BodyStructure{text("plain", "7bit", 100, nil), inner},
	}

	parts := imapprovider.FlattenBodyStructure(structure)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want the forwarded message counted once: %v", len(parts), parts)
	}
	if !parts[1].IsAttachment || parts[1].MIMEType != "message/rfc822" {
		t.Errorf("the forwarded message = %+v", parts[1])
	}
}

func TestTheEncodingAndCharsetNeededToDecodeAPartAreKept(t *testing.T) {
	// A part fetched on its own arrives still transfer-encoded and in its own
	// charset; without these two values it cannot be turned back into text.
	parts := imapprovider.FlattenBodyStructure(
		text("plain", "QUOTED-PRINTABLE", 100, map[string]string{"charset": "ISO-8859-1"}))

	if parts[0].Encoding != "quoted-printable" {
		t.Errorf("encoding = %q, want it lowercased", parts[0].Encoding)
	}
	if got := imapprovider.Charset(parts[0]); got != "iso-8859-1" {
		t.Errorf("charset = %q", got)
	}
}

func TestAPartWithNoCharsetDefaultsTheWayMIMEDoes(t *testing.T) {
	parts := imapprovider.FlattenBodyStructure(text("plain", "7bit", 10, nil))
	if got := imapprovider.Charset(parts[0]); got != "us-ascii" {
		t.Fatalf("charset = %q, want us-ascii", got)
	}
}

func TestANilBodyStructureYieldsNoParts(t *testing.T) {
	if parts := imapprovider.FlattenBodyStructure(nil); parts != nil {
		t.Fatalf("got %v", parts)
	}
}

func TestPartInfoSurvivesARoundTripThroughItsPathString(t *testing.T) {
	info := provider.PartInfo{Path: []int{2, 1}}
	if got := info.PathString(); got != "2.1" {
		t.Fatalf("PathString = %q", got)
	}
}

func TestAFilenameSentOnlyInItsRFC2231FormIsKept(t *testing.T) {
	// go-imap reads the plain filename and name parameters only. A name with
	// anything but ASCII in it often arrives only as filename*=, and without
	// this the attachment would be listed as "part-2.pdf".
	pdf := &goimap.BodyStructureSinglePart{
		Type: "application", Subtype: "pdf", Encoding: "base64", Size: 5000,
		Extended: &goimap.BodyStructureSinglePartExt{
			Disposition: &goimap.BodyStructureDisposition{
				Value:  "attachment",
				Params: map[string]string{"filename*": "utf-8''Relat%C3%B3rio%202026.pdf"},
			},
		},
	}
	named := &goimap.BodyStructureSinglePart{
		Type: "application", Subtype: "pdf", Encoding: "base64", Size: 5000,
		Params: map[string]string{"name*0*": "utf-8''Or%C3%A7a", "name*1": "mento.pdf"},
	}
	parts := imapprovider.FlattenBodyStructure(&goimap.BodyStructureMultiPart{
		Subtype:  "mixed",
		Children: []goimap.BodyStructure{text("plain", "7bit", 10, nil), pdf, named},
	})
	if len(parts) != 3 {
		t.Fatalf("got %d parts", len(parts))
	}
	if got := parts[1].Filename; got != "Relatório 2026.pdf" {
		t.Errorf("filename* = %q, want it decoded", got)
	}
	if got := parts[2].Filename; got != "Orçamento.pdf" {
		t.Errorf("name*0*/name*1 = %q, want the continuations joined and decoded", got)
	}
	if !parts[1].IsAttachment || !parts[2].IsAttachment {
		t.Error("a named PDF is an attachment")
	}
}
