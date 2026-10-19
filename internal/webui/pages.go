package webui

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Pages is what a binary that embeds the daemon adds to how the console is
// served: headers of its own on the page documents it chooses, and small
// files of its own beside them. `serve` sets none of it, and with none of it
// set every answer is what it always was.
//
// Nothing here can make a page less protected than the handler serves it.
// The headers that protect the page or say what the answer is are the
// handler's own, and a document's own header for any of them is ignored
// (protectedHeader); a referrer policy that would send another origin the
// page's path or query is ignored too (privateReferrerPolicy); and a
// document answered whole is never stored.
type Pages struct {
	// Document, when set, is asked about every page document the handler
	// answers: the console's page, served for a GET or HEAD of a path
	// without an extension that is no file of the build, "/" included (never
	// for a file, an API path, a refusal or a 404). It is given the request
	// and answers what that document carries besides the handler's own: a
	// header that depends on the path, or on who asked (the request's
	// Referer, say), is decided there.
	Document func(r *http.Request) Document
	// Files are small files served at their path exactly, for GET and HEAD,
	// from memory, each with its own content type, never stored
	// (Cache-Control: no-store) and always answered whole. A path is a
	// clean absolute path with an extension, of letters, digits and
	// "-._~/", never "/index.html", under "/assets/" (the build's, named by
	// content) or the API's; one the build also has is served from here.
	Files map[string]File
}

// Document is what one page document carries besides the handler's own
// headers.
type Document struct {
	// Header is set on the answer, name by name, after the handler's own
	// headers, so it may replace Referrer-Policy (with a policy that never
	// sends another origin the page's path or query: the address of a
	// provider's return carries a code, and a document's own meta tag
	// applies after the header) or add any other header, but never one the
	// handler protects the page or describes the answer with, which stays
	// as the handler set it.
	Header http.Header
	// Whole answers the document whole and never stored: Cache-Control is
	// no-store in place of no-cache, and the request's conditional and
	// range headers are ignored, so the answer is never a 304 nor a part.
	// A document whose headers depend on who asked needs it: a stored copy
	// revalidated with a 304 would keep the headers of the answer it was
	// stored from for whoever asks next.
	Whole bool
}

// File is a small file Pages serves from memory.
type File struct {
	// ContentType is its media type, as Content-Type says it.
	ContentType string
	// Body is the file as it is served.
	Body []byte
}

// ErrPages says Pages asked for something the handler does not serve.
var ErrPages = errors.New("webui: pages")

// Check reports what in p the handler would not serve, ErrPages wrapped:
// a file at a path it does not take, or without a media type.
func (p Pages) Check() error {
	for name, file := range p.Files {
		if err := checkFilePath(name); err != nil {
			return fmt.Errorf("%w: a file at %q: %w", ErrPages, name, err)
		}
		media, _, err := mime.ParseMediaType(file.ContentType)
		if err != nil {
			return fmt.Errorf("%w: a file at %q: content type %q: %w", ErrPages, name, file.ContentType, err)
		}
		if kind, sub, ok := strings.Cut(media, "/"); !ok || kind == "" || sub == "" {
			return fmt.Errorf("%w: a file at %q: content type %q is no type/subtype", ErrPages, name, file.ContentType)
		}
	}
	return nil
}

// checkFilePath says why a page file cannot be served at name, or nil.
func checkFilePath(name string) error {
	switch {
	case !strings.HasPrefix(name, "/") || path.Clean(name) != name || name == "/":
		return errors.New("not a clean absolute path")
	case strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~/") != "":
		return errors.New("only letters, digits and -._~/")
	case path.Ext(name) == "":
		// Without one it would be a page route of the console.
		return errors.New("no extension")
	case name == "/index.html":
		return errors.New("the console's page")
	case strings.HasPrefix(name, "/assets/"):
		return errors.New("the build's own, named by their content")
	case isAPIPath(name):
		return errors.New("the API's")
	}
	return nil
}

// protectedHeaders are the headers a page document carries as the handler
// sets them, whatever Document says: what protects the page (headers), what
// says what the answer is and how long it is kept, and cookies, since the
// console's session is a bearer token and never a cookie.
var protectedHeaders = []string{
	"Content-Security-Policy", "X-Content-Type-Options", "Cross-Origin-Opener-Policy",
	"Cross-Origin-Resource-Policy", "X-Frame-Options", "Permissions-Policy",
	"Cache-Control", "Content-Type", "Content-Length", "Content-Encoding", "Content-Range", "Content-Disposition",
	"Accept-Ranges", "Last-Modified", "Etag", "Set-Cookie",
}

func protectedHeader(name string) bool {
	canonical := http.CanonicalHeaderKey(name)
	for _, protected := range protectedHeaders {
		if canonical == protected {
			return true
		}
	}
	return false
}

// privateReferrerPolicies never send another origin more than the page's
// origin: its path and query, where a provider's code may be, stay home.
var privateReferrerPolicies = []string{
	"no-referrer", "same-origin", "origin", "strict-origin", "origin-when-cross-origin",
	"strict-origin-when-cross-origin",
}

// privateReferrerPolicy reports whether values are one policy of
// privateReferrerPolicies.
func privateReferrerPolicy(values []string) bool {
	if len(values) != 1 {
		return false
	}
	policy := strings.ToLower(strings.TrimSpace(values[0]))
	for _, private := range privateReferrerPolicies {
		if policy == private {
			return true
		}
	}
	return false
}

// conditionalHeaders make a request ask for less than the whole page.
var conditionalHeaders = []string{
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range", "Range",
}

// document applies what p's Document says of r's page document to w's
// header, and returns the request to answer it with: r itself, or, for a
// document answered whole, r without its conditional and range headers.
func (p Pages) document(w http.ResponseWriter, r *http.Request) *http.Request {
	if p.Document == nil {
		return r
	}
	d := p.Document(r)
	header := w.Header()
	for name, values := range d.Header {
		switch {
		case protectedHeader(name):
		case http.CanonicalHeaderKey(name) == "Referrer-Policy" && !privateReferrerPolicy(values):
		default:
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	if !d.Whole {
		return r
	}
	header.Set("Cache-Control", "no-store")
	whole := r.Clone(r.Context())
	for _, name := range conditionalHeaders {
		whole.Header.Del(name)
	}
	return whole
}

// serveFile answers a file of Pages.Files, whole, never stored.
func serveFile(w http.ResponseWriter, r *http.Request, file File) {
	header := w.Header()
	header.Set("Content-Type", file.ContentType)
	header.Set("Content-Length", strconv.Itoa(len(file.Body)))
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		//nolint:errcheck // the client hung up; there is nothing left to say to it
		_, _ = w.Write(file.Body)
	}
}
