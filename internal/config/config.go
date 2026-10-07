// Package config loads configuration from the environment.
//
// Environment only, deliberately. Every secret this server holds — the key that
// unwraps stored OAuth refresh tokens above all — is one file away from being
// committed the moment configuration lives in a file that is pleasant to edit.
// Values arrive through the process environment, are never written back out,
// and String redacts them so a startup log cannot leak one.
//
// Accounts are not configuration: they are rows, created through the API, with
// their credentials encrypted at rest. What lives here is the handful of
// settings a deployment chooses once — where the data directory is, which OAuth
// client to authenticate as, what to listen on.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type Env string

const (
	EnvDev  Env = "dev"
	EnvProd Env = "prod"
)

func (e Env) IsProd() bool { return e == EnvProd }

type Config struct {
	Env Env

	// HTTPAddr carries the REST API, the SSE stream and the MCP endpoint.
	// It defaults to loopback: this daemon can read every message in several
	// mailboxes and send mail as their owner, so binding it to the world is a
	// decision someone has to make on purpose.
	HTTPAddr string
	// MetricsAddr serves /metrics on a listener of its own when set, keeping
	// operational data off the port that answers mail queries.
	MetricsAddr string
	// TrustedProxies are the addresses whose X-Forwarded-For header names the
	// real client. Empty means the connection's own address is the client,
	// which is right for a loopback daemon and wrong behind a reverse proxy,
	// where every caller would otherwise share one rate limit.
	TrustedProxies []netip.Prefix

	// DataDir holds mail.db (plus its -wal and -shm), the lock file that stops
	// two daemons syncing the same database, and the attachment cache.
	DataDir string

	Credentials Credentials
	Google      OAuthClient
	Microsoft   OAuthClient
	// GoogleWeb and MicrosoftWeb are the console's OAuth registrations: web
	// clients with a real secret and a redirect to PublicURL. They sit beside
	// the installed clients above rather than replacing them, because a
	// refresh token belongs to the client that issued it and the CLI's
	// loopback flow needs a client that allows loopback redirects.
	GoogleWeb    OAuthClient
	MicrosoftWeb OAuthClient
	Cache        Cache
	Webhooks     Webhooks
	Log          Log

	// DownloadSpoolBytes bounds what originals and attachments being
	// downloaded hold in the spool under DataDir at once. Each is fetched
	// whole before its first byte is sent and kept until the client has it,
	// on the disk that also holds the database; past this, a download is
	// answered 429 until another ends.
	DownloadSpoolBytes int64

	// WebDir is the built console (web/dist, or the cloud app's build on the
	// hosted service). Empty serves the API only, which is a perfectly good
	// deployment.
	WebDir string
	// PublicURL is the origin a browser uses to reach the console, such as
	// https://console.mailie.example. It is the single source for the
	// web OAuth redirect and for invite links, and is never derived from a
	// request's Host or X-Forwarded-* headers, which the caller controls.
	PublicURL string
	// ConnectSrc are origins the console's pages may connect to besides
	// their own (MAIL_CONNECT_SRC), added to the connect-src of the policy
	// internal/webui sends: each a scheme and a host (a name or an IPv4
	// address: a CSP source cannot hold an IPv6 one), optionally a port, as
	// a browser writes an origin. Empty, the default, leaves connect-src at
	// 'self', where the console and the API share one origin and a page
	// talks to nothing else.
	ConnectSrc []string
	// AccountAllowPrivate lets mail accounts point at loopback, private,
	// link-local or CGNAT addresses. Off by default: a host somebody typed
	// into the console is a server-side connection to wherever they chose,
	// which is the shape of every SSRF.
	AccountAllowPrivate bool

	// AdminAPI mounts the instance-key routes (/v1/apikeys). Off by default:
	// an endpoint that mints credentials is not something to leave answering
	// by accident. `apikey create|list|revoke` are clients of these routes;
	// with them off, only `apikey create --bootstrap`, with the daemon
	// stopped, issues a key.
	AdminAPI bool
	// AdminKey is the key the CLI presents to the daemon. The CLI is a REST
	// client because only the daemon holds the sync workers, the token sources
	// and the OAuth callback listener.
	AdminKey string
	// MCPKey is the API key `serve --mcp-stdio` acts with: an MCP client
	// that launches the daemon has no Authorization header to send, so the
	// key it would have sent comes from here. Verified at start like any
	// bearer key, and never printed.
	MCPKey string

	// MicrosoftDeviceCode offers the device-code flow for Microsoft accounts.
	// Off by default because Entra security defaults block it outright, and a
	// flow that fails for most tenants is worse than no flow at all.
	MicrosoftDeviceCode bool
	// MCPHTTP serves MCP over Streamable HTTP at /mcp, beside the REST API.
	// On by default. Off, nothing is mounted there and /mcp answers the
	// API's JSON 404: MCP is then only `serve --mcp-stdio`, for a deployment
	// whose MCP clients must not reach it over the network with a key.
	MCPHTTP bool
	// KeysMaySend lets a workspace's API keys send email, where the key
	// holds the send flag on the mailbox and has the send scope. On by
	// default under the open console's key terms, which say a key may send.
	// Off, for an edition whose key terms do not cover sending, the send
	// scope is refused when a key is created and every send by a workspace
	// key is refused; instance keys are the operator's and are not affected.
	// A deployment whose MAIL_CONSENT_VERSION_KEYS names other terms sets it
	// itself: only those terms say whether a key may send, so no default
	// stands for them.
	KeysMaySend bool
	// KeysActUnderCreatorConsent holds a workspace key's actions to its
	// creator's own actions consent as well: the key acts only while the
	// person who created it allows actions at the current revision (and is
	// active). Off by default, under the open console's key terms, which
	// say the key terms cover what a key does. On, for an edition whose key
	// terms promise that a key acts only while its person allows actions.
	// Like MAIL_KEYS_MAY_SEND, a deployment whose MAIL_CONSENT_VERSION_KEYS
	// names other terms sets it itself.
	KeysActUnderCreatorConsent bool
	// Consent names the revisions of the texts a person agrees to in the
	// console (see ConsentVersions).
	Consent ConsentVersions
	// MCPElicitSend asks MCP clients that support elicitation to confirm a
	// send interactively. Off by default: it blocks non-interactive runs, and
	// confirm=true is already the gate.
	MCPElicitSend bool
	// IMAPDebug tees the raw IMAP protocol to the log. It carries the XOAUTH2
	// line, which contains a bearer token, so Load refuses it in prod.
	IMAPDebug bool
}

