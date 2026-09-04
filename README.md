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
| `pull <repo_id>` | Resolve → find providers → exchange mapping → stream and reconstruct blobs → relink the snapshot; falls back to Hugging Face when the swarm cannot serve |
| `ingest <repo_id>` | Share one revision now (default: whatever `refs/main` points at) |
| `list` | Revisions this node shares |
| `status` | Daemon state and listen addresses |
| `id` | This node's persistent peer ID |
| `resolve <repo_id>` | Latest commit and its dummy CID, straight from the HF API |
| `shutdown` | Stop a running daemon |

Useful flags: `--commit`, `--ref`, `--repo-type model|dataset|space`,
`--from p2p|hf|p2p,hf` (where content may come from; default `p2p,hf`),
`--hf-token <token>` (credentials for gated/private repos; default `$HF_TOKEN`),
`--peer <multiaddr>` (try this peer directly), `--connect <multiaddr>` (dial
only, to join the DHT), `--bootstrap <multiaddr>` (replace the default
bootstrappers), `--isolated` (private DHT, no bootstrappers), `--force`,
`--chunk-size`, `--no-watch`, `--dht-client`, `--rescan <dur>`, `--log-level`,
and for reachability: `--listen <ma>`, `--no-relay`, `--no-hole-punch`,
`--no-nat-portmap`, `--relay-bulk`.

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
  CID. The daemon waits for the routing table to fill first; announcements that
  still had nowhere to go are retried on each rescan.
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
  `blobs/`, write `trees/<commit>.json`, and update `refs/main`.

Blob naming is carried in the manifest rather than guessed: the sharing peer
reports the exact filename it found in `blobs/`, so reconstruction stays
compatible with the upstream `hf` CLI no matter which hash HF used to name a
given file. Content is then verified against the manifest's SHA-256 before the
file is renamed into place.


### `trees/<commit>.json`

The `hf` CLI records per-file metadata for each revision in
`trees/<commit>.json`, and `hf-ipfs` writes it too, byte-for-byte identically:
1-space indent, no trailing newline, mode `0600`, keys sorted.

```json
{
 "format_version": 1,
 "files": {
  "config.json": {
   "size": 3917,
   "blob_id": "8e95fad94e16745e1e15701fe2596058ca25c0d4"
  },
  "model.safetensors": {
   "size": 1626451404,
   "blob_id": "f303a51187cdc9d0d003880e8eac8805972af58a",
   "lfs_sha256": "b85fdb50b7d6123a967d5ee4a505e222baff8d2f7ad6bbf353578c1a61dfbac9",
   "lfs_size": 1626451404,
   "xet_hash": "64b4eafe68248256b27c108654f9afeb6c52bcfc6e3c26c96ea20bdd02e9f3cf"
  }
 }
}
```

The `lfs_*` and `xet_hash` fields appear **only** for LFS/Xet-backed files; a
plain git blob carries just `size` and `blob_id`. That distinction cannot be
inferred from a content hash — `hf-ipfs` SHA-256s every file it touches — so
it rides on the manifest as an explicit `LFS` flag, set from the Hub's `lfs`
object. `xet_hash` comes from the tree endpoint (`/api/<type>/<repo>/tree/<rev>`),
the only place the Hub exposes it, and is carried in the manifest so seeders
can hand it to pullers without each of them re-querying the API.

When a repo was first downloaded by `hf`, ingest reads that existing tree and
recovers the LFS/Xet metadata the symlink walk cannot see, so a cache touched
by `hf` propagates at full fidelity over P2P. Re-writing a tree merges over the
existing record rather than replacing it: a manifest that never carried a Xet
hash cannot silently strip one the `hf` CLI already recorded.

Dotfiles are revision content. `.gitattributes` ships with essentially every
HF repo, and a filter matching a bare `.git` prefix used to drop it during
ingest — so it never entered the DAG, no puller ever received it, and a p2p
copy silently differed from the `hf` CLI's by one file and one blob. Only
the *contents* of `.git/` and `.cache/` are plumbing; `.gitignore`,
`.gitkeep` and `.github/…` are content too.

**Falling back to Hugging Face.** A missing seeder should not fail a download,
and `--from` decides where content may come from:

| `--from` | Behaviour |
| --- | --- |
| `p2p,hf` *(default)* | Try the swarm; if nothing can serve it, download from the Hub and ingest locally |
| `p2p` | Swarm only — fails when no provider can serve the repo, the original behaviour |
| `hf` | Skip the swarm entirely; download straight from the Hub |

The fallback is not a dead end. After downloading, the revision goes through the
same nocopy ingest a daemon uses, so the puller announces it on the DHT and
becomes a seeder: a fallback pull lands in exactly the same cache and mapping
state as a p2p one.

Integrity is established per file class, because the Hub tells us different
things about each:

| File class | Check |
| --- | --- |
| LFS object | SHA-256 of the bytes against `lfs.sha256` from the API |
| Plain git file | SHA-1 over `"blob <size>\0" + content` must equal `blobId` |

The second is the check git itself performs, and it is what confirms `blobId` is
the correct blob filename — which is why a fallback download is byte-compatible
with what the `hf` CLI would have produced. Both classes land in a temp file and
are renamed into place only after verification, so a partial blob never appears
at its final path where the filestore and snapshot links would take it on faith.

Fallback downloads stream through a client with **no whole-request timeout**: a
fixed deadline would kill every multi-gigabyte shard. Liveness comes from the
caller's context plus per-connection dial, TLS and response-header timeouts.

### 4. Swarm membership

By default the daemon joins the **public IPFS DHT** using the canonical
libp2p bootstrappers (`dht.GetDefaultBootstrapPeerAddrInfos`). Three ways to
control it:

