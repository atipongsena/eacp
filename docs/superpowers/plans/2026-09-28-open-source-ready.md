# Open-Source Ready Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the repository meet MASTER_PLAN §112 and be ready to push as `github.com/atipongsena/eacp`, with
bilingual illustrated documentation, CI, a release process and a guard test that keeps it so.

**Architecture:** A `test/opensource` package enforces the checklist. `scripts/ci/*.sh` hold every CI step, so a local
run proves what GitHub Actions will do. The workflows only call those scripts. Screenshots come from a separate Go
module (`tools/screenshots`) that drives a headless browser against the compose stack populated by the examples.

**Tech Stack:** Go 1.27, GitHub Actions, bash, Python (the `anthropic` SDK) for one example, chromedp (separate
module), Mermaid.

**Spec:** `docs/superpowers/specs/2026-09-28-open-source-ready-design.md`

## Global Constraints

- Module path: `github.com/atipongsena/eacp`. Binary names, the schema `eacp`, the roles `eacp_app`/`eacp_owner` and
  `EACP_*` settings are unchanged.
- License: Apache-2.0, with the text exactly from `https://www.apache.org/licenses/LICENSE-2.0.txt`. `NOTICE` holds
  `Copyright 2026 atipongsena`.
- Workflows: `permissions: contents: read` by default. Every `uses:` pins a 40-hex commit SHA, resolved with
  `git ls-remote` and commented with its tag.
- Bilingual set: README, `docs/ARCHITECTURE`, `docs/security/THREAT_MODEL`, `docs/FEATURES`, `docs/DEMO`,
  `docs/RELEASING`, `CONTRIBUTING`, `SECURITY`, `examples/README`, `examples/*/README`.
  - English is `X.md` and Thai is `X.th.md`. The first line of each file is the language switch
    `[English](X.md) | [ไทย](X.th.md)`.
  - Both files of a pair have the same heading-level sequence, the same number of code fences and the same image
    targets.
- Thai characters appear only in `*.th.md`. Elsewhere, the only Thai allowed is the language label ไทย.
- The README pair never contains "portfolio" (any case). No document calls the project a portfolio or a showcase.
- The README pair quotes benchmark figures only as result-table cells of `docs/BENCHMARKS.md` (§105).
- Never print, log, commit or screenshot a key. `examples/.env` is git-ignored.
- Never invent an upstream API or version. Verify each against the module source or with `go list -m -versions` or
  `git ls-remote` first.
- Git history is not rewritten, nothing is pushed and no tag is created.
- Commit as the user only, with no Co-Authored-By trailer.
- Tests with PostgreSQL need
  `EACP_TEST_ADMIN_DSN=postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable`. Without it they are
  skipped, not passed.

## Review Focus

1. **Running the examples from Git Bash on Windows.** MSYS path conversion, CRLF and `.exe` names all come up. The
   examples must run there as documented. Verification (Task 7): run every example on this Windows machine as well as
   in the Linux CI script.
2. **A key leaking into the repository or an image.** `examples/.env` must be ignored and no PNG may show a key.
   `TestExamplesEnvIsIgnored` (Task 7) and a manual image check (Task 8).
3. **Re-running `setup.sh` on a stack that is already set up.** It must reuse `.env` and not fail with a conflict.
   Verification (Task 7): run it twice.
4. **Stale `eacp/...` import paths left in non-Go files** (Dockerfiles, scripts, Helm) after the rename would break
   a build that `go build` does not see. `TestNoOldModulePath` (Task 1) checks every tracked file outside
   `docs/superpowers/` and `docs/reviews/`.
