# Open-Source Ready (design)

Date: 2026-09-28 · Status: approved by the owner in chat, sections 1–3 (section 3 revised once). Scope: MASTER_PLAN
§112 (Open-Source Ready Definition). No ADR: nothing here changes what the control plane decides; the one code change
is a version string.

Owner's choices:

| Question | Choice |
|---|---|
| Scope | Everything inside the repository is made ready to push. The owner creates the GitHub repository and pushes. |
| License | Apache-2.0 |
| Repository and Go module | `github.com/atipongsena/eacp` |
| AI-assisted development artefacts | Kept and disclosed (`AGENTS.md`, `CLAUDE.md`, `docs/superpowers/`, `docs/reviews/`) |
| Thai text in `docs/MASTER_PLAN.md` | Translated to English (only the Thai lines) |
| CI | Tiered: every push and PR; nightly and manual for the heavy runs |
| Examples | Three runnable examples, checked nightly |
| Release | Documented process plus a workflow on `v*` tags |
| Approach | One branch in dependency order, with a guard test that enforces the §112 checklist |
| README | Illustrated, with real screenshots of the running console; detailed; never describes the project as a portfolio |
| Languages | User and contributor documents in English and Thai; normative and evidence documents in English only |

## 1. Intent

A reader who has never seen EACP opens the repository and, within minutes, understands:

- the problem it solves;
- what it guarantees, and where that guarantee stops;
- how it is built;
- what it looks like running;
- how to run it themselves.

A contributor finds how to build, test and propose changes. Every §112 item exists, and a test keeps it true. Nothing
in the repository is embarrassing when public: the README is current, links resolve and dependencies are attributed.
Nothing is hidden about how the project was built.

## 2. Out of scope

- **Publishing.** Creating the GitHub repository, pushing, or changing its settings (branch protection, private
  vulnerability reporting, GHCR visibility). `docs/RELEASING.md` lists them as the owner's post-push checklist.
- **Git history.** It is not rewritten, including the one commit authored as `Codex <codex@localhost>`.
- **The first release.** No tag is pushed; v0.1.0 is prepared in `CHANGELOG.md` only.
- **New product features.** The only code change is `internal/version` (§3.2).
- **Translating ADRs, MASTER_PLAN, reviews, specs, plans, BENCHMARKS, INVARIANTS or KUBERNETES into Thai.**
- **Multi-arch images, SBOMs and signing.**

## 3. Design

### 3.1 Module path and license

- **Module path.** `go.mod`'s module becomes `github.com/atipongsena/eacp`. Every import, `-ldflags` path, Dockerfile
  and script that names a package follows.
  - Binary names, the PostgreSQL schema `eacp`, the roles (`eacp_app`, `eacp_owner`) and `EACP_*` settings do not change.
- **`LICENSE`** is the Apache License 2.0 text exactly as published at `https://www.apache.org/licenses/LICENSE-2.0.txt`.
  No per-file headers are added; the license does not require them.
- **`NOTICE`** names the project and holds `Copyright 2026 atipongsena`. It points to `THIRD_PARTY_NOTICES.md`.
- **`THIRD_PARTY_NOTICES.md`** lists, with each item's license and source URL:
  - every module in `go.mod` (direct and indirect);
  - every package in `sidecars/agt-pdp/requirements.txt`;
  - the base images (`golang`, `gcr.io/distroless/static-debian12`, `python:3.12-slim-bookworm`, `postgres:18-alpine`,
    NATS, Vault);
  - the tools the sidecar image bundles (OPA);
  - Helm, which the tests download.

  Licenses are read from each module's own LICENSE file in the module cache or its upstream repository, never guessed.

### 3.2 Version

- **`internal/version`.** A new package with `var Version = "dev"`.
- **Stamping.** Release builds set it with `-ldflags "-X github.com/atipongsena/eacp/internal/version.Version=vX.Y.Z"`.
- **Where it appears.**
  - Every service logs it once at startup (`internal/service`).
  - `eacpctl version` prints it.
