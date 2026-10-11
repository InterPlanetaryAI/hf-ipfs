package node

import (
	"testing"

	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
)

// The watermarks are only a guard because they sit under go-libp2p's default
// resource-manager budget. The budget scales *up* with RAM, so the base limit is
// the smallest it ever gets — and the outbound sub-limit is scaled the same way,
// meaning outbound alone can consume the whole system scope and starve the
// listener. Raise the watermarks past the base and the guard silently stops
// binding, which is the failure this exists to prevent.
func TestConnWatermarksUnderBudget(t *testing.T) {
	base := rcmgr.DefaultLimits.SystemBaseLimit
	if connLowWater >= connHighWater {
		t.Fatalf("low water %d must be below high water %d", connLowWater, connHighWater)
	}
	if connHighWater >= base.Conns {
		t.Fatalf("high water %d must stay under the system conn budget %d, or outbound starves inbound accepts",
			connHighWater, base.Conns)
	}
}
