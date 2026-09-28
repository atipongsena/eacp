[English](RELEASING.md) | [ไทย](RELEASING.th.md)

# Releasing EACP

A release is a Git tag `vX.Y.Z` on `main`. Pushing the tag runs [`release.yml`](../.github/workflows/release.yml),
which publishes nothing unless the full CI passes on the tagged commit.

## Before the first release

These are settings on GitHub, not files in the repository. Do them once, after the repository is created and `main`
is pushed:

1. **Watch the first CI run.** The workflows were verified by running `scripts/ci/*.sh` locally and with
   `actionlint`, but a GitHub runner may still differ (Docker Compose version, disk space). Fix anything it finds
   before tagging.
2. **Protect `main`.** Require pull requests and the `ci` checks (`lint`, `test (slow)`, `test (rest)`, `helm`,
   `sidecar`, `vuln`) before merging. Block force pushes.
3. **Turn on private vulnerability reporting** (Settings → Security → Private vulnerability reporting), which
   [SECURITY.md](../SECURITY.md) points reporters to.
4. **Turn on Dependabot alerts and security updates.** [`.github/dependabot.yml`](../.github/dependabot.yml) already
   schedules the version updates.
5. **After the first release,** make the two GHCR packages public (`eacp` and `eacp-agt-pdp`, under the package
   settings) if the images should be pullable without a login.

## Cutting a release

1. Move the entries under `## [Unreleased]` in [CHANGELOG.md](../CHANGELOG.md) into a new `## [X.Y.Z]` section, add
   its comparison link at the bottom, and merge that change to `main` through a pull request.
2. Tag the merged commit and push the tag:

   ```bash
   git tag -a vX.Y.Z -m "EACP vX.Y.Z"
   git push origin vX.Y.Z
   ```

3. Watch the `release` workflow. When it finishes, check the release page: the notes, four archives and
   `SHA256SUMS`.

## What the workflow does

| Job | What it does |
|---|---|
| `verify` | Runs the whole `ci.yml` on the tagged commit. Every later job needs it. |
| `binaries` | `scripts/ci/release-binaries.sh` builds `controlplane-api`, `execution-worker`, `llm-gateway` and `eacpctl` for linux/amd64, linux/arm64, darwin/arm64 and windows/amd64, with the version stamped in, and writes `SHA256SUMS`. |
| `images` | Pushes `ghcr.io/atipongsena/eacp:vX.Y.Z` (every EACP binary) and `ghcr.io/atipongsena/eacp-agt-pdp:vX.Y.Z` (the AGT sidecar), linux/amd64 only. |
| `publish` | Takes the release notes from the `CHANGELOG.md` section for the version and creates the GitHub release with the archives. A missing section fails the job. |

Every binary reports its version: `eacpctl version` prints it, and every service logs it at startup.

## Checking a release

Anyone can verify a downloaded archive:

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

A build from source reports `dev`. To reproduce a release binary, build it with the same flags:

```bash
bash scripts/ci/release-binaries.sh vX.Y.Z dist
```