// Credentials is the keyring that wraps stored account credentials.
//
// Keys are numbered because the envelope carries the id that sealed it: adding
// a new key and rewrapping rows is then a background job rather than a flag
// day, and a daemon can read rows written under keys it no longer encrypts
// with.
type Credentials struct {
	ActiveKeyID uint8
	// Keys maps key id to a 32-byte AES-256 key, including the active one.
	Keys map[uint8][]byte
}

// OAuthClient is a client registration. For the installed clients the Google
// "Desktop app" secret is stored because Google issues one, but it protects
// nothing: the loopback redirect plus PKCE is what makes the flow safe. The web
// clients' secrets are real ones.
type OAuthClient struct {
	ClientID     string
	ClientSecret string
	Tenant       string // Microsoft only; "common" covers work and personal accounts
}

func (c OAuthClient) Configured() bool { return c.ClientID != "" }

type Cache struct {
	// BodyMaxBytes bounds the cached message bodies held in the database.
	BodyMaxBytes int64
	// AttachMaxBytes bounds the attachment files on disk.
	AttachMaxBytes int64
}

type Webhooks struct {
	// AllowHTTP permits cleartext webhook URLs. Payloads carry subjects and
	// sender addresses, so this is a development convenience.
	AllowHTTP bool
	// AllowPrivate permits webhook URLs that resolve to loopback or private
	// address space. Off by default: a webhook is a server-side fetch of a
	// user-supplied URL, which is the shape of every SSRF.
	AllowPrivate bool
}

