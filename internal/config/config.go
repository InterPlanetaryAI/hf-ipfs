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

	// Bootstrap are Kademlia bootstrap peer multiaddrs. Empty means the node
	// runs on an isolated DHT (useful for local two-node testing).
	Bootstrap []string

	// Connect are peers to dial right after startup.
	Connect []string

	// ChunkSize is the fixed chunk size used when ingesting blobs.
	ChunkSize int64

	// DHTServer runs the Kademlia node in server mode (required to answer
	// provider queries for other peers).
	DHTServer bool

	// HFEndpoint is the Hugging Face Hub API base URL.
	HFEndpoint string

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

	return &Config{
		RepoDir:        repo,
		HFHubDir:       hub,
		Listen:         []string{"/ip4/0.0.0.0/tcp/0"},
		ChunkSize:      DefaultChunkSize,
		DHTServer:      true,
		HFEndpoint:     endpoint,
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
