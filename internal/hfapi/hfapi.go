// Package hfapi is a minimal client for the Hugging Face Hub API, used by the
// pull path to resolve a repo id to its latest commit hash.
package hfapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/ipai/hf-ipfs/internal/hfcache"
)

var log = logging.Logger("sf/hfapi")

// LFSInfo is the LFS pointer metadata HF returns with ?blobs=true.
type LFSInfo struct {
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	PointerSize int64  `json:"pointerSize"`
}

// Sibling is one file of a revision.
type Sibling struct {
	RFilename string   `json:"rfilename"`
	BlobID    string   `json:"blobId"`
	Size      int64    `json:"size"`
	LFS       *LFSInfo `json:"lfs"`
}

// RepoInfo is the subset of the HF Hub API response swarmface uses.
type RepoInfo struct {
	ID       string    `json:"id"`
	SHA      string    `json:"sha"`
	Siblings []Sibling `json:"siblings"`
}

// Client talks to a HF Hub endpoint.
type Client struct {
	Endpoint string

	// HTTP is for metadata calls: small, bounded, fail-fast.
	HTTP *http.Client

	// Stream is for file downloads. It deliberately has no whole-request
	// timeout: model shards run to gigabytes and a fixed deadline would kill
	// every large download. Liveness comes from the caller's context plus the
	// per-connection timeouts below instead.
	Stream *http.Client

	Token string
}

// NewClient builds a client for the given endpoint.
func NewClient(endpoint string) *Client {
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		Stream: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   30 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				MaxIdleConnsPerHost:   4,
			},
		},
	}
}

// RepoInfo fetches revision metadata. With blobs=true the response carries the
// git blob id and LFS sha256 for every file, which swarmface uses to cross-check
// what a peer advertised.
func (c *Client) RepoInfo(ctx context.Context, repoID string, t hfcache.RepoType, blobs bool) (*RepoInfo, error) {
	return c.RepoInfoAt(ctx, repoID, t, "", blobs)
}

// RepoInfoAt fetches metadata pinned to a specific commit or ref. Passing an
// empty revision asks for the default branch. The fallback pull needs the
// revision-pinned form so it fetches exactly the commit the DHT advertised,
// not whatever main moved to meanwhile.
func (c *Client) RepoInfoAt(ctx context.Context, repoID string, t hfcache.RepoType, revision string, blobs bool) (*RepoInfo, error) {
	url := fmt.Sprintf("%s/api/%s/%s?blobs=%t", c.Endpoint, t.APIPath(), escapePath(repoID), blobs)
	if revision != "" {
		url = fmt.Sprintf("%s/api/%s/%s/revision/%s?blobs=%t",
			c.Endpoint, t.APIPath(), escapePath(repoID), escapePath(revision), blobs)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.attachAuth(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, snippet(body))
	}

	info := &RepoInfo{}
	if err := json.Unmarshal(body, info); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", url, err)
	}
	if len(info.SHA) != 40 {
		return nil, fmt.Errorf("%s returned no usable commit hash (sha=%q)", repoID, info.SHA)
	}
	if _, err := hex.DecodeString(info.SHA); err != nil {
		return nil, fmt.Errorf("%s returned a non-hex commit hash (sha=%q)", repoID, info.SHA)
	}
	return info, nil
}

// LatestCommit resolves a repo id to its current commit hash.
func (c *Client) LatestCommit(ctx context.Context, repoID string, t hfcache.RepoType) (string, error) {
	info, err := c.RepoInfo(ctx, repoID, t, false)
	if err != nil {
		return "", err
	}
	return info.SHA, nil
}

// TreeEntry is one file from the Hub tree endpoint.
//
// The revision endpoint's siblings carry no Xet hash — only the tree
// endpoint exposes it — which is why a fallback pull needs this in
// addition to RepoInfoAt to produce a fully faithful trees/<commit>.json.
type TreeEntry struct {
	Path    string   `json:"path"`
	Type    string   `json:"type"`
	Oid     string   `json:"oid"`
	Size    int64    `json:"size"`
	LFS     *LFSInfo `json:"lfs"`
	XetHash string   `json:"xetHash"`
}

