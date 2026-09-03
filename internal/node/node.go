// Package node assembles the embedded IPFS/libp2p node: a persistent identity,
// a Kademlia DHT, a filestore-backed blockstore (nocopy), and the custom
// hf-ipfs stream protocols that let peers resolve HF commits to content CIDs
// and stream the blocks.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	bserv "github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	filestore "github.com/ipfs/boxo/filestore"
	posinfo "github.com/ipfs/boxo/filestore/posinfo"
	merkledag "github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	mh "github.com/multiformats/go-multihash"

	"github.com/ipai/hf-ipfs/internal/config"
	"github.com/ipai/hf-ipfs/internal/dummy"
	"github.com/ipai/hf-ipfs/internal/identity"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/protoio"
	"github.com/ipai/hf-ipfs/internal/store"
	"github.com/ipai/hf-ipfs/internal/wire"
)

var log = logging.Logger("hf-ipfs")

// Node is the embedded sharing/pulling node.
type Node struct {
	Cfg     *config.Config
	Host    host.Host
	PeerID  peer.ID
	DHT     *dht.IpfsDHT
	Store   *store.Store
	Lock    *store.Lock
	MainBS  blockstore.Blockstore
	Fstore  *filestore.Filestore
	BServ   bserv.BlockService
	DAG     ipld.DAGService
	Mapping *mapping.DB

	cidBuilder cid.Builder
	ctx        context.Context
}

// New builds a node over the configured repo, taking the exclusive repo lock.
func New(ctx context.Context, cfg *config.Config) (*Node, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}

	lock, err := store.AcquireLock(cfg.LockPath())
	if err != nil {
		return nil, err
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		_ = lock.Close()
		return nil, err
	}

	key, pid, err := identity.LoadOrGenerate(cfg.KeyPath())
	if err != nil {
		_ = st.Close()
		_ = lock.Close()
		return nil, err
	}

	h, err := libp2p.New(
		libp2p.Identity(key),
		libp2p.ListenAddrStrings(cfg.Listen...),
	)
	if err != nil {
		_ = st.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}

	dhtOpts := []dht.Option{dht.Mode(dht.ModeClient)}
	if cfg.DHTServer {
		dhtOpts = append(dhtOpts, dht.Mode(dht.ModeServer))
	}
	if len(cfg.Bootstrap) > 0 {
		addrs, err := parseAddrs(cfg.Bootstrap)
		if err != nil {
			_ = h.Close()
			_ = st.Close()
			_ = lock.Close()
			return nil, err
		}
		infos, err := peer.AddrInfosFromP2pAddrs(addrs...)
		if err != nil {
			_ = h.Close()
			_ = st.Close()
			_ = lock.Close()
			return nil, fmt.Errorf("parse bootstrap peers: %w", err)
		}
		dhtOpts = append(dhtOpts, dht.BootstrapPeers(infos...))
	}

	dhtInstance, err := dht.New(h, dhtOpts...)
	if err != nil {
		_ = h.Close()
		_ = st.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("create kad-dht: %w", err)
	}

	// Zero-copy storage: leaf chunks are recorded as (path, offset) references
	// into the HF blobs directory, never copied into our own datastore.
	mainBS := blockstore.NewBlockstore(st.DS("blocks"))
	fm := filestore.NewFileManager(st.DS("filestore"), cfg.HFHubDir)
	fm.AllowFiles = true
	fstore := filestore.NewFilestore(mainBS, fm, nil)

	bsvc := bserv.New(fstore, nil)

	n := &Node{
		Cfg:        cfg,
		Host:       h,
		PeerID:     pid,
		DHT:        dhtInstance,
		Store:      st,
		Lock:       lock,
		MainBS:     mainBS,
		Fstore:     fstore,
		BServ:      bsvc,
		DAG:        merkledag.NewDAGService(bsvc),
		Mapping:    mapping.New(st.DS("map")),
		ctx:        ctx,
		cidBuilder: cid.V1Builder{Codec: cid.DagProtobuf, MhType: mh.SHA2_256},
	}

	n.registerHandlers()
	return n, nil
}

// registerHandlers advertises the custom hf-ipfs libp2p protocols.
func (n *Node) registerHandlers() {
	n.Host.SetStreamHandler(config.ProtocolMapID, n.handleMap)
	n.Host.SetStreamHandler(config.ProtocolBlockID, n.handleBlocks)
}

