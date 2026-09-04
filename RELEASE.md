# Releasing

This repo uses [release-please](https://github.com/googleapis/release-please). Releases are driven by
[Conventional Commits](https://www.conventionalcommits.org/) on `main` — no manual version bumping.

## How it works

1. Merge PRs into `main` with conventional commit messages:
   - `fix: ...` → patch bump (0.1.1)
   - `feat: ...` → minor bump (0.2.0)
   - `feat!: ...` / `BREAKING CHANGE:` → major bump (1.0.0)
   - `chore:`, `docs:`, `test:` → no release
2. The **Release Please** workflow (`.github/workflows/release-please.yml`) runs on each push to `main`
   and opens/updates a single "chore(main): release X.Y.Z" PR with generated release notes.
3. When that release PR is merged, the workflow creates the git tag `vX.Y.Z` and the GitHub release.
   The **Release Assets** workflow (`release-assets.yml`) then attaches cross-built binaries.

## Creating a release

Nothing to do manually — just merge conventional commits into `main`, then merge the release PR
when you want to ship. To hold a release back, simply don't merge the release PR yet; it keeps
accumulating new commits.

## Forcing a specific version

Edit `.release-please-manifest.json` on `main` (it holds the last released version, e.g.
`{ ".": "0.1.0" }`) and push. The next release-please run bumps from that value.

Note: the first release was pinned to **0.1.0** via `initial-version` in
`release-please-config.json`.

## Re-running / debugging

- Re-run a workflow from the **Actions** tab → **Re-run jobs** (or `gh run rerun <run-id>`).
- Check logs: `gh run list --workflow=Release\ Please`
- If the release PR gets into a bad state, close it and re-run the workflow — it will recreate the PR.

## Files

| File | Purpose |
|---|---|
| `release-please-config.json` | Release type (`go`) + `initial-version: 0.1.0` |
| `.release-please-manifest.json` | Last released version per path |
| `.github/workflows/release-please.yml` | Opens release PRs / creates releases |
| `.github/workflows/release-assets.yml` | Builds & uploads binaries on tag |