| Invocation | Result |
| --- | --- |
| *(no flags)* | public swarm via libp2p defaults |
| `--bootstrap <ma>` (repeatable) | public swarm via **your** list (replaces the defaults) |
| `--isolated` | private DHT, no bootstrap peers — two local nodes only |

The startup banner reports what actually happened:

```
  dht     : server=true bootstrap=libp2p defaults
  nat     : portmap=on relay=on holepunch=on bulk-over-relay=off
  swarm   : routing table 171 peer(s)
  reach   : probing (AutoNAT)…
  shared  : re-announced 7 revision(s)
  running — Ctrl-C to stop
  reach   : private — NOT dialable by strangers; NAT-PMP/UPnP did not map
            the port; port it forward on the gateway
```

`swarm : routing table 0 peer(s)` means the node never reached anyone, so
provider records can be neither announced nor resolved. The daemon prints an
explicit warning in that case rather than silently announcing nothing.
`hf-ipfs status` shows the same number for a running daemon.

**A routing table is not optional.** Two races used to make sharing silently
do nothing, and both are now handled by waiting for the table to fill before
announcing or querying:

* libp2p hands freshly dialed peers to the DHT asynchronously, so a
  `--connect`-joined node would query before it knew anyone.
* A cold-start bootstrap takes several seconds, so a DHT-only `pull` returned
  `step B: no peers found` instantly instead of waiting.

**Being dialable: AutoNAT, relays and hole punching.** The bound address only
says what we asked for. AutoNATv2 is what tells us whether the internet
agrees, and the daemon reports that verdict: `probing…` at startup, then the
real answer the moment AutoNAT delivers it. The verdict typically lands tens
of seconds after boot, so the banner deliberately does not block on it —
blocking just produced a confidently wrong line.

On by default: `libp2p.EnableAutoNATv2()`, `libp2p.EnableRelay()`
(circuit v2), `libp2p.EnableHolePunching()` (DCUtR) and
`libp2p.NATPortMap()` (UPnP/NAT-PMP). Override with `--no-relay`,
`--no-hole-punch`, `--no-nat-portmap`. All four are skipped for
`--isolated` nodes, which have no swarm to probe against.

Ranked by how much a seeder should trust each mechanism:

| # | Mechanism | Verdict |
| --- | --- | --- |
| 1 | Fixed port + router port-forward | The only thing that reliably makes you dialable by strangers |
| 2 | `NATPortMap()` (UPnP/NAT-PMP) | Same effect, automatic; needs a cooperative router |
| 3 | DCUtR + public relays | Best-effort; great for the puller side, partial for the seeder |
| 4 | Relay-carried bulk | Works, slow, costs someone else bandwidth — refused by default |

Two of these bear directly on the transfer:

* **Hole punching does not make you a server.** A punched connection is
  dialable only by the peer that punched it. AutoNAT still reports
  `Private`, and kad-dht will not advertise the node as a server peer.
  Punching is additive to bootstrap, never a substitute for reachability.
* **Bulk over relay is refused by default.** Circuit v2 relays are a
  signalling path, not a CDN, and carrying a 40 GiB safetensors shard
  through a public relay is somebody else's bandwidth bill. `handleMap`
  refuses the mapping query for relayed requesters (`Conn().Stat().Limited`)
  so the puller gets a readable error and moves to the next candidate, and
  `handleBlocks` resets as defence in depth. `--relay-bulk` opts back in.
  Direct and hole-punched connections are never `Limited`, so they are
  unaffected.

**Fixed port.** The default listen address is `/ip4/0.0.0.0/tcp/4008` —
fixed rather than `tcp/0` on purpose, because an ephemeral port cannot be
port-forwarded and so silently disqualifies a node from option 1. 4008 is
chosen to avoid colliding with Kubo's 4001. Override with
`--listen /ip4/0.0.0.0/tcp/<port>`.

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
* `internal/pull` — source selection (`--from` parsing, aliases, rejection of
  typos) and blob verification: a wrong git blob id, a wrong LFS SHA-256 and a
  truncated body must each be rejected without publishing anything at the
  blob's final path, and an already-cached blob must short-circuit without
  touching the network.

* `internal/pull` — golden `hf` compatibility: the generated
  `trees/<commit>.json` is asserted byte-identical to the file the real `hf`
  CLI produced for the same commit, using verbatim live Hub API fixtures. Also
  covers the LFS/plain-git-blob field split, the no-downgrade merge rule, and
  `EnrichFromTree` recovering metadata a symlink walk cannot see.

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
* **HF auth.** `--hf-token` and `$HF_TOKEN` authenticate Hub API and fallback
  download requests, which is what unlocks gated and private repos.
  `$HUGGING_FACE_HUB_TOKEN` is honoured as a fallback for the older name, with
  `$HF_TOKEN` winning when both are set. A `--hf-token` on a `pull` overrides
  whatever the daemon was started with, so a gated download never requires
  restarting it. The token goes out only as an `Authorization: Bearer` header —
  never in a URL — and is never echoed into an error message, since both URLs
  and errors get logged. The daemon banner reports it masked: `token=set (…9876)`.
  Prefer the env var over the flag: argv is world-readable through `ps`.
* **Control socket is owner-only.** Because a `pull` can carry a token to the
  daemon, the control socket is chmod'd `0600`. Go creates Unix sockets
  `0777 & ~umask`, which under a normal umask of `022` leaves them
  connectable by any local user.

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
internal/pull                steps A–E of the pull pipeline + HF fallback
internal/watch               fsnotify + rescan finalize detection
internal/controls            daemon Unix-socket control API and CLI proxy
```