type Log struct {
	Level  string // debug | info | warn | error
	Format string // json | text
}

const (
	defaultHTTPAddr       = "127.0.0.1:8080"
	defaultBodyCacheBytes = 512 << 20
	defaultAttachBytes    = 2 << 30
	defaultDownloadSpool  = 1 << 30
	credentialKeyLen      = 32
)

// Load reads configuration from the environment and validates it.
//
// It returns every problem at once rather than the first: a misconfigured
// deployment should be fixable in one pass, not one restart per typo.
func Load() (Config, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	env := Env(str("MAIL_ENV", string(EnvDev)))
	if env != EnvDev && env != EnvProd {
		bad("MAIL_ENV: want %q or %q, got %q", EnvDev, EnvProd, env)
	}

	cfg := Config{
		Env:                        env,
		HTTPAddr:                   str("MAIL_HTTP_ADDR", defaultHTTPAddr),
		MetricsAddr:                str("MAIL_METRICS_ADDR", ""),
		TrustedProxies:             prefixes("MAIL_TRUSTED_PROXIES", &errs),
		DataDir:                    dataDir(&errs),
		Credentials:                credentials(&errs),
		Google:                     OAuthClient{ClientID: str("MAIL_GOOGLE_CLIENT_ID", ""), ClientSecret: str("MAIL_GOOGLE_CLIENT_SECRET", "")},
		Microsoft:                  OAuthClient{ClientID: str("MAIL_MICROSOFT_CLIENT_ID", ""), Tenant: str("MAIL_MICROSOFT_TENANT", "common")},
		GoogleWeb:                  OAuthClient{ClientID: str("MAIL_GOOGLE_WEB_CLIENT_ID", ""), ClientSecret: str("MAIL_GOOGLE_WEB_CLIENT_SECRET", "")},
		MicrosoftWeb:               OAuthClient{ClientID: str("MAIL_MICROSOFT_WEB_CLIENT_ID", ""), ClientSecret: str("MAIL_MICROSOFT_WEB_CLIENT_SECRET", ""), Tenant: str("MAIL_MICROSOFT_TENANT", "common")},
		WebDir:                     webDir(&errs),
		PublicURL:                  publicURL(env, &errs),
		ConnectSrc:                 connectSrc(env, &errs),
		AccountAllowPrivate:        boolean("MAIL_ACCOUNT_ALLOW_PRIVATE", false, &errs),
		Cache:                      Cache{BodyMaxBytes: bytes("MAIL_CACHE_BODY_MAX_BYTES", defaultBodyCacheBytes, &errs), AttachMaxBytes: bytes("MAIL_CACHE_ATTACH_MAX_BYTES", defaultAttachBytes, &errs)},
		DownloadSpoolBytes:         bytes("MAIL_DOWNLOAD_SPOOL_MAX_BYTES", defaultDownloadSpool, &errs),
		Webhooks:                   Webhooks{AllowHTTP: boolean("MAIL_WEBHOOK_ALLOW_HTTP", false, &errs), AllowPrivate: boolean("MAIL_WEBHOOK_ALLOW_PRIVATE", false, &errs)},
		Log:                        Log{Level: str("MAIL_LOG_LEVEL", "info"), Format: str("MAIL_LOG_FORMAT", defaultLogFormat(env))},
		AdminAPI:                   boolean("MAIL_ADMIN_API", false, &errs),
		AdminKey:                   str("MAIL_ADMIN_KEY", ""),
		MCPKey:                     str("MAIL_MCP_KEY", ""),
		MicrosoftDeviceCode:        boolean("MAIL_MICROSOFT_DEVICE_CODE", false, &errs),
		MCPHTTP:                    boolean("MAIL_MCP_HTTP", true, &errs),
		KeysMaySend:                boolean("MAIL_KEYS_MAY_SEND", true, &errs),
		KeysActUnderCreatorConsent: boolean("MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT", false, &errs),
		Consent:                    consentVersions(&errs),
		MCPElicitSend:              boolean("MAIL_MCP_ELICIT_SEND", false, &errs),
		IMAPDebug:                  boolean("MAIL_IMAP_DEBUG", false, &errs),
	}

	errs = append(errs, cfg.Log.validate()...)

	// The open key terms say a key may send; another edition's say what
	// they say, and a daemon serving them is told, never assumed.
	if v, _ := os.LookupEnv("MAIL_KEYS_MAY_SEND"); v == "" && cfg.Consent.Keys != DefaultKeyTermsVersion {
		bad("MAIL_KEYS_MAY_SEND: set it to true or false: MAIL_CONSENT_VERSION_KEYS names key terms other " +
			"than the open console's, and only those terms say whether an API key may send")
	}
	if v, _ := os.LookupEnv("MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT"); v == "" && cfg.Consent.Keys != DefaultKeyTermsVersion {
		bad("MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT: set it to true or false: MAIL_CONSENT_VERSION_KEYS names key terms " +
			"other than the open console's, and only those terms say whether a key acts under its creator's actions consent")
	}

	// Google issues a client secret for Desktop clients and the token endpoint
	// may ask for it, so it is accepted — but on its own it configures nothing.
	if cfg.Google.ClientSecret != "" && cfg.Google.ClientID == "" {
		bad("MAIL_GOOGLE_CLIENT_SECRET is set without MAIL_GOOGLE_CLIENT_ID")
	}
	if cfg.Microsoft.Tenant == "" {
		bad("MAIL_MICROSOFT_TENANT: must not be empty (use \"common\" for work and personal accounts)")
	}

	// A web client is a confidential client: without its secret the token
	// endpoint refuses every code, and without an origin there is nowhere to
	// redirect to. Half a configuration is an error rather than a flow that
	// fails at the last step of somebody's consent.
	for _, web := range []struct {
		prefix string
		client OAuthClient
	}{
		{"MAIL_GOOGLE_WEB_CLIENT", cfg.GoogleWeb},
		{"MAIL_MICROSOFT_WEB_CLIENT", cfg.MicrosoftWeb},
	} {
		if (web.client.ClientID == "") != (web.client.ClientSecret == "") {
			bad("%s_ID and %s_SECRET: set both or neither", web.prefix, web.prefix)
		}
		// Only when it is unset: a malformed one has its own complaint already,
		// and a second one here would send the operator looking for a
		// variable they did set.
		if web.client.ClientID != "" && cfg.PublicURL == "" && str("MAIL_PUBLIC_URL", "") == "" {
			bad("%s_ID: a web client needs MAIL_PUBLIC_URL, the origin its redirect points at", web.prefix)
		}
	}

	if env.IsProd() {
		if cfg.IMAPDebug {
			bad("MAIL_IMAP_DEBUG: the raw IMAP trace contains the XOAUTH2 bearer token " +
				"and is not allowed in prod")
		}
		if cfg.Webhooks.AllowHTTP {
			bad("MAIL_WEBHOOK_ALLOW_HTTP: refusing to deliver message metadata in cleartext in prod")
		}
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// webDir resolves the built console's directory. Whether it holds a console is
// checked when the daemon starts, which carries on without one.
func webDir(errs *[]error) string {
	v := str("MAIL_WEB_DIR", "")
	if v == "" {
		return ""
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("MAIL_WEB_DIR: %w", err))
		return v
	}
	return abs
}

// publicURL reads the console's origin: a scheme and a host, optionally a
// port, and nothing else. A path would make "the origin" ambiguous for the
// OAuth redirect and the invite links built from it, and cleartext is only
// accepted where it never crosses a network.
func publicURL(env Env, errs *[]error) string {
	raw := str("MAIL_PUBLIC_URL", "")
	if raw == "" {
		return ""
	}
	bad := func(format string, a ...any) string {
		*errs = append(*errs, fmt.Errorf("MAIL_PUBLIC_URL: "+format, a...))
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return bad("%q is not a URL", raw)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return bad("want http:// or https://, got %q", raw)
	case u.Host == "" || u.Hostname() == "":
		return bad("%q has no host", raw)
	case strings.HasSuffix(u.Host, ":"):
		// url.Parse accepts it, and the redirect_uri built from it would
		// then match no URI anyone registered.
		return bad("%q has an empty port", raw)
	case u.Port() != "" && !validPort(u.Port()):
		return bad("%q: port must be 1-65535", raw)
	case u.User != nil:
		return bad("%q must not carry credentials", raw)
	case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(raw, "?") || strings.HasSuffix(raw, "#"):
		return bad("%q must be an origin, without a query or fragment", raw)
	case u.Path != "" && u.Path != "/":
		return bad("%q must be an origin, without a path: the console is served at the root", raw)
	case u.Scheme == "http" && !IsLoopbackHost(u.Hostname()) && !localhostName(u.Hostname()):
		// A name under .localhost never leaves the machine either (RFC 6761,
		// and browsers treat it as a secure context), as MAIL_CONNECT_SRC
		// accepts; it lets a development console share a parent domain with
		// another development site.
		return bad("http is only allowed for localhost, a name under .localhost, 127.0.0.1 or [::1]; use https for %q", u.Host)
	case u.Scheme == "http" && env.IsProd():
		return bad("prod requires https")
	}
	// The origin as a browser writes it: without the scheme's default port,
	// so the redirect_uri built from it is the one a provider has registered.
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.TrimSuffix(host, ":"+u.Port())
	}
	return u.Scheme + "://" + host
}

