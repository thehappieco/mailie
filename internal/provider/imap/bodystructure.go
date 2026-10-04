package imap

import (
	"strings"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/mime"
	"github.com/thehappieco/mailie/internal/provider"
)

// FlattenBodyStructure turns a BODYSTRUCTURE into a flat list of leaves.
//
// This is what lets the API answer "what is attached to this message" without
// downloading a byte: the server describes the MIME tree in the same FETCH
// that returns the envelope, and every leaf carries the path a later
// BODY[n.m] fetch needs, plus the encoding and charset required to decode that
// part on its own.
//
// Flattening here rather than storing the tree is deliberate. go-imap's
// BodyStructure is an interface over two concrete types, so it does not
// round-trip through JSON without a hand-written marshaller, and the only
// question anyone asks of it is "which path holds the text, and which paths
// are attachments".
func FlattenBodyStructure(bs imap.BodyStructure) []provider.PartInfo {
	if bs == nil {
		return nil
	}
	var out []provider.PartInfo
	// Walk visits the root too, and a root that is a single part has an empty
	// path; IMAP addresses that part as BODY[1].
	bs.Walk(func(path []int, part imap.BodyStructure) bool {
		single, ok := part.(*imap.BodyStructureSinglePart)
		if !ok {
			// A multipart node is structure, not content. Keep walking.
			return true
		}
		info := partInfo(path, single)
		out = append(out, info)
		// A message/rfc822 part has its own tree inside it. Walk does not
		// descend into it, and neither do we: a forwarded message is fetched
		// and parsed as a message when someone asks for it, rather than
		// flattened into its parent's part list where its paths would collide.
		return false
	})
	markBodies(out)
	return out
}

func partInfo(path []int, part *imap.BodyStructureSinglePart) provider.PartInfo {
	if len(path) == 0 {
		// The whole message is one part: IMAP calls it section 1.
		path = []int{1}
	}
	info := provider.PartInfo{
		Path:     append([]int(nil), path...),
		MIMEType: strings.ToLower(part.MediaType()),
		Params:   part.Params,
		Encoding: strings.ToLower(part.Encoding),
		//nolint:gosec // G115: a MIME part larger than 2 GiB is not something
		// this server fetches, and the value is only ever advisory
		Size:      int64(part.Size),
		ContentID: strings.Trim(part.ID, "<>"),
		Filename:  part.Filename(),
	}
	var dispositionParams map[string]string
	if d := part.Disposition(); d != nil {
		info.Disposition = strings.ToLower(d.Value)
		dispositionParams = d.Params
	}
	if info.Filename == "" {
		// go-imap reads only the plain filename and name parameters. A name
		// with anything but ASCII in it is often sent only in the RFC 2231
		// form (filename*=utf-8''...), which it leaves raw.
		info.Filename = mime.FilenameFromParams(dispositionParams, part.Params)
	}
	info.IsAttachment = looksLikeAttachment(info)
	return info
}

// looksLikeAttachment decides what a person would call an attachment.
//
// Not the Content-Disposition alone: plenty of mail attaches a PDF with no
// disposition at all, and plenty marks an inline logo "attachment". The rule
// that matches what people expect is: an explicit attachment disposition, or
// anything non-text that carries a filename, or any non-text leaf that is not
// an inline image referenced by the body.
func looksLikeAttachment(info provider.PartInfo) bool {
	switch info.Disposition {
	case "attachment":
		return true
	case "inline":
		// An inline part with a Content-ID is referenced from the HTML; an
		// inline part without one is usually just a file.
		return info.ContentID == "" && info.Filename != ""
	}
	if strings.HasPrefix(info.MIMEType, "text/") {
		return false
	}
	if info.MIMEType == "message/rfc822" {
		return true
	}
	return info.Filename != "" || info.ContentID == ""
}

// markBodies picks the parts that could be the message body.
//
// The first text/plain and the first text/html that are not attachments. A
// later text/plain is usually a signature block or a forwarded quote, and
// treating it as the body is how a reply ends up showing the wrong text.
func markBodies(parts []provider.PartInfo) {
	var havePlain, haveHTML bool
	for i := range parts {
		if parts[i].IsAttachment {
			continue
		}
		switch parts[i].MIMEType {
		case "text/plain":
			if !havePlain {
				parts[i].IsBody = true
				havePlain = true
			}
		case "text/html":
			if !haveHTML {
				parts[i].IsBody = true
				haveHTML = true
			}
		}
	}
}

// Charset reports a part's charset, defaulting to us-ascii the way MIME does.
func Charset(info provider.PartInfo) string {
	if cs, ok := info.Params["charset"]; ok && cs != "" {
		return strings.ToLower(cs)
	}
	if cs, ok := info.Params["CHARSET"]; ok && cs != "" {
		return strings.ToLower(cs)
	}
	return "us-ascii"
}
