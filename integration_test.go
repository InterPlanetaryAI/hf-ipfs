package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ufsio "github.com/ipfs/boxo/ipld/unixfs/io"
	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"

	"github.com/ipai/hf-ipfs/internal/config"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/ingest"
	"github.com/ipai/hf-ipfs/internal/node"
	"github.com/ipai/hf-ipfs/internal/pull"
)

const integrationCommit = "607a30d783dfa663caf39e06633721c8d4cfcd7e"

// writeFixtureHub lays out a finalized HF revision: blobs plus a snapshot tree of
// relative symlinks plus refs/main, exactly as the `hf` CLI would leave it.
func writeFixtureHub(t *testing.T, hub, repoID string) map[string][]byte {
	t.Helper()
	p := hfcache.NewPaths(hub, repoID, hfcache.Model)
	if err := os.MkdirAll(p.Blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	snap := p.SnapshotDir(integrationCommit)
	if err := os.MkdirAll(filepath.Join(snap, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string][]byte{
		"config.json":           []byte(`{"architectures":["LlamaForCausalLM"]}`),
		"tokenizer.json":        []byte(`{"vocab":{"hello":0}}`),
		"sub/weights-00001.bin": makeBlob(t, 3<<20),    // 3 MiB -> 3 chunks
		"sub/deep/notes.txt":    makeBlob(t, 1<<20+13), // crosses a chunk boundary
		"empty.bin":             nil,
	}

	for rel, data := range files {
		name := hexOf(data)
		if err := os.WriteFile(filepath.Join(p.Blobs, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(snap, filepath.FromSlash(rel))
		target, err := filepath.Rel(filepath.Dir(link), filepath.Join(p.Blobs, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := hfcache.WriteRef(p, "main", integrationCommit); err != nil {
		t.Fatal(err)
	}
	return files
}

func makeBlob(t *testing.T, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i*7 + 13)
	}
	return buf
}

func hexOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func nodeConfig(repo, hub string, dhtServer bool) *config.Config {
	return &config.Config{
		RepoDir:        repo,
		HFHubDir:       hub,
		Listen:         []string{"/ip4/127.0.0.1/tcp/0"},
		ChunkSize:      1 << 20,
		DHTServer:      dhtServer,
		HFEndpoint:     "https://huggingface.co",
		APISocket:      filepath.Join(repo, "api.sock"),
		RescanInterval: 0,
	}
}

// TestIngestThenPull runs the whole bridge in-process: a sharing node ingests a
// finalized HF revision with nocopy adds, a second node pulls it over libp2p and
// must reproduce the HF cache layout byte for byte.
func TestIngestThenPull(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const repoID = "testorg/testmodel"
	srcHub := t.TempDir()
	dstHub := t.TempDir()
	want := writeFixtureHub(t, srcHub, repoID)

	src, err := node.New(ctx, nodeConfig(filepath.Join(t.TempDir(), "repoSrc"), srcHub, true))
	if err != nil {
		t.Fatalf("source node: %v", err)
	}
	defer src.Close()

	res, err := ingest.Run(ctx, src, repoID, hfcache.Model, integrationCommit, nil)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Files != len(want) {
		t.Fatalf("ingested %d files, want %d", res.Files, len(want))
	}
	if res.TotalSize == 0 {
		t.Fatal("ingest reported zero total size")
	}

	dst, err := node.New(ctx, nodeConfig(filepath.Join(t.TempDir(), "repoDst"), dstHub, false))
	if err != nil {
		t.Fatalf("destination node: %v", err)
	}
	defer dst.Close()

	if err := pull.Run(ctx, dst, pull.Options{
		RepoID:   repoID,
		RepoType: hfcache.Model,
		Commit:   integrationCommit,
		Ref:      "main",
		Peers:    []string{src.Addrs()[0]},
	}, nil); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Every file must be present in the destination blobs with matching content.
	dstPaths := hfcache.NewPaths(dstHub, repoID, hfcache.Model)
	gotFiles, err := hfcache.ReadSnapshot(dstPaths, integrationCommit)
	if err != nil {
		t.Fatalf("read pulled snapshot: %v", err)
	}
	if len(gotFiles) != len(want) {
		t.Fatalf("pulled %d files, want %d", len(gotFiles), len(want))
	}

	byPath := map[string][]byte{}
	for _, f := range gotFiles {
		data, err := os.ReadFile(filepath.Join(dstPaths.Blobs, f.BlobName))
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		byPath[f.Path] = data
	}
	for rel, wantData := range want {
		gotData, ok := byPath[rel]
		if !ok {
			t.Errorf("%s: not present after pull", rel)
			continue
		}
		if hexOf(gotData) != hexOf(wantData) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)",
				rel, len(gotData), len(wantData))
		}
	}

	// The pulled node must have recorded the mapping and be able to answer for it.
	entry, ok, err := dst.Mapping.Get(ctx, integrationCommit)
	if err != nil || !ok {
		t.Fatalf("destination has no mapping after pull (ok=%v err=%v)", ok, err)
	}
	if entry.ActualCID != res.ActualCID.String() {
		t.Errorf("actual cid mismatch: %s vs %s", entry.ActualCID, res.ActualCID)
	}
	if entry.Origin != "pull" {
		t.Errorf("origin = %q, want %q", entry.Origin, "pull")
	}

	// The destination must serve the tree from its own store: read every chunk
	// back through its filestore, which resolves to the newly written blobs.
	root, err := cid.Decode(entry.ActualCID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dst.DAG.Get(ctx, root); err != nil {
		t.Fatalf("destination cannot serve its own root: %v", err)
	}
	for _, f := range entry.Files {
		fileCid, err := findFileNode(ctx, dst.DAG, root, f.Path)
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		var leaves []cid.Cid
		if err := walkLeaves(ctx, dst.DAG, fileCid, &leaves); err != nil {
			t.Errorf("%s: walk: %v", f.Path, err)
			continue
		}
		if f.Size == 0 {
			continue
		}
		if len(leaves) == 0 {
			t.Errorf("%s: no chunks found", f.Path)
			continue
		}
		var total int64
		for _, c := range leaves {
			blk, err := dst.Fstore.Get(ctx, c)
			if err != nil {
				t.Errorf("%s: chunk %s not served locally: %v", f.Path, c, err)
				continue
			}
			total += int64(len(blk.RawData()))
		}
		if total != f.Size {
			t.Errorf("%s: served %d bytes through filestore, want %d", f.Path, total, f.Size)
		}
	}
}

func findFileNode(ctx context.Context, ds ipld.DAGService, root cid.Cid, rel string) (cid.Cid, error) {
	cur := root
	for _, part := range strings.Split(strings.Trim(rel, "/"), "/") {
		nd, err := ds.Get(ctx, cur)
		if err != nil {
			return cid.Undef, err
		}
		dir, err := ufsio.NewDirectoryFromNode(ds, nd)
		if err != nil {
			return cid.Undef, err
		}
		child, err := dir.Find(ctx, part)
		if err != nil {
			return cid.Undef, err
		}
		cur = child.Cid()
	}
	return cur, nil
}

func walkLeaves(ctx context.Context, ds ipld.DAGService, c cid.Cid, out *[]cid.Cid) error {
	if c.Type() == cid.Raw {
		*out = append(*out, c)
		return nil
	}
	nd, err := ds.Get(ctx, c)
	if err != nil {
		return err
	}
	for _, l := range nd.Links() {
		if err := walkLeaves(ctx, ds, l.Cid, out); err != nil {
			return err
		}
	}
	return nil
}