// connectSrc reads the origins the console may also connect to: a list
// separated by spaces, each an origin as publicURL reads one, without a path,
// a query, a fragment or credentials, and without the wildcards a CSP source
// could hold, so that each names exactly one origin. Cleartext is only
// accepted for an origin that never leaves the machine (loopback, or a name
// under .localhost), and never in prod. Each comes back as a browser writes
// it, lower case and without the scheme's default port, once.
func connectSrc(env Env, errs *[]error) []string {
	var out []string
	for _, raw := range strings.Fields(os.Getenv("MAIL_CONNECT_SRC")) {
		origin, complaint := connectOrigin(env, raw)
		if complaint != "" {
			*errs = append(*errs, fmt.Errorf("MAIL_CONNECT_SRC: %q: %s", raw, complaint))
			continue
		}
		if !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	return out
}

// connectOrigin is raw as an origin, or why it is not one.
func connectOrigin(env Env, raw string) (string, string) {
	if strings.Contains(raw, "*") {
		return "", "wildcards are not allowed; list each origin"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "not a URL"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "want https://, or http:// for loopback"
	case u.Opaque != "" || u.Host == "" || host == "":
		return "", "no host"
	case strings.HasSuffix(u.Host, ":"):
		return "", "empty port"
	case u.Port() != "" && !validPort(u.Port()):
		return "", "port must be 1-65535"
	case u.User != nil:
		return "", "must not carry credentials"
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#"):
		return "", "must be an origin, without a query or fragment"
	case u.Path != "" && u.Path != "/":
		return "", "must be an origin, without a path"
	case isIPv6(host):
		// CSP's host-source has no brackets: a browser drops the source as
		// invalid, and the origin would be refused for all it was accepted.
		return "", "an IPv6 address cannot be a CSP source; use a name such as localhost"
	case !validHost(host):
		return "", "the host must be a DNS name or an IPv4 address"
	case u.Scheme == "http" && !IsLoopbackHost(host) && !strings.HasSuffix(host, ".localhost"):
		return "", "http is only allowed for loopback addresses and localhost names; use https"
	case u.Scheme == "http" && env.IsProd():
		return "", "prod requires https"
	}
	origin := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		origin = strings.TrimSuffix(origin, ":"+u.Port())
	}
	return u.Scheme + "://" + origin, ""
}

