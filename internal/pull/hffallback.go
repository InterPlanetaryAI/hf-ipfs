package pull

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/ipai/hf-ipfs/internal/hfapi"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/ingest"
	"github.com/ipai/hf-ipfs/internal/mapping"
	"github.com/ipai/hf-ipfs/internal/node"
	"github.com/ipai/hf-ipfs/internal/wire"
)

// pullFromHF downloads a revision straight from the Hugging Face Hub and then
// ingests it locally, so a fallback pull lands in exactly the same state as a
// p2p one: blobs in the cache, snapshot links written, mapping recorded, and the
// commit announced on the DHT. Falling back must not leave the node a mere
// bystander — the whole point is that it becomes a seeder afterwards.
func pullFromHF(
	ctx context.Context,
	n *node.Node,
	paths hfcache.Paths,
	commit, ref string,
	opts Options,
	ev EventFunc,
) error {
	ev(wire.ControlEvent{Type: "progress",
		Message: fmt.Sprintf("hf: fetching %s @ %s from %s",
			opts.RepoID, short(commit), n.Cfg.HFEndpoint)})

	client := hfClient(n, opts.Token)
	info, err := client.RepoInfoAt(ctx, opts.RepoID, opts.RepoType, commit, true)
	if err != nil {
		return fmt.Errorf("hf: repo metadata: %w", err)
	}
	if len(info.Siblings) == 0 {
		return fmt.Errorf("hf: %s has no files at %s", opts.RepoID, short(commit))
	}

	// The repo may never have been touched by the hf CLI, so the cache
	// skeleton has to come from us.
	if err := os.MkdirAll(paths.Blobs, 0o755); err != nil {
		return fmt.Errorf("hf: create %s: %w", paths.Blobs, err)
	}

	// The tree endpoint is the only source for the Xet hash; the revision
	// endpoint omits it. Losing it costs fidelity in the generated
	// trees/<commit>.json and nothing else, so this degrades rather than
	// aborting the pull.
	xetByPath := map[string]string{}
	if tree, terr := client.TreeAt(ctx, opts.RepoID, opts.RepoType, commit); terr == nil {
		for _, e := range tree {
			if e.XetHash != "" {
				xetByPath[e.Path] = e.XetHash
			}
		}
	} else {
		log.Debugf("hf: tree lookup failed for %s@%s: %s (trees json will omit xet_hash)",
			opts.RepoID, short(commit), terr)
	}

	files, total, err := manifestFromRevision(info.Siblings, xetByPath)
	if err != nil {
		return fmt.Errorf("hf: %w", err)
	}

	ev(wire.ControlEvent{Type: "progress",
		Message: fmt.Sprintf("hf: %d file(s), %s", len(files), humanBytes(total))})

	state := &xferState{total: total}
	for i := range files {
		if err := fetchBlob(ctx, client, paths, opts.RepoID, opts.RepoType, commit, &files[i], state, ev); err != nil {
			return err
		}
	}

	if err := hfcache.WriteSnapshot(paths, commit, files, ref); err != nil {
		return fmt.Errorf("hf: step E (symlinking): %w", err)
	}

	ev(wire.ControlEvent{Type: "progress",
		Message: "hf: downloaded; ingesting so this node can serve it"})

	res, err := ingest.Run(ctx, n, opts.RepoID, opts.RepoType, commit, func(m string) {
		ev(wire.ControlEvent{Type: "progress", Message: m})
	})
	if err != nil {
		return fmt.Errorf("hf: ingest: %w", err)
	}

	ev(wire.ControlEvent{Type: "done",
		Message: fmt.Sprintf("downloaded %s @ %s (%s) from Hugging Face into %s; shared as %s",
			opts.RepoID, short(commit), humanBytes(res.TotalSize), paths.RepoDir, res.ActualCID)})
	return nil
}