5. **Links to headings or images that do not exist.** The link check covers images and relative files. Heading
   anchors are not checked (GitHub's slug rules for Thai are not specified); this is ruled in the spec §7 at Task 4.

---

### Task 1: Module path

**Files:**
- Create: `test/opensource/opensource_test.go`, `test/opensource/helpers_test.go`
- Modify: `go.mod`, every `.go` file importing `eacp/...`, and any non-Go file naming the old path outside
  `docs/superpowers/` and `docs/reviews/` (historical records keep their paths)

**Interfaces:**
- Produces:
  - `repoRoot(t) string`: walks up to `go.mod`.
  - `trackedFiles(t) []string`: repo-relative, from `git ls-files`.
  - Both are used by every later guard test.

- [ ] **Step 1: Write the failing tests** `TestModulePath` and `TestNoOldModulePath`.
  - `TestModulePath`: `go.mod`'s `module` line equals `github.com/atipongsena/eacp`.
  - `TestNoOldModulePath`: no tracked text file outside `docs/superpowers/` and `docs/reviews/` contains the regexp
    `(^|[^/\w])eacp/(internal|cmd|integrations|test|sidecars)/`.
- [ ] **Step 2: Run them.** `go test ./test/opensource -run 'ModulePath|OldModulePath'` should FAIL on both.
- [ ] **Step 3: Rename.**
  - Run `go mod edit -module github.com/atipongsena/eacp`.
  - Rewrite the import prefix `"eacp/` to `"github.com/atipongsena/eacp/` in `.go` files with a script, not by hand.
  - Fix any remaining non-Go hits.
  - Run `gofmt -w`, because the import grouping may change.
- [ ] **Step 4: Verify.**
  - `go build ./... && go vet ./...` passes.
  - The two tests PASS.
  - `docker compose build controlplane-api` succeeds.
- [ ] **Step 5: Commit.** `refactor: the module is github.com/atipongsena/eacp`

### Task 2: Version

**Files:**
- Create: `internal/version/version.go`, `internal/version/version_test.go`
- Modify: `internal/service/service.go` (the startup log), `cmd/eacpctl/main.go` (the `version` subcommand and its
  usage line), and their tests

**Interfaces:**
- Produces: `version.Version string` (a `var`, default `"dev"`). The ldflag is
  `-X github.com/atipongsena/eacp/internal/version.Version=<v>`.

- [ ] **Step 1: Write the failing tests.**
  - `TestVersionDefaultsToDev`: `version.Version == "dev"`.
  - In `internal/service`, extend an existing startup test: the "service started" record has attribute
    `version` = `version.Version`.
  - In `cmd/eacpctl`, `TestVersionPrintsTheBuildVersion`: `run(ctx, []string{"version"}, ...)` writes
    `version.Version + "\n"`.
- [ ] **Step 2: Run them.** All three FAIL (package missing, attribute missing, unknown command).
- [ ] **Step 3: Implement.**
  - Add the package.
  - Add `"version", version.Version` to `log.Info("service started", ...)`.
  - Add `case "version"` and `eacpctl version` to the usage text.
- [ ] **Step 4: Verify.** `go test -race ./internal/version ./internal/service ./cmd/eacpctl` PASSes (DSN set).
- [ ] **Step 5: Commit.** `feat: a build version in every service log and eacpctl version`

### Task 3: License and attribution

**Files:**
- Create: `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`
- Test: `test/opensource/license_test.go`

- [ ] **Step 1: Write the failing tests.**
  - `TestLicenseIsApache2`: `LICENSE` contains `Apache License` and `Version 2.0, January 2004` and
    `END OF TERMS AND CONDITIONS`.
  - `TestNoticeNamesTheCopyright`: `NOTICE` contains `Copyright 2026 atipongsena`.
  - `TestThirdPartyNoticesCoverDependencies`: every module path in `go.mod`'s `require` blocks (parse with
    `golang.org/x/mod/modfile` if it is already in `go.sum`, else by line) and every `name==` in
    `sidecars/agt-pdp/requirements.txt` appears in `THIRD_PARTY_NOTICES.md`.
- [ ] **Step 2: Run them.** They FAIL (files missing).
- [ ] **Step 3: Write the files.**
  - `LICENSE`: download the text from apache.org, byte for byte.
  - `THIRD_PARTY_NOTICES.md`: a table of component, version, license and source.
    - Take each Go module's license from its LICENSE file in `$(go env GOMODCACHE)`.
    - Take each Python package's license from its PyPI metadata, read with `pip download --no-deps` or the PyPI JSON
      API.
    - Also list the base images from both Dockerfiles and compose, OPA 1.20.2 and Helm v4.3.0.
    - Never guess a license. Mark anything unreadable as such and resolve it before committing.
