// Package hfapi is a minimal client for the Hugging Face Hub API, used by the
// pull path to resolve a repo id to its latest commit hash.
package hfapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	HTTP     *http.Client
	Token    string
}

// NewClient builds a client for the given endpoint.
func NewClient(endpoint string) *Client {
	return &Client{
		Endpoint: strings.TrimRight(endpoint, "/"),
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// RepoInfo fetches revision metadata. With blobs=true the response carries the
// git blob id and LFS sha256 for every file, which hf-ipfs uses to cross-check
// what a peer advertised.
func (c *Client) RepoInfo(ctx context.Context, repoID string, t hfcache.RepoType, blobs bool) (*RepoInfo, error) {
	url := fmt.Sprintf("%s/api/%s/%s?blobs=%t", c.Endpoint, t.APIPath(), repoID, blobs)
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

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
