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
	"sync/atomic"
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
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
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

	reach    atomic.Int32
	reachSub event.Subscription
	reachCh  chan network.Reachability
	isolated bool
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

	opts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.ListenAddrStrings(cfg.Listen...),
	}
	if cfg.Relay {
		opts = append(opts, libp2p.EnableRelay())
	} else {
		opts = append(opts, libp2p.DisableRelay())
	}
	// AutoNATv2 is what lets us *report* reachability, so it is on
	// whenever the node expects to meet strangers, independently of
	// whether relays or punching are enabled.
	if !cfg.Isolated {
		opts = append(opts, libp2p.EnableAutoNATv2())
		if cfg.HolePunch {
			opts = append(opts, libp2p.EnableHolePunching())
		}
		if cfg.NATPortMap {
			opts = append(opts, libp2p.NATPortMap())
		}
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		_ = st.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}

	mode := dht.ModeClient
	if cfg.DHTServer {
		mode = dht.ModeServer
	}
	dhtOpts := []dht.Option{dht.Mode(mode)}

	booters, err := resolveBootstrap(cfg)
	if err != nil {
		_ = h.Close()
		_ = st.Close()
		_ = lock.Close()
		return nil, err
	}
	if len(booters) > 0 {
		dhtOpts = append(dhtOpts, dht.BootstrapPeers(booters...))
	}

	// Announce only addresses a remote peer could actually dial. Without this
	// the provider record carries loopback and Docker bridges, and every
	// transparent pull spends its budget on connections that cannot work.
	if !cfg.Isolated {
		dhtOpts = append(dhtOpts, dht.AddressFilter(announceAddrs))
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
		isolated:   cfg.Isolated,
		reachCh:    make(chan network.Reachability, 4),
	}

	// AutoNAT's verdict is the single most useful diagnostic here: the
	// bound address tells us what we asked for, not what peers can
	// actually dial.
	if !cfg.Isolated {
		sub, err := h.EventBus().Subscribe(&event.EvtLocalReachabilityChanged{})
		if err != nil {
			log.Warnf("reachability subscription unavailable: %s", err)
		} else {
			n.reachSub = sub
			go n.trackReachability(sub)
		}
	}

	n.registerHandlers()
	return n, nil
}

