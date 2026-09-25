# Phase 22b — Operator console (design)

Date: 2026-09-26 · Status: approved by the owner in chat (scope: "read all + incidents + containment";
build: "embedded, no build"; design sections 1–8 approved: "yes, write the spec and plan").
Scope: MASTER_PLAN §55–§57 and §94 (Phase 22). 22a (ADR-027) delivered the incident domain and the SOC
summary API; 22b is the operator web UI on top of the existing `/v1` API. Normative decision: ADR-028.

## 1. Intent

Operators need to see what is wrong and act on it from one screen: the §55 counters, the §56 incident
view with its blast radius, and the §94 areas (Inventory, Fleet, Security, Approvals, Execution,
Dependencies, Cost, Incidents). The §56 containment responses (kill, tool quarantine, circuit
disable, fleet pause) are one click away from the incident, and the operator decides (§57).

Success: on the Slice C demo stack an operator signs in with their key, sees the MCP drift and kill
incidents on the overview, opens the drift incident, sees the affected agents, pauses the affected agent
through a dry-run-then-confirm fleet operation, acknowledges and links the operation, and a second
operator resolves the critical incident. Every refusal (two-person rules, missing role) comes from the
server and is shown verbatim.

## 2. Principles (non-negotiable)

1. **A client, never an authority.** The console is static files that call the existing `/v1` API with the
   operator's own principal key. **No new API routes, no new tables, no new roles.** Role checks in the UI
   only hide controls; the API and PostgreSQL enforce every rule again. A gap in the API is recorded
   (§10) and deferred, never worked around.
2. **The key never persists.** It lives in one JavaScript module variable. It is never written to
   `localStorage`, `sessionStorage`, IndexedDB, a cookie or a URL, and never logged. A reload, sign-out,
   a 401 answer or 30 minutes without user input drops it.
3. **Hostile data is text.** Tool definitions, MCP display hints, action targets and every string from the
   API may be attacker-influenced (ADR-023: server hints are untrusted). The DOM is built only with
   `createElement`, `textContent` and attribute setters that cannot run code; HTML-parsing sinks are banned
   and a Go test enforces the ban. The CSP forbids inline script and every non-self origin, and requires
   Trusted Types.
4. **Every write is deliberate.** Each containment or incident write needs a reason where the API takes
   one, and goes through a confirm dialog that restates the target and effect. A tenant kill needs the word
   `tenant` typed. A fleet operation is always dry-run first; the confirmed request is the dry-run request
   with `dry_run` removed.
5. **Signals stay signals.** Freshness comes from polling the API while the tab is visible. No websocket,
   no NATS path to the browser (ADR-014).

## 3. Serving

- Package `internal/ui`: `//go:embed static` and `Register(mux *http.ServeMux)`, which mounts
  `GET /ui/` (the files) and `GET /ui` (redirect to `/ui/`). Only files in the embedded tree are served;
  a directory other than the root returns 404, never a listing. `index.html` is served for `/ui/`.
- Content types are explicit per extension: `.html` → `text/html; charset=utf-8`, `.js` →
  `text/javascript; charset=utf-8`, `.css` → `text/css; charset=utf-8`, `.svg` → `image/svg+xml`.
  Any other extension is not embedded (a test pins the file set).
- Every UI response carries:
  - `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self';
    connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none';
    require-trusted-types-for 'script'; trusted-types 'none'`
  - `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
    `Cache-Control: no-store`, `Cross-Origin-Opener-Policy: same-origin`,
    `Cross-Origin-Resource-Policy: same-origin`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`.
- `writeJSON` in `internal/api` additionally sets `X-Content-Type-Options: nosniff` (an API response is never
  sniffed as HTML).
- Configuration: `EACP_UI` = `on` (default) or `off`. `off` mounts nothing (`/ui/` is 404). Any other value
  fails startup (fail closed on a typo). Only controlplane-api reads it.

## 4. Front end structure (plain ES modules, no build)

`internal/ui/static/`:

