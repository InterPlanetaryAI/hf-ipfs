# AGENTS.md

Operational notes for coding agents working on `hf-ipfs`. The README explains the
design; this file records the **invariants, traps and verification habits** that
are not obvious from the code and that are expensive to relearn.

`hf-ipfs` is a single self-contained Go binary (`github.com/ipai/hf-ipfs`,
Go 1.27.1) bridging the Hugging Face local cache and IPFS. It embeds its own
libp2p host, kad-dht and `boxo/filestore`. **There is no Kubo.** Never introduce
a dependency on an external IPFS daemon.

## Commands

```bash
go build -o hf-ipfs .        # build
go test ./...                # unit + in-process integration (~30s)
go test -short ./...         # unit only; skips the bridge test
gofmt -l .                   # must print nothing
go vet ./...                 # must print nothing
```

**CI does not run `go test`, `gofmt` or `go vet`.** It only builds Docker images
and cuts releases. Nothing will catch a broken build or unformatted code for you —
run them yourself before committing.

Release builds inject the version with
`go build -trimpath -ldflags "-s -w -X main.version=$ver" -o hf-ipfs .`;
`main.version` defaults to `0.1.0` in `main.go`.

If `go` is not on PATH on this workstation: `/tmp/state/sdk/go1.27.1/bin` with
`GOPATH=/tmp/state/go`.

## Commits

`release-please` (`release-type: go`) runs on `main` and derives the version and
changelog from commit messages. **Use Conventional Commits.**

- Types in use: `feat`, `fix`, `docs`, `test`, `chore`, `build`, `ci` — scoped
  (`feat(pull):`, `fix(hfcache):`, …).
- One logical change per commit, whole files. Do **not** split hunks of a single
  file across commits.
- When a file unavoidably carries a secondary change, title the commit by the
  dominant change and name the rest in the body.
- Bodies explain *why* — the constraint or bug that forced the change, not what
  the diff says.

## The `hf` CLI cache is a hard contract

Compatibility with the real `hf` CLI is the project's core correctness claim, and
it is **verified differentially, not asserted**. The reference implementation is
available on this machine (`hf`, v1.29.0).

For `trees/<commit>.json`, byte-identity is required:

| Property | Value |
| --- | --- |
| Indent | one space |
| Trailing newline | **none** |
| File mode | `0600` |
| Keys | sorted |
| Envelope | `{"format_version": 1, "files": {...}}` |

Prove format changes against the real CLI:

```bash
HF_HOME=/tmp/hfref hf download <repo>
./hf-ipfs pull <repo> --from hf --repo /tmp/a/repo --hf-hub /tmp/a/hub
cmp /tmp/hfref/hub/models--<org>--<name>/trees/<commit>.json \
    /tmp/a/hub/models--<org>--<name>/trees/<commit>.json
```

`TestTreeByteIdenticalToHFCLI` is the committed version of that proof. Its
fixtures in `internal/pull/testdata/` are **verbatim live Hub API output plus the
real `hf`-written tree**. Regenerate them only from real `hf` / live API sources.
Regenerating a golden fixture from our own output makes the test vacuous.

Do not write tests that assert our output matches our own expectation of itself.
When a claim is about matching an external system, get the external system.

### Two fields cannot be inferred from our own hashes

- **`lfs_sha256` / `lfs_size`** belong only to LFS/Xet-backed files. We SHA-256
  *every* file we touch, so a content hash says nothing about LFS-ness. The
  manifest carries an explicit `FileManifest.LFS` flag, set from the Hub's `lfs`
  object. Stamping plain git blobs with their content hash produces a tree `hf`
  never wrote.
- **`xet_hash`** comes only from `/api/<type>/<repo>/tree/<rev>?recursive=true`.
  The revision endpoint omits it entirely. It is carried in the manifest so a
  seeder can hand it to pullers instead of each re-querying the Hub.

`WriteTree` **merges over** an existing record rather than replacing it: a
manifest that never carried a Xet hash must not silently strip one `hf` already
recorded. `EnrichFromTree` is the mirror image — it recovers LFS/Xet metadata
for a manifest built by walking a symlink tree, so a repo first downloaded by
`hf` propagates at full fidelity over P2P.

### Dotfiles are content; bookkeeping directories are not

`isInsideBookkeeping` matches a path only when it *equals* `.git`/`.cache` or
lies *beneath* it. A bare `HasPrefix(rel, ".git")` swallowed `.gitattributes`
(and would swallow `.gitignore`, `.gitkeep`, `.github/…`).

That was not cosmetic: the file never entered the ingest DAG, so it was absent
from the mapping and never transmitted to anyone. Every p2p share was silently
missing a file and a blob. `TestReadSnapshotKeepsDotfilesSkipsBookkeeping` is
the guard — touch the snapshot filter and that test is the thing to run.

### Changing the DAG changes `actual_cid`

Chunk size, `RawLeaves`, `balanced.Layout`, `Maxlinks`, or the file set all move
the root CID. Adding a file (as the dotfile fix did) means existing seeded repos
need a re-ingest to pick it up, and old/new copies of the same commit will not
share chunks at the root.

