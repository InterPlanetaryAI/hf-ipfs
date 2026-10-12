// Package evict removes a shared revision from every structure that records
// it, in the reverse order of ingest: filestore refs and DAG blocks (only when
// no other mapping still references them), the unreferenced blob files, the
// mapping entry itself, and the HF cache plumbing for that revision
// (snapshot symlinks, trees/<commit>.json, refs/main).
//
// Provider records are not revoked — kad-dht has no unannounce — but the
// mapping entry is what makes a peer answer /hf-ipfs/map/1.0.0 and what
// ReprovideAll re-announces, so once it is gone the node stops claiming the
// revision and stale provider records expire on their own TTL.
package evict

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/filestore"
	"github.com/ipfs/boxo/filestore/pb"
	u "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	dstore "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	"google.golang.org/protobuf/proto"

	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/node"
)

// Target is the storage surface evict operates on. FromNode derives it from a
// live node; tests build it directly over a temp store.
type Target struct {
	Mapping  *mapping.DB
	MainBS   blockstore.Blockstore
	FstoreDS dstore.Batching
	HubDir   string
}

// FromNode wires a Target to a running node's store buckets.
func FromNode(n *node.Node) *Target {
	return &Target{
		Mapping:  n.Mapping,
		MainBS:   n.MainBS,
		FstoreDS: n.Store.DS("filestore"),
		HubDir:   n.Cfg.HFHubDir,
	}
}

// Repo evicts every mapped revision of repoID and returns how many went.
func Repo(ctx context.Context, tgt *Target, repoID string, progress func(string)) (int, error) {
	entries, err := tgt.Mapping.List(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.RepoID != repoID {
			continue
		}
		if err := Commit(ctx, tgt, e, progress); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("no shared revision for %s", repoID)
	}
	return n, nil
}

// Commit removes one mapped revision from all coupled structures.
func Commit(ctx context.Context, tgt *Target, entry *mapping.Entry, progress func(string)) error {
	if progress == nil {
		progress = func(string) {}
	}
	t := hfcache.RepoType(entry.RepoType)
	paths := hfcache.NewPaths(tgt.HubDir, entry.RepoID, t)
	fm := filestore.NewFileManager(tgt.FstoreDS, tgt.HubDir)

	// Blocks and filestore refs are content-addressed and shared between
	// revisions (and repos) with identical files. Collect everything the
	// OTHER mappings still reference before deleting anything.
	keep := make(map[cid.Cid]struct{})
	others, err := tgt.Mapping.List(ctx)
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.CommitHash == entry.CommitHash {
			continue
		}
		oc, err := cid.Decode(o.ActualCID)
		if err != nil {
			continue
		}
		walk(ctx, tgt.MainBS, oc, keep)
	}

	target := make(map[cid.Cid]struct{})
	root, err := cid.Decode(entry.ActualCID)
	if err != nil {
		return fmt.Errorf("entry %s: bad actual cid: %w", entry.RepoID, err)
	}
	walk(ctx, tgt.MainBS, root, target)

	for c := range target {
		if _, shared := keep[c]; shared {
			continue
		}
		if has, err := fm.Has(ctx, c); err == nil && has {
			if err := fm.DeleteBlock(ctx, c); err != nil {
				return fmt.Errorf("delete filestore ref %s: %w", c, err)
			}
			continue
		}
		if has, err := tgt.MainBS.Has(ctx, c); err == nil && has {
			if err := tgt.MainBS.DeleteBlock(ctx, c); err != nil {
				return fmt.Errorf("delete block %s: %w", c, err)
			}
		}
	}

	// A blob file may only go when no remaining filestore reference points
	// into it (another revision can share the exact same file).
	remaining, err := referencedPaths(ctx, tgt.FstoreDS, tgt.HubDir)
	if err != nil {
		return err
	}
	for _, f := range entry.Files {
		if !hfcache.SafeBlobName(f.BlobName) {
			// Mapping written by a pre-validation release: never remove
			// outside blobs/, leave the stray file for a manual sweep.
			continue
		}
		blob := filepath.Clean(paths.BlobPath(f.BlobName))
		if remaining[blob] {
			continue
		}
		if err := os.Remove(blob); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove blob %s: %w", blob, err)
		}
	}

	if err := tgt.Mapping.Delete(ctx, entry.CommitHash); err != nil {
		return fmt.Errorf("delete mapping: %w", err)
	}

	// HF cache plumbing for the evicted revision. Idempotent: the prune
	// path calls this after someone already deleted the files by hand.
	_ = os.RemoveAll(paths.SnapshotDir(entry.CommitHash))
	_ = os.Remove(paths.TreePath(entry.CommitHash))
	if cur, err := paths.CurrentCommit("main"); err == nil && cur == entry.CommitHash {
		_ = os.Remove(paths.RefPath("main"))
	}

	progress(fmt.Sprintf("evicted %s@%s", entry.RepoID, short(entry.CommitHash)))
	return nil
}

// walk collects every CID reachable from root through blocks present in bs.
// CIDs absent from bs are filestore leaves (or already gone): recorded, not
// followed. Loading only dag-pb blocks keeps this metadata-cheap — raw leaf
// bytes are never read.
func walk(ctx context.Context, bs blockstore.Blockstore, c cid.Cid, out map[cid.Cid]struct{}) {
	if _, seen := out[c]; seen {
		return
	}
	out[c] = struct{}{}
	has, err := bs.Has(ctx, c)
	if err != nil || !has {
		return
	}
	blk, err := bs.Get(ctx, c)
	if err != nil {
		return
	}
	nd, err := u.DecodeProtobuf(blk.RawData())
	if err != nil {
		return // raw leaf or non-pb block: no links
	}
	for _, l := range nd.Links() {
		if l.Cid.Defined() {
			walk(ctx, bs, l.Cid, out)
		}
	}
}

// referencedPaths returns the set of absolute file paths still referenced by
// any filestore DataObj.
func referencedPaths(ctx context.Context, fds dstore.Batching, root string) (map[string]bool, error) {
	out := make(map[string]bool)
	res, err := fds.Query(ctx, dsq.Query{})
	if err != nil {
		return nil, err
	}
	defer res.Close()
	for r := range res.Next() {
		if r.Error != nil {
			return nil, r.Error
		}
		var dobj pb.DataObj
		if err := proto.Unmarshal(r.Value, &dobj); err != nil {
			continue
		}
		p := dobj.GetFilePath()
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, filepath.FromSlash(p))
		}
		out[filepath.Clean(p)] = true
	}
	return out, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
