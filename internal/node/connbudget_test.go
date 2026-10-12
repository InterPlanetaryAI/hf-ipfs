package node

import "testing"

// The watermarks only protect inbound while they sit under the ceilings we
// configure, and the ceilings only protect inbound while neither direction can
// consume the whole system scope. Raise the watermarks or lower a ceiling and
// the guard silently stops binding — which is the failure these numbers exist
// to prevent.
func TestConnWatermarksUnderBudget(t *testing.T) {
	if connLowWater >= connHighWater {
		t.Fatalf("low water %d must be below high water %d", connLowWater, connHighWater)
	}
	if connHighWater >= connConnsInbound {
		t.Fatalf("high water %d must stay under the inbound ceiling %d, or outbound "+
			"trimming no longer leaves room for strangers", connHighWater, connConnsInbound)
	}
	if connConnsInbound+connConnsOutbound > connConns {
		t.Fatalf("inbound %d + outbound %d must fit the system scope %d, so one "+
			"direction cannot starve the other", connConnsInbound, connConnsOutbound, connConns)
	}
	if connTransientInbound < connHighWater {
		t.Fatalf("transient inbound %d must cover the watermark %d, or handshakes are "+
			"refused before the steady-state ceiling is reached", connTransientInbound, connHighWater)
	}

	// The ceilings the host actually gets come from connLimits, not from the
	// constants above: assert through the function so a build that drops
	// libp2p.ResourceManager(rm) — or scales the ceilings back down — cannot
	// pass this test on constant arithmetic alone.
	sys := connLimits().ToPartialLimitConfig().System
	if int(sys.ConnsInbound) <= connHighWater {
		t.Fatalf("effective inbound ceiling %d must stay above the high watermark %d",
			int(sys.ConnsInbound), connHighWater)
	}
	// Neither direction may be able to eat the whole system scope — that is
	// what starved the listener. go-libp2p's own defaults do not satisfy this
	// (outbound sub-limit == total), which is exactly the trap.
	if int(sys.Conns) <= int(sys.ConnsOutbound) {
		t.Fatalf("system scope %d must exceed the outbound ceiling %d, or outbound "+
			"alone starves inbound accepts", int(sys.Conns), int(sys.ConnsOutbound))
	}
	if int(sys.Conns) <= int(sys.ConnsInbound) {
		t.Fatalf("system scope %d must exceed the inbound ceiling %d",
			int(sys.Conns), int(sys.ConnsInbound))
	}
}
