package hfcache

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

const testCommit = "607a30d783dfa663caf39e06633721c8d4cfcd7e"

func writeBlob(t *testing.T, p, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	name := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(p, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

// buildHub lays out a finalized revision exactly like the `hf` CLI does.
func buildHub(t *testing.T, hub string) map[string]string {
	t.Helper()
	p := NewPaths(hub, "testorg/testmodel", Model)
	for _, d := range []string{p.Blobs, p.Snapshots, p.Refs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	snap := p.SnapshotDir(testCommit)
	if err := os.MkdirAll(filepath.Join(snap, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	names := map[string]string{
		"config.json":       writeBlob(t, p.Blobs, `{"hidden":42}`),
		"sub/weights.bin":   writeBlob(t, p.Blobs, "weights-weights-weights"),
		"sub/deep/note.txt": writeBlob(t, p.Blobs, "deep note"),
		"empty.bin":         writeBlob(t, p.Blobs, ""),
	}
	for rel, name := range names {
		link := filepath.Join(snap, filepath.FromSlash(rel))
		target, err := filepath.Rel(filepath.Dir(link), filepath.Join(p.Blobs, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteRef(p, "main", testCommit); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestReadSnapshot(t *testing.T) {
	hub := t.TempDir()
	want := buildHub(t, hub)
	p := NewPaths(hub, "testorg/testmodel", Model)

	files, err := ReadSnapshot(p, testCommit)
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d", len(files), len(want))
	}

	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.BlobName
		if f.SrcPath == "" {
			t.Errorf("%s: SrcPath not recorded", f.Path)
		}
		if f.SHA256 != f.BlobName {
			t.Errorf("%s: blob name %q is not the content sha256 %q", f.Path, f.BlobName, f.SHA256)
		}
		if f.Path == "empty.bin" && f.Size != 0 {
			t.Errorf("empty.bin size = %d, want 0", f.Size)
		}
	}
	for rel, name := range want {
		if got[rel] != name {
			t.Errorf("%s: blob = %q, want %q", rel, got[rel], name)
		}
	}
}

func TestReadSnapshotSortedAndStable(t *testing.T) {
	hub := t.TempDir()
	buildHub(t, hub)
	p := NewPaths(hub, "testorg/testmodel", Model)

	a, err := ReadSnapshot(p, testCommit)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadSnapshot(p, testCommit)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("unstable file count %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Path != b[i].Path || a[i].BlobName != b[i].BlobName {
			t.Errorf("index %d differs: %s vs %s", i, a[i].Path, b[i].Path)
		}
		if i > 0 && a[i].Path < a[i-1].Path {
			t.Errorf("manifest not sorted at %d: %s after %s", i, a[i].Path, a[i-1].Path)
		}
	}
}

func TestSnapshotComplete(t *testing.T) {
	hub := t.TempDir()
	names := buildHub(t, hub)
	p := NewPaths(hub, "testorg/testmodel", Model)

	if !SnapshotComplete(p, testCommit) {
		t.Fatal("a fully materialised snapshot should report complete")
	}

	// Drop the blob behind one symlink: the download is no longer complete.
	if err := os.Remove(p.BlobPath(names["config.json"])); err != nil {
		t.Fatal(err)
	}
	if SnapshotComplete(p, testCommit) {
		t.Error("snapshot with a missing blob should not report complete")
	}
}

func TestWriteSnapshotRecreatesLayout(t *testing.T) {
	srcHub := t.TempDir()
	buildHub(t, srcHub)
	src := NewPaths(srcHub, "testorg/testmodel", Model)

	files, err := ReadSnapshot(src, testCommit)
	if err != nil {
		t.Fatal(err)
	}

	dstHub := t.TempDir()
	dst := NewPaths(dstHub, "testorg/testmodel", Model)
	if err := os.MkdirAll(dst.Blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Copy the blobs across, as a pull would have done.
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(src.Blobs, f.BlobName))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst.BlobPath(f.BlobName), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := WriteSnapshot(dst, testCommit, files, "main"); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	for _, f := range files {
		link := filepath.Join(dst.SnapshotDir(testCommit), filepath.FromSlash(f.Path))
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s: not a symlink: %v", f.Path, err)
			continue
		}
		abs := filepath.Join(filepath.Dir(link), target)
		if abs != dst.BlobPath(f.BlobName) {
			t.Errorf("%s: link resolves to %s, want %s", f.Path, abs, dst.BlobPath(f.BlobName))
		}
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("%s: link target missing: %v", f.Path, err)
		}
	}

	gotRef, err := dst.CurrentCommit("main")
	if err != nil {
		t.Fatalf("CurrentCommit: %v", err)
	}
	if gotRef != testCommit {
		t.Errorf("refs/main = %q, want %q", gotRef, testCommit)
	}

	// Writing again must be idempotent.
	if err := WriteSnapshot(dst, testCommit, files, "main"); err != nil {
		t.Errorf("second WriteSnapshot should be idempotent: %v", err)
	}
}

func TestRepoDirNameAndParse(t *testing.T) {
	cases := []struct {
		repoID string
		typ    RepoType
		dir    string
	}{
		{"testorg/testmodel", Model, "models--testorg--testmodel"},
		{"testorg/data", Dataset, "datasets--testorg--data"},
		{"testorg/demo", Space, "spaces--testorg--demo"},
	}
	for _, c := range cases {
		if got := RepoDirName(c.repoID, c.typ); got != c.dir {
			t.Errorf("RepoDirName(%q, %s) = %q, want %q", c.repoID, c.typ, got, c.dir)
		}
		id, typ, ok := RepoFromDirName(c.dir)
		if !ok {
			t.Fatalf("RepoFromDirName(%q) failed", c.dir)
		}
		if id != c.repoID || typ != c.typ {
			t.Errorf("RepoFromDirName(%q) = (%q, %s), want (%q, %s)", c.dir, id, typ, c.repoID, c.typ)
		}
	}
	if _, _, ok := RepoFromDirName("not-a-repo-dir"); ok {
		t.Error("unrelated directory wrongly parsed as a repo")
	}
}

func TestListRepos(t *testing.T) {
	hub := t.TempDir()
	buildHub(t, hub)
	if err := os.MkdirAll(filepath.Join(hub, "datasets--org--set"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(hub, "random-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	repos, err := ListRepos(hub)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("ListRepos = %v, want 2 entries", repos)
	}
	found := map[string]bool{}
	for _, r := range repos {
		found[r] = true
	}
	if !found["testorg/testmodel"] || !found["org/set"] {
		t.Errorf("unexpected repo set: %v", repos)
	}
}

func TestCurrentCommitRejectsGarbage(t *testing.T) {
	hub := t.TempDir()
	p := NewPaths(hub, "a/b", Model)
	if err := os.MkdirAll(p.Refs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.RefPath("main"), []byte("not-a-hash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CurrentCommit("main"); err == nil {
		t.Error("a non-40-char ref should be rejected")
	}
}