- **Tests.** They come first: the startup log carries `version`, and `eacpctl version` prints the variable.

### 3.3 CI and release (`.github/`)

Every step's logic lives in `scripts/ci/*.sh`, and the workflows only call those scripts. The workflows cannot run
before the repository is on GitHub, so each script is run locally as the verification. `actionlint` checks the
workflow files.

Every workflow defaults to `permissions: contents: read`. Every `uses:` pins a full commit SHA, resolved with
`git ls-remote` against the action's release tag and commented with that tag. Go comes from `go.mod`
(`go-version-file`).

**`ci.yml`** runs on push to `main`, on pull requests and on `workflow_call`, with concurrency that cancels superseded
runs. Its jobs:

| Job | Script | Content |
|---|---|---|
| `lint` | `scripts/ci/lint.sh` | `gofmt -l` prints nothing; `go vet ./...`; `go mod tidy` leaves `go.mod`/`go.sum` unchanged |
| `test` (shards `slow`, `rest`) | `scripts/ci/test.sh <shard>` | `docker compose up -d --wait postgres`, `EACP_TEST_ADMIN_DSN`, `EACP_UI_NODE_REQUIRED=1` (node from `setup-node`), `go test -race -count=1`. `slow` = `internal/registry`, `internal/worker`; `rest` = every other package |
| `helm` | `scripts/ci/helm.sh` | downloads Helm v4.3.0 for linux-amd64 into `.tools/`, checks its published sha256, runs `EACP_HELM_REQUIRED=1 go test ./test/helm` |
| `sidecar` | `scripts/ci/sidecar.sh` | `docker build -f sidecars/agt-pdp/Dockerfile --target test .` |
| `vuln` | `scripts/ci/vuln.sh` | `govulncheck ./...` at a pinned version; any reachable finding fails |

Reachable vulnerabilities that `govulncheck` finds today are fixed in this work by upgrading the module. If no fixed
version exists, the finding is recorded in §7 and the job's allowlist names its ID. The allowlist is empty unless
something is recorded.

**`nightly.yml`** runs on a schedule (daily, 03:00 UTC) and on `workflow_dispatch`. Its jobs:

| Job | Script | Content |
|---|---|---|
| `security` | `scripts/ci/compose-security.sh` | `docker compose up -d --build --wait`, then `EACP_COMPOSE_TEST=1 go test ./test/security/` |
| `demos` | — | `scripts/demo.sh` (all demos) |
| `bench` | — | `scripts/bench.sh --quick`, with `bench-results/` uploaded as an artifact; baselines are never touched |
| `examples` | `scripts/ci/examples.sh` | runs `examples/setup.sh` and the three examples against the compose stack, failing on any mismatch with their documented results |

**`release.yml`** runs on tags `v*`:

1. **`verify`** calls `ci.yml`. Nothing is released unless it passes.
2. **`binaries`** (`scripts/ci/release-binaries.sh <version>`) builds every `cmd/` binary except the fakes, for
   linux/amd64, linux/arm64, darwin/arm64 and windows/amd64. It uses `CGO_ENABLED=0`, `-trimpath` and the version
   ldflag, and writes archives plus `SHA256SUMS`.
3. **`images`** pushes `ghcr.io/atipongsena/eacp:<version>` (the compose Dockerfile) and
   `ghcr.io/atipongsena/eacp-agt-pdp:<version>` for linux/amd64 only. This job has `packages: write`.
4. **`publish`** creates the GitHub Release from the `CHANGELOG.md` section for that version. A missing section
   fails the job. This job has `contents: write`.

**`.github/dependabot.yml`** runs weekly for `gomod`, `pip` (`/sidecars/agt-pdp`), `docker` and `github-actions`.

### 3.4 Documents