- [ ] **Step 4: Verify.** `go test ./test/opensource` PASSes.
- [ ] **Step 5: Commit.** `docs: Apache-2.0 license, NOTICE and third-party notices`

### Task 4: Language guards and the MASTER_PLAN translation

**Files:**
- Create: `test/opensource/docs_test.go`
- Modify: `docs/MASTER_PLAN.md` (the 595 Thai lines), plus any file with a broken relative link

**Interfaces:**
- Produces:
  - `markdownFiles(t) []string`: every tracked `.md` file.
  - `pairs(t) [][2]string`: each `X.th.md` with its `X.md`.
  - `outline(md string) []string`: the heading levels, then `fence` per code fence and `img:<target>` per image, in
    document order.

- [ ] **Step 1: Write the failing tests.**
  - `TestThaiOnlyInThaiFiles`: fails on `docs/MASTER_PLAN.md` now. Allowed exception: the exact word `ไทย`.
  - `TestTranslationsMatch`: for each pair, `outline(en) == outline(th)`, and each file of a pair has the language
    switch on line 1. Prove it with a table-driven test on inline samples: a pair missing one `##` fails, and a pair
    with an extra image fails.
  - `TestMarkdownLinksResolve`: `[text](target)` and `![alt](target)` with a relative target, with any `#anchor`
    stripped and ignoring `http(s):`/`mailto:`, must name an existing file or directory relative to the markdown
    file. Code spans and fences are skipped.
  - `TestReadmeNeverSaysPortfolio`: `README.md`, and `README.th.md` if present, contain no case-insensitive
    `portfolio`.
- [ ] **Step 2: Run them.**
  - `TestThaiOnlyInThaiFiles` FAILs, listing `docs/MASTER_PLAN.md`.
  - `TestMarkdownLinksResolve` may FAIL on existing broken links; record each one.
- [ ] **Step 3: Translate and fix.**
  - Translate every Thai line of MASTER_PLAN in place. Keep structure, numbering and code blocks, and translate
    meaning faithfully.
  - Fix the broken links found in Step 2 (the target file, not the text).
  - Ledger ruling: heading anchors are not checked.
- [ ] **Step 4: Verify.** `go test ./test/opensource` PASSes. Also read the translated sections §0–§5, §68, §112 and
  §114 back for sense.
- [ ] **Step 5: Commit.** `docs: MASTER_PLAN in English; guards for languages and links`

### Task 5: CI

**Files:**
- Create:
  - `scripts/ci/lint.sh`, `scripts/ci/test.sh`, `scripts/ci/helm.sh`, `scripts/ci/sidecar.sh`, `scripts/ci/vuln.sh`,
    `scripts/ci/compose-security.sh`
  - `.github/workflows/ci.yml`, `.github/workflows/nightly.yml`, `.github/dependabot.yml`
- Test: `test/opensource/workflows_test.go`

**Interfaces:**
- Produces:
  - `scripts/ci/test.sh slow|rest`. `slow` = `./internal/registry/... ./internal/worker/...`; `rest` =
    `go list ./... | grep -v -e /internal/registry -e /internal/worker`.
  - `ci.yml` has an `on: workflow_call` trigger (Task 6 calls it).

- [ ] **Step 1: Write the failing test.** `TestWorkflowActionsArePinned`: every `uses:` line in
  `.github/workflows/*.yml` matches `@[0-9a-f]{40}( #.*)?$`. Local `./` uses are exempt. It also fails when the
  directory is missing.
