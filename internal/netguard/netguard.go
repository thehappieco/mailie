// Package netguard keeps the daemon's own outbound connections off addresses
// a caller should not be able to reach through it.
//
// A mail account is a host somebody typed. Once the console is hosted, that
// somebody is anyone with an invite, and "connect to imap_host:imap_port" is a
// server-side connection to wherever they chose: the metadata service, the
// database on the next machine, the daemon's own loopback ports. That is the
// shape of every SSRF, and a login banner or a TLS error is enough of an
// oracle to map a private network with.
//
// The rule is enforced where it cannot be raced: in net.Dialer.Control, which
// sees the address the socket is about to connect to after the name has been
// resolved. Checking a name and then dialing it separately leaves a window a
// DNS answer with a zero TTL walks straight through; checking the resolved
// address on the socket leaves none. CheckHost exists only to give a person
// adding an account a clear answer early.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
)

// ErrPrivateAddress is a connection to an address this daemon refuses to
// reach on a caller's behalf.
var ErrPrivateAddress = errors.New("netguard: that address is on a loopback, private or link-local network")

// ErrUnresolvable is a host name that resolves to nothing.
var ErrUnresolvable = errors.New("netguard: the host name does not resolve")

// blocked are the ranges net/netip has no predicate for.
var blocked = []netip.Prefix{
	// "This network": 0.0.0.0/8 reaches the local host on several systems.
	netip.MustParsePrefix("0.0.0.0/8"),
	// Carrier-grade NAT. Not private by RFC 1918, but just as internal: it
	// is where cloud providers put their own service endpoints.
	netip.MustParsePrefix("100.64.0.0/10"),
	// IETF protocol assignments, and benchmarking: neither is anywhere a
	// mail server lives, and both are routed internally where they are
	// routed at all.
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	// Reserved, including the limited broadcast address.
	netip.MustParsePrefix("240.0.0.0/4"),
	// Local-use NAT64 (RFC 8215): a translator a network runs precisely to
	// reach IPv4 addresses that are not global.
	netip.MustParsePrefix("64:ff9b:1::/48"),
	// Teredo: the IPv4 address inside is obfuscated, so there is no telling
	// where the tunnel ends.
	netip.MustParsePrefix("2001::/32"),
	// Site-local, deprecated but still honoured by some stacks as internal.
	netip.MustParsePrefix("fec0::/10"),
}

// Prefixes of IPv6 addresses that carry an IPv4 address a translator or a
// tunnel will connect to.
var (
	// nat64 is the well-known NAT64 prefix (RFC 6052): 64:ff9b::10.0.0.5 is
	// 10.0.0.5 on any network with a NAT64 gateway, which is what an
	// IPv6-only host in a cloud VPC has.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	// siit is ::ffff:0:a.b.c.d, the IPv4-translated form (RFC 6145). Not the
	// IPv4-mapped ::ffff:a.b.c.d, which Unmap has already turned into IPv4.
	siit = netip.MustParsePrefix("::ffff:0:0:0/96")
	// sixToFour (RFC 3056) puts the IPv4 address in bits 16 to 48.
	sixToFour = netip.MustParsePrefix("2002::/16")
	// ipv4Compatible (::a.b.c.d) is deprecated, and nothing public uses it.
	ipv4Compatible = netip.MustParsePrefix("::/96")
)

// Allowed reports whether a connection to addr may be made on a caller's
// behalf: a unicast address on the public internet.
func Allowed(addr netip.Addr) bool {
	// An IPv4 address written as IPv6 (::ffff:127.0.0.1) is the IPv4 address,
	// and would otherwise slip past every IPv4 range below.
	addr = addr.Unmap()
	// A zone only means something on a link-local address, and a prefix never
	// contains a zoned address: judged with its zone, 64:ff9b::a00:1%eth0
	// would match none of the ranges below.
	addr = addr.WithZone("")
	if addr.Is6() {
		if inner, ok := embeddedIPv4(addr); ok {
			// Judged as the IPv4 address it reaches, so NAT64 to a public
			// server keeps working and NAT64 to 10.0.0.5 does not.
			return inner.IsValid() && Allowed(inner)
		}
	}
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsMulticast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// embeddedIPv4 finds the IPv4 address an IPv6 address stands for through a
// translator or a tunnel. ok with an invalid address means the address embeds
// one this package will not judge, and must be refused.
func embeddedIPv4(addr netip.Addr) (inner netip.Addr, ok bool) {
	b := addr.As16()
	v4 := func(b []byte) netip.Addr { return netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]}) }
	switch {
	case nat64.Contains(addr):
		return v4(b[12:16]), true
	case siit.Contains(addr):
		return v4(b[12:16]), true
	case sixToFour.Contains(addr):
		return v4(b[2:6]), true
	case ipv4Compatible.Contains(addr):
		// :: and ::1 included, which are refused either way.
		return netip.Addr{}, true
	}
	return netip.Addr{}, false
}

// Control is a net.Dialer Control function that refuses every address
// Allowed rejects. It runs once per address the dialer tries, after
// resolution, which is what makes a name that resolves differently on the
// second lookup harmless.
func Control(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		// Control is only ever handed an ip:port; anything else is a dialer
		// this package does not understand, and refusing is the safe answer.
		return fmt.Errorf("%w: cannot read %q as an address", ErrPrivateAddress, address)
	}
	if !Allowed(addrPort.Addr()) {
		return ErrPrivateAddress
	}
	return nil
}

// CheckHost resolves host and reports whether every address it has is
// allowed. Every one, not any: a dialer tries them in turn, and a name with one
// public and one private address would reach the private one as soon as the
// public one stopped answering.
//
// This is the early, friendly check. Control is the one that holds.
func CheckHost(ctx context.Context, resolver *net.Resolver, host string) error {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if addr, err := netip.ParseAddr(host); err == nil {
		if !Allowed(addr) {
			return ErrPrivateAddress
		}
		return nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnresolvable, host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w: %s", ErrUnresolvable, host)
	}
	for _, addr := range addrs {
		if !Allowed(addr) {
			return ErrPrivateAddress
		}
	}
	return nil
}