// CidBuilder is the CIDv1/dag-pb/sha2-256 builder used for ingested trees.
func (n *Node) CidBuilder() cid.Builder { return n.cidBuilder }

// Addrs returns the node's listen addresses with its peer id appended.
func (n *Node) Addrs() []string {
	out := make([]string, 0, len(n.Host.Addrs()))
	for _, a := range n.Host.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, n.PeerID))
	}
	return out
}

// Dial connects to a peer given a full /p2p multiaddr.
func (n *Node) Dial(ctx context.Context, maddr string) (peer.ID, error) {
	m, err := ma.NewMultiaddr(maddr)
	if err != nil {
		return "", fmt.Errorf("parse peer address %q: %w", maddr, err)
	}
	ai, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return "", fmt.Errorf("parse peer address %q: %w", maddr, err)
	}
	if err := n.Host.Connect(ctx, *ai); err != nil {
		return "", fmt.Errorf("connect %s: %w", maddr, err)
	}
	return ai.ID, nil
}

// DialPeers connects to every configured --connect address, logging failures.
func (n *Node) DialPeers(ctx context.Context) {
	for _, a := range n.Cfg.Connect {
		if _, err := n.Dial(ctx, a); err != nil {
			log.Warnf("dial %s: %s", a, err)
			continue
		}
		log.Infof("connected to %s", a)
	}
}

// ProvideCommit announces this node as a provider for the HF commit's dummy CID
// (the key pullers search) and for the actual content CID.
func (n *Node) ProvideCommit(ctx context.Context, commit string, actual cid.Cid) error {
	dummyCID, err := dummy.FromCommit(commit)
	if err != nil {
		return err
	}
	if err := n.DHT.Provide(ctx, dummyCID, true); err != nil {
		return fmt.Errorf("provide dummy cid %s: %w", dummyCID, err)
	}
	if actual.Defined() && actual != dummyCID {
		if err := n.DHT.Provide(ctx, actual, true); err != nil {
			log.Warnf("provide actual cid %s: %s", actual, err)
		}
	}
	return nil
}

// ReprovideAll re-announces every stored mapping so provider records survive a
// restart of the daemon.
func (n *Node) ReprovideAll(ctx context.Context) (int, error) {
	rtSize := n.DHT.RoutingTable().Size()
	log.Debugf("reprovide: routing table holds %d peer(s)", rtSize)
	if rtSize == 0 {
		return 0, nil
	}
	entries, err := n.Mapping.List(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range entries {
		actual, perr := cid.Decode(e.ActualCID)
		if perr != nil {
			log.Warnf("skipping malformed mapping %s: %s", e.CommitHash, perr)
			continue
		}
		if perr := n.ProvideCommit(ctx, e.CommitHash, actual); perr != nil {
			log.Warnf("reprovide %s: %s", e.CommitHash, perr)
			continue
		}
		count++
	}
	log.Debugf("reprovide: announced %d revision(s)", count)
	return count, nil
}

// WaitForRoutingTable blocks until the DHT routing table holds at least one
// peer or the timeout elapses. libp2p absorbs freshly dialed peers through an
// asynchronous event notifee, so a lookup issued immediately after Connect
// would otherwise see an empty table.
func (n *Node) WaitForRoutingTable(ctx context.Context, timeout time.Duration) int {
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if size := n.DHT.RoutingTable().Size(); size > 0 {
			return size
		}
		select {
		case <-ctx.Done():
			return 0
		case <-deadline:
			return n.DHT.RoutingTable().Size()
		case <-tick.C:
		}
	}
}

// FindProvidersForCommit resolves the dummy CID for a commit and returns the
// peers the DHT advertises as hosts, excluding ourselves.
func (n *Node) FindProvidersForCommit(ctx context.Context, commit string, limit int) ([]peer.AddrInfo, error) {
	dummyCID, err := dummy.FromCommit(commit)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 16
	}
	log.Debugf("find providers: key %s, routing table %d peer(s)", dummyCID, n.DHT.RoutingTable().Size())
	out := make([]peer.AddrInfo, 0, limit)
	for p := range n.DHT.FindProvidersAsync(ctx, dummyCID, limit) {
		if p.ID == n.PeerID {
			continue
		}
		log.Debugf("find providers: %s", p)
		out = append(out, p)
	}
	return out, nil
}