- [ ] **Step 2: Run it.** It FAILs (no workflows).
- [ ] **Step 3: Write the scripts** (`set -euo pipefail`, runnable from the repo root on Linux and Git Bash).
  - **`lint.sh`:** `test -z "$(gofmt -l $(git ls-files '*.go'))"`, `go vet ./...`, then `go mod tidy` and
    `git diff --exit-code go.mod go.sum`.
  - **`test.sh`:**
    - `docker compose up -d --wait postgres`;
    - export the DSN and `EACP_UI_NODE_REQUIRED=1`;
    - `go test -race -count=1 -timeout 45m` over the shard's packages.
  - **`helm.sh`:**
    - fetch `https://get.helm.sh/helm-v4.3.0-linux-amd64.tar.gz` and its `.sha256sum` (verify both URLs exist);
    - `sha256sum -c`, and extract to `.tools/helm`;
    - `EACP_HELM_REQUIRED=1 go test -count=1 ./test/helm`.
  - **`sidecar.sh`:** the sidecar test-stage build.
  - **`vuln.sh`:** `go run golang.org/x/vuln/cmd/govulncheck@<latest tag verified with go list -m -versions> ./...`.
  - **`compose-security.sh`:** `docker compose up -d --build --wait`, then
    `EACP_COMPOSE_TEST=1 go test -count=1 ./test/security/`.
- [ ] **Step 4: Write the workflows.**
  - **`ci.yml` triggers:** push to main, pull_request and workflow_call, with
    `concurrency: ci-${{ github.ref }}` and cancel-in-progress.
  - **`ci.yml` jobs:** lint; test (matrix shard slow and rest; `actions/setup-node` with node 22); helm; sidecar;
    vuln. All jobs use `actions/checkout` and `actions/setup-go` with `go-version-file: go.mod`.
  - **`nightly.yml`:** runs on `schedule: cron '0 3 * * *'` and `workflow_dispatch`, with jobs security, demos
    (`scripts/demo.sh`), bench (`scripts/bench.sh --quick` plus `actions/upload-artifact` of `bench-results/`) and
    examples (added in Task 7).
  - **Pinning:** resolve each action's SHA with `git ls-remote https://github.com/<owner>/<repo> refs/tags/<tag>`
    (use the peeled `^{}` SHA when present) and comment the tag.
  - **`dependabot.yml`:** weekly updates for gomod `/`, pip `/sidecars/agt-pdp`, docker `/deployments/docker` and
    `/sidecars/agt-pdp`, and github-actions `/`.
- [ ] **Step 5: Verify locally.**
  - `TestWorkflowActionsArePinned` PASSes.
  - `go run github.com/rhysd/actionlint/cmd/actionlint@<verified tag>` is clean.
  - `bash scripts/ci/lint.sh`, `test.sh slow`, `test.sh rest`, `helm.sh`, `sidecar.sh` and `vuln.sh` each exit 0.
    Run `test.sh` in the background with output to the workspace; `helm.sh` on Windows should skip the download and
    use the existing `.tools/helm` only when not on linux. Any govulncheck finding is fixed by upgrading, followed by
    the full suite.
  - `compose-security.sh` exits 0.
- [ ] **Step 6: Commit.** `ci: GitHub Actions workflows over scripts/ci, Dependabot`

### Task 6: Release

**Files:**
- Create: `scripts/ci/release-binaries.sh`, `.github/workflows/release.yml`, `CHANGELOG.md`, `docs/RELEASING.md`,
  `docs/RELEASING.th.md`

**Interfaces:**
- Consumes: `ci.yml` (`workflow_call`) and `version.Version`.
- Produces: `scripts/ci/release-binaries.sh <version> [outdir]`. The default outdir is `dist/`, which is added to
  `.gitignore`.

- [ ] **Step 1: Write the release script.**
  - Build the binaries `controlplane-api execution-worker llm-gateway eacpctl` for linux/amd64, linux/arm64,
    darwin/arm64 and windows/amd64, with `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X <version ldflag>"`.
  - Package each target as `eacp_<version>_<os>_<arch>.tar.gz`, or `.zip` for windows.
  - Write `SHA256SUMS`.
- [ ] **Step 2: Verify it.**
  - `bash scripts/ci/release-binaries.sh v0.0.0-test` produces 4 archives.
  - `(cd dist && sha256sum -c SHA256SUMS)` succeeds.
  - Extracting the linux/amd64 `eacpctl` and running it prints `v0.0.0-test` (or run the Windows one natively).
