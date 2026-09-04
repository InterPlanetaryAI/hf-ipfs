package hfcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipai/hf-ipfs/internal/mapping"
)

// TreeFormatVersion is the schema version the `hf` CLI stamps into
// `trees/<commit>.json`. Bumping it without a matching change in the CLI's
// reader would make our cache unreadable to it.
const TreeFormatVersion = 1

// TreeFile is one entry of `trees/<commit>.json`.
//
// Field order is load-bearing: it fixes the emitted JSON key order, which has
// to match the `hf` CLI byte-for-byte for the two caches to be
// interchangeable. Do not reorder these fields or switch the omitempty set
// without re-checking a real file.
type TreeFile struct {
	Size      int64  `json:"size"`
	BlobID    string `json:"blob_id"`
	LFSSHA256 string `json:"lfs_sha256,omitempty"`
	LFSSize   int64  `json:"lfs_size,omitempty"`
	XetHash   string `json:"xet_hash,omitempty"`
}

// Tree is the whole document: per-file metadata for one revision.
//
// The LFS and Xet fields appear only for files stored in those backends, so
// their absence on a plain git blob is meaningful, not missing data. That is
// why the writer keys off FileManifest.LFS rather than off the presence of a
// content hash: we SHA-256 every file we touch, and using that alone would
// stamp lfs_sha256 onto files the `hf` CLI records with just size+blob_id.
type Tree struct {
	FormatVersion int                 `json:"format_version"`
	Files         map[string]TreeFile `json:"files"`
}

// TreePath is `trees/<commit>.json`.
func (p Paths) TreePath(commit string) string {
	return filepath.Join(p.Trees, commit+".json")
}

// WriteTree writes `trees/<commit>.json` from a manifest in the exact shape
// the `hf` CLI produces: 1-space indent, no trailing newline, mode 0600.
//
// It merges over any existing record instead of replacing it. A manifest that
// never carried Xet metadata — one rebuilt from a bare symlink tree, or one
// ingested before we tracked it — must not silently strip the richer record
// the `hf` CLI wrote.
func WriteTree(p Paths, commit string, files []mapping.FileManifest) error {
	if err := os.MkdirAll(p.Trees, 0o755); err != nil {
		return fmt.Errorf("create trees dir: %w", err)
	}

	doc := Tree{FormatVersion: TreeFormatVersion, Files: make(map[string]TreeFile, len(files))}
	if existing, ok, err := ReadTree(p, commit); err == nil && ok && existing != nil {
		for path, tf := range existing.Files {
			doc.Files[path] = tf
		}
	}

	for _, f := range files {
		nt := TreeFile{Size: f.Size, BlobID: f.BlobName}
		if prev, ok := doc.Files[f.Path]; ok {
			nt.LFSSHA256 = prev.LFSSHA256
			nt.LFSSize = prev.LFSSize
			nt.XetHash = prev.XetHash
		}
		if f.LFS {
			nt.LFSSHA256 = f.SHA256
			nt.LFSSize = f.Size
		}
		if f.XetHash != "" {
			nt.XetHash = f.XetHash
		}
		doc.Files[f.Path] = nt
	}

	// MarshalIndent with a single space and no trailing newline is what the
	// `hf` CLI emits; map keys come out sorted, which matches too.
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return fmt.Errorf("encode tree for %s: %w", commit, err)
	}

	target := p.TreePath(commit)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, target, err)
	}
	return nil
}

// ReadTree loads `trees/<commit>.json`. The bool reports absence, which is
// normal for caches written before the `hf` CLI added trees.
func ReadTree(p Paths, commit string) (*Tree, bool, error) {
	target := p.TreePath(commit)
	data, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", target, err)
	}
	var t Tree
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", target, err)
	}
	return &t, true, nil
}

// EnrichFromTree recovers the LFS and Xet metadata that walking a symlink
// tree cannot produce, from the `trees/<commit>.json` the `hf` CLI left
// behind.
//
// This is what lets a repo first downloaded by `hf` propagate full
// fidelity over P2P: the ingest walk sees only bytes and filenames, but the
// tree already knows which files are LFS-backed and what their Xet hashes
// are. Missing tree or missing entry leaves the manifest untouched.
func EnrichFromTree(p Paths, commit string, files []mapping.FileManifest) {
	tree, ok, err := ReadTree(p, commit)
	if err != nil || !ok || tree == nil {
		return
	}
	for i := range files {
		tf, ok := tree.Files[files[i].Path]
		if !ok {
			continue
		}
		if tf.LFSSHA256 != "" || tf.XetHash != "" {
			files[i].LFS = true
		}
		if tf.XetHash != "" {
			files[i].XetHash = tf.XetHash
		}
	}
}
