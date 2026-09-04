package node

import (
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

func mustAddr(t *testing.T, s string) ma.Multiaddr {
	t.Helper()
	a, err := ma.NewMultiaddr(s)
	if err != nil {
		t.Fatalf("bad multiaddr %q: %v", s, err)
	}
	return a
}

func strs(addrs []ma.Multiaddr) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// A multi-homed seeder must announce only its routable address. Announcing
// loopback and Docker bridges is what made transparent pulls fail while an
// explicit --peer worked: the puller spent its budget on addresses that
// cannot possibly connect.
func TestAnnounceAddrsKeepsOnlyPublic(t *testing.T) {
	in := []ma.Multiaddr{
		mustAddr(t, "/ip4/127.0.0.1/tcp/4008"),
		mustAddr(t, "/ip4/172.17.0.1/tcp/4008"),
		mustAddr(t, "/ip4/192.168.1.5/tcp/4008"),
		mustAddr(t, "/ip4/100.64.0.1/tcp/4008"), // CGNAT
		mustAddr(t, "/ip4/173.249.47.163/tcp/4008"),
	}
	got := strs(announceAddrs(in))
	want := []string{"/ip4/173.249.47.163/tcp/4008"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("announceAddrs() = %v, want %v", got, want)
	}
}

// A node with no public address must keep its local ones rather than announce
// nothing: it is still reachable inside its own network, and silence would
// hide it from LAN peers too.
func TestAnnounceAddrsFallsBackToLocal(t *testing.T) {
	in := []ma.Multiaddr{
		mustAddr(t, "/ip4/127.0.0.1/tcp/4008"),
		mustAddr(t, "/ip4/172.17.0.11/tcp/4008"),
	}
	got := announceAddrs(in)
	if len(got) != len(in) {
		t.Fatalf("announceAddrs() dropped everything; want the local fallback %v, got %v", in, got)
	}
}

func TestAnnounceAddrsEmpty(t *testing.T) {
	if got := announceAddrs(nil); len(got) != 0 {
		t.Fatalf("announceAddrs(nil) = %v, want empty", got)
	}
}

// The pull side must try the routable address before loopback, but keep the
// non-routable ones as a fallback for pullers that share the provider's LAN.
func TestDialOrderPutsPublicFirst(t *testing.T) {
	in := []ma.Multiaddr{
		mustAddr(t, "/ip4/127.0.0.1/tcp/4008"),
		mustAddr(t, "/ip4/172.17.0.1/tcp/4008"),
		mustAddr(t, "/ip4/173.249.47.163/tcp/4008"),
		mustAddr(t, "/ip4/172.18.0.1/tcp/4008"),
	}
	got := strs(dialOrder(in))
	if got[0] != "/ip4/173.249.47.163/tcp/4008" {
		t.Fatalf("dialOrder() = %v, want the public address first", got)
	}
	if len(got) != len(in) {
		t.Fatalf("dialOrder() must preserve every candidate, got %v", got)
	}
	for _, a := range got[1:] {
		if a == "/ip4/173.249.47.163/tcp/4008" {
			t.Fatalf("public address duplicated: %v", got)
		}
	}
}

func TestDialOrderAllLocalKeepsAll(t *testing.T) {
	in := []ma.Multiaddr{
		mustAddr(t, "/ip4/10.0.0.1/tcp/1"),
		mustAddr(t, "/ip4/10.0.0.2/tcp/2"),
	}
	if got := dialOrder(in); len(got) != 2 {
		t.Fatalf("dialOrder() = %v, want both local addresses retained", got)
	}
}

// A seeder bound only to loopback cannot be dialed by anyone: the DHT hands
// out 127.0.0.1, which resolves on the puller's own machine. The daemon must
// detect this and say so rather than look healthy.
func TestLoopbackOnlyDetectsLoopbackBind(t *testing.T) {
	in := []ma.Multiaddr{
		mustAddr(t, "/ip4/127.0.0.1/tcp/4008"),
		mustAddr(t, "/ip6/::1/tcp/4008"),
	}
	if !loopbackOnly(in) {
		t.Fatalf("loopbackOnly(%v) = false, want true", strs(in))
	}
}

func TestLoopbackOnlyFalseWhenAnyNonLoopback(t *testing.T) {
	for _, extra := range []string{
		"/ip4/172.17.0.11/tcp/4008", // Docker bridge
		"/ip4/192.168.1.5/tcp/4008", // LAN
		"/ip4/173.249.47.163/tcp/4008",
	} {
		in := []ma.Multiaddr{
			mustAddr(t, "/ip4/127.0.0.1/tcp/4008"),
			mustAddr(t, extra),
		}
		if loopbackOnly(in) {
			t.Errorf("loopbackOnly() = true with %s present", extra)
		}
	}
}

// Empty must not read as loopback-only: there is nothing to warn about, and a
// false positive here would cry wolf on every node.
func TestLoopbackOnlyEmpty(t *testing.T) {
	if loopbackOnly(nil) {
		t.Fatal("loopbackOnly(nil) = true, want false")
	}
}

// A hostname-backed address has no IP to classify; treat it as not-loopback
// rather than failing the whole check.
func TestLoopbackOnlyNonIPAddress(t *testing.T) {
	in := []ma.Multiaddr{mustAddr(t, "/dns4/example.com/tcp/4008")}
	if loopbackOnly(in) {
		t.Fatal("loopbackOnly(dns) = true, want false")
	}
}
