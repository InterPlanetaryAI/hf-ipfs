# Running hf-ipfs in Docker

The image is a static Go binary on `alpine:3.21` — no runtime dependencies,
no sidecars. The daemon shares whatever is in the Hugging Face cache you mount;
the CLI lives in the same image, so one image covers seeding and pulling.

## The one rule: mount paths must match

`hf-ipfs` uses IPFS Filestore: the node stores **absolute paths** into
`~/.cache/huggingface/hub/.../blobs/`, recorded at ingest time. A path
recorded by one process must resolve identically for every later one, or the
shared blocks read as missing files.

Inside the container, the defaults resolve to `/root/.hf-ipfs` and
`/root/.cache/huggingface/hub`. So you bind-mount the **real host
directories at those exact same absolute paths**:

```
${HOME}/.hf-ipfs            →  /root/.hf-ipfs              # identity, bbolt DB, control socket
${HOME}/.cache/huggingface  →  /root/.cache/huggingface    # the real HF cache (the shared bytes)
```

Same path on both sides means the host `hf` CLI and the container agree on
every reference: the container seeds what `hf` downloaded, and files the
container pulled are already where the host expects them. Do **not** remap
to different container paths (`/data`, `/cache`, …) — the filestore records
would be valid in only one of the two worlds.

## Pull the published image

Versioned images are published when a release is published; `:main` is rebuilt on every push to `main`:

```bash
docker pull ghcr.io/<owner>/hf-ipfs:latest   # newest tag
docker pull ghcr.io/<owner>/hf-ipfs:main     # newest main
```

Tags: `X.Y.Z`, `X.Y`, `X` and `latest` per release (multi-arch: `linux/amd64`,
`linux/arm64`), `main` on the branch, `pr-N` for maintainer-approved PRs.
Public repo: no login needed. Private repo: `docker login ghcr.io`
(username = your GitHub handle, PAT with `read:packages`) or `gh auth docker`.

## Build locally

```bash
docker build -t hf-ipfs:local .
docker build -t hf-ipfs:dev --build-arg VERSION=v0.2.0-rc1 .   # stamps `hf-ipfs version`
```

Multi-stage: `golang:alpine` compiles with `CGO_ENABLED=0 -trimpath
-s -w`; the final layer is plain `alpine:3.21` plus `ca-certificates`.
`.dockerignore` keeps local state (`.hf-ipfs/`, `.ssh/`, the built binary)
out of the build context.

## docker compose (recommended)

`docker-compose.yml` in the repo does the whole thing:

```bash
docker compose up -d          # build + start the daemon
docker compose logs -f        # startup banner: peer id, swarm, reachability
docker compose down           # stop (state persists in the mounted dirs)
```

The daemon watches the mounted HF cache, ingests finalized downloads with
nocopy, announces on the public DHT, and serves peers on port 4008.

## Plain docker run

```bash
docker run -d --name hf-ipfs \
  -p 4008:4008 \
  -v "$HOME/.hf-ipfs:/root/.hf-ipfs" \
  -v "$HOME/.cache/huggingface:/root/.cache/huggingface" \
  --restart unless-stopped \
  hf-ipfs:local
```

Any CLI flag works as container args — the entrypoint is `hf-ipfs` itself:

```bash
docker run --rm \
  -v "$HOME/.hf-ipfs:/root/.hf-ipfs" \
  -v "$HOME/.cache/huggingface:/root/.cache/huggingface" \
  hf-ipfs:local daemon --listen /ip4/0.0.0.0/tcp/4101 --isolated
```

## CLI commands

Run them inside the container, where they find the daemon's control socket in
the mounted repo dir and proxy to it:

```bash
docker compose exec hf-ipfs status
docker compose exec hf-ipfs list
docker compose exec hf-ipfs pull meta-llama/Llama-2-7b --commit <sha>
```

A one-off `docker run` of the same image also proxies — same repo mount,
same `api.sock` — so ad-hoc pulls reuse the running daemon's swarm:

```bash
docker run --rm \
  -v "$HOME/.hf-ipfs:/root/.hf-ipfs" \
  -v "$HOME/.cache/huggingface:/root/.cache/huggingface" \
  hf-ipfs:local pull testorg/testmodel
```

The host CLI works against the containerized daemon too: the socket lives
inside the bind-mounted `~/.hf-ipfs`, so `hf-ipfs status` on the host
connects to the daemon in the container. Keep the two versions in sync —
the proxy speaks the daemon's CLI surface.

To stop the containerized daemon cleanly (flushes announcements, releases
the repo lock), prefer `shutdown` over `kill`:

```bash
docker compose exec hf-ipfs shutdown
```

## Tokens and secrets

Pass Hub credentials by environment, never in the image or a Compose
literal you commit:

```bash
HF_TOKEN=hf_xxx docker compose up -d          # compose inherits it from the shell
docker run -d -e HF_TOKEN ... hf-ipfs:local   # plain run
```

The token is used for Hub API resolution and `--from hf` fallback
downloads, sent only as `Authorization: Bearer`. The banner reports it
masked (`token=set (…xxxx)`). A `pull --hf-token` through the CLI overrides
what the daemon started with, so gated repos never need a daemon restart.

## Reachability

Port 4008 must be published (`-p 4008:4008` / compose `ports`) for the node
to accept incoming peers at all. Beyond that:

| Setup | Result |
| --- | --- |
| Bridge + published port, no router forward | AutoNAT reports `private`; peers reach you only via relays/hole-punch |
| Bridge + router port-forward 4008 → host | Dialable by strangers; the good default for a real seeder |
| `--network host` (Linux) | Simplest fully-dialable container: no NAT in the path at all |

UPnP/NAT-PMP from inside a bridge container generally cannot reach the
router (it needs the LAN segment), so don't count on `NATPortMap()` there —
use host networking or a manual forward. `--network host` also means `-p` is
ignored; the daemon binds 4008 on the host directly.

The startup banner and `status` report the verdict (`reach : public` vs
`private — NOT dialable by strangers`); check it before trusting a seedbox.

## Troubleshooting

* **`shared : 0 revision(s)` / blocks missing later** — the cache was
  ingested under a *different* absolute path (e.g. an earlier run mounted
  `/data`). The filestore references no longer resolve. Re-ingest with the
  correct mount (`hf-ipfs ingest <repo> --force`), or fix the mount.
* **`pull` says no daemon, but one is running** — the CLI isn't seeing the
  same repo mount; check `-v` paths and that `api.sock` exists inside the
  container at `/root/.hf-ipfs/api.sock`.
* **`bind: address already in use`** — the host (or another container) owns
  4008. Change the published port and `--listen` together so the advertised
  port matches what's actually reachable.
* **Container restarts loop** — usually the repo lock: a second daemon holds
  `~/.hf-ipfs`. Only one writer per repo dir; stop the other node.
