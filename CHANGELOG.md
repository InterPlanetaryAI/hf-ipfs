# Changelog

## 0.1.0 (2026-09-04)


### Features

* **ci:** build binaries and multi-arch images on release publish ([fb545a5](https://github.com/InterPlanetaryAI/hf-ipfs/commit/fb545a5b5bf5d3c4e26d96adc03e5a76e7bdf442))
* **cli:** --from, --hf-token and an honest startup banner ([98a9232](https://github.com/InterPlanetaryAI/hf-ipfs/commit/98a9232006c292f84ec63d29915acd0f16be2d94))
* **cli:** add daemon, pull, ingest, list, status, id and resolve commands ([f20b283](https://github.com/InterPlanetaryAI/hf-ipfs/commit/f20b28359b89038f9f3e90f30e91c7b399d0b12f))
* **config:** fixed listen port, swarm toggles and HF token ([020542b](https://github.com/InterPlanetaryAI/hf-ipfs/commit/020542b455b1364a78c83841ff49ecb48cadcf6c))
* **config:** resolve node settings from flags and HF env vars ([885668b](https://github.com/InterPlanetaryAI/hf-ipfs/commit/885668b770a0d1cb4c595f2e82e52e14e27062f2))
* **controls:** carry source spec and token over an owner-only socket ([2c91a1f](https://github.com/InterPlanetaryAI/hf-ipfs/commit/2c91a1fe5df9c6d4ab28fcf7382a370c1a72621a))
* **controls:** expose a daemon control socket and CLI proxy ([1405fba](https://github.com/InterPlanetaryAI/hf-ipfs/commit/1405fba1228b72ec28e34d77da1c25b2ebf5f033))
* **dummy:** map HF commit hashes to reversible dummy CIDs ([8ad794a](https://github.com/InterPlanetaryAI/hf-ipfs/commit/8ad794a90ebdd51ddfd5ce9169e0ed44cf460de5))
* **hfapi:** add a minimal Hugging Face Hub API client ([041baa1](https://github.com/InterPlanetaryAI/hf-ipfs/commit/041baa1b4af3b2de85b5988c363269baaed40a8c))
* **hfapi:** revision lookups, tree endpoint and blob streaming ([b60c7c4](https://github.com/InterPlanetaryAI/hf-ipfs/commit/b60c7c488e18ca0a9e8e4b21dbf27d47139d9cec))
* **hfcache:** model the hf CLI cache layout for transparent read and relink ([c1456b2](https://github.com/InterPlanetaryAI/hf-ipfs/commit/c1456b25e2705580dcf4b10c43ac4405c0615315))
* **hfcache:** record per-revision trees/&lt;commit&gt;.json ([f65e0ef](https://github.com/InterPlanetaryAI/hf-ipfs/commit/f65e0eff80bf79a697dd7688f42ad1e5990bba1b))
* **identity:** persist the libp2p Ed25519 key across restarts ([895430b](https://github.com/InterPlanetaryAI/hf-ipfs/commit/895430be11fdf9d908d5c8f813b1679967abcca1))
* **ingest:** add finalized HF snapshots to the DAG without copying bytes ([c7c7d44](https://github.com/InterPlanetaryAI/hf-ipfs/commit/c7c7d444ea37790b07664ad179059813b9c315fe))
* **mapping:** store the commit to actual-CID database locally ([b6a2444](https://github.com/InterPlanetaryAI/hf-ipfs/commit/b6a2444153cb983de66e8793265edd601fe17472))
* **node:** add --relay-service and --static-relay for NATed peers ([9754602](https://github.com/InterPlanetaryAI/hf-ipfs/commit/97546025a04988701082004c48eab778b374c04e))
* **node:** embed host, kad-dht, filestore and protocol handlers ([06dfc50](https://github.com/InterPlanetaryAI/hf-ipfs/commit/06dfc50546b816aa79d44849cca1b0e48db83da1))
* **node:** reachability tracking, loopback guard and dial ordering ([8536e94](https://github.com/InterPlanetaryAI/hf-ipfs/commit/8536e945152124aade0680e9ca8ac8ea98582315))
* **protoio:** length-prefixed framing for streams and the control socket ([8e80a88](https://github.com/InterPlanetaryAI/hf-ipfs/commit/8e80a88048541a73299f7c25a6107f91daadb544))
* **pull:** implement the pull pipeline steps A through E ([d292d9f](https://github.com/InterPlanetaryAI/hf-ipfs/commit/d292d9fa7d33a0ebf7f675c57aa1fd88581d1e56))
* **pull:** source selection with a Hugging Face fallback ([e4264c7](https://github.com/InterPlanetaryAI/hf-ipfs/commit/e4264c7ebf73b2bfe97bc719a71040eedc371432))
* **store:** adapt bbolt to go-datastore Batching with a repo flock ([b6f6e16](https://github.com/InterPlanetaryAI/hf-ipfs/commit/b6f6e16b854a60202fb617cdc66112a8fd19848f))
* **watch:** detect finalized downloads with fsnotify plus a rescan ([d8a0d46](https://github.com/InterPlanetaryAI/hf-ipfs/commit/d8a0d46d61cb37f15eb91a4d35d5414c685b55a2))
* **wire:** shared JSON messages for the map protocol and control API ([b3c02df](https://github.com/InterPlanetaryAI/hf-ipfs/commit/b3c02df6d492e02a82fabd768f64d67b83c5644e))


### Bug Fixes

* **ci:** make the docker CI workflow actually push images ([088dfd0](https://github.com/InterPlanetaryAI/hf-ipfs/commit/088dfd039c5ac83a8d799a239221561c59143c28))
* **hfcache:** keep dotfiles in snapshots, skip only bookkeeping dirs ([b0f355a](https://github.com/InterPlanetaryAI/hf-ipfs/commit/b0f355ac9249a42a5e46fa7a90c842c8501f48fe))
* **release:** declare the root package so release-please opens PRs ([6baf55d](https://github.com/InterPlanetaryAI/hf-ipfs/commit/6baf55dabbca7da22fc228abe3aeb1081dd3ff34))
