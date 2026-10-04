package netguard_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/thehappieco/mailie/internal/netguard"
)

func TestOnlyPublicUnicastAddressesAreAllowed(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14":        true,
		"2606:2800:21f:cb07::": true,
		"142.250.0.108":        true,

		"127.0.0.1":        false, // loopback
		"127.8.9.10":       false,
		"::1":              false,
		"::ffff:127.0.0.1": false, // IPv4 loopback spelled as IPv6
		"::ffff:10.0.0.1":  false,
		"10.1.2.3":         false, // RFC 1918
		"172.16.0.1":       false,
		"172.31.255.255":   false,
		"192.168.1.1":      false,
		"fd00:ec2::254":    false, // unique local, where AWS puts its IPv6 metadata service
		"169.254.169.254":  false, // link-local, where every cloud puts its metadata service
		"fe80::1":          false,
		"100.64.0.1":       false, // carrier-grade NAT
		"100.127.255.254":  false,
		"0.0.0.0":          false, // unspecified
		"0.1.2.3":          false, // "this network"
		"::":               false,
		"224.0.0.1":        false, // multicast
		"ff02::1":          false,
		"255.255.255.255":  false, // broadcast
		"240.0.0.1":        false, // reserved
		"192.0.0.1":        false, // IETF protocol assignments
		"198.18.0.1":       false, // benchmarking
		"fec0::1":          false, // site-local

		// IPv6 addresses a translator or a tunnel turns into IPv4 ones are
		// judged as the IPv4 address they reach.
		"64:ff9b::a00:1":                       false, // NAT64 to 10.0.0.1
		"64:ff9b::a9fe:a9fe":                   false, // NAT64 to the metadata service
		"64:ff9b::7f00:1":                      false, // NAT64 to loopback
		"64:ff9b::5db8:d70e":                   true,  // NAT64 to public 93.184.215.14
		"64:ff9b:1::a00:1":                     false, // local-use NAT64, for non-global IPv4
		"2002:7f00:1::1":                       false, // 6to4 around 127.0.0.1
		"2002:a9fe:a9fe::1":                    false, // 6to4 around 169.254.169.254
		"2002:5db8:d70e::1":                    true,  // 6to4 around a public address
		"::ffff:0:7f00:1":                      false, // SIIT-translated loopback
		"::127.0.0.1":                          false, // IPv4-compatible, deprecated
		"2001:0:4136:e378:8000:63bf:80ff:fffe": false, // Teredo
		"64:ff9b::a00:1%eth0":                  false, // a zone does not hide the range
		"fd00::1%eth0":                         false,
	} {
		if got := netguard.Allowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Allowed(%s) = %t, want %t", addr, got, want)
		}
	}
}

func TestTheDialerControlRefusesAPrivateAddressAfterResolution(t *testing.T) {
	// Control sees what the socket is about to connect to, so it is the
	// check a rebinding DNS answer cannot get around.
	if err := netguard.Control("tcp4", "127.0.0.1:993", nil); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("loopback: %v", err)
	}
	if err := netguard.Control("tcp6", "[::1]:993", nil); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("IPv6 loopback: %v", err)
	}
	if err := netguard.Control("tcp", "not an address", nil); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("an unreadable address was let through: %v", err)
	}
	if err := netguard.Control("tcp4", "93.184.215.14:993", nil); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
}

func TestADialToAPrivateAddressNeverConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	dialer := &net.Dialer{Control: netguard.Control}
	// "localhost" is a name, checked only once it has become an address.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_, err = dialer.DialContext(t.Context(), "tcp", net.JoinHostPort("localhost", port))
	if !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Fatalf("dial localhost = %v, want ErrPrivateAddress", err)
	}
	select {
	case <-accepted:
		t.Fatal("the listener accepted a connection the guard refused")
	default:
	}
}

func TestCheckingAHostLooksAtEveryAddressItHas(t *testing.T) {
	ctx := context.Background()
	if err := netguard.CheckHost(ctx, nil, "127.0.0.1"); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("a loopback literal: %v", err)
	}
	if err := netguard.CheckHost(ctx, nil, "[::1]"); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("a bracketed IPv6 literal: %v", err)
	}
	if err := netguard.CheckHost(ctx, nil, "localhost"); !errors.Is(err, netguard.ErrPrivateAddress) {
		t.Errorf("a name for the loopback address: %v", err)
	}
	if err := netguard.CheckHost(ctx, nil, "93.184.215.14"); err != nil {
		t.Errorf("a public literal: %v", err)
	}
	if err := netguard.CheckHost(ctx, nil, "no-such-host.invalid"); !errors.Is(err, netguard.ErrUnresolvable) {
		t.Errorf("a name that cannot resolve: %v", err)
	}
}
