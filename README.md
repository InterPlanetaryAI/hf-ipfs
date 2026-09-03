# hf-ipfs

Part of **IPAI** (InterPlanetary Artificial Intelligence).

`hf-ipfs` is a single, self-contained Go binary that transparently bridges the
Hugging Face local cache and the IPFS network. It runs as a background sharing
daemon and as a CLI pulling tool, letting users share and pull multi-gigabyte
models peer-to-peer using standard Hugging Face repository names
(`meta-llama/Llama-2-7b`), so the hub's centralised bandwidth stops being the
bottleneck.

It embeds the IPFS/libp2p node **inside the binary**. No Kubo, no external
daemon, no sidecar service.

```
        ~/.cache/huggingface/hub/          ┌──────────────────────────┐
   ┌──────────────────────────────────┐   │  hf-ipfs (one binary)    │
   │ models--org--name/               │   │                          │
   │   blobs/<sha256>   <── the bytes │◄──┤ filestore: (path,offset) │
   │   snapshots/<commit>/file        │   │ kad-dht: dummy CID keys  │
   │     file -> ../../blobs/<sha256>│   │ /hf-ipfs/map/1.0.0       │
   └──────────────────────────────────┘   │ /hf-ipfs/block/1.0.0   │
                                          └───────────┬────────────┘
                                                      │ libp2p
                                          ┌───────────┴────────────┐
                                          │ other hf-ipfs peers    │
                                          └────────────────────────┘
```

## Why this works

Two systems that look unrelated are the same kind of system:

1. **Shared CAS.** The Hugging Face cache backend and IPFS are both
   content-addressed stores. `hf-ipfs` translates between them: it chunks HF
   flat-file blobs into IPFS Merkle DAGs and reconstructs them back into flat
   files on the receiving end.
2. **Zero-copy.** `hf-ipfs` never duplicates model bytes. It uses IPFS
   **Filestore**: the embedded node stores only block *metadata* — absolute path
   plus byte offset — pointing straight at files in
   `~/.cache/huggingface/hub/.../blobs/`.
3. **Pure P2P discovery.** Kademlia (`go-libp2p-kad-dht`) announces peer
   availability; native libp2p streams exchange mapping metadata and blocks. No
   central servers, no blockchain, no PubSub.

## Build

Requires Go 1.21+ (developed against 1.27).

```bash
git clone <this repo> && cd hf-ipfs
go build -o hf-ipfs .
./hf-ipfs version
```

## Quick start (two nodes, one laptop)

```bash
# Node A — share whatever is already in your HF cache
hf-ipfs daemon --listen /ip4/127.0.0.1/tcp/4101
#   peer id : 12D3KooWEB…
#   addr    : /ip4/127.0.0.1/tcp/4101/p2p/12D3KooWEB…

# Node B — pull instead of downloading from Hugging Face
hf-ipfs pull testorg/testmodel \
  --commit 607a30d783dfa663caf39e06633721c8d4cfcd7e \
  --peer /ip4/127.0.0.1/tcp/4101/p2p/12D3KooWEB… \
  --repo ~/.hf-ipfs-b --hf-hub ~/.cache/huggingface-b
```

`hf-ipfs` keeps its own state in `~/.hf-ipfs` (`--repo`) and reads/writes the
HF cache at `--hf-hub` (default `$HF_HUB_CACHE`, else `$HF_HOME/hub`, else
`~/.cache/huggingface/hub`).

## Commands

| Command | What it does |
| --- | --- |
| `daemon` | Watch the HF cache, ingest finalized downloads (nocopy), announce on the DHT, serve peers, expose a control socket |
| `pull <repo_id>` | Resolve → find providers → exchange mapping → stream and reconstruct blobs → relink the snapshot |
| `ingest <repo_id>` | Share one revision now (default: whatever `refs/main` points at) |
| `list` | Revisions this node shares |
| `status` | Daemon state and listen addresses |
| `id` | This node's persistent peer ID |
| `resolve <repo_id>` | Latest commit and its dummy CID, straight from the HF API |
| `shutdown` | Stop a running daemon |

Useful flags: `--commit`, `--ref`, `--repo-type model|dataset|space`,
`--peer <multiaddr>` (try this peer directly), `--connect <multiaddr>` (dial
only, to join the DHT), `--force`, `--chunk-size`, `--no-watch`,
`--dht-client`, `--rescan <dur>`, `--log-level`.

## How it works

### 1. The embedded node and the zero-copy datastore

`go-libp2p` provides the host; `go-libp2p-kad-dht` provides Kademlia. The
blockstore is `boxo/filestore`:

```go
mainBS := blockstore.NewBlockstore(store.DS("blocks"))
fm     := filestore.NewFileManager(store.DS("filestore"), cfg.HFHubDir)
fm.AllowFiles = true
fstore := filestore.NewFilestore(mainBS, fm, nil)
```

Leaf chunks become `FilestoreNode`s carrying `(FullPath, Offset)` into
`blobs/`. Intermediate dag-pb nodes are small and live in the normal blockstore.

**Dummy CID mapping.** A HF commit hash is 40 hex characters (a git SHA-1). To
key DHT provider records we need a CID, so the commit bytes are wrapped in an
**identity** multihash:

```
commit (40 hex)  <->  CIDv1{DagProtobuf, identity(20 bytes)}
```

The mapping is a bijection and needs no hash function: the DHT key itself
carries the commit hash, and identity is on IPFS' default multihash allowlist.

### 2. Daemon mode