// TreeAt lists every file of a revision, recursively. Directory entries are
// filtered out.
//
// A failure here costs only Xet-hash fidelity in the generated
// trees/<commit>.json, so callers may treat it as non-fatal.
func (c *Client) TreeAt(ctx context.Context, repoID string, t hfcache.RepoType, revision string) ([]TreeEntry, error) {
	url := fmt.Sprintf("%s/api/%s/%s/tree/%s?recursive=true", c.Endpoint, t.APIPath(), escapePath(repoID), escapePath(revision))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.attachAuth(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	// A repo with tens of thousands of files is rare but not impossible;
	// 32 MiB of JSON metadata bounds it without capping real repos.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, snippet(body))
	}

	var all []TreeEntry
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", url, err)
	}
	files := make([]TreeEntry, 0, len(all))
	for _, e := range all {
		if e.Type == "directory" {
			continue
		}
		files = append(files, e)
	}
	return files, nil
}

// ResolveURL builds the canonical download URL for one file of a revision.
// Models sit at the hub root; datasets and spaces are namespaced under their
// own segment.
func (c *Client) ResolveURL(repoID string, t hfcache.RepoType, revision, path string) string {
	prefix := ""
	if t != hfcache.Model {
		prefix = t.APIPath() + "/"
	}
	return fmt.Sprintf("%s/%s%s/resolve/%s/%s", c.Endpoint, prefix, escapePath(repoID), escapePath(revision), escapePath(path))
}

// Download streams one file of a revision into w, following the redirect to
// the CDN. maxBytes bounds the response body (pass -1 for no bound): the
// copy stops after one byte over the claim instead of writing terabytes to
// a temp file before the caller's size check fires. bodyIdleTimeout is the
// liveness bound that stands in for the deliberately absent whole-request
// timeout: a server that sends headers and then trickles nothing would
// otherwise hang the pull until the operator kills it. It returns the number
// of bytes written.
func (c *Client) Download(
	ctx context.Context,
	repoID string,
	t hfcache.RepoType,
	revision, path string,
	maxBytes int64,
	w io.Writer,
) (int64, error) {
	u := c.ResolveURL(repoID, t, revision, path)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	c.attachAuth(req)

	resp, err := c.Stream.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, snippet(body))
	}

	var src io.Reader = resp.Body
	if maxBytes >= 0 {
		src = io.LimitReader(resp.Body, maxBytes+1)
	}
	last := &atomic.Int64{}
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(bodyIdleTimeout / 3)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if time.Since(time.Unix(0, last.Load())) > bodyIdleTimeout {
					log.Warnf("hf: %s silent for over %s, aborting", u, bodyIdleTimeout)
					cancel()
					return
				}
			}
		}
	}()
	return io.Copy(w, &trackedReader{r: src, last: last})
}

// bodyIdleTimeout bounds silence from a streaming response body. The Stream
// client has no whole-request timeout (multi-GB shards must survive); this
// is the per-operation liveness bound that stands in for it.
var bodyIdleTimeout = 60 * time.Second

// insecureWarned keeps the cleartext-token warning to one line per process.
var insecureWarned atomic.Bool

// attachAuth sets the bearer header, warning once when a token is
// configured against a non-TLS endpoint: the header would then cross the
// wire in cleartext (URLs and errors are logged, the token never is).
func (c *Client) attachAuth(req *http.Request) {
	if c.Token == "" {
		return
	}
	if !strings.HasPrefix(c.Endpoint, "https://") && insecureWarned.CompareAndSwap(false, true) {
		log.Warnf("hf endpoint %s is not TLS but a token is configured: the Authorization header travels in cleartext", c.Endpoint)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
}

// trackedReader stamps last with the time of the most recent completed read
// so the watchdog can tell a slow stream from a dead one.
type trackedReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (tr *trackedReader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	tr.last.Store(time.Now().UnixNano())
	return n, err
}

// escapePath percent-encodes each path segment so filenames with spaces or
// other reserved characters survive the round trip.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		// url.PathEscape leaves "." and ".." verbatim, so dot segments
		// would survive into the request URI and be cleaned away — letting
		// an unvalidated repo id or revision rewrite the fetched path while
		// the bearer token rides along. Percent-encoding the dots keeps the
		// path literal.
		switch s {
		case ".":
			segs[i] = "%2E"
		case "..":
			segs[i] = "%2E%2E"
		default:
			segs[i] = url.PathEscape(s)
		}
	}
	return strings.Join(segs, "/")
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
