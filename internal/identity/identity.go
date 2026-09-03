// Package identity persists the libp2p Ed25519 key so a hf-ipfs node keeps a
// stable Peer ID across restarts.
package identity

import (
	"fmt"
	"os"
	"path/filepath"

	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Load reads an existing identity without creating one.
func Load(path string) (ic.PrivKey, peer.ID, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	key, err := ic.UnmarshalPrivateKey(raw)
	if err != nil {
		return nil, "", fmt.Errorf("parse identity %s: %w", path, err)
	}
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return nil, "", fmt.Errorf("derive peer id: %w", err)
	}
	return key, id, nil
}

// LoadOrGenerate reads the private key at path, creating and persisting a fresh
// Ed25519 key pair when none exists yet.
func LoadOrGenerate(path string) (ic.PrivKey, peer.ID, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("read identity %s: %w", path, err)
	}

	if err == nil {
		key, uerr := ic.UnmarshalPrivateKey(raw)
		if uerr != nil {
			return nil, "", fmt.Errorf("parse identity %s: %w", path, uerr)
		}
		id, perr := peer.IDFromPrivateKey(key)
		if perr != nil {
			return nil, "", fmt.Errorf("derive peer id: %w", perr)
		}
		return key, id, nil
	}

	key, _, gerr := ic.GenerateKeyPair(ic.Ed25519, 0)
	if gerr != nil {
		return nil, "", fmt.Errorf("generate ed25519 key: %w", gerr)
	}
	marshalled, merr := ic.MarshalPrivateKey(key)
	if merr != nil {
		return nil, "", fmt.Errorf("marshal key: %w", merr)
	}
	if dirErr := os.MkdirAll(filepath.Dir(path), 0o700); dirErr != nil {
		return nil, "", fmt.Errorf("create identity dir: %w", dirErr)
	}
	// Write atomically so a crash cannot leave a truncated key behind.
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, marshalled, 0o600); werr != nil {
		return nil, "", fmt.Errorf("write identity: %w", werr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		return nil, "", fmt.Errorf("persist identity: %w", rerr)
	}
	id, perr := peer.IDFromPrivateKey(key)
	if perr != nil {
		return nil, "", fmt.Errorf("derive peer id: %w", perr)
	}
	return key, id, nil
}
