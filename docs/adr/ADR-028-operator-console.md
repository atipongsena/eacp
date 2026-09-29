# ADR-028: The operator console

Status: Accepted (Rev 1.0, 2026-09-26; Rev 1.1, 2026-09-29). Scope: Phase 22b (MASTER_PLAN §55–§57 and §94); Rev 1.1 is Phase 26-UI (design system and language).
Related: ADR-027 (incidents and the SOC summary), ADR-016 (kill switch), ADR-022 (circuits), ADR-023 (MCP tools),
ADR-024 (fleet operations), ADR-005 (approvals), ADR-014 (NATS carries signals).

## Context

Phase 22a gave operators the incident domain and `GET /v1/soc/summary`, but only through the API and eacpctl.
MASTER_PLAN §94 asks for one screen over the operator areas: Inventory, Fleet, Security, Approvals, Execution,
Dependencies, Cost and Incidents. §56 wants the incident view to put its containment one step away, and §57 says the
operator decides.

The console shows data that other parties control. MCP servers write tool descriptions and display hints (ADR-023:
untrusted), and agents supply action targets and payloads. A web UI is also a new place for an operator's key to leak.

## Decision

### 1. A client, never an authority

The console is static files that call the existing `/v1` API with the operator's own principal key.
- It adds no API route, table, role or migration.
- Role checks in the console only hide links and buttons. The API and PostgreSQL enforce every rule again, as for
  eacpctl.
- Every refusal (a 403, or a 409 from a two-person rule or a stale state) is shown with the server's detail. A
  write is never retried automatically.
- A gap in the API is recorded (below) and deferred, never worked around.

### 2. Serving

- **Package.** `internal/ui` embeds `static/` with `go:embed`. controlplane-api serves it at `GET /ui/` and
  redirects `/ui` there. The console has the same origin as the API, so it needs no CORS.
- **Files.** Only the embedded `.html`, `.js` and `.css` files are served, with explicit content types. A
  directory or an unknown file is 404, never a listing.
- **Setting.** `EACP_UI` is `on` (default) or `off`. Any other value fails startup.
- **Headers.** Every console response carries this policy:
  ```text
  Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self';
    connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none';
    require-trusted-types-for 'script'; trusted-types 'none'
  ```
  It also carries `nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `Cache-Control: no-store`,
  same-origin opener and resource policies, and a restrictive `Permissions-Policy`.
- **API JSON.** API JSON responses now carry `X-Content-Type-Options: nosniff` too.

### 3. Key custody

- The operator pastes a principal key into a password field.
- The key lives in one JavaScript closure (`session.js`). It is never written to `localStorage`, `sessionStorage`,
  IndexedDB, a cookie or a URL, and never logged.
- `GET /v1/me` checks the key before it is kept. An agent key is refused, because `/v1/me` answers principals only.
- A reload, sign-out, any 401, or 30 minutes without pointer or key input drops the key. Polling does not count as
  activity, and an expired session refuses to authorize a request even before the idle check runs.
- There is no cookie authentication, so there is no CSRF surface. Every request uses `credentials: 'omit'` and
  `redirect: 'error'`.

### 4. Hostile data is text

- The DOM is built only by `dom.js` `h()`: elements from an allow-list, attributes from an allow-list, event
  handlers only as functions, links only to `#/`, and every string as a text node.
- `TestConsoleUsesNoDangerousSinks` bans, anywhere in the served files:
  - HTML sinks, `eval` and code from strings;
  - `javascript:`, browser storage, `document.cookie` and dynamic `import(`;
  - any `http://` or `https://` URL, and `fetch(` outside `api.js` and `session.js`.
- Trusted Types in the CSP backs this up in browsers that enforce it.
- Routing is by hash and carries ids and filters only. Path segments and query values are validated, and every id
  put into an API path is validated again.

### 5. Every write is deliberate

- Every containment and incident write passes through `confirm.js`. The dialog restates the target and the effect
  as text and collects the reason the API takes.
- Only one dialog is open at a time, and a view sends one write at a time (the incident page, "Link this to the incident"), so a double click never sends twice.
- The polled incident list holds no form: a manual incident is opened on its own page (`#/incidents/new`), so a poll never discards what the operator typed or the server's answer.
- A `tenant` kill needs the word `tenant` typed.
- A fleet operation is always run as `dry_run` first. The confirmed request is the previewed one without `dry_run`,
  and the console never sends `selector.all`.
- The §56 "recommended containment" links only navigate and pre-fill a form. From an incident, a completed
  containment offers to link itself to the incident, which is a journaled timeline event.

### 6. One route table

- `api.js` `ROUTES` is the single list of every call the console makes (method, path template, allowed query keys).
- `TestEveryConsoleCallIsARealRoute` resolves each entry on the real API mux to the same pattern. It also checks
  that every `/v1` literal in the console is a `ROUTES` path, and every `.call(` names a `ROUTES` entry by a string
  literal.

