package evict_test

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/ipfs/boxo/blockstore"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/boxo/filestore"
	"github.com/ipfs/boxo/filestore/posinfo"
	u "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	mh "github.com/multiformats/go-multihash"

	"github.com/ipai/hf-ipfs/internal/evict"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/store"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

type env struct {
	tgt     *evict.Target
	fm      *filestore.FileManager
	paths   hfcache.Paths
	body    []byte
	blobAbs string
	leaf    cid.Cid
	root    cid.Cid
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	hub := filepath.Join(dir, "hub")
	st, err := store.Open(filepath.Join(dir, "hf-ipfs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &env{
		tgt: &evict.Target{
			Mapping:  mapping.New(st.DS("map")),
			MainBS:   blockstore.NewBlockstore(st.DS("blocks")),
			FstoreDS: st.DS("filestore"),
			HubDir:   hub,
		},
		fm:    filestore.NewFileManager(st.DS("filestore"), hub),
		paths: hfcache.NewPaths(hub, "org/test", hfcache.Model),
		body:  []byte("hello swarm, one chunk of model bytes"),
	}
	e.fm.AllowFiles = true

	if err := os.MkdirAll(e.paths.Blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(e.body)
	e.blobAbs = e.paths.BlobPath(hex(sum[:]))
	if err := os.WriteFile(e.blobAbs, e.body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.paths.SnapshotDir(testCommit), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.paths.SnapshotDir(testCommit), "f.bin")
	if err := os.Symlink(filepath.Join("..", "..", "blobs", filepath.Base(e.blobAbs)), link); err != nil {
		t.Fatal(err)
	}
	if err := hfcache.WriteRef(e.paths, "main", testCommit); err != nil {
		t.Fatal(err)
	}

	mhash, err := mh.Sum(e.body, mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	e.leaf = cid.NewCidV1(cid.Raw, mhash)
	blk, err := blocks.NewBlockWithCid(e.body, e.leaf)
	if err != nil {
		t.Fatal(err)
	}
	nd, err := u.DecodeRawBlock(blk)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fm.Put(context.Background(), &posinfo.FilestoreNode{
		Node:    nd,
		PosInfo: &posinfo.PosInfo{FullPath: e.blobAbs, Offset: 0},
	}); err != nil {
		t.Fatal(err)
	}

	root := &u.ProtoNode{}
	if err := root.SetLinks([]*format.Link{{Name: "f.bin", Size: uint64(len(e.body)), Cid: e.leaf}}); err != nil {
		t.Fatal(err)
	}
	e.root = root.Cid()
	rblk, err := blocks.NewBlockWithCid(root.RawData(), e.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.tgt.MainBS.Put(context.Background(), rblk); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) share(commit string) *mapping.Entry {
	entry := &mapping.Entry{
		RepoID: "org/test", RepoType: string(hfcache.Model), CommitHash: commit,
		ActualCID: e.root.String(),
		Files: []mapping.FileManifest{{
			Path: "f.bin", BlobName: filepath.Base(e.blobAbs), Size: int64(len(e.body)),
		}},
		TotalSize: int64(len(e.body)),
	}
	if err := e.tgt.Mapping.Put(context.Background(), entry); err != nil {
		panic(err)
	}
	return entry
}

func TestEvictRemovesEverything(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	entry := e.share(testCommit)

	if err := evict.Commit(ctx, e.tgt, entry, nil); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := e.tgt.Mapping.Get(ctx, testCommit); ok {
		t.Error("mapping entry survived")
	}
	if has, _ := e.tgt.MainBS.Has(ctx, e.root); has {
		t.Error("root block survived")
	}
	if has, _ := e.fm.Has(ctx, e.leaf); has {
		t.Error("filestore ref survived")
	}
	if _, err := os.Stat(e.blobAbs); !os.IsNotExist(err) {
		t.Error("blob file survived")
	}
	if _, err := os.Stat(e.paths.SnapshotDir(testCommit)); !os.IsNotExist(err) {
		t.Error("snapshot dir survived")
	}
	if _, err := e.paths.CurrentCommit("main"); err == nil {
		t.Error("refs/main still points at evicted commit")
	}
}

func TestEvictKeepsSharedContent(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	a := e.share(testCommit)
	b := e.share("1111111111111111111111111111111111111111") // same root+leaf, different commit

	if err := evict.Commit(ctx, e.tgt, a, nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.tgt.MainBS.Has(ctx, e.root); !has {
		t.Error("shared root block deleted")
	}
	if has, _ := e.fm.Has(ctx, e.leaf); !has {
		t.Error("shared filestore ref deleted")
	}
	if _, err := os.Stat(e.blobAbs); err != nil {
		t.Error("blob referenced by remaining mapping was deleted")
	}
	if _, ok, _ := e.tgt.Mapping.Get(ctx, "1111111111111111111111111111111111111111"); !ok {
		t.Error("other mapping entry was touched")
	}

	if err := evict.Commit(ctx, e.tgt, b, nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.fm.Has(ctx, e.leaf); has {
		t.Error("filestore ref survived last eviction")
	}
	if _, err := os.Stat(e.blobAbs); !os.IsNotExist(err) {
		t.Error("blob survived last eviction")
	}
}

func TestRepoEvictsAllRevisions(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	e.share(testCommit)
	e.share("2222222222222222222222222222222222222222")

	n, err := evict.Repo(ctx, e.tgt, "org/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("evicted %d, want 2", n)
	}
	entries, _ := e.tgt.Mapping.List(ctx)
	if len(entries) != 0 {
		t.Fatalf("mapping not empty: %d", len(entries))
	}
}

func hex(b []byte) string {
	const l = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, l[c>>4], l[c&0xf])
	}
	return string(out)
}