**Bilingual convention.** `X.md` is English and `X.th.md` is Thai. Each file's first line links both
(`English | ไทย`). The Thai is written as natural Thai, not word-for-word. Technical terms (lease, fencing,
`UNKNOWN_OUTCOME`) and every identifier from the code stay as they are. Both files carry the same headings, code
blocks and images in the same order (§3.7 enforces it).

**Bilingual set:**

- `README`
- `docs/ARCHITECTURE`
- `docs/security/THREAT_MODEL`
- `docs/FEATURES`
- `docs/DEMO`
- `docs/RELEASING`
- `CONTRIBUTING`
- `SECURITY`
- `examples/README` and each `examples/*/README`

**English only:** `CODE_OF_CONDUCT.md`, `CHANGELOG.md`, ADRs, MASTER_PLAN, reviews, specs, plans, BENCHMARKS,
INVARIANTS, KUBERNETES and the issue and PR templates.

**README.** It never calls the project a portfolio or a showcase. It is detailed rather than short, and its sections
are, in order:

1. **What EACP is and the problem it solves.** Concrete failures: a retried agent call that orders twice; an
   approval that authorises a different action; a credential the agent holds; an outcome nobody knows.
2. **A screenshot of the console's overview.**
3. **Guarantees and where they stop.** Slice A's goal and ADR-001 §3a's conforming-deployment scope.
4. **Architecture.** A Mermaid diagram of the components and networks, and a paragraph for each component.
5. **The life of an action.** A Mermaid sequence diagram, then each step explained: submit, govern, approve, release,
   claim, dispatch intent, execute, complete or reconcile.
6. **A tour of the console.** A screenshot and an explanation for each of approvals, execution with its evidence,
   fleet, incidents, dependencies (blast radius) and cost.
7. **Quick start.** `docker compose up -d --build`, `examples/setup.sh` and example 01, with excerpts of its real
   output.
8. **The ten modules.** One paragraph each, linked to its ADR.
9. **Evidence of quality.** The invariant map, the security tests, the benchmarks (figures only as §105 permits) and CI.
10. **Status and what is not built yet.** For example, inbound A2A, global and run kill scopes, multi-region.
11. **How this was built.** An account of the method, disclosed plainly:
    - ADRs first;
    - a written spec and plan per phase;
    - test-first implementation with `-race`;
    - independent reviews;
    - the AI assistants used (Claude Code, and Codex for early phases).

    It links `AGENTS.md`, `docs/superpowers/` and `docs/reviews/`.
12. **Contributing, security, license.**

The current "What exists today" table and phase list move, updated to the current phases, to `docs/FEATURES.md`.
Nothing is lost.

**Other documents:**

- **`docs/ARCHITECTURE.md`:**
  - components and their responsibilities;
  - compose networks and trust boundaries (`agents`, `core`, `erp`, `llm`, `bus`);
  - PostgreSQL as the only authority and NATS as signals only;
  - the action state machine;
  - the execution fabric (lease, fencing, dispatch intent, reconciliation);
  - governance through the PDP;
  - the LLM gateway;
  - HA and Kubernetes;
  - pointers to each ADR.
- **`docs/security/THREAT_MODEL.md`**, the path MASTER_PLAN §68 already names:
  - assets (connector secrets, provider keys, the audit journal, tenant data);
  - trust boundaries;
  - STRIDE threats per boundary, each with its mitigation, the ADR that decides it and the test that enforces it
    (from `docs/INVARIANTS.md`);
  - residual risks and assumptions (conforming deployment, a trusted database administrator).

  It draws on MASTER_PLAN §67–§70 and the ADRs and invents no control.
- **`CONTRIBUTING.md`:**
  - prerequisites and setup (the commands in `AGENTS.md`);
  - the rules: an ADR before a normative change; failing test first; `-race`; never weaken a test or fail-closed
    behaviour; the RLS convention; conventional commit messages;
  - how to run each test tier;
  - Apache-2.0 inbound = outbound, no CLA.