### 7. Freshness

The overview and the incident list poll every 15 s while the tab is visible and no dialog or input has focus. Other
pages refresh on navigation. There is no websocket and no NATS path to the browser: signals never carry authority
(ADR-014).

## Consequences

- Operators get the §94 areas in one place. Containment is two clicks and one confirmation away from an incident,
  and nothing new decides anything.
- The console depends on no JavaScript package and has no build step. Its pure modules are unit-tested with
  `node --test`: `TestJavaScriptUnitTests` runs them and fails when `EACP_UI_NODE_REQUIRED=1` and node is missing.
- Detection latency in the console is the incident evaluator interval plus the 15 s poll.

## Revision 1.1: design system and language (Phase 26-UI)

Rev 1.1 changes how the console looks and speaks. It adds no route, no authority and no served origin, and every rule of
Rev 1.0 still holds (`TestConsoleUsesNoDangerousSinks` and `TestEveryConsoleCallIsARealRoute` are unchanged in force).
The design is informed by `research/STUDIO_REFERENCES.md`: ideas only, no copied code or visuals.

### 8. Design system

- **Tokens, not colours.** `app.css` defines semantic pairs (`--surface`, `--text`, `--accent`, `--danger`, `--warning-text`,
  ...) for light and dark, following the value scales of Radix Colors (MIT). The scheme follows the browser
  (`prefers-color-scheme`); nothing is stored. `jstest/tokens.test.mjs` computes WCAG AA contrast for every text pair
  in both schemes and fails below 4.5:1 (3:1 for non-text).
- **Components are functions in `dom.js`.** `statCard`, `banner`, `emptyState`, `badge`, `loading`, `pageHeader`,
  `relTime` and `icon` build DOM through `h()` only. A banner has `role="status"`; a zero stat is dimmed; an empty
  table says what it would show and why it is empty.
- **Icons are CSS.** `h()` allows no `svg` tag and CSP allows no `data:` image, so each Bootstrap Icons (MIT,
  commit 6945b70) glyph is one `clip-path: path()` per pseudo-element on an `.icon.i-<name>` span, keeping the
  original path's fill rule. `icon(name)` refuses an unknown name. No SVG file, no font and no image is served.
- **Layout.** A grouped side navigation (Monitor, Operate, Govern) on wide screens; a wrapping top bar under 800 px.
  Motion is off under `prefers-reduced-motion`.

### 9. Language

- **English and Thai** ship together. `i18n.js` exports `t('English text', {params})`: English is the key, `{name}`
  parameters are filled in a single pass, and a missing Thai entry falls back to the English text, never to a blank.
- **Catalogue.** `messages.th.js` maps each English key to Thai. `sameOnPurpose` lists the terms kept in English on
  purpose (product and protocol words).
- **Choice is never stored.** The language is carried in the address as `?lang=th|en`, written with
  `history.replaceState` (no reload, so the session survives); without it, the browser's language list decides.
  `TestLanguageChoiceIsNotStored` pins this. Values that come from the server (state names, ids, reason codes) are
  shown as they are.
- **Completeness is a test.** `TestEveryTranslatedTextHasAThaiEntry` (Go, so `go test` alone catches it) and
  `jstest/i18n.test.mjs` require that every `t()` call has a string literal, every literal has a Thai entry, and
  every catalogue entry is used. `index.html` still references only `app.js` and `app.css`.

### Rev 1.1 consequences

- Two new served files (`i18n.js`, `messages.th.js`), listed in `consoleFiles`. Adding a `t()` text means adding its
  Thai entry in the same change.
- A third language is a new catalogue plus one entry in `LANGS`.
- Not done here (unchanged from Rev 1.0): any write flow beyond Rev 1.0, a directory of people, a dark/light toggle
  (the browser decides).

## Unresolved assumptions (conservative choices)

| Assumption | Choice |
|---|---|
| Keeping the key across reloads | Never: memory only; a reload signs out. |
| Idle timeout | 30 minutes without pointer or key input. |
| Tenant kill from the browser | Allowed, behind a typed confirmation word. |
| Fleet `selector.all` | Never sent by the console. |
| Action resolution, FinOps alert acknowledgement, release management, fleet rollback, registry and bundle editing | Stay in eacpctl. |
| Showing people | Principals are shown as ids: there is no principal directory route, so assignment is "assign to me" or a pasted id. |
| Tool listing | Per connector (at most 50 connectors fetched on the Security page); there is no tenant-wide tool route. |
| A browser offering to save the key | Residual risk. The sign-in field is a password field with `autocomplete="off"`, but a browser may still offer to save it after sign-in. Operators decline; managed browsers should disable the password manager for the console's origin. The console itself never stores the key. |