// RegisterFilestoreRef records a nocopy reference: "the bytes of block blk
// live at absPath[offset:offset+len(blk)]". The block must carry its real
// bytes because the filestore derives the reference size from them.
func (n *Node) RegisterFilestoreRef(ctx context.Context, blk blocks.Block, absPath string, offset uint64) error {
	if blk.Cid().Type() != cid.Raw {
		return fmt.Errorf("filestore references require raw blocks, got codec %#x for %s", blk.Cid().Type(), blk.Cid())
	}
	nd, err := merkledag.DecodeRawBlock(blk)
	if err != nil {
		return fmt.Errorf("wrap chunk %s as ipld node: %w", blk.Cid(), err)
	}
	fn := &posinfo.FilestoreNode{
		Node:    nd,
		PosInfo: &posinfo.PosInfo{Offset: offset, FullPath: absPath},
	}
	return n.Fstore.Put(ctx, fn)
}

// handleMap serves /hf-ipfs/map/1.0.0: commit hash in, actual CID + manifest out.
func (n *Node) handleMap(s network.Stream) {
	defer s.Close()

	var req wire.MapRequest
	if err := protoio.ReadJSON(s, &req); err != nil {
		log.Warnf("map: bad request: %s", err)
		return
	}

	resp := wire.MapResponse{CommitHash: req.CommitHash}
	entry, ok, err := n.Mapping.Get(n.ctx, req.CommitHash)
	switch {
	case err != nil:
		resp.Error = fmt.Sprintf("lookup failed: %v", err)
	case !ok:
		resp.Error = "commit not shared by this peer"
	default:
		resp.OK = true
		resp.RepoID = entry.RepoID
		resp.RepoType = entry.RepoType
		resp.ActualCID = entry.ActualCID
		resp.DummyCID = entry.DummyCID
		resp.TotalSize = entry.TotalSize
		resp.Files = entry.Files
	}

	_ = s.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := protoio.WriteJSON(s, &resp); err != nil {
		log.Warnf("map: write response: %s", err)
	}
}

// handleBlocks serves /hf-ipfs/block/1.0.0.
//
// Each request frame is a JSON batch, {"cids": ["bafy…", …]}. The server
// answers with exactly one response frame per requested CID, in the same
// order: 0x00 + block bytes, or 0x01 when the block is absent. Because the
// stream keeps order, a client can pipeline batches without request ids.
func (n *Node) handleBlocks(s network.Stream) {
	defer s.Close()

	for {
		var req struct {
			CIDs []string `json:"cids"`
		}
		if err := protoio.ReadJSON(s, &req); err != nil {
			if isStreamEnd(err) {
				return
			}
			log.Debugw("block stream request", "err", err)
			return
		}

		for _, raw := range req.CIDs {
			c, err := cid.Decode(raw)
			if err != nil {
				if werr := protoio.WriteFrame(s, []byte{wire.BlockNotFound}); werr != nil {
					return
				}
				continue
			}
			blk, err := n.Fstore.Get(n.ctx, c)
			if err != nil || blk == nil {
				if werr := protoio.WriteFrame(s, []byte{wire.BlockNotFound}); werr != nil {
					return
				}
				continue
			}
			out := make([]byte, 0, len(blk.RawData())+1)
			out = append(out, wire.BlockOK)
			out = append(out, blk.RawData()...)
			if err := protoio.WriteFrame(s, out); err != nil {
				log.Debugw("block stream write", "err", err)
				return
			}
		}
	}
}

func isStreamEnd(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) ||
		errors.Is(err, network.ErrReset) {
		return true
	}
	if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
		return true
	}
	return false
}

func parseAddrs(strs []string) ([]ma.Multiaddr, error) {
	out := make([]ma.Multiaddr, 0, len(strs))
	for _, s := range strs {
		m, err := ma.NewMultiaddr(s)
		if err != nil {
			return nil, fmt.Errorf("parse multiaddr %q: %w", s, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Close releases every resource owned by the node.
func (n *Node) Close() error {
	var errs []error
	if n.DHT != nil {
		errs = append(errs, n.DHT.Close())
	}
	if n.Host != nil {
		errs = append(errs, n.Host.Close())
	}
	if n.Store != nil {
		errs = append(errs, n.Store.Close())
	}
	if n.Lock != nil {
		errs = append(errs, n.Lock.Close())
	}
	return errors.Join(errs...)
}
