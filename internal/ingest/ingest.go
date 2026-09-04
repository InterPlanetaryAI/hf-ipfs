// Package ingest turns a finalized Hugging Face snapshot revision into a UnixFS
// DAG whose leaf chunks are filestore (nocopy) references pointing straight at
// `hub/<repo>/blobs/<name>`. No model bytes are copied into hf-ipfs' own
// datastore; only (path, offset, length) metadata is.
package ingest

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	chunker "github.com/ipfs/boxo/chunker"
	balanced "github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	h "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	ufsio "github.com/ipfs/boxo/ipld/unixfs/io"
	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"

	"github.com/ipai/hf-ipfs/internal/dummy"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/node"
)

var log = logging.Logger("hf-ipfs/ingest")

// Result reports what a single ingest produced.
type Result struct {
	RepoID    string
	Commit    string
	ActualCID cid.Cid
	DummyCID  cid.Cid
	Files     int
	Chunks    int
	TotalSize int64
}

// Run ingests one revision of one repo: build the DAG, record the mapping,
// announce both the dummy (commit-keyed) and actual CIDs on the DHT.
func Run(ctx context.Context, n *node.Node, repoID string, t hfcache.RepoType, commit string, progress func(string)) (*Result, error) {
	paths := hfcache.NewPaths(n.Cfg.HFHubDir, repoID, t)

	files, err := hfcache.ReadSnapshot(paths, commit)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("snapshot %s of %s contains no files", short(commit), repoID)
	}

	// Walking symlinks recovers bytes, sizes and hashes but not which files
	// are LFS-backed nor what their Xet hashes are. The trees/<commit>.json
	// the `hf` CLI left behind knows both, and carrying them into the
	// manifest is what lets this node serve full-fidelity metadata to
	// pullers. No-op when no tree exists.
	hfcache.EnrichFromTree(paths, commit, files)

	root, chunks, err := buildTree(ctx, n, files, progress)
	if err != nil {
		return nil, err
	}

	actual := root.Cid()
	dummyCID, err := dummy.FromCommit(commit)
	if err != nil {
		return nil, err
	}

	var total int64
	for _, f := range files {
		total += f.Size
	}

	entry := &mapping.Entry{
		RepoID:     repoID,
		RepoType:   string(t),
		CommitHash: commit,
		ActualCID:  actual.String(),
		DummyCID:   dummyCID.String(),
		Files:      files,
		TotalSize:  total,
		AddedAt:    time.Now().UTC(),
		Origin:     "ingest",
	}

	if err := n.Mapping.Put(ctx, entry); err != nil {
		return nil, fmt.Errorf("record mapping: %w", err)
	}
	// A DHT with an empty routing table cannot accept the announcement yet.
	// The mapping is already durable; the daemon re-provides on rescan.
	if err := n.ProvideCommit(ctx, commit, actual); err != nil {
		log.Warnf("announce %s@%s: %s", repoID, short(commit), err)
	}

	return &Result{
		RepoID:    repoID,
		Commit:    commit,
		ActualCID: actual,
		DummyCID:  dummyCID,
		Files:     len(files),
		Chunks:    chunks,
		TotalSize: total,
	}, nil
}

// RunRepo ingests whatever `refs/<ref>` currently points at, if not already shared.
func RunRepo(ctx context.Context, n *node.Node, repoID string, t hfcache.RepoType, ref string, progress func(string)) (*Result, bool, error) {
	paths := hfcache.NewPaths(n.Cfg.HFHubDir, repoID, t)
	commit, err := paths.CurrentCommit(ref)
	if err != nil {
		return nil, false, err
	}
	if ok, err := n.Mapping.Has(ctx, commit); err != nil {
		return nil, false, err
	} else if ok {
		return nil, false, nil
	}
	if !hfcache.SnapshotComplete(paths, commit) {
		return nil, false, fmt.Errorf("snapshot %s of %s is not fully downloaded yet", short(commit), repoID)
	}
	res, err := Run(ctx, n, repoID, t, commit, progress)
	return res, true, err
}

