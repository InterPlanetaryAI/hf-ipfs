// Package pull implements the pull pipeline:
//
//	A. resolve <repo_id> -> commit hash via the HF Hub API
//	B. commit hash -> dummy multihash -> DHT FindProviders
//	C. open /hf-ipfs/map/1.0.0 to a provider, get the actual CID + manifest
//	D. stream the DAG, hashing on the fly, writing flat blobs and registering
//	   each chunk as a filestore (nocopy) reference so we can re-serve it
//	E. recreate the snapshots/<commit>/ symlink tree and update refs/main
package pull

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	bserv "github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	merkledag "github.com/ipfs/boxo/ipld/merkledag"
	ufsio "github.com/ipfs/boxo/ipld/unixfs/io"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/ipai/hf-ipfs/internal/config"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/node"
	"github.com/ipai/hf-ipfs/internal/protoio"
	"github.com/ipai/hf-ipfs/internal/wire"
)

var log = logging.Logger("hf-ipfs/pull")

// BatchSize is how many chunks are requested per round trip on the block stream.
const BatchSize = 64

// Options controls a single pull.
type Options struct {
	RepoID   string
	RepoType hfcache.RepoType
	Commit   string
	Ref      string

	// Peers are dialed and used as map-protocol candidates directly.
	Peers []string

	// Connect are peers dialed only to join the DHT, so Step B can resolve
	// providers by content lookup instead of being told who to ask.
	Connect []string

	Force bool

	// Sources selects where content may come from. The zero value means
	// "use the default": swarm first, Hugging Face as fallback.
	Sources Sources

	// Token authenticates Hub API and fallback download requests. Empty
	// means "use whatever this node was configured with".
	Token string
}

type EventFunc func(wire.ControlEvent) error

// Run executes the full pull pipeline.
func Run(ctx context.Context, n *node.Node, opts Options, ev EventFunc) error {
	if ev == nil {
		ev = func(wire.ControlEvent) error { return nil }
	}
	if strings.TrimSpace(opts.RepoID) == "" {
		return errors.New("pull requires a repo id")
	}
	ref := opts.Ref
	if ref == "" {
		ref = "main"
	}
	paths := hfcache.NewPaths(n.Cfg.HFHubDir, opts.RepoID, opts.RepoType)

	// ---- Step A: resolve the commit hash --------------------------------
	commit := opts.Commit
	if commit == "" {
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("step A: resolving %s against %s", opts.RepoID, n.Cfg.HFEndpoint)})
		client := hfClient(n, opts.Token)
		got, err := client.LatestCommit(ctx, opts.RepoID, opts.RepoType)
		if err != nil {
			return fmt.Errorf("step A (resolve hash): %w", err)
		}
		commit = got
	}
	ev(wire.ControlEvent{Type: "progress", Message: fmt.Sprintf("step A: %s @ %s", opts.RepoID, commit)})

	// Already shared and linked locally? Nothing to do.
	if !opts.Force {
		if entry, ok, err := n.Mapping.Get(ctx, commit); err == nil && ok {
			if hfcache.SnapshotComplete(paths, commit) {
				ev(wire.ControlEvent{Type: "done",
					Message: fmt.Sprintf("already present: %s @ %s", opts.RepoID, short(commit))})
				return nil
			}
			// Mapping exists but the symlink tree is missing: rebuild it.
			if err := hfcache.WriteSnapshot(paths, commit, entry.Files, ref); err != nil {
				return fmt.Errorf("step E (relink): %w", err)
			}
			ev(wire.ControlEvent{Type: "done", Message: "restored snapshot links from local mapping"})
			return nil
		}
	}

	src := opts.Sources.resolve()

	var p2pErr error
	if src.P2P {
		if err := pullFromPeers(ctx, n, paths, commit, ref, opts, ev); err != nil {
			p2pErr = err
		} else {
			return nil
		}
	}
	if !src.HF {
		return p2pErr
	}
	if src.P2P {
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("p2p could not deliver (%v); falling back to Hugging Face", p2pErr)})
	}
	return pullFromHF(ctx, n, paths, commit, ref, opts, ev)
}