// isIPv6 reports whether host, without brackets, is an IPv6 address, with or
// without a zone, an IPv4-mapped one included.
func isIPv6(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6()
}

// validHost reports whether host, in lower case and without brackets, is an
// IPv4 address or a DNS name of letters, digits and hyphens: what CSP's
// host-source can hold, and nothing that could end a source or start another.
func validHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Is4()
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

// IsLoopbackHost reports whether a host name or address never leaves the
// machine: localhost, or a loopback address. It decides where cleartext is
// acceptable and where a daemon-side loopback OAuth listener can be reached by
// the browser.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && addr.Unmap().IsLoopback()
}

// localhostName reports whether host is a DNS name under .localhost, which
// never leaves the machine (RFC 6761): app.example.localhost, never
// localhost.example.com.
func localhostName(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".localhost") && validHost(host)
}

// dataDir resolves where the database and the attachment cache live.
func dataDir(errs *[]error) string {
	if v := str("MAIL_DATA_DIR", ""); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("MAIL_DATA_DIR: %w", err))
			return v
		}
		return abs
	}
	base, err := os.UserConfigDir()
	if err != nil {
		*errs = append(*errs, fmt.Errorf("MAIL_DATA_DIR is unset and the user config directory "+
			"could not be determined: %w", err))
		return ""
	}
	return filepath.Join(base, "mailserver")
}

