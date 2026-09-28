## What and why

<!-- What does this change, and why? Name the issue and the ADR it follows. -->

## Checklist

- [ ] The tests came first: each new test failed for the right reason before the change made it pass.
- [ ] `go vet ./...` and `go test -race ./...` pass with `EACP_TEST_ADMIN_DSN` set (PostgreSQL tests ran, not skipped).
- [ ] No test or fail-closed behaviour was weakened to make CI green.
- [ ] A normative change has an ADR or ADR revision, and the docs (and `docs/INVARIANTS.md` if a guarantee moved) are updated.
- [ ] Both languages are updated: every changed `X.md` has its `X.th.md` updated too.
- [ ] No secret value is logged, stored, journaled or committed.
