package hfapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if _, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", -1, &buf); err != nil {
		t.Fatalf("download: %v", err)
	}
	if *auth != "Bearer hf_abc123" {
		t.Errorf("Authorization = %q, want %q", *auth, "Bearer hf_abc123")
	}
}

func TestDownloadOmitsAuthorizationWhenUnset(t *testing.T) {
	c, auth := recorder(t, http.StatusOK, "payload")

	var buf bytes.Buffer
	if _, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", -1, &buf); err != nil {
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
	_, err := c.Download(context.Background(), "org/gated", hfcache.Model, "abc", "f.bin", -1, &buf)
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

// A lying endpoint that claims size=4 and streams forever must not fill the
// disk: Download stops at maxBytes+1 and lets the caller's size check fire.
func TestDownloadCapsOversizedBody(t *testing.T) {
	c, _ := recorder(t, http.StatusOK, "0123456789")
	var buf bytes.Buffer
	n, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", 4, &buf)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != 5 {
		t.Errorf("wrote %d bytes, want 5 (maxBytes+1)", n)
	}
}

// Headers arrive, then silence: the watchdog must cancel the request instead
// of hanging io.Copy forever.
func TestDownloadAbortsOnStalledBody(t *testing.T) {
	defer func(d time.Duration) { bodyIdleTimeout = d }(bodyIdleTimeout)
	bodyIdleTimeout = 150 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // body never arrives
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	var buf bytes.Buffer
	if _, err := c.Download(context.Background(), "org/m", hfcache.Model, "abc", "f.bin", -1, &buf); err == nil {
		t.Fatal("stalled body did not abort")
	}
}
