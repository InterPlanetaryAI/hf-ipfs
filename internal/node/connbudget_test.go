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
}
