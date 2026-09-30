# Phase 27a-3b: the Agent Studio page — implementation plan

**Goal:** `/studio/`: the form with the leave-balance template, status in plain words, approvals, runs; Thai; screenshots.

**Spec:** `docs/superpowers/specs/2026-09-30-phase-27a-3b-studio-page-design.md`

## Global constraints

- ADR-028 rules hold for every new file: `dom.js` only, no sink, no storage, `fetch(` only in `api.js`/`session.js`,
  every `.call('literal')` in ROUTES, every visible text `t('literal')` with a Thai entry, colours as tokens.
- No new API route, table or migration; `/v1/me` gains `groups` only.
- Never print, log or store a key or the master; screenshots never show a key.
- Commit as the user, no Co-Authored-By trailer.

## Tasks

1. **`/v1/me` groups.** Failing test in `internal/api/api_test.go` (a member sees their groups; a removed
   membership disappears; a principal without groups gets `[]`). `registry.Service.MemberGroups(ctx, a)
   ([]GroupRef, error)` with `GroupRef{ID, Name, DisplayName}`; `me` adds `groups`. `session.js` keeps `groups`.
2. **Serving `/studio/`.** Failing tests in `internal/ui/ui_test.go` and `lint_test.go` (served with headers,
   redirect, index rule for `studio.html`, `consoleFiles`). `ui.Register` mounts `/studio/`; `Handler` takes the
   prefix and index. `studio.html`, `studio.js` (boot, sign-in, nav, render, poll), `router.parse(hash, areas)`,
   `signin.js` shared by `app.js` and `studio.js`.
3. **Pure modules.** Failing `jstest/studio.test.mjs`. `studio/templates.js` (TEMPLATES), `studio/definition.js`
   (`toDefinition(form)`, `fromDefinition(def)`, `emptyForm()`), `studio/status.js` (`statusSentence(v)`,
   `stageIndex(v)`, `failureSentence(reason)`, `STAGES`).
4. **Views.** `studio/agents.js` (list, detail, run form), `studio/form.js` (new agent and new version),
   `studio/requests.js`, `studio/run.js`; ROUTES entries; CSS; Thai entries.
5. **Screenshots.** `examples/setup` Studio step and env keys; `hr-mcp` for the examples tenant in the worker's
   manifest; `tools/screenshots -page`; `scripts/screenshots.sh` Studio flow; images in `docs/USER_GUIDE(.th).md`.
6. **Docs and verification.** ADR-028 Rev 1.2, AGENTS.md, FEATURES pair, CHANGELOG, MASTER_PLAN, program spec,
   memory. `go vet`, `go test -race` with the database, node tests (`EACP_UI_NODE_REQUIRED=1`), compose security,
   `scripts/demo.sh`, `scripts/ci/examples.sh`, `scripts/screenshots.sh`. Commit, report, stop.