// buildTree adds every file as a nocopy DAG and wires them into a directory tree.
func buildTree(ctx context.Context, n *node.Node, files []mapping.FileManifest, progress func(string)) (ipld.Node, int, error) {
	leaves := make(map[string]ipld.Node, len(files))
	byDir := make(map[string][]mapping.FileManifest)
	chunks := 0

	for _, f := range files {
		leaf, nchunks, err := addFileNocopy(ctx, n, f)
		if err != nil {
			return nil, 0, fmt.Errorf("add %s: %w", f.Path, err)
		}
		leaves[f.Path] = leaf
		dir := dirOf(f.Path)
		byDir[dir] = append(byDir[dir], f)
		chunks += nchunks
		if progress != nil {
			progress(fmt.Sprintf("ingested %s (%d chunks, %s)", f.Path, nchunks, humanBytes(f.Size)))
		}
	}

	root, err := buildDir(ctx, n, "", byDir, leaves)
	if err != nil {
		return nil, 0, err
	}
	return root, chunks, nil
}

func buildDir(ctx context.Context, n *node.Node, dir string, byDir map[string][]mapping.FileManifest, leaves map[string]ipld.Node) (ipld.Node, error) {
	d, err := ufsio.NewDirectory(n.DAG, ufsio.WithCidBuilder(n.CidBuilder()))
	if err != nil {
		return nil, err
	}

	subs := make([]string, 0, 4)
	for k := range byDir {
		if k != dir && dirOf(k) == dir {
			subs = append(subs, k)
		}
	}
	sort.Strings(subs)

	for _, sub := range subs {
		subNode, err := buildDir(ctx, n, sub, byDir, leaves)
		if err != nil {
			return nil, err
		}
		if err := d.AddChild(ctx, path.Base(sub), subNode); err != nil {
			return nil, fmt.Errorf("link subdir %s: %w", sub, err)
		}
	}

	for _, f := range byDir[dir] {
		leaf, ok := leaves[f.Path]
		if !ok {
			return nil, fmt.Errorf("missing leaf node for %s", f.Path)
		}
		if err := d.AddChild(ctx, path.Base(f.Path), leaf); err != nil {
			return nil, fmt.Errorf("link %s: %w", f.Path, err)
		}
	}

	root, err := d.GetNode()
	if err != nil {
		return nil, err
	}
	if err := n.DAG.Add(ctx, root); err != nil {
		return nil, err
	}
	return root, nil
}

// addFileNocopy chunks a file and writes its leaves as filestore references.
func addFileNocopy(ctx context.Context, n *node.Node, f mapping.FileManifest) (ipld.Node, int, error) {
	if f.SrcPath == "" {
		return nil, 0, fmt.Errorf("no source path recorded for %s", f.Path)
	}
	fh, err := os.Open(f.SrcPath)
	if err != nil {
		return nil, 0, err
	}
	defer fh.Close()

	fi, err := fh.Stat()
	if err != nil {
		return nil, 0, err
	}

	spl := chunker.NewSizeSplitter(&absFile{f: fh, abs: f.SrcPath, fi: fi}, n.Cfg.ChunkSize)
	dbp := h.DagBuilderParams{
		Dagserv:    n.DAG,
		Maxlinks:   h.DefaultLinksPerBlock,
		RawLeaves:  true,
		CidBuilder: n.CidBuilder(),
		NoCopy:     true,
	}
	db, err := dbp.New(spl)
	if err != nil {
		return nil, 0, err
	}
	root, err := balanced.Layout(db)
	if err != nil {
		return nil, 0, err
	}

	nchunks := 0
	if sz := fi.Size(); sz > 0 {
		nchunks = int((sz + n.Cfg.ChunkSize - 1) / n.Cfg.ChunkSize)
	}
	return root, nchunks, nil
}

// absFile lets the unixfs importer record absolute paths for nocopy adds.
type absFile struct {
	f   *os.File
	abs string
	fi  os.FileInfo
}

func (a *absFile) Read(p []byte) (int, error) { return a.f.Read(p) }
func (a *absFile) Close() error               { return a.f.Close() }
func (a *absFile) Mode() os.FileMode          { return a.fi.Mode() }
func (a *absFile) ModTime() time.Time         { return a.fi.ModTime() }
func (a *absFile) Size() (int64, error)       { return a.fi.Size(), nil }
func (a *absFile) AbsPath() string            { return a.abs }
func (a *absFile) Stat() os.FileInfo          { return a.fi }

func dirOf(rel string) string {
	d := path.Dir(rel)
	if d == "." {
		return ""
	}
	return d
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
