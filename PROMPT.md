You are an expert Distributed Systems and Go (Golang) developer. 

## Project Overview

Under the umbrella of a project named "IPAI" (InterPlanetary Artificial Intelligence), we are building the first building block: a single, standalone Go binary called `hf-ipfs` that transparently bridges the Hugging Face local cache and the IPFS network. It must act as both a background sharing daemon and a CLI pulling tool.

The goal is to allow users to seamlessly share and pull massive machine learning models peer-to-peer over IPFS using standard Hugging Face repository names (e.g., `meta-llama/Llama-2-7b`), drastically reducing reliance on Hugging Face's centralized bandwidth. 

Crucially, this tool must NOT rely on an external IPFS daemon (like Kubo). It must embed an IPFS/libp2p node directly within the Go binary.

## Core Premises
1. Shared CAS Architecture: Hugging Face’s cache backend and IPFS are both Content-Addressed Storage (CAS) systems. This tool translates between the two, chunking HF flat-file blobs into IPFS Merkle DAGs and reconstructing them back into flat-file blobs on the receiving end.
2. The tool must access and manage the hf cli filesystem (`~/.cache/huggingface`) transparently and in a fully compatible way with upstream hf cli.
3. Zero-Copy Storage (Filestore): To avoid duplicating massive multi-gigabyte models locally, the tool must use IPFS Filestore. Instead of copying raw bytes into an internal IPFS datastore, the embedded node must store only block metadata (path and byte offsets) pointing directly to the files inside `~/.cache/huggingface/hub/blobs/`.
4. Pure P2P Discovery via libp2p: We will use the IPFS Kademlia DHT (`go-libp2p-kad-dht`) to announce peer availability, and native `go-libp2p` streams to exchange mapping metadata, avoiding central servers, blockchains, or PubSub.

## Implementation Requirements

1. The Embedded Node & Zero-Copy Datastore
- Use `go-libp2p` and `go-libp2p-kad-dht` to instantiate an embedded IPFS node.
- Configure the node's blockstore using the `github.com/ipfs/boxo/filestore` package. This enables the "nocopy" feature where chunks refer to external absolute file paths rather than duplicating data.
- Dummy CID Mapping: Create a deterministic mapping from a 40-character HF commit hash to an IPFS-compatible multihash. Use this multihash to interact with the DHT.

2. The Daemon Mode (`hf-ipfs daemon`)
- File Watcher: Monitor `~/.cache/huggingface/hub/` (using a package like `fsnotify`). Detect when a model download is fully finalized (indicated by the creation of `snapshots/<commit_hash>/` symlinks and `refs/main`).
- Ingestion (Nocopy): Dereference the `snapshots/<commit_hash>/` directory. Add the files to the embedded IPFS node using the filestore abstraction so it registers the absolute paths (`Abspath`) of the files in `blobs/` without copying their contents. This yields the `actual_cid`.
- Local Mapping Database: Store the mapping `{"commit_hash": actual_cid}` in a local embedded key-value store (like BoltDB or a JSON file).
- DHT Announcement: Use `dht.Provide(ctx, dummy_cid, true)` to announce this node as a provider for the specific HF commit.
- Custom libp2p Protocol: Implement a custom libp2p stream handler (e.g., `/hf-ipfs/map/1.0.0`). When a peer connects and sends a commit hash, read from the local mapping DB and return the `actual_cid`.

3. The Pull CLI (`hf-ipfs pull <repo_id>`)
- Step A (Resolve Hash): Make a standard HTTP GET request to the Hugging Face Hub API to get the latest `commit_hash` for the provided `repo_id`.
- Step B (Find Providers): Generate the dummy multihash for the commit. Query the libp2p DHT (`dht.FindProviders`) to get a list of `peer.ID`s hosting the model.
- Step C (P2P Stream Exchange): Connect to a discovered peer. Open a stream using the `/hf-ipfs/map/1.0.0` protocol, send the commit hash, and read the `actual_cid` returned by the peer.
- Step D (Zero-Copy Reconstruction): Download the directory structure from the `actual_cid` over IPFS. As data streams in, compute the SHA256 hash on the fly, write the raw files directly into the local HF `blobs/` directory, and register these new absolute paths into the local `boxo/filestore` to serve them to others without duplication.
- Step E (Symlinking): Recreate the `snapshots/<commit_hash>/` directory layout by creating symlinks pointing to the respective files in `blobs/`. Update `refs/main` to point to the commit hash.

## Constraints for the PoC
- The binary must be entirely self-contained. Do not require users to install or run Kubo.
- Handle state persistence gracefully (e.g., saving the libp2p Ed25519 identity key so the node doesn't generate a new Peer ID on every restart).
- Output the complete Go source code structure (using `main.go` and clear packages), a `go.mod` file, and a `README.md` explaining how to build and test the tool.
