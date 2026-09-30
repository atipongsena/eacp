# Phase 27a-3b: the Agent Studio page (design)

Status: the owner asked to proceed to the end of the phase ("next and dev until finished phase", 2026-09-30); the
decisions below follow the owner's 27a-3 answers (approvals in `/studio/`, the template static in the page, split in
two). Program: `2026-09-29-agent-studio-program-design.md` (sections 7 and 8.3). Rules: ADR-028 (Rev 1.2 records this
phase) and ADR-033. Previous sub-phase: `2026-09-30-phase-27a-3a-studio-packaging-demo-design.md`.

## 1. What 27a-3b delivers

An employee opens `/studio/`, signs in with their own principal key, starts from the leave-balance template, saves
an agent and sees, in plain words, where it is: waiting for a registry approver, waiting for its runtime key, ready.
A registry approver approves the request and the runtime's key in the same page. The author runs the agent and
reads the answer, or a plain reason when it fails. English and Thai, screenshots in the user guide.

The thin slice (program section 8.3) stays narrow: `tool_call` and `respond` steps, one form, one template. No Hub,
no listing, no clone, no run list, no `llm` or `branch` step.

## 2. Decisions

- **A second page, not a console area.** `/studio/` serves `studio.html`, which loads `studio.js`. It shares the
  console's modules (`api.js`, `session.js`, `dom.js`, `confirm.js`, `i18n.js`, `router.js`, `app.css`) and every
  ADR-028 rule: the same embedded file set and headers (CSP, no-store), DOM only through `dom.js`, the key in
  `session.js` memory only (a key does not follow a link between `/ui/` and `/studio/`; each page signs in),
  every write through `confirm.js`, every call in `api.js` ROUTES. Serving is the only Go change in `internal/ui`:
  `/studio/` answers `studio.html`, every other name answers the same file as under `/ui/`, and `/studio` redirects.
- **One API addition: `/v1/me` lists the caller's groups** (`groups: [{id, name, display_name}]`, active
  memberships only). An author must pick a department they belong to (PostgreSQL checks it in `eacp.studio_save`),
  and no route lists groups to a non-admin. It is a read of the caller's own rows and grants nothing. No other
  route, table or migration changes.
- **Runs are not listed.** There is no list route (a run list is Hub work). The page keeps the ids of runs started
  in this tab, in memory, and shows a run by its id (`#/runs/<id>`).
- **The template is static in the page** (`studio/templates.js`), deep-equal to the demo fixture
  `test/demo/testdata/leave-balance.json` (a node test pins it).
- **Plain words are the page's, not the server's.** Status (`waiting_for_approval`, `waiting_for_credential`,
  `ready`, `rejected`, `replaced`, other states) and failure reasons (`credential_*`, `version_replaced`, `action_*`,
  `result_unavailable`, `answer_too_large`, `deadline_exceeded`) map to translated sentences in a pure module
  (`studio/status.js`); an unknown value is shown as sent, never hidden. A refusal shows the API's `detail`.
- **Areas** (hash routes, `router.parse` takes the page's own area list):
  - `agents`: the caller's agents (authors: their own; approvers and auditors: all), each with its stage; an agent
    page shows the stepper (saved, approved, key approved, ready), the version history, the definition in plain
    form, "New version" (the form, pre-filled) and, when ready, the run form (its declared inputs).
  - `new`: the form (`?template=leave-balance` pre-fills it). Fields: template, name, display name, description,
    department (the caller's groups), inputs (name, max length), steps in order (tool: a picker of the tenant's
    executable tools with read-only and risk badges; operation, target, resource, payload as JSON; the last step
    `respond` text), timeout. Save opens `confirm.js` with the tools it asks for and who approves next.
  - `requests`: registry approvers only. Each waiting version with its tools in plain words (approve or reject, a
    reason required) and each runtime key waiting (approve). The API refuses the author's own request; the page
    says so before asking.
  - `runs`: one run by id: state, each step's action state, the answer (requester only) or the reason. It polls
    every 2 s while `QUEUED` or `RUNNING`.
- **Nav by role** (it only hides links; the API decides): My agents (`studio_author`, `registry_approver`,
  `auditor`), New agent (`studio_author`), Requests (`registry_approver`).
- **Screenshots** come from `scripts/screenshots.sh` against the running compose stack. `examples/setup` adds the
  Studio cast to the examples tenant (stella, `studio_author` in group `hr`; the `studio-runtime` service principal;
  the `hr-mcp` connector, certified; a policy rule for target `hr`), with keys only in `examples/.env`. The script
  starts `agent-runtime` with a fresh random master written through stdin (never printed, never on disk in the
  repository), drives the flow through the API and captures `studio-new`, `studio-agent`, `studio-requests` and
  `studio-run`. `tools/screenshots` gains `-page ui|studio`.

## 3. Tests

- **Go (`internal/ui`):** `/studio/` and `/studio/studio.js` are served with the console headers; `/studio`
  redirects; `studio.html` loads only `studio.js` and `app.css` (the index rule, for both pages); the new files are in
  `consoleFiles`; the route contract, the sink ban and the Thai completeness tests cover the new modules unchanged.
- **Go (`internal/api`):** `/v1/me` lists exactly the caller's active groups (a removed membership disappears).
- **node (`jstest/studio.test.mjs`):** the template equals the fixture; the form's `toDefinition`/`fromDefinition`
  round-trip the template and build the server's shape; every status and failure reason has a sentence; an unknown
  one is shown as sent; the stepper index for each status; `router.parse` with the Studio areas.
- **examples:** `examples/setup` sets up the Studio cast (its unit test covers the env keys); `scripts/ci/examples.sh`
  still runs each example twice.

## 4. Out of scope

The Hub, a run list, department-wide discovery, the `llm` and `branch` steps, the test mode, schedules, an operator
view of runs or of `revoke-all` in the page, a link between the two pages' sessions.
