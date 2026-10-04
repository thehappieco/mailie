package mime

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	stdmime "mime"
	"strings"

	"github.com/emersion/go-message/textproto"
)

// ErrNoSection is returned when a message has no part at the given path.
var ErrNoSection = errors.New("mime: no such section")

// Section returns what an IMAP server answers for BODY[<path>] of a raw
// message: the part's body as it sits in the message, still transfer-encoded,
// without its MIME header. It is the other half of the part paths
// BODYSTRUCTURE hands out, for code that holds a whole message — a fake mail
// server in tests — and has to serve one section of it.
//
// Paths are 1-based, as in IMAP. A message that is not multipart has a single
// part, 1, which is its body. A multipart's part n is its n-th child. Inside a
// message/rfc822 part, the path continues into the embedded message the same
// way. An empty path is the whole message.
func Section(raw []byte, path []int) ([]byte, error) {
	if len(path) == 0 {
		return raw, nil
	}
	header, body, err := splitEntity(raw)
	if err != nil {
		return nil, err
	}
	return section(header, body, path, "text/plain")
}

// section resolves path against one entity. defaultType is what the entity's
// Content-Type is when it has none: text/plain, except for the children of a
// multipart/digest, which default to message/rfc822.
func section(header textproto.Header, body []byte, path []int, defaultType string) ([]byte, error) {
	mt, params := contentType(header, defaultType)
	n := path[0]
	if n < 1 {
		return nil, fmt.Errorf("%w: part numbers start at 1", ErrNoSection)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		// A single-part entity has exactly one part: its body.
		if n != 1 {
			return nil, ErrNoSection
		}
		if len(path) == 1 {
			return body, nil
		}
		if !isEmbeddedMessage(mt) {
			return nil, ErrNoSection
		}
		inner, innerBody, err := splitEntity(body)
		if err != nil {
			return nil, err
		}
		return section(inner, innerBody, path[1:], "text/plain")
	}

	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("%w: multipart without a boundary", ErrNoSection)
	}
	childDefault := "text/plain"
	if mt == "multipart/digest" {
		childDefault = "message/rfc822"
	}
	reader := textproto.NewMultipartReader(bytes.NewReader(body), boundary)
	for i := 1; ; i++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoSection
		}
		if err != nil {
			return nil, fmt.Errorf("mime: read multipart: %w", err)
		}
		if i < n {
			continue
		}
		content, err := io.ReadAll(part)
		if err != nil {
			return nil, fmt.Errorf("mime: read part %d: %w", i, err)
		}
		if len(path) == 1 {
			return content, nil
		}
		childType, _ := contentType(part.Header, childDefault)
		switch {
		case strings.HasPrefix(childType, "multipart/"):
			return section(part.Header, content, path[1:], childDefault)
		case isEmbeddedMessage(childType):
			inner, innerBody, err := splitEntity(content)
			if err != nil {
				return nil, err
			}
			return section(inner, innerBody, path[1:], "text/plain")
		default:
			return nil, ErrNoSection
		}
	}
}

// splitEntity separates a header block from the body after it.
func splitEntity(raw []byte) (textproto.Header, []byte, error) {
	br := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return textproto.Header{}, nil, fmt.Errorf("mime: read header: %w", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		return textproto.Header{}, nil, fmt.Errorf("mime: read body: %w", err)
	}
	return header, body, nil
}

// contentType parses a Content-Type leniently: a missing or unparsable one is
// the default, as RFC 2045 says.
func contentType(header textproto.Header, defaultType string) (string, map[string]string) {
	value := header.Get("Content-Type")
	if value == "" {
		return defaultType, nil
	}
	mt, params, err := stdmime.ParseMediaType(value)
	if err != nil && mt == "" {
		return defaultType, nil
	}
	return strings.ToLower(mt), params
}

func isEmbeddedMessage(mt string) bool {
	return mt == "message/rfc822" || mt == "message/global"
}