// credentials builds the keyring from the active key and any previous ones.
func credentials(errs *[]error) Credentials {
	out := Credentials{ActiveKeyID: 1, Keys: map[uint8][]byte{}}

	if v := str("MAIL_CREDENTIAL_KEY_ID", ""); v != "" {
		id, err := strconv.ParseUint(v, 10, 8)
		if err != nil || id == 0 {
			*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_KEY_ID: want a number between 1 and 255, got %q", v))
		} else {
			out.ActiveKeyID = uint8(id)
		}
	}

	raw := str("MAIL_CREDENTIAL_KEY_HEX", "")
	switch key, err := decodeKey(raw); {
	case raw == "":
		*errs = append(*errs, errors.New("MAIL_CREDENTIAL_KEY_HEX is required: "+
			"it encrypts every stored refresh token and IMAP password "+
			"(generate one with `openssl rand -hex 32`)"))
	case err != nil:
		*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_KEY_HEX: %w", err))
	default:
		out.Keys[out.ActiveKeyID] = key
	}

	// Previous keys are needed while a rewrap is in flight, and after a
	// restore from a backup taken before a rotation: they decrypt rows that
	// have not been re-sealed under the active key yet.
	for _, part := range strings.Split(str("MAIL_CREDENTIAL_PREVIOUS_KEYS", ""), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idStr, keyStr, ok := strings.Cut(part, ":")
		if !ok {
			*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_PREVIOUS_KEYS: want <id>:<hex>, got %q", part))
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(idStr), 10, 8)
		if err != nil || id == 0 {
			*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_PREVIOUS_KEYS: %q is not a key id between 1 and 255", idStr))
			continue
		}
		key, err := decodeKey(strings.TrimSpace(keyStr))
		if err != nil {
			*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_PREVIOUS_KEYS: key %d: %w", id, err))
			continue
		}
		if uint8(id) == out.ActiveKeyID {
			*errs = append(*errs, fmt.Errorf("MAIL_CREDENTIAL_PREVIOUS_KEYS: key id %d is also the active key id", id))
			continue
		}
		out.Keys[uint8(id)] = key
	}
	return out
}

func decodeKey(v string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, errors.New("not valid hex")
	}
	if len(key) != credentialKeyLen {
		return nil, fmt.Errorf("want %d bytes (%d hex characters), got %d", credentialKeyLen, credentialKeyLen*2, len(key))
	}
	return key, nil
}