- [ ] **Step 3: Write `release.yml`** on `push: tags: ['v*']`:
  - `verify` uses `./.github/workflows/ci.yml`;
  - `binaries` runs the script and uploads `dist/`;
  - `images` uses docker login, setup-buildx and build-push for `ghcr.io/atipongsena/eacp:${{ github.ref_name }}`
    (file `deployments/docker/Dockerfile`) and `ghcr.io/atipongsena/eacp-agt-pdp:${{ github.ref_name }}` (file
    `sidecars/agt-pdp/Dockerfile`, final stage), `platforms: linux/amd64`, with `packages: write`;
  - `publish` extracts the `## [<version without v>]` section of `CHANGELOG.md` (the job fails if it is empty) and
    runs `gh release create` with the dist files and `contents: write`.

  Every action is pinned by SHA.
- [ ] **Step 4: Write the documents.**
  - `CHANGELOG.md`: Keep a Changelog format, `## [Unreleased]` and `## [0.1.0]`, summarising Slices A, B and C,
    Phases 23–25 and the benchmark in a few lines each.
  - `docs/RELEASING.md` and `docs/RELEASING.th.md`: the steps, what the workflow does, and the post-push checklist
    (branch protection, private vulnerability reporting, GHCR visibility, Dependabot alerts, watching the first CI
    run).
- [ ] **Step 5: Verify.** actionlint is clean, and `go test ./test/opensource` PASSes (pins, Thai, pair outline,
  links).
- [ ] **Step 6: Commit.** `ci: release workflow, binaries with checksums, CHANGELOG and RELEASING`

### Task 7: Examples

**Files:**
- Create:
  - `examples/setup.sh`, `examples/setup/main.go`
  - `examples/01-agent-action/run.sh`
  - `examples/02-llm-gateway/main.py`, `examples/02-llm-gateway/requirements.txt`
  - `examples/03-governance-as-code/bundle/*.yaml` and `examples/03-governance-as-code/run.sh`
  - `examples/README.md` and `.th.md`, and each example's `README.md` and `.th.md`
  - `scripts/ci/examples.sh`
- Modify: `deployments/docker/secrets/connector-secrets.dev.json`, `llm-secrets.dev.json` and whatever else the
  bench added for its tenant; `.gitignore` (`/examples/.env`); `nightly.yml` (the examples job)
- Test: `test/opensource/examples_test.go`

**Interfaces:**
- Produces:
  - **Examples tenant:** `00000000-0000-4000-8000-0000000000e0`, slug `examples`.
  - **`examples/.env`:** one `KEY=value` per line. The keys are `EACP_API`, `EACP_GATEWAY`, `TENANT_ID`, `ADMIN_KEY`,
    `ADMIN2_KEY`, `OPERATOR_KEY`, `APPROVER_KEY`, `APPROVER2_KEY` and `AGENT_KEY`.
  - Task 8 consumes the file.

- [ ] **Step 1: Write the failing tests.**
  - `TestExamplesEnvIsIgnored`: `git check-ignore -q examples/.env` succeeds.
  - `TestEveryExampleHasBothReadmes`: each `examples/*/` directory has `README.md` and `README.th.md`.