// pullFromPeers runs steps B through E against the swarm: locate providers, ask
// each which CID backs the commit, stream the DAG, and link the result into the
// HF cache. It returns the last failure when no provider could serve the repo.
func pullFromPeers(
	ctx context.Context,
	n *node.Node,
	paths hfcache.Paths,
	commit, ref string,
	opts Options,
	ev EventFunc,
) error {
	// ---- Step B: find providers on the DHT ------------------------------
	providers := make([]peer.AddrInfo, 0, 8)
	for _, maddr := range opts.Peers {
		pid, err := n.Dial(ctx, maddr)
		if err != nil {
			ev(wire.ControlEvent{Type: "progress", Message: "warn: " + err.Error()})
			continue
		}
		providers = append(providers, peer.AddrInfo{ID: pid})
	}
	// Dial DHT-only peers first so the routing table has someone to ask.
	for _, maddr := range opts.Connect {
		if _, err := n.Dial(ctx, maddr); err != nil {
			ev(wire.ControlEvent{Type: "progress",
				Message: "warn: connect " + maddr + ": " + err.Error()})
			continue
		}
		ev(wire.ControlEvent{Type: "progress", Message: "joined the DHT via " + maddr})
	}
	// libp2p hands freshly dialed peers to the DHT asynchronously, and a
	// cold-start bootstrap takes a few seconds. Without this the lookup races
	// an empty routing table and reports "no peers found".
	wait := 3 * time.Second
	if len(opts.Peers) == 0 && len(opts.Connect) == 0 {
		wait = 15 * time.Second
	}
	if size := n.WaitForRoutingTable(ctx, wait); size == 0 {
		ev(wire.ControlEvent{Type: "progress",
			Message: "warn: DHT routing table still empty; provider lookup may come up empty"})
	}
	dhtProviders, err := n.FindProvidersForCommit(ctx, commit, 16)
	if err != nil {
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("warn: DHT lookup failed: %v", err)})
	}
	providers = append(providers, dhtProviders...)
	if len(providers) == 0 {
		return fmt.Errorf("step B: no peers found hosting %s (commit %s)", opts.RepoID, short(commit))
	}
	ev(wire.ControlEvent{Type: "progress",
		Message: fmt.Sprintf("step B: %d candidate provider(s)", len(providers))})

	// ---- Step C/D/E: try providers until one succeeds -------------------
	var lastErr error
	for _, p := range providers {
		actual, resp, err := mapExchange(ctx, n, p, opts.RepoID, commit)
		if err != nil {
			lastErr = err
			ev(wire.ControlEvent{Type: "progress",
				Message: fmt.Sprintf("step C: %s unusable: %v", shortPeer(p.ID), err)})
			continue
		}
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("step C: %s -> %s (%d files, %s)",
				shortPeer(p.ID), actual, len(resp.Files), humanBytes(resp.TotalSize))})

		state := &xferState{total: resp.TotalSize}
		if err := reconstruct(ctx, n, paths, p, resp, state, ev); err != nil {
			lastErr = err
			ev(wire.ControlEvent{Type: "progress",
				Message: fmt.Sprintf("step D: %s failed: %v", shortPeer(p.ID), err)})
			continue
		}

		entry := &mapping.Entry{
			RepoID:     opts.RepoID,
			RepoType:   string(opts.RepoType),
			CommitHash: commit,
			ActualCID:  resp.ActualCID,
			DummyCID:   resp.DummyCID,
			Files:      resp.Files,
			TotalSize:  resp.TotalSize,
			AddedAt:    time.Now().UTC(),
			Origin:     "pull",
		}
		if err := n.Mapping.Put(ctx, entry); err != nil {
			return fmt.Errorf("record pulled mapping: %w", err)
		}
		if err := n.ProvideCommit(ctx, commit, actual); err != nil {
			ev(wire.ControlEvent{Type: "progress",
				Message: "warn: could not re-announce pulled commit: " + err.Error()})
		}

		if err := hfcache.WriteSnapshot(paths, commit, resp.Files, ref); err != nil {
			return fmt.Errorf("step E (symlinking): %w", err)
		}
		ev(wire.ControlEvent{Type: "done",
			Message: fmt.Sprintf("pulled %s @ %s (%s) into %s",
				opts.RepoID, short(commit), humanBytes(state.done), paths.RepoDir)})
		return nil
	}
	return fmt.Errorf("pull failed for %s: %w", opts.RepoID, lastErr)
}

