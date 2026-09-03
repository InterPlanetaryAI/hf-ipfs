#!/usr/bin/env python3
"""Create a fake, already-finalized Hugging Face cache tree for testing.

Layout mirrors what the `hf` CLI produces:
  <hub>/models--<org>--<name>/blobs/<sha256>
  <hub>/models--<org>--<name>/snapshots/<commit>/<path> -> ../../blobs/<sha256>
  <hub>/models--<org>--<name>/refs/main -> <commit>
"""
import hashlib
import json
import os
import sys


def blob_name(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def main() -> int:
    hub = sys.argv[1]
    repo = sys.argv[2] if len(sys.argv) > 2 else "testorg/testmodel"
    commit = sys.argv[3] if len(sys.argv) > 3 else "607a30d783dfa663caf39e06633721c8d4cfcd7e"

    repo_dir = os.path.join(hub, "models--" + repo.replace("/", "--"))
    blobs = os.path.join(repo_dir, "blobs")
    snap = os.path.join(repo_dir, "snapshots", commit)
    refs = os.path.join(repo_dir, "refs")
    for d in (blobs, snap, refs):
        os.makedirs(d, exist_ok=True)

    files = {
        "config.json": json.dumps({"architectures": ["LlamaForCausalLM"], "hidden_size": 4096}).encode(),
        "tokenizer.json": b'{"model": {"vocab": {"hello": 0, "world": 1}}}\n',
        "subdir/weights-00001.safetensors": os.urandom(5 * 1024 * 1024),
        "subdir/nested/deep.bin": os.urandom(1024 * 1024 + 7),
        "empty.txt": b"",
    }

    for rel, data in files.items():
        name = blob_name(data)
        blob_path = os.path.join(blobs, name)
        with open(blob_path, "wb") as fh:
            fh.write(data)

        link = os.path.join(snap, rel)
        os.makedirs(os.path.dirname(link), exist_ok=True)
        if os.path.islink(link) or os.path.exists(link):
            os.remove(link)
        rel_target = os.path.relpath(blob_path, os.path.dirname(link))
        os.symlink(rel_target, link)
        print(f"{rel:40s} sha256={name} size={len(data)}")

    with open(os.path.join(refs, "main"), "w") as fh:
        fh.write(commit + "\n")

    print(f"\nhub    = {hub}")
    print(f"repo   = {repo}")
    print(f"commit = {commit}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
