// Package hfcache models the on-disk layout of the upstream `hf` CLI cache so
// hf-ipfs can read and write it transparently.
//
//	$HF_HUB_CACHE/
//	  models--openai-community--gpt2/
//	    refs/
//	      main                 # 40 hex commit hash + "\n"
//	    snapshots/
//	      <commit>/
//	        config.json        -> ../../blobs/<blob_name>
//	        subdir/weights.safetensors -> ../../../blobs/<blob_name>
//	    blobs/
//	      <blob_name>          # the real bytes, named by the hf CLI
//
// The symlink target basename is authoritative for blob naming: whatever the hf
// CLI chose (LFS sha256 for large files, git blob id for small ones) is exactly
// what a receiving peer must reproduce, so hf-ipfs carries it verbatim in the
// manifest instead of re-deriving it.
package hfcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipai/hf-ipfs/internal/mapping"
)

// RepoType selects the hf cache namespace.
type RepoType string

// Supported hf repo types.
const (
	Model   RepoType = "model"
	Dataset RepoType = "dataset"
	Space   RepoType = "space"
)

// DirPrefix is the `hub/` directory prefix for this repo type.
func (t RepoType) DirPrefix() string {
	switch t {
	case Model:
		return "models--"
	case Dataset:
		return "datasets--"
	case Space:
		return "spaces--"
	default:
		return "models--"
	}
}

// APIPath is the HF Hub API path segment for this repo type.
func (t RepoType) APIPath() string {
	switch t {
	case Model:
		return "models"
	case Dataset:
		return "datasets"
	case Space:
		return "spaces"
	default:
		return "models"
	}
}

// ParseRepoType normalises a user supplied repo type.
func ParseRepoType(s string) (RepoType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "model", "models":
		return Model, nil
	case "dataset", "datasets":
		return Dataset, nil
	case "space", "spaces":
		return Space, nil
	default:
		return "", fmt.Errorf("unknown repo type %q (want model, dataset or space)", s)
	}
}

// Paths locates one repo inside the hub cache.
type Paths struct {
	Hub       string
	RepoID    string
	Type      RepoType
	RepoDir   string
	Blobs     string
	Snapshots string
	Refs      string
	Trees     string
}

// RepoFromDirName maps a `hub/` directory name back to (repo id, type).
func RepoFromDirName(name string) (string, RepoType, bool) {
	for _, t := range []RepoType{Model, Dataset, Space} {
		p := t.DirPrefix()
		if strings.HasPrefix(name, p) {
			return strings.ReplaceAll(strings.TrimPrefix(name, p), "--", "/"), t, true
		}
	}
	return "", "", false
}

// NewPaths resolves the cache paths for a repo.
func NewPaths(hubDir, repoID string, t RepoType) Paths {
	repoDir := filepath.Join(hubDir, RepoDirName(repoID, t))
	return Paths{
		Hub:       hubDir,
		RepoID:    repoID,
		Type:      t,
		RepoDir:   repoDir,
		Blobs:     filepath.Join(repoDir, "blobs"),
		Snapshots: filepath.Join(repoDir, "snapshots"),
		Refs:      filepath.Join(repoDir, "refs"),
		Trees:     filepath.Join(repoDir, "trees"),
	}
}

// RepoDirName renders the hf cache directory name for a repo id.
func RepoDirName(repoID string, t RepoType) string {
	return t.DirPrefix() + strings.ReplaceAll(repoID, "/", "--")
}

// SnapshotDir is `snapshots/<commit>`.
func (p Paths) SnapshotDir(commit string) string {
	return filepath.Join(p.Snapshots, commit)
}

// RefPath is `refs/<ref>`.
func (p Paths) RefPath(ref string) string {
	return filepath.Join(p.Refs, ref)
}

// BlobPath is `blobs/<name>`.
func (p Paths) BlobPath(name string) string {
	return filepath.Join(p.Blobs, name)
}

// Exists reports whether the repo has any cache presence at all.
func (p Paths) Exists() bool {
	st, err := os.Stat(p.RepoDir)
	return err == nil && st.IsDir()
}

// CurrentCommit reads refs/<ref> (default "main").
func (p Paths) CurrentCommit(ref string) (string, error) {
	if ref == "" {
		ref = "main"
	}
	data, err := os.ReadFile(p.RefPath(ref))
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(data))
	if len(commit) != 40 {
		return "", fmt.Errorf("refs/%s contains %q, not a 40 char commit hash", ref, commit)
	}
	return commit, nil
}

// ListRepos returns every repo id present in the hub cache.
func ListRepos(hubDir string) ([]string, error) {
	entries, err := os.ReadDir(hubDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		var prefix string
		switch {
		case strings.HasPrefix(name, "models--"):
			prefix = "models--"
		case strings.HasPrefix(name, "datasets--"):
			prefix = "datasets--"
		case strings.HasPrefix(name, "spaces--"):
			prefix = "spaces--"
		default:
			continue
		}
		repoID := strings.ReplaceAll(strings.TrimPrefix(name, prefix), "--", "/")
		out = append(out, repoID)
	}
	sortStrings(out)
	return out, nil
}