// mapExchange performs step C: ask a peer which actual CID backs this commit.
func mapExchange(ctx context.Context, n *node.Node, p peer.AddrInfo, repoID, commit string) (cid.Cid, *wire.MapResponse, error) {
	if err := n.Host.Connect(ctx, p); err != nil {
		return cid.Undef, nil, fmt.Errorf("connect: %w", err)
	}
	s, err := n.Host.NewStream(ctx, p.ID, config.ProtocolMapID)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("open map stream: %w", err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(60 * time.Second))

	if err := protoio.WriteJSON(s, wire.MapRequest{CommitHash: commit, RepoID: repoID}); err != nil {
		return cid.Undef, nil, fmt.Errorf("send map request: %w", err)
	}
	var resp wire.MapResponse
	if err := protoio.ReadJSON(s, &resp); err != nil {
		return cid.Undef, nil, fmt.Errorf("read map response: %w", err)
	}
	if !resp.OK {
		return cid.Undef, nil, errors.New(resp.Error)
	}
	actual, err := cid.Decode(resp.ActualCID)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("peer sent unusable actual cid %q: %w", resp.ActualCID, err)
	}
	return actual, &resp, nil
}

type xferState struct {
	total int64
	done  int64
}

// reconstruct streams the DAG and materialises every manifest file.
func reconstruct(
	ctx context.Context,
	n *node.Node,
	paths hfcache.Paths,
	p peer.AddrInfo,
	resp *wire.MapResponse,
	state *xferState,
	ev EventFunc,
) error {
	actual, err := cid.Decode(resp.ActualCID)
	if err != nil {
		return err
	}

	bc, err := newBlockClient(ctx, n.Host, p.ID)
	if err != nil {
		return err
	}
	defer bc.Close()

	rt := &readThrough{
		local: n.Fstore,
		fetch: func(ctx context.Context, c cid.Cid) ([]byte, error) {
			out, err := bc.Fetch(ctx, []cid.Cid{c})
			if err != nil {
				return nil, err
			}
			return out[0], nil
		},
	}
	ds := merkledag.NewDAGService(bserv.New(rt, nil))

	root, err := ds.Get(ctx, actual)
	if err != nil {
		return fmt.Errorf("fetch root %s: %w", actual, err)
	}
	log.Debugf("resolved root %s: %d bytes, %d links", actual, len(root.RawData()), len(root.Links()))

	for _, f := range resp.Files {
		fileCid, err := findNode(ctx, ds, actual, f.Path)
		if err != nil {
			return fmt.Errorf("locate %s in %s: %w", f.Path, actual, err)
		}
		if err := reconstructFile(ctx, n, ds, paths, f, fileCid, bc, state, ev); err != nil {
			return err
		}
	}
	return nil
}

