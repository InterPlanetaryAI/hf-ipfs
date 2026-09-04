# Releasing

This repo uses [release-please](https://github.com/googleapis/release-please). Releases are driven by
[Conventional Commits](https://www.conventionalcommits.org/) on `main` — no manual version bumping.

## The flow

1. **Push / merge conventional commits to `main`.**
   - `fix: ...` → patch bump (0.1.1)
   - `feat: ...` → minor bump (0.2.0)
   - `feat!: ...` / `BREAKING CHANGE:` → major bump (1.0.0)
   - `chore:`, `docs:`, `test:` → no release

   `bump-minor-pre-major` is on, so while the version is below 1.0.0 a `feat` moves the
   minor, not the major.

2. **Release Please opens a release PR.** `.github/workflows/release-please.yml` runs on every
   push to `main` and opens/updates a single `chore(main): release X.Y.Z` PR containing the
   generated `CHANGELOG.md`, the bumped `.release-please-manifest.json`, and the bumped
   `version` in `main.go`.

3. **Merge the release PR.** The same workflow then creates the git tag `vX.Y.Z` and the
   GitHub release.

4. **Artifacts are built and published** by `.github/workflows/release.yml`, triggered by
   the published release:
   - `hf-ipfs-<ver>-<os>-<arch>.tar.gz` for linux/darwin × amd64/arm64, attached to the release
   - multi-arch (`linux/amd64`, `linux/arm64`) images on `ghcr.io`, tagged
     `X.Y.Z`, `X.Y`, `X` and `latest`

Nothing is manual. To hold a release back, simply don't merge the release PR — it keeps
accumulating new commits until you do.

## Files

| File | Purpose |
|---|---|
| `release-please-config.json` | Per-path release config: `go` release type, `initial-version: 0.1.0`, `main.go` as a generic extra file |
| `.release-please-manifest.json` | Last released version per path (`"."` → `"0.0.0"` means nothing released yet) |
| `.github/workflows/release-please.yml` | Opens the release PR; mints tag + release on merge |
| `.github/workflows/release.yml` | Binaries and versioned container images for a published release |
| `.github/workflows/docker-ghcr.yml` | CI-only images: `:main` on the branch, `:pr-N` for approved PRs |

## Gotchas

**`release-please-config.json` must have a `packages` map.** release-please builds its list
of release paths from `Object.keys(config.packages)` and has **no root fallback** — a config
with only top-level `release-type`/`initial-version` yields zero paths and silently opens no
PR, which is exactly how this repo sat unreleased for its first 46 commits. Top-level keys are
only *defaults* for the paths declared under `packages`.

**The image name is lowercased in the workflows.** GHCR rejects uppercase in image
references and the owning org name is not lowercase, so both Docker workflows compute
`ghcr.io/${GITHUB_REPOSITORY,,}` rather than interpolating `github.repository` directly.

**`main.go` carries release-please markers** around `var version`:

```go
// x-release-please-start-version
var version = "0.1.0" // release-please bumps this; -ldflags -X main.version overrides it
// x-release-please-end
```

Keep the markers intact or the generic extra-file updater stops bumping the in-source default.

## Re-running / debugging

- Re-run from the **Actions** tab → **Re-run jobs** (or `gh run rerun <run-id>`).
- `gh run list --workflow=Release\ Please`
- If the release PR gets into a bad state, close it and re-run the workflow — it recreates it.
- To force a specific version, edit `.release-please-manifest.json` on `main` and push; the
  next run bumps from that value.
