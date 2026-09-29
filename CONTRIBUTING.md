[English](CONTRIBUTING.md) | [ไทย](CONTRIBUTING.th.md)

# Contributing to EACP

Thank you for considering a contribution. EACP decides whether AI agents may act on enterprise systems, so its
rules are strict: a change is accepted when it keeps every guarantee and proves it with tests. This page explains
how to set up, what the rules are and how to run each tier of tests.

## Before you start

- For a bug, open an issue with the steps to reproduce it. For a suspected vulnerability, do not open an issue:
  follow [SECURITY.md](SECURITY.md).
- For a new capability or a change to a guarantee, open an issue first. A normative change needs an ADR in
  [`docs/adr/`](docs/adr/README.md) before the code, and it is easier to agree on the ADR before anyone writes it.
- Read [AGENTS.md](AGENTS.md). It is written for AI coding assistants and for people alike, and it lists the rules
  every change must keep.
- To learn how EACP is used before changing it, read the [user guide](docs/USER_GUIDE.md).

## Setup

You need Go (the version in `go.mod`), Docker with Compose v2, Python 3 and Bash (Git Bash on Windows). Node.js is
needed for the console's JavaScript tests, and Helm v4.3.0 for the chart tests
(`scripts/ci/helm.sh` fetches it into `.tools/` on Linux).

```bash
python3 deployments/docker/secrets/prepare_fakeerp_token.py
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
go vet ./... && go test -race ./...
```

Without `EACP_TEST_ADMIN_DSN`, the PostgreSQL integration tests are skipped, not passed. Run them before you open a
pull request.

## The rules

These come from [MASTER_PLAN](docs/MASTER_PLAN.md) §106–§107 and [AGENTS.md](AGENTS.md):

- **An ADR before a normative change.** ADRs are the source of truth, and an ADR wins over the master plan. A change
  to a state transition, a guarantee, a trust boundary or an authority needs an ADR or a revision of one.
- **A failing test first.** Write the test, watch it fail for the right reason, then make it pass.
- **Run the tests with `-race`.** Concurrency is where this system's guarantees live.
- **Never weaken a test or fail-closed behaviour to make CI green.** If a test is wrong, fix the test and say why in
  the pull request.
- **Never invent an upstream API.** Verify it against the module source or the documentation first, and record what
  you verified in [`research/REFERENCES.md`](research/REFERENCES.md) when it matters.
- **Follow the Row-Level Security convention** in `migrations/00001_foundation.sql` for every tenant-scoped table,
  and add each new table, policy or `SECURITY DEFINER` function to `internal/storage/rls_catalog_test.go`.
- **Keep PostgreSQL the authority.** Registry rules live in triggers, and every privileged write binds its actor and
  appends its audit event in the same transaction.
- **Never log, store or journal a secret value.**
- **Never claim exactly-once.** Use the terms in MASTER_PLAN §21.
- **Keep both languages.** User and contributor documents exist in English (`X.md`) and Thai (`X.th.md`). Change
  both, keeping the same headings, code blocks and images in the same order; `test/opensource` checks it.
- **Write conventional commit messages**, for example `feat: …`, `fix: …`, `docs: …`, `ci: …`, `refactor: …`.

## Tests

| Tier | Command | Needs |
|---|---|---|
| Unit and PostgreSQL integration | `go vet ./... && go test -race ./...` | PostgreSQL from compose and `EACP_TEST_ADMIN_DSN` |
| Lint | `bash scripts/ci/lint.sh` | Go |
| Console JavaScript | `(cd internal/ui && node --test jstest/*.test.mjs)` | Node.js |
| Helm chart | `bash scripts/ci/helm.sh` | Helm v4.3.0 in `.tools/` |
| AGT sidecar and conformance | `docker build -f sidecars/agt-pdp/Dockerfile --target test .` | Docker |
| Network isolation | `bash scripts/ci/compose-security.sh` | The full compose stack |
| Examples | `bash scripts/ci/examples.sh` | The full compose stack, `jq`, Python 3.10 or later |
| Demos | `scripts/demo.sh` | Docker |
| Kubernetes | `bash scripts/k8s-e2e.sh` | minikube |

Pull requests run the first tiers in CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)); the compose,
example, demo and benchmark runs are nightly ([`.github/workflows/nightly.yml`](.github/workflows/nightly.yml)).

## Pull requests

Keep a pull request to one purpose. Describe what changes and why, name the ADR it follows, and list the tests that
prove it. The pull request template has the checklist.

## License

EACP is licensed under the [Apache License 2.0](LICENSE). By contributing, you agree that your contribution is
licensed under the same license (inbound = outbound, as section 5 of the license says). There is no contributor
license agreement.
