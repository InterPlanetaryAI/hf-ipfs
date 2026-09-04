package pull

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipai/hf-ipfs/internal/hfapi"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
)

// Fixtures are verbatim live Hub API output and the trees/<commit>.json the
// real `hf` CLI wrote for the same commit. Regenerate with:
//
//	curl -sS https://huggingface.co/api/models/$R/revision/$C?blobs=true  > revision.json
//	curl -sS https://huggingface.co/api/models/$R/tree/$C?recursive=true  > tree.json
const (
	goldenRepo   = "facebook/sapiens2-seg-0.4b"
	goldenCommit = "449b3c5335e6722bb94990abdd1aa6e612432f22"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

// hubStub serves the recorded revision and tree responses off their URL shape.
func hubStub(t *testing.T) *httptest.Server {
	t.Helper()
	revision := readTestdata(t, "revision.json")
	tree := readTestdata(t, "tree.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/tree/") {
			_, _ = w.Write(tree)
			return
		}
		_, _ = w.Write(revision)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTreeByteIdenticalToHFCLI is the 1:1 compatibility proof. It runs the
// actual pull path — RepoInfoAt + TreeAt + manifestFromRevision + WriteTree —
// against recorded live Hub responses and requires the generated
// trees/<commit>.json to match, byte for byte, the file the real `hf` CLI
// produced for the same commit.
func TestTreeByteIdenticalToHFCLI(t *testing.T) {
	ctx := context.Background()
	client := hfapi.NewClient(hubStub(t).URL)

	info, err := client.RepoInfoAt(ctx, goldenRepo, hfcache.Model, goldenCommit, true)
	if err != nil {
		t.Fatalf("RepoInfoAt: %v", err)
	}
	tree, err := client.TreeAt(ctx, goldenRepo, hfcache.Model, goldenCommit)
	if err != nil {
		t.Fatalf("TreeAt: %v", err)
	}
	xetByPath := make(map[string]string, len(tree))
	for _, e := range tree {
		if e.XetHash != "" {
			xetByPath[e.Path] = e.XetHash
		}
	}

	files, _, err := manifestFromRevision(info.Siblings, xetByPath)
	if err != nil {
		t.Fatalf("manifestFromRevision: %v", err)
	}

	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, goldenRepo, hfcache.Model)
	if err := hfcache.WriteTree(paths, goldenCommit, files); err != nil {
		t.Fatalf("WriteTree: %v", err)
	}

	got, err := os.ReadFile(paths.TreePath(goldenCommit))
	if err != nil {
		t.Fatalf("read generated tree: %v", err)
	}
	want := readTestdata(t, "golden_tree.json")

	if !bytes.Equal(got, want) {
		t.Fatalf("generated tree is not byte-identical to the hf CLI's\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	st, err := os.Stat(paths.TreePath(goldenCommit))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("tree mode = %o, want 600 (matching the hf CLI)", perm)
	}
}

// The two safetensors shards are LFS/Xet backed and must carry the extra
// fields; the four plain git blobs must carry only size and blob_id. Getting
// this wrong is the easy way to produce a tree that is not 1:1.
func TestTreeLFSFieldsOnlyForLFSFiles(t *testing.T) {
	ctx := context.Background()
	client := hfapi.NewClient(hubStub(t).URL)

	info, err := client.RepoInfoAt(ctx, goldenRepo, hfcache.Model, goldenCommit, true)
	if err != nil {
		t.Fatalf("RepoInfoAt: %v", err)
	}
	tree, err := client.TreeAt(ctx, goldenRepo, hfcache.Model, goldenCommit)
	if err != nil {
		t.Fatalf("TreeAt: %v", err)
	}
	xetByPath := make(map[string]string, len(tree))
	for _, e := range tree {
		if e.XetHash != "" {
			xetByPath[e.Path] = e.XetHash
		}
	}
	files, _, err := manifestFromRevision(info.Siblings, xetByPath)
	if err != nil {
		t.Fatalf("manifestFromRevision: %v", err)
	}

	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, goldenRepo, hfcache.Model)
	if err := hfcache.WriteTree(paths, goldenCommit, files); err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	doc, ok, err := hfcache.ReadTree(paths, goldenCommit)
	if err != nil || !ok {
		t.Fatalf("ReadTree: ok=%v err=%v", ok, err)
	}

	lfsFiles := map[string]bool{}
	plainFiles := map[string]bool{}
	for _, f := range files {
		if f.LFS {
			lfsFiles[f.Path] = true
		} else {
			plainFiles[f.Path] = true
		}
	}
	if len(lfsFiles) == 0 || len(plainFiles) == 0 {
		t.Fatalf("fixture must contain both LFS and plain files (lfs=%d plain=%d)", len(lfsFiles), len(plainFiles))
	}

	for path := range lfsFiles {
		tf := doc.Files[path]
		if tf.LFSSHA256 == "" || tf.LFSSize == 0 {
			t.Errorf("LFS file %s missing lfs_sha256/lfs_size: %+v", path, tf)
		}
		if tf.XetHash == "" {
			t.Errorf("Xet-backed file %s missing xet_hash", path)
		}
	}
	for path := range plainFiles {
		tf := doc.Files[path]
		if tf.LFSSHA256 != "" || tf.LFSSize != 0 {
			t.Errorf("plain git blob %s must not carry lfs fields: %+v", path, tf)
		}
		if tf.XetHash != "" {
			t.Errorf("plain git blob %s must not carry xet_hash: %+v", path, tf)
		}
	}
}

// A manifest that never carried Xet metadata must not strip what an existing
// tree already recorded. This is the downgrade guard: re-writing a snapshot
// from a bare symlink walk would otherwise silently lose the hashes.
func TestWriteTreePreservesRicherExistingRecord(t *testing.T) {
	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, "org/m", hfcache.Model)
	commit := "0123456789abcdef0123456789abcdef01234567"

	rich := []mapping.FileManifest{
		{Path: "model.bin", BlobName: "blobA", Size: 100, SHA256: "shaA", LFS: true, XetHash: "xetA"},
	}
	if err := hfcache.WriteTree(paths, commit, rich); err != nil {
		t.Fatalf("WriteTree(rich): %v", err)
	}

	// Same file, but the manifest knows nothing about LFS or Xet.
	poor := []mapping.FileManifest{
		{Path: "model.bin", BlobName: "blobA", Size: 100},
	}
	if err := hfcache.WriteTree(paths, commit, poor); err != nil {
		t.Fatalf("WriteTree(poor): %v", err)
	}

	doc, ok, err := hfcache.ReadTree(paths, commit)
	if err != nil || !ok {
		t.Fatalf("ReadTree: ok=%v err=%v", ok, err)
	}
	tf := doc.Files["model.bin"]
	if tf.XetHash != "xetA" {
		t.Errorf("xet_hash was downgraded away: %+v", tf)
	}
	if tf.LFSSHA256 != "shaA" || tf.LFSSize != 100 {
		t.Errorf("lfs fields were downgraded away: %+v", tf)
	}
}

