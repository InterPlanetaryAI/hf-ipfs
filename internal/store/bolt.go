// Package store wraps bbolt as a `go-datastore` Batching implementation and
// provides the single-writer advisory lock that guards the hf-ipfs repo.
//
// One bbolt file holds three logical buckets:
//
//	blocks    -> CID -> raw dag-pb / intermediate block bytes
//	filestore -> multihash -> DataObj{path, offset, size} (nocopy refs)
//	map       -> HF commit hash -> mapping JSON
//
// bbolt allows a single process to hold the file open, so every hf-ipfs
// command takes an exclusive advisory flock on <repo>/hf-ipfs.lock for as long
// as it owns the store. The daemon exposes a Unix control socket so that
// `hf-ipfs pull` keeps working while the daemon is running.
package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	bolt "go.etcd.io/bbolt"
)

// Bucket names inside the shared bbolt file.
var (
	BucketBlocks    = []byte("blocks")
	BucketFilestore = []byte("filestore")
	BucketMap       = []byte("map")
)

// Store owns the bbolt database file.
type Store struct {
	db   *bolt.DB
	path string
}

// Open creates (if needed) and opens the bbolt backing file.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o644, &bolt.Options{Timeout: 0})
	if err != nil {
		return nil, fmt.Errorf("open bolt db %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{BucketBlocks, BucketFilestore, BucketMap} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialise bolt buckets: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

// Path reports the backing file path.
func (s *Store) Path() string { return s.path }

// Close flushes and closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DS returns a datastore view over one bbolt bucket.
func (s *Store) DS(bucket string) ds.Batching {
	return &boltDS{db: s.db, bucket: []byte(bucket)}
}

// boltDS adapts bbolt to ds.Batching.
type boltDS struct {
	db     *bolt.DB
	bucket []byte
}

func (d *boltDS) raw(k ds.Key) []byte {
	return []byte(strings.TrimPrefix(k.String(), "/"))
}

func (d *boltDS) Get(ctx context.Context, k ds.Key) ([]byte, error) {
	var out []byte
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(d.bucket)
		if b == nil {
			return ds.ErrNotFound
		}
		v := b.Get(d.raw(k))
		if v == nil {
			return ds.ErrNotFound
		}
		out = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (d *boltDS) Has(ctx context.Context, k ds.Key) (bool, error) {
	var found bool
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(d.bucket)
		if b == nil {
			return nil
		}
		found = b.Get(d.raw(k)) != nil
		return nil
	})
	return found, err
}

func (d *boltDS) GetSize(ctx context.Context, k ds.Key) (int, error) {
	size := -1
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(d.bucket)
		if b == nil {
			return ds.ErrNotFound
		}
		v := b.Get(d.raw(k))
		if v == nil {
			return ds.ErrNotFound
		}
		size = len(v)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

func (d *boltDS) Put(ctx context.Context, k ds.Key, v []byte) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(d.bucket)
		if err != nil {
			return err
		}
		return b.Put(d.raw(k), v)
	})
}

func (d *boltDS) Delete(ctx context.Context, k ds.Key) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(d.bucket)
		if b == nil {
			return nil
		}
		return b.Delete(d.raw(k))
	})
}

// Query implements ds.Query. Entries are produced in bbolt (lexicographic key)
// order and then passed through the standard naive query pipeline so that
// prefixes, filters, orders, offsets and limits all behave as callers expect.
func (d *boltDS) Query(ctx context.Context, q dsq.Query) (dsq.Results, error) {
	entries := make([]dsq.Entry, 0, 64)
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(d.bucket)
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			e := dsq.Entry{Key: "/" + string(k), Size: len(v)}
			if !q.KeysOnly {
				e.Value = append([]byte(nil), v...)
			}
			entries = append(entries, e)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return dsq.NaiveQueryApply(q, dsq.ResultsWithEntries(q, entries)), nil
}

// Sync is a no-op: bbolt commits are durable by the time Put returns.
func (d *boltDS) Sync(ctx context.Context, prefix ds.Key) error { return nil }

// Close is a no-op; the *Store owns the file lifecycle.
func (d *boltDS) Close() error { return nil }

// Batch implements ds.Batching.
func (d *boltDS) Batch(ctx context.Context) (ds.Batch, error) {
	return &boltBatch{ds: d}, nil
}

type boltBatch struct {
	ds      *boltDS
	puts    []kv
	deletes []ds.Key
}

type kv struct {
	key   ds.Key
	value []byte
}

func (b *boltBatch) Put(ctx context.Context, k ds.Key, v []byte) error {
	b.puts = append(b.puts, kv{key: k, value: append([]byte(nil), v...)})
	return nil
}

func (b *boltBatch) Delete(ctx context.Context, k ds.Key) error {
	b.deletes = append(b.deletes, k)
	return nil
}

func (b *boltBatch) Commit(ctx context.Context) error {
	return b.ds.db.Update(func(tx *bolt.Tx) error {
		bkt, err := tx.CreateBucketIfNotExists(b.ds.bucket)
		if err != nil {
			return err
		}
		for _, p := range b.puts {
			if err := bkt.Put(b.ds.raw(p.key), p.value); err != nil {
				return err
			}
		}
		for _, k := range b.deletes {
			if err := bkt.Delete(b.ds.raw(k)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Lock is an advisory exclusive flock over the repo directory.
type Lock struct {
	file *os.File
}

// AcquireLock takes an exclusive non-blocking flock on path. The caller owns the
// returned Lock until Close.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another hf-ipfs process holds %s: %w", path, err)
	}
	return &Lock{file: f}, nil
}

// Close releases the lock.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
