// Package watch detects finalized Hugging Face downloads and ingests them.
//
// fsnotify is the fast path; a periodic rescan is the correctness path. A
// download is considered finalized when `refs/<ref>` names a commit whose
// `snapshots/<commit>/` tree exists and every entry resolves to a real blob.
package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	logging "github.com/ipfs/go-log/v2"

	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/ingest"
	"github.com/ipai/hf-ipfs/internal/node"
)

var log = logging.Logger("hf-ipfs/watch")

// DebounceWindow is how long a repo must be quiet before we try to ingest it.
const DebounceWindow = 1500 * time.Millisecond

// Watcher observes the HF hub cache.
type Watcher struct {
	n *node.Node

	mu      sync.Mutex
	pending map[string]time.Time // repo id -> last event time
	known   map[string]bool      // directories we already watch
}

// New builds a watcher for the node's configured hub directory.
func New(n *node.Node) *Watcher {
	return &Watcher{
		n:       n,
		pending: make(map[string]time.Time),
		known:   make(map[string]bool),
	}
}

// Run blocks until ctx is cancelled, ingesting finalized downloads.
func (w *Watcher) Run(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fw.Close()

	if err := os.MkdirAll(w.n.Cfg.HFHubDir, 0o755); err != nil {
		return err
	}
	if err := fw.Add(w.n.Cfg.HFHubDir); err != nil {
		return err
	}
	w.trackExistingRepos(fw)

	// Catch anything that landed before we started watching.
	w.Scan(ctx)

	debounce := time.NewTicker(DebounceWindow / 2)
	defer debounce.Stop()

	var rescan <-chan time.Time
	if w.n.Cfg.RescanInterval > 0 {
		t := time.NewTicker(w.n.Cfg.RescanInterval)
		defer t.Stop()
		rescan = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-fw.Events:
			if !ok {
				return errors.New("fsnotify event channel closed")
			}
			w.handle(fw, ev)

		case err := <-fw.Errors:
			log.Warnf("fsnotify: %s", err)

		case <-debounce.C:
			w.flush(ctx)

		case <-rescan:
			w.Scan(ctx)
			// Peers may have joined since the last announcement attempt.
			if _, err := w.n.ReprovideAll(ctx); err != nil {
				log.Debugf("reprovide: %s", err)
			}
		}
	}
}

// Scan ingests every repo whose current ref is finalized but not yet shared.
func (w *Watcher) Scan(ctx context.Context) {
	repos, err := hfcache.ListRepos(w.n.Cfg.HFHubDir)
	if err != nil {
		log.Warnf("rescan: %s", err)
		return
	}
	for _, repoID := range repos {
		if ctx.Err() != nil {
			return
		}
		w.tryIngest(ctx, repoID)
	}
}

func (w *Watcher) handle(fw *fsnotify.Watcher, ev fsnotify.Event) {
	// New repo directory: start watching its refs/ and snapshots/ too.
	if ev.Op.Has(fsnotify.Create) {
		if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
			if base := filepath.Base(ev.Name); strings.HasPrefix(base, "models--") ||
				strings.HasPrefix(base, "datasets--") || strings.HasPrefix(base, "spaces--") {
				w.addRepoWatches(fw, ev.Name)
			}
		}
	}

	repoDir := repoDirOf(w.n.Cfg.HFHubDir, ev.Name)
	if repoDir == "" {
		return
	}
	repoID, _, ok := hfcache.RepoFromDirName(filepath.Base(repoDir))
	if !ok {
		return
	}

	w.mu.Lock()
	w.pending[repoID] = time.Now()
	w.mu.Unlock()
}

// flush ingests repos that have been quiet for the debounce window.
func (w *Watcher) flush(ctx context.Context) {
	w.mu.Lock()
	due := make([]string, 0, len(w.pending))
	now := time.Now()
	for repoID, at := range w.pending {
		if now.Sub(at) >= DebounceWindow {
			due = append(due, repoID)
		}
	}
	for _, r := range due {
		delete(w.pending, r)
	}
	w.mu.Unlock()

	for _, repoID := range due {
		if ctx.Err() != nil {
			return
		}
		w.tryIngest(ctx, repoID)
	}
}

func (w *Watcher) tryIngest(ctx context.Context, repoID string) {
	t := w.repoType(repoID)
	paths := hfcache.NewPaths(w.n.Cfg.HFHubDir, repoID, t)

	commit, err := paths.CurrentCommit("main")
	if err != nil {
		return // no ref yet
	}
	if ok, err := w.n.Mapping.Has(ctx, commit); err == nil && ok {
		return
	}
	if !hfcache.SnapshotComplete(paths, commit) {
		log.Debugf("%s@%s not finalized yet", repoID, short(commit))
		return
	}

	res, err := ingest.Run(ctx, w.n, repoID, t, commit, nil)
	if err != nil {
		log.Warnf("ingest %s@%s: %s", repoID, short(commit), err)
		return
	}
	log.Infof("shared %s@%s: %s, %d files, %d chunks (actual %s)",
		repoID, short(commit), humanBytes(res.TotalSize), res.Files, res.Chunks, res.ActualCID)
}

// repoType infers the hf repo type from the on-disk directory prefix.
func (w *Watcher) repoType(repoID string) hfcache.RepoType {
	for _, t := range []hfcache.RepoType{hfcache.Model, hfcache.Dataset, hfcache.Space} {
		st, err := os.Stat(filepath.Join(w.n.Cfg.HFHubDir, hfcache.RepoDirName(repoID, t)))
		if err == nil && st.IsDir() {
			return t
		}
	}
	return hfcache.Model
}

func (w *Watcher) trackExistingRepos(fw *fsnotify.Watcher) {
	entries, err := os.ReadDir(w.n.Cfg.HFHubDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, _, ok := hfcache.RepoFromDirName(name); !ok {
			continue
		}
		w.addRepoWatches(fw, filepath.Join(w.n.Cfg.HFHubDir, name))
	}
}

func (w *Watcher) addRepoWatches(fw *fsnotify.Watcher, repoDir string) {
	for _, sub := range []string{"refs", "snapshots"} {
		dir := filepath.Join(repoDir, sub)
		w.mu.Lock()
		seen := w.known[dir]
		w.mu.Unlock()
		if seen {
			continue
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		if err := fw.Add(dir); err != nil {
			log.Debugf("watch %s: %s", dir, err)
			continue
		}
		w.mu.Lock()
		w.known[dir] = true
		w.mu.Unlock()
	}
}

// repoDirOf walks up from path to the containing `hub/<repo>` directory.
func repoDirOf(hub, path string) string {
	cur := path
	for i := 0; i < 6; i++ {
		if cur == hub {
			return ""
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		if filepath.Dir(parent) == hub {
			return cur
		}
		cur = parent
	}
	return ""
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