- [ ] **Step 2: Run them.** Both FAIL.
- [ ] **Step 3: Write `examples/setup/main.go`** (package main, run with `go run ./examples/setup`).
  - It mirrors `cmd/eacp-bench/setup.go`'s sequence against `EACP_API` (default `http://127.0.0.1:8080`):
    - tenant creation through `docker compose run --rm migrate /eacpctl tenant create` (with
      `MSYS_NO_PATHCONV=1`);
    - principals and two-person role grants, including an operator;
    - a policy that requires one approval for `create_po` amounts over 1000;
    - the ERP connector, `create_po` and its contract;
    - the `sonnet` model and its price;
    - one agent `procurement-bot` with an allowlist;
    - budgets.
  - Keys are generated in process (the bench's `newKey`) and written to `examples/.env` with mode 0600.
  - If `.env` exists and its admin key authenticates, it exits 0 with "already set up".
  - It never prints a key. `examples/setup.sh` is `cd` to the repo root plus `go run ./examples/setup "$@"`.
- [ ] **Step 4: Write the three examples.** Each prints numbered steps and exits non-zero on any unexpected status.
  - **01 (`jq` and `curl`):**
    1. submit a 5000 THB purchase order as the agent → `PENDING_APPROVAL`;
    2. two approvers vote;
    3. poll until `SUCCEEDED`;
    4. print the evidence summary (decisions, votes, attempt, the chain verified).
  - **02:** `anthropic.Anthropic(api_key=AGENT_KEY, base_url=EACP_GATEWAY)`:
    1. `messages.create(model="sonnet", ...)` succeeds and prints usage;
    2. `model="opus"` (not allowlisted) raises a 403 whose type the script checks;
    3. the script lists the agent's LLM calls through `eacpctl llm-calls list` or the API as the operator.
  - **03:**
    1. `eacpctl bundle validate -C bundle` then `plan`;
    2. `deploy` as ADMIN, `approve` as ADMIN2;
    3. `drift` shows no difference.
- [ ] **Step 5: Write the READMEs, then wire CI.**
  - READMEs (EN and TH): purpose, prerequisites (Docker, Go, `jq`, Python 3.12 for 02), commands, the expected output
    (real, trimmed) and what it demonstrates.
  - `scripts/ci/examples.sh`: `docker compose up -d --build --wait`, setup, then 01, 02 (in a venv) and 03.
  - Add the nightly `examples` job.
- [ ] **Step 6: Verify.**
  - The tests PASS.
  - On this Windows machine: `docker compose up -d --build --wait`, then `bash examples/setup.sh` twice (the second
    says "already set up"), then the three examples each exit 0.
  - `bash scripts/ci/examples.sh` exits 0.
  - `git status` does not show `.env`.
  - The Thai and pair guards PASS.
- [ ] **Step 7: Commit.** `docs: three runnable examples and their setup`

### Task 8: Screenshots

**Files:**
- Create: `tools/screenshots/go.mod`, `tools/screenshots/main.go`, `scripts/screenshots.sh`, `docs/images/*.png`
- Note: no `go.work`; the tool is its own module, run with `go run .` from its directory.

**Interfaces:**
- Consumes: `examples/.env` (`EACP_API`, `OPERATOR_KEY`) and the console at `/ui/`.
- Produces: `docs/images/console-{overview,approvals,execution,evidence,fleet,incidents,dependencies,cost}.png` at
  1440×900.

- [ ] **Step 1: Verify the chromedp API** from its module source: the latest tag from `go list -m -versions`,
  `NewExecAllocator`, `ExecPath`, `Navigate`, `SendKeys`, `Click`, `WaitVisible`, `FullScreenshot`/`CaptureScreenshot`
  and `EmulateViewport`. Also read `internal/ui/static/index.html` and `app.js` for the sign-in form's selectors and
  each view's hash route.
- [ ] **Step 2: Write the tool.**
  - Flags: `-api`, `-key-env` (the name of the env var holding the key, read from the environment, never a flag
    value) and `-out`.
  - It signs in by typing the key, visits each hash route, waits for the view's main heading, and saves a PNG.
  - It fails if the page shows an error banner.
- [ ] **Step 3: Write `scripts/screenshots.sh`.** It sources `examples/.env` without echoing, then runs:
  1. the setup and examples;
  2. the two scenarios:
     - an action whose Fake ERP scenario makes the outcome unknown, so it ends `UNKNOWN_OUTCOME` then
       `NEEDS_HUMAN_RESOLUTION` (use the scenario the Slice A demo uses, read from `test/demo`);
     - `eacpctl kill activate agent_version <id>` as the operator, with the second operator per ADR-016 where needed;
  3. waits for the incident evaluator;
  4. runs the tool, finding Chrome or Edge on PATH or the standard Windows paths.
- [ ] **Step 4: Verify.**
  - The script produces 8 PNGs.
  - Open every PNG (Read tool) and check that it shows real data, that no key or token is visible, and that no
    error banner appears.
  - `go vet` in `tools/screenshots` is clean.
- [ ] **Step 5: Commit.** `docs: console screenshots from a running stack`

### Task 9: Architecture, threat model, features, demo, contributing, security

**Files:**
- Create:
  - `docs/ARCHITECTURE.md` and `.th.md`
  - `docs/security/THREAT_MODEL.md` and `.th.md`
  - `docs/FEATURES.md` and `.th.md`
  - `docs/DEMO.th.md`
  - `CONTRIBUTING.md` and `.th.md`
  - `SECURITY.md` and `.th.md`
  - `CODE_OF_CONDUCT.md`
  - `.github/ISSUE_TEMPLATE/{bug_report.yml,feature_request.yml,config.yml}`
  - `.github/pull_request_template.md`
- Modify: `docs/DEMO.md` (add the language switch line)

- [ ] **Step 1: ARCHITECTURE pair.** Cover, per spec §3.4:
  - components;
  - networks and trust boundaries, as a Mermaid flowchart built from `docker-compose.yml`'s networks;
  - PostgreSQL as authority and NATS as signals;
  - the state machine, as a Mermaid state diagram of the main states from ADR-004;
  - the execution fabric, governance, the LLM gateway and HA;
  - links to the ADRs.

  Every claim cites an ADR or code path.
- [ ] **Step 2: THREAT_MODEL pair.**
  - Content: assets; boundaries; STRIDE per boundary; each threat's mitigation, its ADR and the test enforcing it
    (from `docs/INVARIANTS.md`); residual risks; assumptions.
  - Source: MASTER_PLAN §67–§70 and the ADRs. Invent no control.
- [ ] **Step 3: FEATURES pair.** Move the README's "What exists today" table and the phase list there, updated through
  Phase 25b and the benchmark.
- [ ] **Step 4: The rest.**
  - The DEMO Thai version.
  - CONTRIBUTING and SECURITY pairs, following spec §3.4.
  - CODE_OF_CONDUCT (the Contributor Covenant 2.1 by link, with reporting through the maintainer's GitHub profile).
  - Issue forms and the PR template, whose checklist is: tests first, `-race`, ADR/docs, both languages.
- [ ] **Step 5: Verify.** `go test ./test/opensource` PASSes (Thai, pairs, links).
- [ ] **Step 6: Commit.** `docs: architecture, threat model, features, contributing and security in English and Thai`

### Task 10: README pair, checklist guard and status

**Files:**
- Create: `README.th.md`
- Modify: `README.md` (rewritten), `internal/bench/docs_test.go`, `test/opensource/checklist_test.go`,
  `AGENTS.md`, `docs/MASTER_PLAN.md` (§112 status)

- [ ] **Step 1: Write the failing tests.**
  - `TestChecklistFilesExist`: the spec §3.7 list, which FAILs on `README.th.md`.
  - Extend `TestReadmeNumbersComeFromTheBaseline` to also read `README.th.md` when it exists. It FAILs once a Thai
    figure is wrong; check this by inserting an invented figure temporarily and seeing it fail.
- [ ] **Step 2: Write `README.md`** in the 12 sections of spec §3.4.
  - It uses the screenshots from Task 8.
  - It has two Mermaid diagrams: the architecture and the action sequence.
  - It includes real excerpts of example 01's output.
  - It has badges for CI (`https://github.com/atipongsena/eacp/actions/workflows/ci.yml/badge.svg`), the license and
    Go.
  - Its benchmark figures are cells only.
- [ ] **Step 3: Write `README.th.md`** with the same outline, in natural Thai.
- [ ] **Step 4: Update the status docs.**
  - `AGENTS.md`: the status line; the layout (examples, tools, `.github`, `scripts/ci`, `test/opensource`); a rule
    stating the bilingual pair rule and that `test/opensource` enforces §112.
  - MASTER_PLAN §112: a status paragraph.
- [ ] **Step 5: Verify.**
  - `go test ./test/opensource ./internal/bench` PASSes.
  - Full suite: `go vet ./...` and `go test -race -count=1 -timeout 40m ./...` with the DSN (0 FAIL), then helm and
    invariants.
  - `bash scripts/ci/lint.sh` exits 0.
- [ ] **Step 6: Commit.** `docs: the illustrated README in English and Thai; §112 status`
