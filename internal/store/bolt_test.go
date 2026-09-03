package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
)

func openTestDS(t *testing.T) (ds.Batching, *Store) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st.DS("blocks"), st
}

func TestPutGetHasGetSizeDelete(t *testing.T) {
	ctx := context.Background()
	d, _ := openTestDS(t)
	k := ds.NewKey("/abc/def")

	if ok, err := d.Has(ctx, k); err != nil || ok {
		t.Fatalf("Has on empty store = (%v, %v), want (false, nil)", ok, err)
	}
	if _, err := d.Get(ctx, k); !errors.Is(err, ds.ErrNotFound) {
		t.Fatalf("Get on missing key = %v, want ErrNotFound", err)
	}

	if err := d.Put(ctx, k, []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := d.Get(ctx, k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("Get = %q, want %q", got, "payload")
	}
	if ok, err := d.Has(ctx, k); err != nil || !ok {
		t.Errorf("Has = (%v, %v), want (true, nil)", ok, err)
	}
	if size, err := d.GetSize(ctx, k); err != nil || size != 7 {
		t.Errorf("GetSize = (%d, %v), want (7, nil)", size, err)
	}
	if err := d.Delete(ctx, k); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := d.Has(ctx, k); ok {
		t.Error("key still present after Delete")
	}
}

func TestBatchCommit(t *testing.T) {
	ctx := context.Background()
	d, _ := openTestDS(t)

	if err := d.Put(ctx, ds.NewKey("/keep"), []byte("1")); err != nil {
		t.Fatal(err)
	}

	b, err := d.Batch(ctx)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if err := b.Put(ctx, ds.NewKey("/a"), []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, ds.NewKey("/b"), []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, ds.NewKey("/keep")); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for _, k := range []string{"/a", "/b"} {
		if ok, _ := d.Has(ctx, ds.NewKey(k)); !ok {
			t.Errorf("batched put missing %s", k)
		}
	}
	if ok, _ := d.Has(ctx, ds.NewKey("/keep")); ok {
		t.Error("batched delete did not remove /keep")
	}
}

func TestUncommittedBatchWritesNothing(t *testing.T) {
	ctx := context.Background()
	d, _ := openTestDS(t)

	b, err := d.Batch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, ds.NewKey("/pending"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	// No Commit: the operation must not be visible.
	if ok, _ := d.Has(ctx, ds.NewKey("/pending")); ok {
		t.Error("uncommitted batch already wrote /pending")
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if ok, _ := d.Has(ctx, ds.NewKey("/pending")); !ok {
		t.Error("/pending missing after Commit")
	}
}
func TestQueryPrefixAndKeysOnly(t *testing.T) {
	ctx := context.Background()
	d, _ := openTestDS(t)

	for _, kv := range []struct{ k, v string }{
		{"/blocks/one", "1"},
		{"/blocks/two", "2"},
		{"/other/three", "3"},
	} {
		if err := d.Put(ctx, ds.NewKey(kv.k), []byte(kv.v)); err != nil {
			t.Fatal(err)
		}
	}

	res, err := d.Query(ctx, dsq.Query{Prefix: "blocks", KeysOnly: true})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer res.Close()

	var keys []string
	for r := range res.Next() {
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		if len(r.Value) != 0 {
			t.Errorf("KeysOnly query returned a value for %s", r.Key)
		}
		keys = append(keys, r.Key)
	}
	if len(keys) != 2 {
		t.Fatalf("prefix query returned %d keys (%v), want 2", len(keys), keys)
	}
	for _, k := range keys {
		if filepath.Base(k) == "three" {
			t.Errorf("prefix leaked unrelated key %s", k)
		}
	}
}

func TestQueryAllReturnsValues(t *testing.T) {
	ctx := context.Background()
	d, _ := openTestDS(t)
	if err := d.Put(ctx, ds.NewKey("/x"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	res, err := d.Query(ctx, dsq.Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()

	entries, err := res.Rest()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || string(entries[0].Value) != "value" {
		t.Fatalf("entries = %+v, want one entry with value", entries)
	}
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hf-ipfs.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if _, err := AcquireLock(path); err == nil {
		t.Fatal("second AcquireLock should fail while the first is held")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}
}

func TestBucketsAreIsolated(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "iso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := st.DS("blocks")
	b := st.DS("map")
	k := ds.NewKey("/same")

	if err := a.Put(ctx, k, []byte("in-blocks")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Has(ctx, k); ok {
		t.Error("key leaked between buckets")
	}
	if err := b.Put(ctx, k, []byte("in-map")); err != nil {
		t.Fatal(err)
	}
	got, err := a.Get(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "in-blocks" {
		t.Errorf("blocks bucket = %q, want %q", got, "in-blocks")
	}
}
