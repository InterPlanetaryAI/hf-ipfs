package pull

import "testing"

// A per-invocation --hf-token must win over whatever the daemon was started
// with, otherwise `pull --hf-token X` against a tokenless daemon would keep
// failing on gated repos.
func TestResolveTokenOverrideWins(t *testing.T) {
	if got := resolveToken("override", "configured"); got != "override" {
		t.Errorf("resolveToken = %q, want %q", got, "override")
	}
}

func TestResolveTokenFallsBackToConfigured(t *testing.T) {
	if got := resolveToken("", "configured"); got != "configured" {
		t.Errorf("resolveToken = %q, want %q", got, "configured")
	}
}

func TestResolveTokenBothEmpty(t *testing.T) {
	if got := resolveToken("", ""); got != "" {
		t.Errorf("resolveToken = %q, want empty", got)
	}
}
