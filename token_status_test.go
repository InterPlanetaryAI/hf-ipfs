package main

import (
	"strings"
	"testing"
)

func TestTokenStatusUnset(t *testing.T) {
	if got := tokenStatus(""); got != "unset" {
		t.Errorf("tokenStatus(\"\") = %q, want %q", got, "unset")
	}
}

// The banner is written to terminals, logs and CI output. The full token must
// never appear there.
func TestTokenStatusHidesSecret(t *testing.T) {
	const secret = "hf_abcdefghijklmnopqrstuvwxyz012345"
	got := tokenStatus(secret)
	if strings.Contains(got, secret) {
		t.Errorf("tokenStatus leaked the full token: %q", got)
	}
	if !strings.Contains(got, "set") {
		t.Errorf("tokenStatus = %q, want it to report the token is set", got)
	}
	// Only the last four characters are exposed.
	if !strings.HasSuffix(got, "2345)") {
		t.Errorf("tokenStatus = %q, want it to end in the last four chars", got)
	}
	if len(got) > len("set (…2345)") {
		t.Errorf("tokenStatus = %q, exposes more than four characters", got)
	}
}

// Short tokens must not be partially exposed at all.
func TestTokenStatusShortToken(t *testing.T) {
	if got := tokenStatus("abc"); got != "set" {
		t.Errorf("tokenStatus(short) = %q, want %q", got, "set")
	}
	if got := tokenStatus("abcd"); got != "set" {
		t.Errorf("tokenStatus(4-char) = %q, want %q", got, "set")
	}
}