// reconstructFile downloads one file's chunks, verifies SHA-256, writes the flat
// blob into the HF cache and registers every chunk as a nocopy reference.
func reconstructFile(
	ctx context.Context,
	n *node.Node,
	ds ipld.DAGService,
	paths hfcache.Paths,
	f mapping.FileManifest,
	fileCid cid.Cid,
	bc *blockClient,
	state *xferState,
	ev EventFunc,
) error {
	blobPath := paths.BlobPath(f.BlobName)
	perm := os.FileMode(f.Mode)
	if perm == 0 {
		perm = 0o644
	}

	// Blob already present with the expected size: nothing to transfer.
	if st, err := os.Stat(blobPath); err == nil && st.Size() == f.Size {
		state.done += f.Size
		ev(wire.ControlEvent{Type: "progress", Message: "cached " + f.Path,
			Bytes: state.done, Total: state.total})
		return nil
	}

	if err := os.MkdirAll(paths.Blobs, 0o755); err != nil {
		return err
	}

	// An empty file is a single zero-length raw leaf. Materialise the blob and
	// keep that leaf in the blockstore (a zero-byte filestore reference would
	// be meaningless) so this node can serve the tree too.
	if f.Size == 0 {
		tf, err := os.OpenFile(blobPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
		if err != nil {
			return fmt.Errorf("create empty blob for %s: %w", f.Path, err)
		}
		if err := tf.Close(); err != nil {
			return fmt.Errorf("close empty blob for %s: %w", f.Path, err)
		}

		datas, err := bc.Fetch(ctx, []cid.Cid{fileCid})
		if err != nil {
			return fmt.Errorf("fetch empty leaf for %s: %w", f.Path, err)
		}
		if len(datas) != 1 || datas[0] == nil {
			return fmt.Errorf("peer is missing the empty leaf %s for %s", fileCid, f.Path)
		}
		blk, err := blocks.NewBlockWithCid(datas[0], fileCid)
		if err != nil {
			return fmt.Errorf("empty leaf %s failed integrity check: %w", fileCid, err)
		}
		if err := n.MainBS.Put(ctx, blk); err != nil {
			return fmt.Errorf("store empty leaf for %s: %w", f.Path, err)
		}
		state.done += f.Size
		ev(wire.ControlEvent{Type: "progress",
			Message: f.Path + ": empty leaf stored",
			Bytes:   state.done,
			Total:   state.total})
		return nil
	}

	leaves := make([]cid.Cid, 0, 16)
	if err := collectLeaves(ctx, ds, fileCid, &leaves); err != nil {
		return fmt.Errorf("enumerate chunks of %s: %w", f.Path, err)
	}
	if len(leaves) == 0 {
		return fmt.Errorf("no chunks found for %s", f.Path)
	}

	tmp := blobPath + ".part-" + randSuffix()
	tf, err := os.Create(tmp)
	if err != nil {
		return err
	}

	hasher := sha256.New()
	var written int64
	registered := make([]cid.Cid, 0, len(leaves))

	rollback := func(cause error) error {
		_ = tf.Close()
		_ = os.Remove(tmp)
		for _, c := range registered {
			_ = n.Fstore.DeleteBlock(ctx, c)
		}
		return fmt.Errorf("%s: %w", f.Path, cause)
	}

	for start := 0; start < len(leaves); start += BatchSize {
		end := start + BatchSize
		if end > len(leaves) {
			end = len(leaves)
		}
		batch := leaves[start:end]
		datas, err := bc.Fetch(ctx, batch)
		if err != nil {
			return rollback(err)
		}
		for i, c := range batch {
			data := datas[i]
			if data == nil {
				return rollback(fmt.Errorf("peer is missing chunk %s", c))
			}
			blk, err := blocks.NewBlockWithCid(data, c)
			if err != nil {
				return rollback(fmt.Errorf("chunk %s failed integrity check: %w", c, err))
			}
			if _, err := tf.Write(data); err != nil {
				return rollback(err)
			}
			hasher.Write(data)
			// Register the nocopy reference against the final blob path. The
			// rename below makes it live; on failure rollback unregisters.
			if err := n.RegisterFilestoreRef(ctx, blk, blobPath, uint64(written)); err != nil {
				return rollback(fmt.Errorf("register filestore ref for %s: %w", c, err))
			}
			registered = append(registered, c)
			written += int64(len(data))
			state.done += int64(len(data))
		}
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("%s: %d/%d chunks", f.Path, len(registered), len(leaves)),
			Bytes:   state.done,
			Total:   state.total})
	}

	if err := tf.Close(); err != nil {
		return rollback(err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	if f.SHA256 != "" && digest != f.SHA256 {
		return rollback(fmt.Errorf("sha256 mismatch: got %s want %s", digest, f.SHA256))
	}
	if written != f.Size {
		return rollback(fmt.Errorf("size mismatch: got %d want %d", written, f.Size))
	}

	if err := os.Rename(tmp, blobPath); err != nil {
		return rollback(err)
	}
	if err := os.Chmod(blobPath, perm); err != nil {
		return rollback(err)
	}
	return nil
}

// findNode walks the directory tree to the node backing a manifest path.
func findNode(ctx context.Context, ds ipld.DAGService, root cid.Cid, rel string) (cid.Cid, error) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	cur := root
	for _, part := range parts {
		nd, err := ds.Get(ctx, cur)
		if err != nil {
			return cid.Undef, err
		}
		dir, err := ufsio.NewDirectoryFromNode(ds, nd)
		if err != nil {
			return cid.Undef, fmt.Errorf("parent of %s is not a directory", part)
		}
		child, err := dir.Find(ctx, part)
		if err != nil {
			return cid.Undef, fmt.Errorf("find %s: %w", part, err)
		}
		cur = child.Cid()
	}
	return cur, nil
}

