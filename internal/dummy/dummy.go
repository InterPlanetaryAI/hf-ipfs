// Package dummy implements the deterministic mapping between a Hugging Face
// commit hash and an IPFS-compatible multihash.
//
// A HF commit hash is a 40 character hex string (a git SHA-1). We need a CID
// to key provider records in the Kademlia DHT, but we do not want to depend on
// the "actual" content CID being known by the peer that only knows the commit
// hash. So we build a *dummy* CID whose multihash digest literally *is* the
// commit hash bytes, tagged with the identity multihash code.
//
// The mapping is:
//
//	bijection: commit (40 hex)  <->  CIDv1{DagProtobuf, identity(20 bytes)}
//
// identity is used rather than a fake sha1 because it is on IPFS' default
// multihash allowlist and because it is losslessly reversible: the DHT key
// carries the commit hash, so a peer can recover which HF commit a provider
// record refers to without any lookup.
package dummy

import (
	"encoding/hex"
	"fmt"

	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// CommitLen is the expected length of a Hugging Face (git) commit hash.
const CommitLen = 40

// FromCommit maps a 40 character hex HF commit hash to its dummy CID.
func FromCommit(commit string) (cid.Cid, error) {
	if len(commit) != CommitLen {
		return cid.Undef, fmt.Errorf("invalid HF commit hash %q: expected %d hex characters, got %d",
			commit, CommitLen, len(commit))
	}
	raw, err := hex.DecodeString(commit)
	if err != nil {
		return cid.Undef, fmt.Errorf("invalid HF commit hash %q: %w", commit, err)
	}
	digest, err := mh.Sum(raw, mh.IDENTITY, -1)
	if err != nil {
		return cid.Undef, fmt.Errorf("build identity multihash: %w", err)
	}
	return cid.NewCidV1(cid.DagProtobuf, digest), nil
}

// Commit recovers the HF commit hash from a dummy CID produced by FromCommit.
func Commit(c cid.Cid) (string, error) {
	dec, err := mh.Decode(c.Hash())
	if err != nil {
		return "", fmt.Errorf("decode dummy multihash %s: %w", c, err)
	}
	if dec.Code != mh.IDENTITY {
		return "", fmt.Errorf("cid %s is not a dummy HF cid (multihash code %#x)", c, dec.Code)
	}
	if len(dec.Digest) != 20 {
		return "", fmt.Errorf("cid %s is not a dummy HF cid (digest length %d)", c, len(dec.Digest))
	}
	return hex.EncodeToString(dec.Digest), nil
}

// Is reports whether c looks like a dummy HF commit CID.
func Is(c cid.Cid) bool {
	_, err := Commit(c)
	return err == nil
}
