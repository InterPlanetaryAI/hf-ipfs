package config

import "testing"

func TestDefaultResolvesHFToken(t *testing.T) {
	t.Setenv("HF_TOKEN", "tok_current")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "tok_current" {
		t.Errorf("HFToken = %q, want %q", cfg.HFToken, "tok_current")
	}
}

// HUGGING_FACE_HUB_TOKEN is the older name still exported by a lot of
// existing tooling, so it has to keep working.
func TestDefaultHonoursLegacyToken(t *testing.T) {
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "tok_legacy")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "tok_legacy" {
		t.Errorf("HFToken = %q, want %q", cfg.HFToken, "tok_legacy")
	}
}

func TestHFTokenWinsOverLegacy(t *testing.T) {
	t.Setenv("HF_TOKEN", "tok_current")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "tok_legacy")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "tok_current" {
		t.Errorf("HFToken = %q, want %q", cfg.HFToken, "tok_current")
	}
}

func TestNoTokenMeansEmpty(t *testing.T) {
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "" {
		t.Errorf("HFToken = %q, want empty", cfg.HFToken)
	}
}

// A stray newline from `export HF_TOKEN=$(cat file)` must not become part of
// the credential.
func TestTokenIsTrimmed(t *testing.T) {
	t.Setenv("HF_TOKEN", "  tok_padded\n")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "tok_padded" {
		t.Errorf("HFToken = %q, want %q", cfg.HFToken, "tok_padded")
	}
}

// Whitespace-only must read as unset, not as a one-character credential.
func TestWhitespaceOnlyTokenIsUnset(t *testing.T) {
	t.Setenv("HF_TOKEN", "   ")
	t.Setenv("HUGGING_FACE_HUB_TOKEN", "")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if cfg.HFToken != "" {
		t.Errorf("HFToken = %q, want empty", cfg.HFToken)
	}
}