Provider lookups key on the commit-derived dummy CID, not `actual_cid`, so
mixed-version peers still interoperate: each seeder serves its own consistent
manifest + CID pair and content is verified regardless. Still, treat the DAG
knobs in `addFileNocopy` as load-bearing.

Nocopy adds require **absolute** source paths — that is what `absFile` exists for.
Never let a relative path reach the importer.

## Storage and process model

- **Single writer.** bbolt admits one writer process; the daemon owns the repo via
  flock for its lifetime. `pull`, `ingest`, `list` and `status` proxy to a running
  daemon over the Unix control socket and only build their own embedded node when
  no daemon is listening.
- **The control socket must be `0600`.** Go creates Unix sockets `0777 & ~umask`,
  which under a normal umask of `022` leaves them connectable by any local user —
  unacceptable because a `pull` can carry an HF access token across it.
- **Verify, then rename.** Both LFS and plain-git blobs land in a temp file and are
  renamed into place only after verification, so a partial blob never appears at
  its final path where the filestore and snapshot links would take it on faith.
  Plain git files use git's own check: SHA-1 over `"blob <size>\0" + content`
  must equal `blobId` — which is what confirms `blobId` is the correct blob
  filename.
- **Crash window.** Filestore references are registered as chunks arrive, before
  the blob is renamed. Failures roll references back; a hard crash is recovered by
  re-running the pull.

## Secrets

The HF token travels **only** as an `Authorization: Bearer` header. Never in a
URL, and never echoed into an error message — both URLs and errors get logged.
`TestErrorsDoNotLeakToken` and `TestTokenStatusHidesSecret` guard this; keep them
honest when touching auth.

Resolution order: `HF_TOKEN` wins over the legacy `HUGGING_FACE_HUB_TOKEN`; a
per-request `--hf-token` overrides the daemon's configured token, so a gated
download never requires restarting the daemon. Prefer the env var over the flag —
argv is world-readable through `ps`. The banner masks the value (`token=set (…9876)`).

## Networking

- **No whole-request timeout on the streaming HTTP client.** A fixed deadline
  kills every multi-gigabyte safetensors shard. Liveness comes from the caller's
  context plus per-connection dial, TLS and response-header timeouts. The
  metadata `Client` may have a request timeout; the `Stream` client must not.
- **The routing table must fill before announcing or querying.** libp2p hands
  freshly dialed peers to the DHT asynchronously, and a cold bootstrap takes
  seconds. `WaitForRoutingTable` gates both paths. A node with `routing table 0`
  announces nothing and resolves nothing — the banner reports it explicitly
  instead of silently announcing into the void.
- **Never bind to loopback while sharing.** The DHT hands `127.0.0.1` out to the
  whole network, so every transparent pull of anything that node seeds fails for
  everyone else. `BoundLoopbackOnly()` warns at startup. For local two-node
  testing use `--isolated` (private DHT, no bootstrap peers).
- **Bulk over relay is refused by default.** Circuit v2 relays are a signalling
  path, not a CDN, and carrying a 40 GiB shard through a public relay is somebody
  else's bandwidth bill. `handleMap` refuses the mapping query for relayed
  requesters (`Conn().Stat().Limited`) so the puller gets a readable error and
  moves to the next candidate; `handleBlocks` resets as defence in depth. Direct
  and hole-punched connections are never `Limited`.
- **Hole punching does not make you a server.** A punched connection is dialable
  only by the peer that punched it; AutoNAT still reports `Private` and kad-dht
  will not advertise the node as a server peer. It is additive to bootstrap,
  never a substitute for reachability.
- **Fixed listen port 4008**, not `tcp/0`. An ephemeral port cannot be
  port-forwarded, which silently disqualifies a node from being dialable by
  strangers. 4008 avoids Kubo's 4001.
- **AutoNAT's verdict lands tens of seconds after boot.** The banner deliberately
  does not block on it — blocking produced a confidently wrong line. The real
  answer arrives later on `ReachabilityChanged`.

## Transport

Blocks move over `/hf-ipfs/block/1.0.0`, fetched from the peer that already
answered `/hf-ipfs/map/1.0.0`. This keeps the node self-contained and the
transfer deterministic, but it also means **other IPFS implementations cannot
Bitswap from a `hf-ipfs` peer.** Swapping in `boxo/bitswap` is the natural next
step if wide interop is ever wanted; until then, do not describe this node as an
IPFS peer.

## Do not commit

`.cache/` (a local HF hub created by test runs) and `.hf-ipfs/` (bbolt DB, lock,
and the **libp2p Ed25519 private key**). Both are gitignored — keep them out of
`git add -A` sweeps.

For Docker development, host directories are mounted at the **same absolute paths**
the container's defaults resolve to, so filestore records stay valid on both host
and container. Changing either side's path breaks nocopy sharing.

## Working style

- Investigate before editing. Read the section, not a snippet. Match the existing
  pattern — a second convention beside an existing one is worse than either.
- Run `lsp references` before changing an exported symbol; a missed callsite is a
  bug.
- Fix the cause, not the symptom. Do not special-case input or suppress a warning
  to make a check pass.
- Prefer updating an existing file over adding a new one.
- Verify by running the thing. For a format-compatibility claim, that means a
  `cmp` against the real `hf` CLI, not a unit test of our own bytes.
