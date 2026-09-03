package dummy

import (
	"strings"
	"testing"

	cid "github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

const sampleCommit = "607a30d783dfa663caf39e06633721c8d4cfcd7e"

func TestRoundTrip(t *testing.T) {
	c, err := FromCommit(sampleCommit)
	if err != nil {
		t.Fatalf("FromCommit: %v", err)
	}
	if !c.Defined() {
		t.Fatal("expected a defined cid")
	}
	if c.Type() != cid.DagProtobuf {
		t.Errorf("codec = %d, want dag-pb", c.Type())
	}
	got, err := Commit(c)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got != sampleCommit {
		t.Errorf("round trip = %q, want %q", got, sampleCommit)
	}
}

func TestDeterministic(t *testing.T) {
	a, err := FromCommit(sampleCommit)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromCommit(sampleCommit)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("mapping is not deterministic: %s != %s", a, b)
	}
}

func TestDigestIsIdentityOfCommit(t *testing.T) {
	c, err := FromCommit(sampleCommit)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := mh.Decode(c.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if dec.Code != mh.IDENTITY {
		t.Errorf("multihash code = %#x, want identity (%#x)", dec.Code, mh.IDENTITY)
	}
	if len(dec.Digest) != 20 {
		t.Errorf("digest length = %d, want 20", len(dec.Digest))
	}
}

func TestRejectsBadCommits(t *testing.T) {
	for _, bad := range []string{
		"",
		"short",
		strings.Repeat("z", 40), // right length, not hex
		strings.Repeat("ab", 19),
	} {
		if _, err := FromCommit(bad); err == nil {
			t.Errorf("FromCommit(%q) should have failed", bad)
		}
	}
}

func TestIsRejectsRealContentCid(t *testing.T) {
	// A sha2-256 CID is not a dummy HF cid.
	real, err := mh.Sum([]byte("hello"), mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	if Is(cid.NewCidV1(cid.Raw, real)) {
		t.Error("sha2-256 cid wrongly reported as a dummy HF cid")
	}
	d, err := FromCommit(sampleCommit)
	if err != nil {
		t.Fatal(err)
	}
	if !Is(d) {
		t.Error("dummy cid not recognised by Is")
	}
}
