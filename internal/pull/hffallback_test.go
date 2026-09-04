package pull

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipai/hf-ipfs/internal/hfapi"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/wire"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// gitBlobID is how git — and therefore the HF API — ids a plain non-LFS file:
// SHA-1 over "blob <size>\0" + content.
func gitBlobID(body []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// newTestHub points a client and a cache path set at a stub HF that serves one
// body at the canonical model resolve URL.
func newTestHub(t *testing.T, body []byte) (*hfapi.Client, hfcache.Paths) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/testorg/tiny/resolve/" + testCommit + "/config.json"
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return hfapi.NewClient(srv.URL),
		hfcache.NewPaths(t.TempDir(), "testorg/tiny", hfcache.Model)
}

func runFetch(t *testing.T, client *hfapi.Client, paths hfcache.Paths, f *mapping.FileManifest) error {
	t.Helper()
	if err := os.MkdirAll(paths.Blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	return fetchBlob(context.Background(), client, paths, "testorg/tiny", hfcache.Model,
		testCommit, f, &xferState{total: f.Size}, func(wire.ControlEvent) error { return nil })
}

// A plain git file carries no content hash from the API, so integrity rests on
// the git blob id matching. This also pins the assumption that blobId is the
// correct blob filename for the hf cache layout.
func TestFetchBlobAcceptsMatchingGitBlobID(t *testing.T) {
	body := []byte(`{"architectures":["Tiny"]}`)
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{
		Path:     "config.json",
		BlobName: gitBlobID(body),
		Size:     int64(len(body)),
	}
	if err := runFetch(t, client, paths, f); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	blob := paths.BlobPath(f.BlobName)
	got, err := os.ReadFile(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("blob contents = %q, want %q", got, body)
	}
	// Content sha256 is filled into the manifest now that we have the bytes.
	sum := sha256.Sum256(body)
	if f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("manifest sha256 = %s, want %s", f.SHA256, hex.EncodeToString(sum[:]))
	}
	leftovers, _ := filepath.Glob(paths.BlobPath("*") + ".download")
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestFetchBlobRejectsWrongGitBlobID(t *testing.T) {
	body := []byte(`{"architectures":["Tiny"]}`)
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{
		Path:     "config.json",
		BlobName: strings.Repeat("a", 40), // an id the content does not hash to
		Size:     int64(len(body)),
	}
	err := runFetch(t, client, paths, f)
	if err == nil {
		t.Fatal("want mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "git blob id mismatch") {
		t.Errorf("err = %v, want git blob id mismatch", err)
	}
	// Nothing may appear at the real blob path: the filestore and the snapshot
	// links both treat a blob's presence as proof it is complete.
	if _, err := os.Stat(paths.BlobPath(f.BlobName)); err == nil {
		t.Error("corrupt blob was published under its final name")
	}
}

func TestFetchBlobRejectsCorruptedLFSContent(t *testing.T) {
	body := []byte("model weights")
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{
		Path:     "config.json",
		BlobName: gitBlobID(body),
		Size:     int64(len(body)),
		SHA256:   strings.Repeat("f", 64),
	}
	err := runFetch(t, client, paths, f)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	if _, err := os.Stat(paths.BlobPath(f.BlobName)); err == nil {
		t.Error("corrupt LFS blob was published under its final name")
	}
}

func TestFetchBlobAcceptsMatchingLFSContent(t *testing.T) {
	body := []byte("model weights")
	sum := sha256.Sum256(body)
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{
		Path:     "config.json",
		BlobName: gitBlobID(body),
		Size:     int64(len(body)),
		SHA256:   hex.EncodeToString(sum[:]),
	}
	if err := runFetch(t, client, paths, f); err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

// A short body against a declared size must fail rather than publish a truncated
// blob, which would otherwise look complete to every later reader.
func TestFetchBlobRejectsTruncatedDownload(t *testing.T) {
	body := []byte("short")
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{
		Path:     "config.json",
		BlobName: gitBlobID(body),
		Size:     1024,
	}
	err := runFetch(t, client, paths, f)
	if err == nil || !strings.Contains(err.Error(), "expected 1024 bytes, got 5") {
		t.Fatalf("err = %v, want byte-count mismatch", err)
	}
}

// Re-running over an existing correct blob must short-circuit, not re-download.
func TestFetchBlobSkipsAlreadyCached(t *testing.T) {
	body := []byte(`{"a":1}`)
	client, paths := newTestHub(t, body)

	f := &mapping.FileManifest{Path: "config.json", BlobName: gitBlobID(body), Size: int64(len(body))}
	if err := runFetch(t, client, paths, f); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// A second pass must report the bytes as done without touching the network.
	// Dead endpoint proves it: any HTTP attempt would fail the fetch.
	dead := hfapi.NewClient("http://127.0.0.1:1")
	state := &xferState{total: f.Size}
	if err := fetchBlob(context.Background(), dead, paths, "testorg/tiny", hfcache.Model,
		testCommit, f, state, func(wire.ControlEvent) error { return nil }); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if state.done != int64(len(body)) {
		t.Errorf("state.done = %d, want %d", state.done, len(body))
	}
}

// The resolve URL must namespace datasets and spaces but not models, and must
// survive filenames with reserved characters.
func TestResolveURL(t *testing.T) {
	c := hfapi.NewClient("https://hub.example/")

	if got, want := c.ResolveURL("org/m", hfcache.Model, "abc", "weights/layer 1.bin"),
		"https://hub.example/org/m/resolve/abc/weights/layer%201.bin"; got != want {
		t.Errorf("model resolve = %s, want %s", got, want)
	}
	if got, want := c.ResolveURL("org/d", hfcache.Dataset, "abc", "x.parquet"),
		"https://hub.example/datasets/org/d/resolve/abc/x.parquet"; got != want {
		t.Errorf("dataset resolve = %s, want %s", got, want)
	}
	if got, want := c.ResolveURL("org/s", hfcache.Space, "abc", "app.py"),
		"https://hub.example/spaces/org/s/resolve/abc/app.py"; got != want {
		t.Errorf("space resolve = %s, want %s", got, want)
	}
}