// fetchBlob downloads one file into blobs/<blobName>, verifying integrity before
// it becomes visible under its final name. A partially written blob must never
// appear at the real path, because the filestore and the snapshot links both take
// the blob's presence as proof it is complete.
//
// Integrity is established per file class:
//
//   - LFS objects carry a content sha256 from the API; we check against it.
//   - Plain git files carry no content hash, so we check the way git does:
//     SHA-1 over "blob <size>\0" + content must equal the blob id. That also
//     confirms the blob id is the correct filename, which is what keeps this
//     cache byte-compatible with the upstream hf CLI.
func fetchBlob(
	ctx context.Context,
	client *hfapi.Client,
	paths hfcache.Paths,
	repoID string,
	t hfcache.RepoType,
	commit string,
	f *mapping.FileManifest,
	state *xferState,
	ev EventFunc,
) error {
	blobPath := paths.BlobPath(f.BlobName)
	if st, err := os.Stat(blobPath); err == nil && st.Size() == f.Size {
		state.done += f.Size
		ev(wire.ControlEvent{Type: "progress",
			Message: fmt.Sprintf("hf: %s already cached", f.Path),
			Bytes:   state.done, Total: state.total})
		return nil
	}

	tmp := blobPath + "." + randSuffix() + ".download"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("hf: create %s: %w", tmp, err)
	}

	sha256h := sha256.New()
	gitH := sha1.New()
	fmt.Fprintf(gitH, "blob %d\x00", f.Size)
	w := io.MultiWriter(fh, sha256h, gitH)

	n, err := client.Download(ctx, repoID, t, commit, f.Path, w)
	if err != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("hf: download %s: %w", f.Path, err)
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hf: close %s: %w", tmp, err)
	}
	if n != f.Size {
		_ = os.Remove(tmp)
		return fmt.Errorf("hf: %s: expected %d bytes, got %d", f.Path, f.Size, n)
	}

	got := hex.EncodeToString(sha256h.Sum(nil))
	switch {
	case f.SHA256 != "" && got != f.SHA256:
		_ = os.Remove(tmp)
		return fmt.Errorf("hf: %s: sha256 mismatch (api says %s, content hashes to %s)",
			f.Path, f.SHA256, got)
	case f.SHA256 == "":
		if want := hex.EncodeToString(gitH.Sum(nil)); want != f.BlobName {
			_ = os.Remove(tmp)
			return fmt.Errorf("hf: %s: git blob id mismatch (api says %s, content hashes to %s)",
				f.Path, f.BlobName, want)
		}
		f.SHA256 = got
	}

	if err := os.Rename(tmp, blobPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hf: place blob %s: %w", f.BlobName, err)
	}
	state.done += n
	ev(wire.ControlEvent{Type: "progress",
		Message: fmt.Sprintf("hf: %s (%s)", f.Path, humanBytes(n)),
		Bytes:   state.done, Total: state.total})
	return nil
}

// manifestFromRevision turns the Hub's sibling list into pull manifests.
//
// xetByPath supplies the Xet hashes that only the tree endpoint exposes; a
// missing entry simply means the file is not Xet-backed.
//
// The LFS flag comes from the sibling's lfs object and is what decides
// whether the file carries lfs_sha256/lfs_size in trees/<commit>.json.
// It cannot be inferred from SHA256: we hash every file we touch, and
// stamping that onto plain git blobs would produce a tree the `hf` CLI
// would not have written.
func manifestFromRevision(siblings []hfapi.Sibling, xetByPath map[string]string) ([]mapping.FileManifest, int64, error) {
	files := make([]mapping.FileManifest, 0, len(siblings))
	var total int64
	for _, s := range siblings {
		if s.BlobID == "" {
			return nil, 0, fmt.Errorf("%s has no blob id", s.RFilename)
		}
		f := mapping.FileManifest{
			Path:     s.RFilename,
			BlobName: s.BlobID,
			Size:     s.Size,
			Mode:     0o644,
			XetHash:  xetByPath[s.RFilename],
		}
		// LFS objects carry the content sha256, which is what we verify
		// against. Plain git files do not; they get verified by their git
		// blob id instead (see fetchBlob).
		if s.LFS != nil {
			f.LFS = true
			f.Size = s.LFS.Size
			f.SHA256 = s.LFS.SHA256
		}
		files = append(files, f)
		total += f.Size
	}
	return files, total, nil
}

// hfClient builds a Hub API client for this node. A non-empty override wins
// over the node's configured token, which is what lets `pull --hf-token X`
// work against a daemon that was started without one.
//
// The token travels only as an Authorization header — never in a URL, and
// never echoed into an error, because hf-ipfs logs both.
func hfClient(n *node.Node, override string) *hfapi.Client {
	c := hfapi.NewClient(n.Cfg.HFEndpoint)
	c.Token = resolveToken(override, n.Cfg.HFToken)
	return c
}

// resolveToken applies a per-request override over the configured default.
func resolveToken(override, configured string) string {
	if override != "" {
		return override
	}
	return configured
}
