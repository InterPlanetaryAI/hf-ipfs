// Package mapping is the local embedded key-value store holding the
// "HF commit hash -> actual IPFS CID" records that this node shares.
package mapping

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
)

// FileManifest describes one file of a HF snapshot revision in a form a
// receiving peer can reconstruct without guessing.
//
// BlobName is the exact filename the sharing peer found in
// `hub/<repo>/blobs/`, which keeps reconstruction compatible with the
// upstream `hf` CLI regardless of which hash function HF used to name it.
type FileManifest struct {
	Path     string `json:"path"`
	BlobName string `json:"blob_name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Mode     uint32 `json:"mode,omitempty"`
	Mtime    int64  `json:"mtime,omitempty"`

	// LFS marks a Git-LFS (or Xet) backed file. Only those files carry
	// lfs_sha256/lfs_size in trees/<commit>.json; a plain git blob is
	// recorded with just size and blob_id. We SHA-256 every file we touch,
	// so the content hash alone cannot make this determination.
	LFS bool `json:"lfs,omitempty"`

	// XetHash is the Xet content hash for Xet-stored files, empty otherwise.
	// Carried in the manifest so a seeder can hand it to pullers, which
	// would otherwise have to hit the Hub API for it.
	XetHash string `json:"xet_hash,omitempty"`

	// SrcPath is the absolute path of the bytes on the sharing host. It is
	// used during ingest and deliberately not transmitted.
	SrcPath string `json:"-"`
}

// Entry is one shared HF revision.
type Entry struct {
	RepoID     string         `json:"repo_id"`
	RepoType   string         `json:"repo_type"`
	CommitHash string         `json:"commit_hash"`
	ActualCID  string         `json:"actual_cid"`
	DummyCID   string         `json:"dummy_cid"`
	Files      []FileManifest `json:"files"`
	TotalSize  int64          `json:"total_size"`
	AddedAt    time.Time      `json:"added_at"`
	Origin     string         `json:"origin"`
}

// DB stores mapping entries in a bbolt-backed datastore.
type DB struct {
	ds ds.Batching
}

// New wraps a datastore bucket view.
func New(store ds.Batching) *DB { return &DB{ds: store} }

func mapKey(commit string) ds.Key { return ds.NewKey("/" + commit) }

// Put stores (or replaces) an entry keyed by commit hash.
func (db *DB) Put(ctx context.Context, e *Entry) error {
	if e.CommitHash == "" {
		return fmt.Errorf("mapping entry requires a commit hash")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal mapping entry: %w", err)
	}
	return db.ds.Put(ctx, mapKey(e.CommitHash), data)
}

// Get fetches an entry by commit hash.
func (db *DB) Get(ctx context.Context, commit string) (*Entry, bool, error) {
	data, err := db.ds.Get(ctx, mapKey(commit))
	if err == ds.ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	e := &Entry{}
	if err := json.Unmarshal(data, e); err != nil {
		return nil, false, fmt.Errorf("unmarshal mapping entry for %s: %w", commit, err)
	}
	return e, true, nil
}

// Has reports whether the commit is shared by this node.
func (db *DB) Has(ctx context.Context, commit string) (bool, error) {
	return db.ds.Has(ctx, mapKey(commit))
}

// Delete removes an entry.
func (db *DB) Delete(ctx context.Context, commit string) error {
	return db.ds.Delete(ctx, mapKey(commit))
}

// List returns every entry sorted by (repo, commit).
func (db *DB) List(ctx context.Context) ([]*Entry, error) {
	results, err := db.ds.Query(ctx, dsq.Query{})
	if err != nil {
		return nil, err
	}
	defer results.Close()

	out := make([]*Entry, 0, 16)
	for res := range results.Next() {
		if res.Error != nil {
			return nil, res.Error
		}
		if len(res.Value) == 0 {
			continue
		}
		e := &Entry{}
		if err := json.Unmarshal(res.Value, e); err != nil {
			return nil, fmt.Errorf("unmarshal mapping entry: %w", err)
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepoID != out[j].RepoID {
			return out[i].RepoID < out[j].RepoID
		}
		return out[i].CommitHash < out[j].CommitHash
	})
	return out, nil
}
