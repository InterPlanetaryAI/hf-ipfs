package pull

import (
	"context"
	"testing"

	bserv "github.com/ipfs/boxo/blockservice"
	blockstore "github.com/ipfs/boxo/blockstore"
	merkledag "github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	cid "github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	ipld "github.com/ipfs/go-ipld-format"
	mh "github.com/multiformats/go-multihash"
)

// A peer-shaped DAG where each interior node links twice to its predecessor
// encodes 2^d leaf paths from d distinct blocks. Without the leaf budget the
// puller materialises the exponential list before writing a single byte.
func TestCollectLeavesCapsExponentialDAG(t *testing.T) {
	defer func(v int) { maxFileChunks = v }(maxFileChunks)
	maxFileChunks = 8

	ctx := context.Background()
	bs := blockstore.NewBlockstore(dssync.MutexWrap(ds.NewMapDatastore()))
	dgs := merkledag.NewDAGService(bserv.New(bs, nil))

	leafCid := rawCid(t, []byte("chunk"))
	leaf, err := blocks.NewBlockWithCid([]byte("chunk"), leafCid)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Put(ctx, leaf); err != nil {
		t.Fatal(err)
	}

	root := leafCid
	for range 20 {
		nd := &merkledag.ProtoNode{}
		nd.SetLinks([]*ipld.Link{
			{Cid: root, Name: "a", Size: 5},
			{Cid: root, Name: "b", Size: 5},
		})
		if err := bs.Put(ctx, nd); err != nil {
			t.Fatal(err)
		}
		root = nd.Cid()
	}

	leaves := make([]cid.Cid, 0, 16)
	if err := collectLeaves(ctx, dgs, root, &leaves); err == nil {
		t.Fatalf("2^20 leaf paths accepted, got %d leaves", len(leaves))
	}
	if len(leaves) > maxFileChunks {
		t.Errorf("budget exceeded: %d leaves", len(leaves))
	}
}

// A plain tree under the budget must be untouched: leaves in order, no error.
func TestCollectLeavesPlainTreeUnaffected(t *testing.T) {
	ctx := context.Background()
	bs := blockstore.NewBlockstore(dssync.MutexWrap(ds.NewMapDatastore()))
	dgs := merkledag.NewDAGService(bserv.New(bs, nil))

	var want []cid.Cid
	var links []*ipld.Link
	for _, name := range []string{"a", "b", "c"} {
		b, err := blocks.NewBlockWithCid([]byte(name), rawCid(t, []byte(name)))
		if err != nil {
			t.Fatal(err)
		}
		if err := bs.Put(ctx, b); err != nil {
			t.Fatal(err)
		}
		want = append(want, b.Cid())
		links = append(links, &ipld.Link{Cid: b.Cid(), Name: name, Size: 1})
	}
	root := &merkledag.ProtoNode{}
	root.SetLinks(links)
	if err := bs.Put(ctx, root); err != nil {
		t.Fatal(err)
	}

	var got []cid.Cid
	if err := collectLeaves(ctx, dgs, root.Cid(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d leaves, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].Equals(want[i]) {
			t.Errorf("leaf %d: got %s want %s", i, got[i], want[i])
		}
	}
}

func rawCid(t *testing.T, data []byte) cid.Cid {
	t.Helper()
	sum, err := mh.Sum(data, mh.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, sum)
}