* **File watcher** — `fsnotify` on `~/.cache/huggingface/hub/`. A download is
  finalized when `refs/main` names a commit whose `snapshots/<commit>/` tree
  exists and every entry resolves to a real blob. Events are debounced; a
  periodic `--rescan` is the correctness path if fsnotify misses anything.
* **Ingestion (nocopy)** — dereference `snapshots/<commit>/`, add each file with
  `RawLeaves: true, NoCopy: true` through the UnixFS importer, and wire the
  results into a directory DAG. That yields the `actual_cid`.
* **Local mapping database** — `{"commit_hash": actual_cid, files: [...]}` in
  bbolt.
* **DHT announcement** — `dht.Provide(ctx, dummy_cid, true)`, plus the actual
  CID. Announcements that had nowhere to go (empty routing table) are retried on
  each rescan.
* **Custom libp2p protocols**
  * `/hf-ipfs/map/1.0.0` — send a commit hash, get back the actual CID and the
    file manifest.
  * `/hf-ipfs/block/1.0.0` — batched block streaming. A request frame is
    `{"cids": [...]}`; the server answers with exactly one frame per CID, in
    order: `0x00 + bytes`, or `0x01` if absent. Order is preserved, so clients
    pipeline batches without request IDs.

### 3. Pull

* **A — resolve hash.** `GET <endpoint>/api/models/<repo_id>` → `sha`.
* **B — find providers.** commit → dummy multihash → `dht.FindProviders`.
* **C — stream exchange.** Connect, open `/hf-ipfs/map/1.0.0`, read the
  `actual_cid` and manifest.
* **D — zero-copy reconstruction.** Walk the DAG in batches of 64 chunks. As
  bytes stream in, SHA-256 them on the fly, write the flat file into `blobs/`,
  and register each chunk's new absolute path into the local `boxo/filestore` so
  the puller serves it to others without duplicating anything.
* **E — symlinking.** Recreate `snapshots/<commit>/` with relative symlinks into
  `blobs/` and update `refs/main`.

Blob naming is carried in the manifest rather than guessed: the sharing peer
reports the exact filename it found in `blobs/`, so reconstruction stays
compatible with the upstream `hf` CLI no matter which hash HF used to name a
given file. Content is then verified against the manifest's SHA-256 before the
file is renamed into place.

## Testing

```bash
go test ./...              # unit + in-process integration
go test -short ./...       # unit tests only
```

What the tests cover:

* `internal/dummy` — the commit ↔ dummy-CID bijection is deterministic,
  reversible, identity-coded, and rejects malformed input.
* `internal/store` — the bbolt `ds.Batching` adapter: CRUD, batches, prefix and
  keys-only queries, bucket isolation, and lock exclusivity.
* `internal/hfcache` — snapshot reading (blob names equal content SHA-256),
  finalize detection, and `WriteSnapshot` round-tripping the symlink layout.
* `integration_test.go` — the whole bridge in one process: a sharing node ingests
  a finalized revision with nocopy adds, a second node pulls it over libp2p, and
  the destination must reproduce the HF cache byte for byte, record the mapping,
  and serve every chunk back out of its own filestore.

A manual three-node run (share → pull → pull-from-puller) confirmed:

```
hubA 6.2M   repoA 67K     # source: ingested
hubB 6.2M   repoB 67K     # pulled from A, now a seeder
hubC 6.2M   repoC 67K     # pulled from B  (chained)
```

~1% of the model size in node storage, because the node stores references rather
than copies. All three hubs are byte-identical, and the `actual_cid` is identical
across all of them, confirming the DAG is deterministic.

## Design notes and limitations

* **Single writer.** bbolt admits one writer process, so the daemon owns the repo
  for its lifetime. `pull`, `ingest`, `list` and `status` transparently proxy to
  a running daemon over its Unix control socket (`<repo>/api.sock`) and only
  build their own embedded node when no daemon is listening.
* **Custom block transport, not Bitswap.** Blocks are fetched over
  `/hf-ipfs/block/1.0.0` from the peer that already answered the mapping query.
  This keeps the node self-contained and the transfer deterministic; it also means
  other IPFS implementations cannot fetch from a `hf-ipfs` peer via Bitswap.
  Swapping in `boxo/bitswap` (with the DHT as provider finder) is the natural
  next step if interop with the wider network is wanted.
* **Crash window during pull.** Filestore references are registered as chunks
  arrive, before the blob is renamed into place, so an interrupted pull can leave
  references to a file that does not exist yet. Failures roll the references back;
  a hard crash is recovered by re-running the pull.
* **Ref handling** covers `refs/main`. Other refs are written but not watched.
* **No HF auth.** Public repos only; `HF_ENDPOINT` is honoured for mirrors.

## Layout

```
main.go                      CLI dispatch, flags, daemon/proxy wiring
internal/config              env + flag resolution (HF_HOME, HF_HUB_CACHE, …)
internal/dummy               commit hash <-> identity-multihash CID
internal/store               bbolt as go-datastore Batching, plus the repo flock
internal/mapping             the commit -> actual_cid + manifest database
internal/identity            persistent libp2p Ed25519 key
internal/protoio             length-prefixed framing for streams and the socket
internal/wire                JSON message types
internal/hfcache             the `hf` CLI cache layout: read, verify, relink
internal/hfapi               minimal HF Hub API client
internal/node                host + DHT + filestore + DAG + protocol handlers
internal/ingest              nocopy add of a finalized snapshot
internal/pull                steps A–E of the pull pipeline
internal/watch               fsnotify + rescan finalize detection
internal/controls            daemon Unix-socket control API and CLI proxy
```