| file | purpose |
|---|---|
| `index.html` | shell: header, nav, `<main>`, one `<script type="module" src="app.js">`; no inline script or style |
| `app.css` | layout, light/dark via `prefers-color-scheme`, severity colours |
| `app.js` | boot, sign-in, idle timer, router wiring, polling |
| `session.js` | the key holder: `signIn(key)`, `signOut()`, `authHeader()`, `me()`, `touch()`; idle expiry |
| `api.js` | `ROUTES` table (method, path template, name) and `call(name, params, body)`; error mapping |
| `dom.js` | `h(tag, attrs, ...children)` builder (text only), `table()`, `badge()`, `dl()`, `time()` helpers |
| `router.js` | hash router: parses `#/area/id?query` into `{area, id, query}`; ids are validated UUIDs |
| `confirm.js` | the confirm dialog (`<dialog>`), reason field, typed confirmation |
| `views/overview.js` | §55 counters from `soc/summary` |
| `views/incidents.js` | list (filters), detail (§56), timeline, affected snapshot, lifecycle and link forms |
| `views/inventory.js` | agents (list, detail with versions), connectors, tools (definition and risk) |
| `views/fleet.js` | fleet health and agents; fleet operation form (pause, resume, quarantine, release) |
| `views/security.js` | kill states (set/clear), connectors' circuits (disable/enable), quarantined tools (quarantine/release) |
| `views/approvals.js` | eligible approvals, enforced payload digest, vote approve/deny |
| `views/execution.js` | actions by state, action detail and evidence |
| `views/dependencies.js` | blast-radius query form and report |
| `views/cost.js` | FinOps dashboard and alerts |

Each view exports `render(ctx)` returning a DOM node, where `ctx = {api, session, route, navigate,
refresh}`. Views never touch the key.

### 4.1 Sign-in and session

- The sign-in form has one `<input type="password" autocomplete="off" spellcheck="false">`. On submit the
  key goes to `session.signIn`, which calls `GET /v1/me`. On 200 the roles and principal id are kept with
  the key; on 401 the key is dropped and "invalid key" shown. `/v1/me` serves principal keys only, so an agent
  key gets 403: the key is dropped and the console says "a principal key is required".
- Idle: `touch()` on every pointer or key event; a 30 s ticker drops the session after 30 minutes without
  one. Polling does not count as activity.
- Any 401 from any call drops the session and returns to sign-in.
- Sign-out clears the key, roles, the rendered view and pending timers.

### 4.2 API client

- `ROUTES` is the single list of every call the console makes, e.g.
  `{name: 'incident.ack', method: 'POST', path: '/v1/incidents/{id}/acknowledge'}`. `call` substitutes
  `{param}` with `encodeURIComponent` of a validated value (UUIDs match
  `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`; `ref` and slugs match
  `^[A-Za-z0-9._-]{1,128}$`), adds query parameters from an allow-list per route, sends
  `Authorization: Bearer <key>`, `Accept: application/json`, `Content-Type: application/json` with a
  body, `credentials: 'omit'`, `cache: 'no-store'`, `redirect: 'error'`.
- Result: `{ok: true, status, data}` or `{ok: false, status, error, detail}` from the API's
  `{"error","detail"}` body (bundle errors carry `code` and `message`; both are shown as text). A network
  failure is `{ok:false, status:0, error:'network'}`. 401 also triggers sign-out.
- A Go test extracts `ROUTES` and checks every entry resolves on the real API mux to a pattern with the same
  method (§8).

### 4.3 Areas (routes the console calls)

| area | reads | writes (role hint) |
|---|---|---|
| Overview | `GET /v1/soc/summary`, `GET /v1/incidents?state=OPEN&limit=20` | — |
| Incidents | `GET /v1/incidents?state=&severity=&kind=&limit=`, `GET /v1/incidents/{id}` | `POST /v1/incidents`, `/{id}/acknowledge`, `/assign`, `/notes`, `/links`, `/resolve` (operator, admin) |
| Inventory | `GET /v1/agents`, `GET /v1/agents/{ref}`, `GET /v1/connectors`, `GET /v1/connectors/{id}/tools`, `GET /v1/tools/{id}`, `GET /v1/tools/{id}/definitions` | — |
| Fleet | `GET /v1/fleet/health`, `GET /v1/fleet/agents`, `GET /v1/fleet/operations/{id}` | `POST /v1/fleet/operations` (operator, registry_approver) |
| Security | `GET /v1/killswitch`, `GET /v1/connectors`, `GET /v1/connectors/{id}/circuit`, `GET /v1/connectors/{id}/tools` | `POST /v1/killswitch` (operator); `POST /v1/connectors/{id}/circuit/disable`, `/enable` (operator); `POST /v1/tools/{id}/quarantine` (operator, registry_approver), `POST /v1/tools/{id}/release` (registry_approver) |
| Approvals | `GET /v1/approvals`, `GET /v1/approvals/{id}` | `POST /v1/approvals/{id}/votes` (approver) |
| Execution | `GET /v1/actions?state=&limit=`, `GET /v1/actions/{id}`, `GET /v1/actions/{id}/evidence` | — |
| Dependencies | `GET /v1/dependencies/blast-radius?kind=&id=&name=` | — |
| Cost | `GET /v1/finops/dashboard`, `GET /v1/finops/alerts?open=true` | — |

Fleet operations offered: `pause` and `quarantine` (selector: agent slugs, environment, risk class, tool;
never `all` from the UI), `resume` and `release` (by source operation id). Rollback stays in eacpctl.

