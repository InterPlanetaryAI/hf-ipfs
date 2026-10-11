package pull

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	swarm "github.com/libp2p/go-libp2p/p2p/net/swarm"
	ma "github.com/multiformats/go-multiaddr"
)

// dialError builds the error swarm returns for a failed address, wrapped the
// way mapExchange wraps it.
func dialError(t *testing.T, cause error) error {
	t.Helper()
	addr, err := ma.NewMultiaddr("/ip4/1.2.3.4/tcp/4008")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := peer.Decode("12D3KooWMpcyUsxuhzBghSovrJc1evVeE5vQa81Qm4b2HWnPzDt7")
	if err != nil {
		t.Fatal(err)
	}
	de := &swarm.DialError{
		Peer:       pid,
		Cause:      cause,
		DialErrors: []swarm.TransportError{{Address: addr, Cause: cause}},
	}
	return fmt.Errorf("connect: %w", de)
}

func TestDialHint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"reset names the address", syscall.ECONNRESET, "/ip4/1.2.3.4/tcp/4008"},
		{"eof is a closed handshake", io.EOF, "closed the handshake"},
		{"refused is unreachable", syscall.ECONNREFUSED, "no reachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := dialError(t, tc.cause)
			hint := dialHint(err)
			if !strings.Contains(hint, tc.want) {
				t.Fatalf("dialHint(%v) = %q, want it to mention %q", err, hint, tc.want)
			}
		})
	}

	t.Run("non-dial errors get no advice", func(t *testing.T) {
		if h := dialHint(errors.New("peer sent no manifest")); h != "" {
			t.Fatalf("dialHint = %q, want empty", h)
		}
	})
}