// DatabasePath is the SQLite file inside the data directory.
func (c Config) DatabasePath() string { return filepath.Join(c.DataDir, "mail.db") }

// LockPath is the file whose exclusive lock marks a running daemon. Two
// daemons on one database would each drive their own IMAP sessions and fight
// over the same UID state.
func (c Config) LockPath() string { return filepath.Join(c.DataDir, "mailserver.lock") }

// AttachmentDir holds cached attachment blobs, named by content hash.
func (c Config) AttachmentDir() string { return filepath.Join(c.DataDir, "att") }

// SpoolDir holds partially fetched body sections. A fetch is spooled here
// while the IMAP connection is held, so no reader bound to a live connection
// is ever handed to an HTTP client.
func (c Config) SpoolDir() string { return filepath.Join(c.DataDir, "tmp") }

// String renders the configuration for startup logs with every secret removed.
func (c Config) String() string {
	keys := make([]string, 0, len(c.Credentials.Keys))
	for id := range c.Credentials.Keys {
		mark := ""
		if id == c.Credentials.ActiveKeyID {
			mark = "*"
		}
		keys = append(keys, strconv.Itoa(int(id))+mark)
	}
	return fmt.Sprintf(
		"env=%s http=%s metrics=%s data=%s credential_keys=%s google=%s microsoft=%s/%s "+
			"google_web=%s microsoft_web=%s public_url=%s web=%s account_allow_private=%t "+
			"microsoft_device_code=%t admin_api=%t mcp_http=%t mcp_key=%s keys_may_send=%t "+
			"consent_versions=sync:%s,actions:%s,send:%s,keys:%s "+
			"download_spool=%dMiB log=%s/%s",
		c.Env, c.HTTPAddr, orDefault(c.MetricsAddr, "inline"), c.DataDir,
		orDefault(strings.Join(keys, ","), "none"),
		configured(c.Google.Configured()), configured(c.Microsoft.Configured()), c.Microsoft.Tenant,
		configured(c.GoogleWeb.Configured()), configured(c.MicrosoftWeb.Configured()),
		orDefault(c.PublicURL, "unset"), orDefault(c.WebDir, "off"), c.AccountAllowPrivate,
		c.MicrosoftDeviceCode, c.AdminAPI, c.MCPHTTP, configured(c.MCPKey != ""), c.KeysMaySend,
		c.Consent.Sync, c.Consent.Actions, c.Consent.Send, c.Consent.Keys,
		c.DownloadSpoolBytes>>20, c.Log.Level, c.Log.Format,
	) + connectSrcField(c.ConnectSrc)
}

// connectSrcField is the console's extra origins for String, only when there
// are some: without them the line is what it always was.
func connectSrcField(origins []string) string {
	if len(origins) == 0 {
		return ""
	}
	return " connect_src=" + strings.Join(origins, ",")
}

func configured(ok bool) string {
	if ok {
		return "set"
	}
	return "unset"
}

func defaultLogFormat(e Env) string {
	if e.IsProd() {
		return "json"
	}
	return "text"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// bytes reads a byte count, accepting a plain number or a K/M/G suffix.
func bytes(key string, def int64, errs *[]error) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	mult := int64(1)
	switch last := raw[len(raw)-1]; last {
	case 'k', 'K':
		mult, raw = 1<<10, raw[:len(raw)-1]
	case 'm', 'M':
		mult, raw = 1<<20, raw[:len(raw)-1]
	case 'g', 'G':
		mult, raw = 1<<30, raw[:len(raw)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: want a positive byte count, "+
			"optionally suffixed K, M or G, got %q", key, os.Getenv(key)))
		return def
	}
	return n * mult
}

// prefixes reads a comma-separated list of addresses or CIDR blocks.
func prefixes(key string, errs *[]error) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(os.Getenv(key), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("%s: %q is not an address or a CIDR block", key, part))
			continue
		}
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out
}

func boolean(key string, def bool, errs *[]error) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a boolean", key, v))
		return def
	}
	return b
}