- **`SECURITY.md`:**
  - report privately through GitHub's private vulnerability reporting; no invented e-mail address;
  - supported versions (the latest release and `main`);
  - the secrets under `deployments/*/secrets/` and `deployments/k8s/` are development values for the fake services
    and are never valid anywhere else.
- **`CODE_OF_CONDUCT.md`** adopts the Contributor Covenant 2.1 by reference and link, with how to report.
- **`docs/RELEASING.md`:**
  - the steps: update `CHANGELOG.md`, tag, push the tag, check the release;
  - what the workflow does;
  - the owner's post-push checklist: branch protection on `main`, private vulnerability reporting, GHCR package
    visibility, Dependabot alerts.
- **`CHANGELOG.md`** is in Keep a Changelog format, with v0.1.0 summarising the phases.
- **`.github/ISSUE_TEMPLATE/`** holds `bug_report.yml`, `feature_request.yml` and `config.yml`; the config sends
  security reports to `SECURITY.md` and disables blank issues. `.github/pull_request_template.md` holds the checklist:
  tests first, `-race`, ADR or docs updated, both languages updated.
- **`docs/MASTER_PLAN.md`** gets its 595 Thai lines translated in place.
  - Structure, numbering, code blocks and meaning are unchanged.
  - Where the plan conflicts with an ADR, the text is still translated faithfully; the ADR still wins.

### 3.5 Illustrations

- **Diagrams** are Mermaid in the markdown. GitHub renders Mermaid natively, and diagrams stay editable in review.
  Labels are in each file's language.