// ReadSnapshot dereferences `snapshots/<commit>/` into a manifest.
//
// Every symlink is resolved to the blob it points at; the blob's bytes are
// streamed through SHA-256 so a receiving peer can verify integrity without
// trusting the sender's filenames.
func ReadSnapshot(p Paths, commit string) ([]mapping.FileManifest, error) {
	snapDir := p.SnapshotDir(commit)
	if _, err := os.Stat(snapDir); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", commit, err)
	}

	var files []mapping.FileManifest
	err := filepath.WalkDir(snapDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".cache" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(snapDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Skip only what lives *inside* VCS/hub bookkeeping directories.
		// Matching a bare ".git" prefix here also swallowed .gitattributes
		// (and would swallow .gitignore, .github/, …), which made every p2p
		// pull silently incomplete: the file never entered the ingest DAG,
		// so no puller ever received it or its blob.
		if isInsideBookkeeping(rel) {
			return nil
		}

		info, err := os.Stat(path) // follows symlinks
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		// The blob name is the symlink target's basename when the link points
		// into blobs/; otherwise fall back to the content hash.
		blobName := info.Name()
		srcPath := path
		if lnk, lerr := os.Readlink(path); lerr == nil {
			resolved := lnk
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			srcPath = resolved
			if under(p.Blobs, resolved) {
				blobName = filepath.Base(resolved)
			}
		}

		digest, err := hashFile(srcPath, info.Size())
		if err != nil {
			return fmt.Errorf("hash %s: %w", rel, err)
		}
		if blobName == info.Name() && !under(p.Blobs, srcPath) {
			blobName = digest
		}

		files = append(files, mapping.FileManifest{
			Path:     rel,
			BlobName: blobName,
			Size:     info.Size(),
			SHA256:   digest,
			Mode:     uint32(info.Mode().Perm()),
			Mtime:    info.ModTime().Unix(),
			SrcPath:  srcPath,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortManifests(files)
	return files, nil
}

// SnapshotComplete reports whether every file referenced by the snapshot already
// exists on disk. This is the "download finalized" test.
func SnapshotComplete(p Paths, commit string) bool {
	snapDir := p.SnapshotDir(commit)
	entries, err := os.ReadDir(snapDir)
	if err != nil {
		return false
	}
	if len(entries) == 0 {
		return false
	}
	ok := true
	var walk func(dir string)
	walk = func(dir string) {
		if !ok {
			return
		}
		es, err := os.ReadDir(dir)
		if err != nil {
			ok = false
			return
		}
		for _, e := range es {
			if !ok {
				return
			}
			path := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(path)
				continue
			}
			if _, err := os.Stat(path); err != nil {
				ok = false
				return
			}
		}
	}
	walk(snapDir)
	return ok
}

// WriteSnapshot recreates the `snapshots/<commit>/` symlink tree and updates
// `refs/<ref>`. Blobs are expected to already be in place.
func WriteSnapshot(p Paths, commit string, files []mapping.FileManifest, ref string) error {
	if ref == "" {
		ref = "main"
	}
	snapDir := p.SnapshotDir(commit)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	for _, f := range files {
		relTarget := filepath.FromSlash(f.Path)
		linkPath := filepath.Join(snapDir, relTarget)
		if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(relTarget), err)
		}
		blobPath := p.BlobPath(f.BlobName)
		if _, err := os.Stat(blobPath); err != nil {
			return fmt.Errorf("blob %s missing for %s: %w", f.BlobName, f.Path, err)
		}
		rel, err := filepath.Rel(filepath.Dir(linkPath), blobPath)
		if err != nil {
			return err
		}
		if existing, lerr := os.Readlink(linkPath); lerr == nil {
			if existing == rel {
				continue
			}
			if err := os.Remove(linkPath); err != nil {
				return fmt.Errorf("replace stale link %s: %w", f.Path, err)
			}
		} else if _, serr := os.Lstat(linkPath); serr == nil {
			if err := os.Remove(linkPath); err != nil {
				return fmt.Errorf("replace stale %s: %w", f.Path, err)
			}
		}
		if err := os.Symlink(rel, linkPath); err != nil {
			return fmt.Errorf("symlink %s: %w", f.Path, err)
		}
	}
	// The tree is part of a complete cache: the `hf` CLI expects
	// trees/<commit>.json alongside the symlink tree.
	if err := WriteTree(p, commit, files); err != nil {
		return err
	}
	return WriteRef(p, ref, commit)
}

// WriteRef writes `refs/<ref>` atomically.
func WriteRef(p Paths, ref, commit string) error {
	if err := os.MkdirAll(p.Refs, 0o755); err != nil {
		return err
	}
	target := p.RefPath(ref)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(commit+"\n"), 0o644); err != nil {
		return fmt.Errorf("write refs/%s: %w", ref, err)
	}
	return os.Rename(tmp, target)
}

func under(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// bookkeepingDirs hold a snapshot's plumbing rather than its content: .git
// is VCS internals, .cache is the hub's in-flight download state.
var bookkeepingDirs = []string{".git", ".cache"}

// isInsideBookkeeping reports whether a slash-separated relative path lies
// inside one of bookkeepingDirs — as opposed to merely starting with the
// same letters, which is the distinction that used to eat .gitattributes.
func isInsideBookkeeping(rel string) bool {
	for _, d := range bookkeepingDirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

func hashFile(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func sortManifests(f []mapping.FileManifest) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Path < f[j-1].Path; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}
