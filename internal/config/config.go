// Package config resolves hf-ipfs runtime configuration from flags,
// environment variables and the well-known Hugging Face cache locations.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ProtocolMapID is the custom libp2p protocol under which peers exchange
	// the "HF commit hash -> actual IPFS CID" mapping.
	ProtocolMapID = "/hf-ipfs/map/1.0.0"

	// ProtocolBlockID is the custom libp2p protocol under which peers stream
	// individual IPFS blocks (dag-pb nodes and raw file chunks).
	ProtocolBlockID = "/hf-ipfs/block/1.0.0"

	// DefaultChunkSize is the size of a single content-addressed chunk.
	// 1 MiB keeps the filestore reference count sane for multi-GB blobs
	// while staying far below the 2 MiB block size limit.
	DefaultChunkSize = 1 << 20

	// MaxFrameSize bounds a single length-prefixed frame on any stream.
	MaxFrameSize = 8 << 20

	// DefaultListenPort is the fixed libp2p TCP port the daemon binds by
	// default. A stable port is a precondition for port-forwarding: an
	// ephemeral port cannot be forwarded, so `tcp/0` silently disqualifies
	// a node from being dialable by strangers. 4008 is chosen to avoid
	// colliding with Kubo's 4001.
	DefaultListenPort = 4008
)

// Config holds every tunable of a hf-ipfs node.
type Config struct {
	// RepoDir is hf-ipfs' own state directory: libp2p identity, blockstore,
	// filestore index, mapping database and control socket live here.
	RepoDir string

	// HFHubDir is the upstream `hf` CLI hub cache (~/.cache/huggingface/hub).
	// It is the root of the filestore: every nocopy reference is stored
	// relative to it.
	HFHubDir string

	// Listen are the libp2p listen multiaddrs.
	Listen []string

	// Bootstrap are Kademlia bootstrap peer multiaddrs. When empty and
	// Isolated is false, the canonical libp2p bootstrap list is used.
	Bootstrap []string

	// Isolated runs the node on a private DHT with no bootstrap peers, so
	// two local nodes can be tested without touching the public swarm.
	Isolated bool

	// Connect are peers to dial right after startup.
	Connect []string

	// HolePunch enables DCUtR, letting two private nodes form a direct
	// connection by punching simultaneous holes.
	HolePunch bool

	// Relay enables circuit v2 dialing and listening. It is the only way a
	// fully private node can be reached at all, but it also means traffic
	// may transit a third-party relay.
	Relay bool

	// NATPortMap asks the local gateway for a UPnP/NAT-PMP mapping, which
	// makes a NAT'd node dialable without manual configuration.
	NATPortMap bool

	// RelayService runs this node as a circuit v2 relay *service* so other
	// private peers can relay through it. libp2p only activates it when
	// AutoNAT deems us publicly reachable.
	RelayService bool

	// StaticRelays are circuit v2 relay multiaddrs a private node
	// reserves its own /p2p-circuit address through. Without this a NAT'd
	// node has no relayed address to announce — --relay-bulk alone does
	// not make it dialable.
	StaticRelays []string

	// RelayBulk permits bulk block streaming over circuit-relayed
	// (limited) connections. Off by default: a relay carrying a 40 GiB
	// safetensors shard is somebody else's bandwidth bill. Hole-punched
	// and direct connections are unaffected.
	RelayBulk bool

	// ChunkSize is the fixed chunk size used when ingesting blobs.
	ChunkSize int64

	// DHTServer runs the Kademlia node in server mode (required to answer
	// provider queries for other peers).
	DHTServer bool

	// HFEndpoint is the Hugging Face Hub API base URL.
	HFEndpoint string

	// HFToken authenticates Hub API and file-download requests, which is
	// what unlocks gated and private repos. Resolved from HF_TOKEN, then
	// the legacy HUGGING_FACE_HUB_TOKEN, and overridable per invocation
	// with --hf-token.
	//
	// It is a secret: never log it, never place it in a URL, and never let
	// it reach an error message.
	HFToken string

	// APISocket is the local Unix socket the daemon serves control on.
	APISocket string

	// RescanInterval re-scans the HF cache for finalized downloads that the
	// fsnotify fast path may have missed. Zero disables the periodic rescan.
	RescanInterval time.Duration
}

// Default builds a configuration from the environment, mirroring the
// `hf` CLI's own env handling (HF_HOME / HF_HUB_CACHE / HF_ENDPOINT).
func Default() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}

	repo := firstNonEmpty(os.Getenv("HF_IPFS_REPO"), filepath.Join(home, ".hf-ipfs"))

	hfHome := firstNonEmpty(os.Getenv("HF_HOME"), filepath.Join(home, ".cache", "huggingface"))
	hub := firstNonEmpty(os.Getenv("HF_HUB_CACHE"), filepath.Join(hfHome, "hub"))

	endpoint := strings.TrimRight(firstNonEmpty(os.Getenv("HF_ENDPOINT"), "https://huggingface.co"), "/")

	// HF_TOKEN is current; HUGGING_FACE_HUB_TOKEN is the older name still
	// set by a lot of existing tooling, so it is honoured as a fallback.
	token := firstNonEmpty(os.Getenv("HF_TOKEN"), os.Getenv("HUGGING_FACE_HUB_TOKEN"))

	return &Config{
		RepoDir:    repo,
		HFHubDir:   hub,
		Listen:     []string{fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", DefaultListenPort)},
		ChunkSize:  DefaultChunkSize,
		DHTServer:  true,
		HolePunch:  true,
		Relay:      true,
		NATPortMap: true,
		// Bulk over relay stays off; see the field comment.
		RelayBulk:      false,
		HFEndpoint:     endpoint,
		HFToken:        strings.TrimSpace(token),
		APISocket:      filepath.Join(repo, "api.sock"),
		RescanInterval: 5 * time.Minute,
	}, nil
}

// EnsureDirs creates the state directories the node needs.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.RepoDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// KeyPath is where the persistent libp2p Ed25519 identity lives.
func (c *Config) KeyPath() string { return filepath.Join(c.RepoDir, "key") }

// DBPath is the bbolt file backing blocks, filestore refs and mappings.
func (c *Config) DBPath() string { return filepath.Join(c.RepoDir, "hf-ipfs.db") }

// LockPath is the advisory lock guarding the (single-writer) bbolt file.
func (c *Config) LockPath() string { return filepath.Join(c.RepoDir, "hf-ipfs.lock") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