// trackReachability mirrors the AutoNAT verdict onto the node so callers can
// read it synchronously.
func (n *Node) trackReachability(sub event.Subscription) {
	wasPublic := false
	for evt := range sub.Out() {
		e, ok := evt.(event.EvtLocalReachabilityChanged)
		if !ok {
			continue
		}
		n.reach.Store(int32(e.Reachability))
		log.Infof("reachability: %s", e.Reachability)
		select {
		case n.reachCh <- e.Reachability:
		default:
		}
		// The first Public verdict is exactly when the provider record should
		// be rewritten: it now carries an address strangers can actually use,
		// and the routing table has had time to fill.
		if e.Reachability == network.ReachabilityPublic && !wasPublic {
			wasPublic = true
			count, err := n.ReprovideAll(n.ctx)
			if err != nil {
				log.Warnf("re-announce after public reachability: %s", err)
			} else if count > 0 {
				log.Infof("re-announced %d revision(s) after public reachability", count)
			}
		}
	}
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

// BoundLoopbackOnly reports whether every address this host listens on is a
// loopback address. A node in that state cannot be dialed by any other peer:
// the DHT hands out 127.0.0.1, which points back at the puller's own
// machine, so every transparent pull fails during dialing while the daemon
// looks perfectly healthy in its own logs.
//
// manet has no loopback predicate — IsPublicAddr and IsPrivateAddr both
// classify loopback as neither — so this goes through net.IP directly.
func (n *Node) BoundLoopbackOnly() bool { return loopbackOnly(n.Host.Addrs()) }

// loopbackOnly is the predicate behind BoundLoopbackOnly, split out so it can
// be tested without standing up a libp2p host.
func loopbackOnly(addrs []ma.Multiaddr) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip, err := manet.ToIP(a)
		if err != nil || !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// RoutingTableSize reports how many peers the DHT currently knows. Zero means
// the node has not joined a swarm and cannot announce or resolve providers.
func (n *Node) RoutingTableSize() int { return n.DHT.RoutingTable().Size() }

// Reachability reports AutoNAT's verdict on whether this node is dialable
// from the public internet. ReachabilityUnknown means the probe has not
// completed — or never will, for an isolated node.
func (n *Node) Reachability() network.Reachability {
	return network.Reachability(n.reach.Load())
}

// ReachabilityChanged yields AutoNAT verdicts as they arrive. AutoNAT's
// first verdict typically lands tens of seconds after startup, so callers
// that report reachability should watch this rather than blocking on it.
func (n *Node) ReachabilityChanged() <-chan network.Reachability { return n.reachCh }

// WaitForReachability blocks until AutoNAT reports a definite verdict or the
// timeout elapses. Isolated nodes return immediately: there is no swarm to
// probe against, so waiting would just burn the timeout.
func (n *Node) WaitForReachability(ctx context.Context, timeout time.Duration) network.Reachability {
	if n.isolated {
		return network.ReachabilityUnknown
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if r := n.Reachability(); r != network.ReachabilityUnknown {
			return r
		}
		select {
		case <-ctx.Done():
			return n.Reachability()
		case <-deadline:
			return n.Reachability()
		case <-tick.C:
		}
	}
}

// relayed reports whether a stream arrived over a circuit v2 relay. libp2p
// marks those connections "limited".
func relayed(s network.Stream) bool { return s.Conn().Stat().Limited }

// announceAddrs keeps only publicly routable addresses for DHT announcements.
// Loopback, RFC1918 (Docker bridges included), link-local and CGNAT ranges are
// dropped: advertising them makes every transparent pull burn its dial budget on
// connections that cannot possibly succeed. A node with no public address at all
// keeps its local ones — it is still reachable inside its own network, and
// announcing nothing would hide it from LAN peers too.
func announceAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	out := make([]ma.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		if manet.IsPublicAddr(a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return addrs
	}
	return out
}

// dialOrder sorts a provider's addresses with publicly routable ones first, so
// the swarm tries the address that can work before loopback. Non-public
// addresses stay as a fallback for pullers sharing the provider's network.
func dialOrder(addrs []ma.Multiaddr) []ma.Multiaddr {
	pub := make([]ma.Multiaddr, 0, len(addrs))
	local := make([]ma.Multiaddr, 0, len(addrs))
	for _, a := range addrs {
		if manet.IsPublicAddr(a) {
			pub = append(pub, a)
		} else {
			local = append(local, a)
		}
	}
	return append(pub, local...)
}

// ReachabilityLine renders AutoNAT's verdict as operator-actionable text.
// The bound address only reports what we asked for; this reports whether the
// internet agrees we are dialable — exactly the gap that made reachability
// hard to diagnose before.
func ReachabilityLine(r network.Reachability, cfg *config.Config) string {
	if cfg.Isolated {
		return "n/a (isolated node: no swarm to probe against)"
	}
	switch r {
	case network.ReachabilityPublic:
		return "public — dialable by strangers"
	case network.ReachabilityPrivate:
		hint := "NAT-PMP/UPnP did not map the port; port it forward on the gateway"
		if !cfg.NATPortMap {
			hint = "port mapping is off; drop --no-nat-portmap or port-forward manually"
		}
		return "private — NOT dialable by strangers; " + hint +
			" (hole punching still works for peers that can reach us at all)"
	default:
		return "probing (AutoNAT)…"
	}
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
		// A record may mix a routable address with loopback and Docker
		// bridges; the swarm tries them in the order given.
		p.Addrs = dialOrder(p.Addrs)
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

	// Answering the map query is what authorises the bulk transfer that
	// follows, so this is the right place to refuse: the puller gets a readable
	// error and moves on to another candidate instead of silently trickling
	// a model through somebody's relay.
	if relayed(s) && !n.Cfg.RelayBulk {
		log.Warnw("refusing map for relayed peer: bulk-over-relay disabled",
			"peer", s.Conn().RemotePeer(), "commit", req.CommitHash)
		resp := wire.MapResponse{
			CommitHash: req.CommitHash,
			Error: "peer will not stream bulk data over a circuit relay; it needs a " +
				"publicly reachable address, or the puller must pass --relay-bulk",
		}
		_ = s.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_ = protoio.WriteJSON(s, &resp)
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

	// Defence in depth: the map gate above should stop relayed pullers
	// before they get here, but never stream gigabytes over a relay that
	// the operator did not opt into.
	if relayed(s) && !n.Cfg.RelayBulk {
		log.Warnw("refusing bulk stream over circuit relay", "peer", s.Conn().RemotePeer())
		_ = s.Reset()
		return
	}

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

// resolveBootstrap decides which Kademlia bootstrap peers the node starts from.
// An explicit --bootstrap list wins; --isolated means none; otherwise the
// canonical libp2p bootstrappers are used so the node joins the public swarm.
func resolveBootstrap(cfg *config.Config) ([]peer.AddrInfo, error) {
	if cfg.Isolated {
		return nil, nil
	}
	if len(cfg.Bootstrap) == 0 {
		return dht.GetDefaultBootstrapPeerAddrInfos(), nil
	}
	addrs, err := parseAddrs(cfg.Bootstrap)
	if err != nil {
		return nil, err
	}
	return peer.AddrInfosFromP2pAddrs(addrs...)
}

// Close releases every resource owned by the node.
func (n *Node) Close() error {
	var errs []error
	if n.reachSub != nil {
		errs = append(errs, n.reachSub.Close())
	}
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
