// Package wire defines the JSON messages exchanged over the hf-ipfs libp2p
// streams and the local daemon control socket.
package wire

import "github.com/ipai/hf-ipfs/internal/mapping"

// Status bytes prefixing every /hf-ipfs/block/1.0.0 response frame.
const (
	// BlockOK is followed by the block bytes.
	BlockOK = 0x00
	// BlockNotFound marks an absent block (empty payload).
	BlockNotFound = 0x01
)

// MapRequest is sent by a puller on /hf-ipfs/map/1.0.0.
type MapRequest struct {
	CommitHash string `json:"commit_hash"`
	RepoID     string `json:"repo_id,omitempty"`
}

// MapResponse answers a MapRequest with the actual content CID plus the file
// manifest needed to rebuild the HF snapshot layout.
type MapResponse struct {
	OK         bool                   `json:"ok"`
	Error      string                 `json:"error,omitempty"`
	CommitHash string                 `json:"commit_hash,omitempty"`
	RepoID     string                 `json:"repo_id,omitempty"`
	RepoType   string                 `json:"repo_type,omitempty"`
	ActualCID  string                 `json:"actual_cid,omitempty"`
	DummyCID   string                 `json:"dummy_cid,omitempty"`
	TotalSize  int64                  `json:"total_size,omitempty"`
	Files      []mapping.FileManifest `json:"files,omitempty"`
}

// Control requests/responses for the daemon's Unix socket.
const (
	CmdPull     = "pull"
	CmdIngest   = "ingest"
	CmdStatus   = "status"
	CmdList     = "list"
	CmdShutdown = "shutdown"
)

// ControlRequest is one daemon control command.
type ControlRequest struct {
	Cmd      string   `json:"cmd"`
	RepoID   string   `json:"repo_id,omitempty"`
	RepoType string   `json:"repo_type,omitempty"`
	Commit   string   `json:"commit,omitempty"`
	Ref      string   `json:"ref,omitempty"`
	Peers    []string `json:"peers,omitempty"`
	Connect  []string `json:"connect,omitempty"`
	Force    bool     `json:"force,omitempty"`
}

// ControlEvent is a streamed progress/completion event.
type ControlEvent struct {
	Type    string `json:"type"` // progress | done | error
	Message string `json:"message,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Total   int64  `json:"total,omitempty"`
}