The incident detail shows the §56 view: title, severity, state, kind, subject, `detail` as a key/value list,
the affected snapshot (confirmed versions table, counts, production-active), and "Recommended containment"
as links to the Security or Fleet form pre-filled for the subject (for example `mcp_drift` → quarantine
the tool or pause the affected agents; `kill` → the kill state; `circuit_open` → the circuit). The links
only navigate and pre-fill; nothing is sent until the operator confirms.

### 4.4 Confirm dialog

`confirm.ask({title, lines, reason: 'required'|'none', typed: string|null, danger: bool})` resolves with
`{reason}` or `null`. The confirm button stays disabled until a required reason is non-blank and any
typed word matches exactly. The dialog restates target ids and names as text.

Uses: every Security write; fleet apply (showing the dry run's targets and skipped rows); vote (showing
the enforced digest); incident acknowledge, resolve (with the resolution code) and manual open; kill with
scope `tenant` (typed `tenant`). Notes, links and assign go straight through (they are journaled and not
containment).

## 5. Polling

The overview and the incident list refresh every 15 s while `document.visibilityState === 'visible'` and
a session exists. Other views refresh on navigation and on a Refresh button. A refresh never re-sends a
write.
The polled incident list holds no form: a manual incident is opened on its own page, `#/incidents/new`.

## 6. Error handling

- Every failed call renders a notice with the HTTP status and the API's `error`/`detail` text.
- 403: "The server refused this." plus the server detail (a two-person rule also answers 403, so the lead never blames a missing role). 409: shown as a conflict (for example the
  two-person rule or a stale state) with the server detail. 404 on a detail route: "not found".
- The UI never retries a write automatically.

## 7. Accessibility and layout

Semantic landmarks (`header`, `nav`, `main`), buttons are `<button>`, tables have `<th scope>`, the dialog
is a native `<dialog>` with focus returned on close, severity is text as well as colour, and the layout
works from 360 px wide.

## 8. Testing

- `internal/ui` (Go):
  - serves `index.html` at `/ui/`, redirects `/ui`, 404 for unknown files and for directories;
  - exact content types, every security header on every response (including 404s from the UI handler);
  - the embedded file set is exactly the expected list (no stray file types);
  - **lint**: no `.js` or `.html` file contains `innerHTML`, `outerHTML`, `insertAdjacentHTML`,
    `document.write`, `eval(`, `new Function`, `Function(`, `setTimeout('`/`setTimeout("`, `srcdoc`,
    `javascript:`, `localStorage`, `sessionStorage`, `indexedDB`, `document.cookie`, `http://`,
    `https://`, `<script>` with inline content, `style=` attributes, or `on*=` HTML attributes; `index.html` references only `app.css` and `app.js`.
- **Route contract** (Go, in `internal/ui`, which imports `internal/api` for the test only): parse
  `ROUTES` from `static/api.js`, build the real mux with `api.New(nil, log).Register(mux)`, and for each
  entry assert `mux.Handler(req)` returns the pattern `"<METHOD> <path template>"`.
- `internal/api`: `writeJSON` sets `nosniff`.
- `internal/config`: `EACP_UI` default `on`, `off`, and an invalid value.
- `cmd/controlplane-api` wiring: `EACP_UI=off` mounts nothing (via a small `mountUI(cfg, mux)` helper test).
- JavaScript: `node --test internal/ui/static/test/` for the pure modules (`router`, `api` path building
  and error mapping with a fake `fetch`, `session` idle expiry with a fake clock, `dom` never producing
  HTML from strings). The Go test `TestJavaScriptUnitTests` runs them when `node` is on PATH and **fails**
  (not skips) when `EACP_UI_NODE_REQUIRED=1` and node is missing; otherwise it skips with a message.
- Manual end-to-end (recorded in the phase notes): the built-in browser against the compose stack with the
  Slice C demo flow from §1, plus the CSP check in the console (no violations).

## 9. Documentation

ADR-028 (operator console), MASTER_PLAN §94 status, AGENTS.md (status line, a rule for the console, layout
`internal/ui`), README (how to open `/ui/`), DEMO.md (a console walk-through step for Slice C).

## 10. Out of scope and known API gaps (deferred)

- Action resolution (unknown outcomes), FinOps alert acknowledgement, registry/bundle/budget/policy editing,
  release management and fleet rollback stay in eacpctl.
- No principal directory route: assign offers "assign to me" and a UUID field; actors show as ids.
- No tenant-wide tool list: the Security view lists tools per connector (one call per connector, at most
  50 connectors shown; more are listed by name only with a link to Inventory).
- No agent-version → agent lookup route: links to an agent use the agent id carried in the payload.
