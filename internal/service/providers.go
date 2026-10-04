package service

import (
	"context"
	"slices"
	"strings"

	"github.com/thehappieco/mailie/internal/account"
	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/provider"
)

// Provider is what a caller can connect, and how.
type Provider struct {
	ID string `json:"id"`
	// OAuth is whether the provider authenticates with OAuth and this daemon
	// has a client registered for it. Flows can still be empty for a caller
	// that none of those clients can serve, such as a hosted console with only
	// the CLI's loopback client.
	OAuth bool `json:"oauth"`
	// Password is whether this caller may connect it with a password.
	Password bool `json:"password"`
	// Flows are the consent flows open to this caller, the default first.
	Flows []string `json:"flows"`
}

// providerOrder is the order the console lists them in. iCloud is not a
// provider.Kind — it is stored as generic IMAP on Apple's servers — but a
// person looking for it should not have to know that.
var providerOrder = []string{
	string(provider.KindGmail), string(provider.KindMicrosoft), account.ICloud, string(provider.KindIMAP),
}

// Providers says which providers this caller can connect and how, so a client
// never offers an option the server would then refuse.
func (s *Service) Providers(_ context.Context, p Principal) ([]Provider, error) {
	if err := s.authorize(p, auth.ScopeRead); err != nil {
		return nil, err
	}
	out := make([]Provider, 0, len(providerOrder))
	for _, id := range providerOrder {
		out = append(out, s.describeProvider(p, id))
	}
	return out, nil
}

// describeProvider says how this caller can connect one provider.
func (s *Service) describeProvider(p Principal, id string) Provider {
	if id == account.ICloud {
		// The same for every caller. Apple offers this server no OAuth, so an
		// app-specific password is the only way in — and the reason Gmail's
		// app password is kept from people, that a better sign-in exists,
		// does not apply.
		return Provider{ID: id, Password: true, Flows: []string{}}
	}
	kind := provider.Kind(id)
	flows := s.flowsFor(p, kind)
	names := make([]string, 0, len(flows))
	for _, f := range flows {
		names = append(names, string(f))
	}
	return Provider{
		ID:       id,
		OAuth:    s.accounts.Availability(kind).OAuth(),
		Password: passwordAllowed(p, kind),
		Flows:    names,
	}
}

// flowsFor lists the consent flows a caller may use for a provider, the
// default first.
//
// A signed-in browser gets the web flow when a web client is configured, and
// the loopback flow only when the console is served from this machine: the
// loopback listener binds to the daemon's own 127.0.0.1, so a browser anywhere
// else would be sent to a redirect that lands on its own machine and fails. A
// hosted console must never offer it. Keys keep what the CLI has always had —
// loopback first — and may also ask for the others by name.
func (s *Service) flowsFor(p Principal, kind provider.Kind) []account.FlowKind {
	avail := s.accounts.Availability(kind)
	var out []account.FlowKind
	if p.IsSession() {
		if avail.Web {
			out = append(out, account.FlowWeb)
		}
		if avail.Installed && s.localConsole {
			out = append(out, account.FlowLoopback)
		}
		return out
	}
	if avail.Installed {
		out = append(out, account.FlowLoopback, account.FlowPasted)
	}
	if avail.Device {
		out = append(out, account.FlowDevice)
	}
	if avail.Web {
		out = append(out, account.FlowWeb)
	}
	return out
}

// chooseFlow decides which flow a request gets: the one it named, if this
// caller may use it, or the caller's default.
func (s *Service) chooseFlow(p Principal, kind provider.Kind, requested string) (account.FlowKind, error) {
	allowed := s.flowsFor(p, kind)
	var flow account.FlowKind
	switch {
	case requested != "":
		parsed, err := parseFlow(requested)
		if err != nil {
			return "", err
		}
		flow = parsed
	case len(allowed) > 0:
		flow = allowed[0]
	default:
		return "", Ef(CodeBadRequest, nil,
			"%s accounts cannot be authorised here: no OAuth client on this server serves this caller", kind)
	}
	if !slices.Contains(allowed, flow) {
		names := make([]string, 0, len(allowed))
		for _, f := range allowed {
			names = append(names, string(f))
		}
		available := strings.Join(names, ", ")
		if available == "" {
			available = "none"
		}
		return "", Ef(CodeBadRequest, nil,
			"the %s flow is not available for %s accounts here (available: %s)", flow, kind, available)
	}
	return flow, nil
}

// passwordAllowed says who may connect a provider with a password. Generic
// IMAP only has passwords, and so does iCloud, which is stored as generic
// IMAP. Microsoft refuses them for IMAP altogether. Gmail still takes an app
// password, which the operator's CLI can use, but a person connects Gmail
// with Google sign-in only — from the console or with a key acting for them:
// an app password is a credential with no scope and no expiry, and a form
// that asks for one teaches people to paste it.
func passwordAllowed(p Principal, kind provider.Kind) bool {
	switch kind {
	case provider.KindIMAP:
		return true
	case provider.KindGmail:
		return p.IsInstance()
	}
	return false
}
