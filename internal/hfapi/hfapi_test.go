package hfapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipai/hf-ipfs/internal/hfcache"
)

// recorder is a stub Hub that captures the Authorization header it was sent.
func recorder(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), &auth
}

func TestDownloadSendsBearerToken(t *testing.T) {
	c, auth := recorder(t, http.StatusOK, "payload")
	c.Token = "hf_abc123"

	var buf bytes.Buffer
	if _, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if *auth != "Bearer hf_abc123" {
		t.Errorf("Authorization = %q, want %q", *auth, "Bearer hf_abc123")
	}
}

func TestDownloadOmitsAuthorizationWhenUnset(t *testing.T) {
	c, auth := recorder(t, http.StatusOK, "payload")

	var buf bytes.Buffer
	if _, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if *auth != "" {
		t.Errorf("Authorization = %q, want empty", *auth)
	}
}

func TestRepoInfoAtSendsBearerToken(t *testing.T) {
	body := `{"id":"org/m","sha":"0123456789abcdef0123456789abcdef01234567","siblings":[]}`
	c, auth := recorder(t, http.StatusOK, body)
	c.Token = "hf_abc123"

	if _, err := c.RepoInfoAt(context.Background(), "org/m", hfcache.Model, "abc", true); err != nil {
		t.Fatalf("repo info: %v", err)
	}
	if *auth != "Bearer hf_abc123" {
		t.Errorf("Authorization = %q, want %q", *auth, "Bearer hf_abc123")
	}
}

// Errors get logged verbatim, so a token echoed into one would end up on disk
// and in whatever log aggregation sits downstream.
func TestErrorsDoNotLeakToken(t *testing.T) {
	const secret = "hf_SUPERSECRET_do_not_log"

	c, _ := recorder(t, http.StatusForbidden, `{"error":"You need access to this repo"}`)
	c.Token = secret

	var buf bytes.Buffer
	_, err := c.Download(context.Background(), "org/gated", hfcache.Model, "abc", "f.bin", &buf)
	if err == nil {
		t.Fatal("want error on 403, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("download error leaked the token: %v", err)
	}

	_, err = c.RepoInfoAt(context.Background(), "org/gated", hfcache.Model, "abc", true)
	if err == nil {
		t.Fatal("want error on 403, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("repo info error leaked the token: %v", err)
	}
}

// The token must never end up in a URL either, since URLs get logged by
// proxies and appear in error strings.
func TestResolveURLNeverContainsToken(t *testing.T) {
	c := NewClient("https://hub.example")
	c.Token = "hf_SUPERSECRET"
	u := c.ResolveURL("org/m", hfcache.Model, "abc", "weights.bin")
	if strings.Contains(u, "SUPERSECRET") {
		t.Errorf("resolve URL leaked the token: %s", u)
	}
}