// New files in a re-written tree still land; the merge must not freeze the set.
func TestWriteTreeAddsNewFiles(t *testing.T) {
	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, "org/m", hfcache.Model)
	commit := "0123456789abcdef0123456789abcdef01234567"

	if err := hfcache.WriteTree(paths, commit, []mapping.FileManifest{
		{Path: "a.bin", BlobName: "blobA", Size: 1},
	}); err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	if err := hfcache.WriteTree(paths, commit, []mapping.FileManifest{
		{Path: "a.bin", BlobName: "blobA", Size: 1},
		{Path: "b.bin", BlobName: "blobB", Size: 2},
	}); err != nil {
		t.Fatalf("WriteTree: %v", err)
	}

	doc, _, err := hfcache.ReadTree(paths, commit)
	if err != nil {
		t.Fatalf("ReadTree: %v", err)
	}
	if len(doc.Files) != 2 {
		t.Fatalf("tree has %d files, want 2: %+v", len(doc.Files), doc.Files)
	}
	if doc.FormatVersion != hfcache.TreeFormatVersion {
		t.Errorf("format_version = %d, want %d", doc.FormatVersion, hfcache.TreeFormatVersion)
	}
}

// EnrichFromTree is what makes a repo first downloaded by `hf` propagate
// full-fidelity metadata over P2P: the ingest walk cannot see LFS-ness or
// Xet hashes, but the tree the CLI left behind can supply them.
func TestEnrichFromTreeRecoversMetadata(t *testing.T) {
	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, goldenRepo, hfcache.Model)

	// Seed the tree the way the hf CLI would have: the directory and the
	// file it wrote, byte for byte.
	if err := os.MkdirAll(paths.Trees, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TreePath(goldenCommit), readTestdata(t, "golden_tree.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A bare symlink-walk manifest: paths, blobs, sizes, hashes. No LFS/Xet.
	files := []mapping.FileManifest{
		{Path: "model.safetensors", BlobName: "f303a51187cdc9d0d003880e8eac8805972af58a", Size: 1626451404},
		{Path: "config.json", BlobName: "8e95fad94e16745e1e15701fe2596058ca25c0d4", Size: 3917},
		{Path: "unlisted.txt", BlobName: "deadbeef", Size: 5},
	}
	hfcache.EnrichFromTree(paths, goldenCommit, files)

	if !files[0].LFS {
		t.Error("model.safetensors should be marked LFS")
	}
	if files[0].XetHash != "64b4eafe68248256b27c108654f9afeb6c52bcfc6e3c26c96ea20bdd02e9f3cf" {
		t.Errorf("xet_hash not recovered: %q", files[0].XetHash)
	}
	if files[1].LFS {
		t.Error("config.json is a plain git blob and must not be marked LFS")
	}
	if files[1].XetHash != "" {
		t.Errorf("config.json should have no xet_hash, got %q", files[1].XetHash)
	}
	// An absent entry must leave the manifest alone, not zero it out.
	if files[2].BlobName != "deadbeef" || files[2].Size != 5 {
		t.Errorf("unlisted file was disturbed: %+v", files[2])
	}
}

// No tree at all is normal for older caches and must be a silent no-op.
func TestEnrichFromTreeMissingTreeIsNoop(t *testing.T) {
	dir := t.TempDir()
	paths := hfcache.NewPaths(dir, "org/m", hfcache.Model)
	files := []mapping.FileManifest{{Path: "a.bin", BlobName: "blobA", Size: 1}}
	hfcache.EnrichFromTree(paths, "0123456789abcdef0123456789abcdef01234567", files)
	if files[0].LFS || files[0].XetHash != "" {
		t.Errorf("no-op violated: %+v", files[0])
	}
}
