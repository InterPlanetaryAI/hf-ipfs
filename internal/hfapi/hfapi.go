// Package hfapi is a minimal client for the Hugging Face Hub API, used by the
// pull path to resolve a repo id to its latest commit hash.
package hfapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ipai/hf-ipfs/internal/hfcache"
)

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

// RepoInfo is the subset of the HF Hub API response hf-ipfs uses.
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
// git blob id and LFS sha256 for every file, which hf-ipfs uses to cross-check
// what a peer advertised.
func (c *Client) RepoInfo(ctx context.Context, repoID string, t hfcache.RepoType, blobs bool) (*RepoInfo, error) {
	return c.RepoInfoAt(ctx, repoID, t, "", blobs)
}

// RepoInfoAt fetches metadata pinned to a specific commit or ref. Passing an
// empty revision asks for the default branch. The fallback pull needs the
// revision-pinned form so it fetches exactly the commit the DHT advertised,
// not whatever main moved to meanwhile.
func (c *Client) RepoInfoAt(ctx context.Context, repoID string, t hfcache.RepoType, revision string, blobs bool) (*RepoInfo, error) {
	url := fmt.Sprintf("%s/api/%s/%s?blobs=%t", c.Endpoint, t.APIPath(), repoID, blobs)
	if revision != "" {
		url = fmt.Sprintf("%s/api/%s/%s/revision/%s?blobs=%t",
			c.Endpoint, t.APIPath(), repoID, revision, blobs)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

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
	url := fmt.Sprintf("%s/api/%s/%s/tree/%s?recursive=true", c.Endpoint, t.APIPath(), repoID, revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

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
	return fmt.Sprintf("%s/%s%s/resolve/%s/%s", c.Endpoint, prefix, repoID, revision, escapePath(path))
}

// Download streams one file of a revision into w, following the redirect to
// the CDN. It returns the number of bytes written.
func (c *Client) Download(
	ctx context.Context,
	repoID string,
	t hfcache.RepoType,
	revision, path string,
	w io.Writer,
) (int64, error) {
	u := c.ResolveURL(repoID, t, revision, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.Stream.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return 0, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, snippet(body))
	}
	return io.Copy(w, resp.Body)
}

// escapePath percent-encodes each path segment so filenames with spaces or
// other reserved characters survive the round trip.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
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