- **Screenshots** are PNGs under `docs/images/`, shared by both languages; the console is English.
  - `scripts/screenshots.sh` produces them with `tools/screenshots/`, a separate Go module (its own `go.mod`), so the
    main module gains no dependency.
  - The tool drives headless Chrome or Edge with chromedp. Its API is verified against the module source before use.
  - The script:
    1. runs `examples/setup.sh` and the three examples on the compose stack;
    2. creates two scenarios for the incidents and execution views: a kill of one agent version, and an action that
       ends `UNKNOWN_OUTCOME` through a Fake ERP scenario;
    3. signs in to `/ui/` with the operator key from `examples/.env`, which is typed into the page and never printed,
       logged or written;
    4. captures each view at 1440×900: overview, approvals, execution (list and one action's evidence), fleet,
       incidents, dependencies and cost.
  - Every image is inspected by eye before it is committed: no key, token or secret may appear.
- **Terminal output** in the README and examples is real output from running them, trimmed, in code blocks.

### 3.6 Examples

All examples run against the default compose stack.

- **`examples/setup.sh`** creates:
  - one tenant;
  - two admins (the two-person rules need two people);
  - an operator;
  - one agent with an allowlist for the Fake ERP tool and the fake LLM model.

  Keys are bring-your-own. They are generated locally with `openssl rand`, registered, and stored only in
  `examples/.env` (git-ignored, mode 600 where the OS allows). They are never echoed. Running it twice is safe: it
  reuses an existing `.env`.
- **`examples/01-agent-action/`** (bash and curl):
  1. the agent submits a purchase order;
  2. governance requires approval;
  3. two approvers vote;
  4. the worker executes against the Fake ERP;
  5. the script fetches the action's evidence and verifies the audit chain.
- **`examples/02-llm-gateway/`** (Python, the official `anthropic` SDK pinned in `requirements.txt`):
  - the agent calls the gateway with its own EACP key as the API key and `base_url` set to the gateway;
  - a model outside the allowlist is refused;
  - the call appears in the ledger and the cost view.
- **`examples/03-governance-as-code/`** walks a YAML bundle through:
  - `eacpctl bundle validate` and `plan`;
  - submission and approval by a second admin;
  - `drift` showing none.
- **Each example's README** (both languages) states the expected result. `scripts/ci/examples.sh` fails when a run
  differs.

### 3.7 The guard: `test/opensource`

This package runs in `go test ./...`. Each check is written first and seen to fail before the repository is changed.

| Test | Asserts |
|---|---|
| `TestChecklistFilesExist` | the §112 list: README pair, ARCHITECTURE pair, `docs/adr/`, THREAT_MODEL pair, `docker-compose.yml`, `examples/` with each README pair, `.github/workflows/ci.yml`, `docs/BENCHMARKS.md`, CONTRIBUTING pair, `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `.github/ISSUE_TEMPLATE/`, RELEASING pair, `CHANGELOG.md`, SECURITY pair, `CODE_OF_CONDUCT.md` |
| `TestMarkdownLinksResolve` | every relative link and image in every tracked `.md` file points to an existing file |
| `TestThaiOnlyInThaiFiles` | Thai characters (U+0E00–U+0E7F) appear only in `*.th.md` files; elsewhere the one Thai word allowed is the language label ไทย |
| `TestTranslationsMatch` | each pair has the same sequence of heading levels, the same number of fenced code blocks and the same image targets |
| `TestReadmeNeverSaysPortfolio` | neither README contains "portfolio" (any case) |
| `TestThirdPartyNoticesCoverDependencies` | every module path in `go.mod` and every package in the sidecar's `requirements.txt` appears in `THIRD_PARTY_NOTICES.md` |
| `TestModulePath` | `go.mod` declares `github.com/atipongsena/eacp` |
| `TestWorkflowActionsArePinned` | every `uses:` in `.github/workflows/*.yml` ends in `@` and 40 hex digits |
| `TestLicenseIsApache2` | `LICENSE` contains the Apache 2.0 title and its "END OF TERMS AND CONDITIONS" line |

`internal/bench`'s `TestReadmeNumbersComeFromTheBaseline` extends to `README.th.md`: benchmark figures in Thai prose
must also be result-table cells.

## 4. Testing

- **TDD.** `internal/version`, the `eacpctl version` command, the startup log and every `test/opensource` check are
  written failing first.
- **Local CI scripts.** Each `scripts/ci/*.sh` is run locally and must pass, and `actionlint` passes on all
  workflows. `release-binaries.sh v0.0.0-test` builds every target and its `SHA256SUMS`, which is checked with
  `sha256sum -c`.
- **Examples** are run end to end on a fresh compose stack, twice (the second run proves `setup.sh` is re-runnable).
- **Screenshots** are generated by the script and inspected.
- **Full suite.** `go vet ./...`, `go test -race ./...` with the PostgreSQL DSN (the slow packages may need
  `-timeout 40m`), helm, node, and the sidecar test stage.
- **Rename check.** After the module rename, `go build ./...` and `docker compose build` both pass.

## 5. Risks

- **Workflows cannot run until pushed.** The scripts carry the logic and are verified locally; actionlint catches YAML
  and expression errors. The first run on GitHub may still expose runner differences (Docker Compose version, disk).
  `docs/RELEASING.md` says to watch the first run.
- **`govulncheck` findings** may force dependency upgrades that touch pinned behaviour. Each upgrade runs the full suite.
- **Screenshot tooling** needs a local Chrome or Edge. The script fails clearly without one, and the images are
  committed, so readers never need it.
- **Two languages drift apart.** `TestTranslationsMatch` checks structure, not meaning. Review checks meaning, and the
  PR template asks for both.

## 6. Done

- Every §112 item exists and `test/opensource` passes.
- The module is `github.com/atipongsena/eacp`, and everything builds and passes.
- CI scripts pass locally and actionlint is clean.
- The README pair is illustrated with real screenshots and diagrams.
- The examples run end to end.
- MASTER_PLAN has no Thai.
- MASTER_PLAN §112 records the status and `AGENTS.md` is updated (status, layout, the bilingual rule).
- The branch is ready to push, and the owner is told exactly what to do on GitHub.

## 7. Rulings made during planning and implementation

(None yet.)