func collectLeaves(ctx context.Context, ds ipld.DAGService, c cid.Cid, out *[]cid.Cid) error {
	if c.Type() == cid.Raw {
		*out = append(*out, c)
		return nil
	}
	nd, err := ds.Get(ctx, c)
	if err != nil {
		return err
	}
	for _, l := range nd.Links() {
		if err := collectLeaves(ctx, ds, l.Cid, out); err != nil {
			return err
		}
	}
	return nil
}

// blockClient is a persistent pipelined /hf-ipfs/block/1.0.0 client.
type blockClient struct {
	h host.Host
	s network.Stream
}

func newBlockClient(ctx context.Context, h host.Host, p peer.ID) (*blockClient, error) {
	s, err := h.NewStream(ctx, p, config.ProtocolBlockID)
	if err != nil {
		return nil, fmt.Errorf("open block stream to %s: %w", shortPeer(p), err)
	}
	return &blockClient{h: h, s: s}, nil
}

// Fetch requests a batch of blocks; the response frames arrive in request order.
func (b *blockClient) Fetch(ctx context.Context, cs []cid.Cid) ([][]byte, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = b.s.SetDeadline(deadline)
	} else {
		_ = b.s.SetDeadline(time.Now().Add(5 * time.Minute))
	}

	strs := make([]string, len(cs))
	for i, c := range cs {
		strs[i] = c.String()
	}
	if err := protoio.WriteJSON(b.s, struct {
		CIDs []string `json:"cids"`
	}{CIDs: strs}); err != nil {
		return nil, fmt.Errorf("send block request: %w", err)
	}

	out := make([][]byte, len(cs))
	for i := range cs {
		frame, err := protoio.ReadFrame(b.s)
		if err != nil {
			return nil, fmt.Errorf("read block response: %w", err)
		}
		if len(frame) == 0 {
			return nil, errors.New("peer sent an empty status frame")
		}
		if frame[0] != wire.BlockNotFound {
			out[i] = frame[1:]
		}
	}
	return out, nil
}

// Close tears down the block stream.
func (b *blockClient) Close() error {
	if b == nil || b.s == nil {
		return nil
	}
	return b.s.Close()
}

// readThrough is a blockstore that falls back to a remote peer for blocks this
// node does not have. Non-leaf (dag-pb) blocks are persisted locally so the tree
// can be served afterwards; raw leaves are handled by the caller, which writes
// them into HF blobs and registers filestore references instead.
type readThrough struct {
	local blockstore.Blockstore
	fetch func(context.Context, cid.Cid) ([]byte, error)
}

func (r *readThrough) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	if blk, err := r.local.Get(ctx, c); err == nil {
		return blk, nil
	}
	data, err := r.fetch(ctx, c)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, ipld.ErrNotFound{Cid: c}
	}
	blk, err := blocks.NewBlockWithCid(data, c)
	if err != nil {
		return nil, fmt.Errorf("block %s failed integrity check: %w", c, err)
	}
	if c.Type() != cid.Raw {
		if perr := r.local.Put(ctx, blk); perr != nil {
			log.Warnf("persist fetched node %s: %s", c, perr)
		}
	}
	return blk, nil
}

func (r *readThrough) Has(ctx context.Context, c cid.Cid) (bool, error) { return r.local.Has(ctx, c) }

func (r *readThrough) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	if size, err := r.local.GetSize(ctx, c); err == nil {
		return size, nil
	}
	blk, err := r.Get(ctx, c)
	if err != nil {
		return 0, err
	}
	return len(blk.RawData()), nil
}

func (r *readThrough) Put(ctx context.Context, b blocks.Block) error { return r.local.Put(ctx, b) }

func (r *readThrough) PutMany(ctx context.Context, bs []blocks.Block) error {
	return r.local.PutMany(ctx, bs)
}

func (r *readThrough) DeleteBlock(ctx context.Context, c cid.Cid) error {
	return r.local.DeleteBlock(ctx, c)
}

func (r *readThrough) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	return r.local.AllKeysChan(ctx)
}

func randSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func shortPeer(p peer.ID) string {
	s := p.String()
	if len(s) > 10 {
		return s[:10]
	}
	return s
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
