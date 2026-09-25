# Phase 22b — Operator console Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An embedded, no-build operator web console at `/ui/` on controlplane-api that reads every §94 area and performs the incident lifecycle and containment writes through the existing `/v1` API with the operator's own key.

**Architecture:** `internal/ui` embeds plain HTML/CSS/ES modules (`go:embed static`) and serves them with a strict CSP and security headers. The front end is a hash-routed client: `session.js` holds the key in memory only, `api.js` is the single table of every route the console calls (a Go test proves each one exists on the real API mux), `dom.js` builds DOM from elements and text only, and `confirm.js` is the one dialog every write passes through. Views are one module per area.

**Tech Stack:** Go 1.27 (`embed`, `net/http`), plain ES2022 modules, `node --test` (Node ≥ 22) for the pure JS modules. No npm dependencies, no build step.

**Spec:** `docs/superpowers/specs/2026-09-26-operator-console-design.md` (read it first; ADR-027 for the incident domain).

## Global Constraints

- No new API routes, tables, roles or migrations. The console calls only routes that already exist (spec §2.1).
- The key lives only in a JS closure variable: never `localStorage`, `sessionStorage`, IndexedDB, a cookie, a URL or a log (spec §2.2).
- DOM only through `dom.js` `h()` (elements + text). Banned anywhere in `internal/ui/static`: `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document.write`, `eval(`, `new Function`, `Function(`, string timers, `srcdoc`, `javascript:`, `localStorage`, `sessionStorage`, `indexedDB`, `document.cookie`, `http://`, `https://`, `DOMParser`, `createContextualFragment`, `import(`, a backtick followed by `/v1`, and `fetch(` outside `api.js` and `session.js`. Comments count: never write these words in a comment.
- Every `.call(` in the console passes a **string literal** route name as its first argument.
- Views change children with `dom.js` `replace(el, …)`, which skips `null`; the DOM's own `replaceChildren` would print "null".
- CSP (exact): `default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; require-trusted-types-for 'script'; trusted-types 'none'`.
- `EACP_UI` is `on` (default) or `off`; anything else fails startup.
- Idle sign-out after 30 minutes without pointer or key input; polling every 15 s on the overview and incident list only.
- A tenant kill needs the word `tenant` typed; a fleet operation is always dry-run first; the console never sends `selector.all`.
- Go tests run with `-race`; the DB tests need `EACP_TEST_ADMIN_DSN` (AGENTS.md). Git Bash: prepend `/c/Program Files/Go/bin` to PATH.
- Commit as the user; no Co-Authored-By trailer (memory: commit-attribution).

## Review Focus

1. **Hostile text from the API** (an MCP tool definition containing `<img src=x onerror=…>`, an action target with markup) must render as literal text — pinned by the `dom.test.mjs` "text children stay text" test (Task 2) and the E2E check with a hostile tool description (Task 8).
2. **A session that went idle while a page was open** must not send the key again — `authorization()` refuses once expired (Task 2, `session.test.mjs` "an expired session refuses to authorize").
3. **Double-clicking a write** must not open two confirm dialogs or send twice — `ask()` refuses a second dialog while one is open (Task 2, `confirm.test.mjs` "only one dialog at a time").
4. **Ids from the hash or the API used in a path** (`../soc/summary`, `a/b`) must never reach another route — `buildPath` validates every parameter (Task 2, `api.test.mjs`) and the router rejects `.`/`..` segments (Task 2, `router.test.mjs`).
5. **A refusal from the server** (409 two-person rule, 403 role) must be shown with the server's detail, and the UI must not retry — error mapping test (Task 2, `api.test.mjs`) and the E2E second-operator resolve (Task 8).

---

### Task 1: `EACP_UI` setting and `nosniff` on API responses

**Files:**
- Modify: `internal/config/config.go` (Config struct near `IncidentInterval`; `Load` after the `EACP_INCIDENT_INTERVAL` block; `LogValue`)
- Modify: `internal/config/config_test.go` (append a test)
- Modify: `internal/api/api.go` (`writeJSON`)
- Create: `internal/api/headers_internal_test.go`

**Interfaces:**
- Produces: `config.Config.UI bool` (true unless `EACP_UI=off`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestConsoleSetting(t *testing.T) {
	load := func(v string) (Config, error) {
		m := map[string]string{"EACP_DATABASE_URL": "postgres://app@localhost/eacp"}
		if v != "" {
			m["EACP_UI"] = v
		}
		return Load(env(m), Options{RequireDatabase: true})
	}
	if cfg, err := load(""); err != nil || !cfg.UI {
		t.Fatalf("default: UI = %v, err = %v; want on", cfg.UI, err)
	}
	if cfg, err := load("on"); err != nil || !cfg.UI {
		t.Fatalf("on: UI = %v, err = %v", cfg.UI, err)
	}
	if cfg, err := load("off"); err != nil || cfg.UI {
		t.Fatalf("off: UI = %v, err = %v", cfg.UI, err)
	}
	for _, bad := range []string{"yes", "OFF", "1"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "EACP_UI") {
			t.Errorf("EACP_UI=%q: err = %v, want an EACP_UI error", bad, err)
		}
	}
}
```

Create `internal/api/headers_internal_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An API answer is JSON only; a browser must never sniff it as HTML.
func TestJSONResponsesAreNeverSniffed(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]string{"title": "<img src=x>"})
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/config -run TestConsoleSetting && go test ./internal/api -run TestJSONResponsesAreNeverSniffed`
Expected: FAIL — `cfg.UI undefined` (compile error) and `X-Content-Type-Options = ""`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, after the `IncidentInterval` field:

```go
	// UI serves the operator console at /ui/ (controlplane-api, ADR-028):
	// EACP_UI is "on" (default) or "off".
	UI bool
```

In `Load`, directly after the `EACP_INCIDENT_INTERVAL` check:

```go
	switch get("EACP_UI", "on") {
	case "on":
		cfg.UI = true
	case "off":
		cfg.UI = false
	default:
		errs = append(errs, errors.New(`EACP_UI: must be "on" or "off"`))
	}
```

In `LogValue`, after `incident_interval`:

```go
		slog.Bool("ui", c.UI),
```

In `internal/api/api.go` `writeJSON`, after the `Cache-Control` line:

```go
	w.Header().Set("X-Content-Type-Options", "nosniff")
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/config ./internal/api -run 'TestConsoleSetting|TestJSONResponsesAreNeverSniffed|TestLoad'`
Expected: PASS (the api package's DB tests skip without the DSN; that is fine for this step).

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/api/api.go internal/api/headers_internal_test.go
git commit -m "feat(config): EACP_UI setting; API responses are never sniffed"
```

---

### Task 2: The console's core modules (pure JS) and their unit tests

**Files:**
- Create: `internal/ui/package.json`
- Create: `internal/ui/static/dom.js`, `router.js`, `session.js`, `api.js`, `confirm.js`
- Create: `internal/ui/jstest/fakedom.mjs`, `dom.test.mjs`, `router.test.mjs`, `session.test.mjs`, `api.test.mjs`, `confirm.test.mjs`

**Interfaces:**
- Produces (used by every view and `app.js`):
  - `dom.js`: `h(tag, attrs, ...children) → Element`, `append(el, children)`, `replace(el, ...children)` (use it, never `replaceChildren`, when a child may be `null`), `fmtTime(v) → string`, `cls(v) → string`, `badge(v)`, `link(label, hash)`, `button(label, onclick, {kind, disabled, type})`, `table(columns, rows, empty)` where `columns = [[label, row => node|string], …]`, `kv(pairs)` where `pairs = [[label, node|string], …]`, `json(value)`, `section(title, ...children)`, `notice({status, error, detail})`, `ok(message)`, `field(label, control)`, `input(name, attrs)`, `select(name, options, value)` where an option is `value` or `[value, label]` and `''` is labelled `any`, `values(form) → {name: trimmed string | boolean}`, `stat(label, value, hash)`.
  - `router.js`: `AREAS`, `parse(hash) → {area, parts: string[], query: {}}`, `format(area, parts = [], query = {}) → '#/…'`, `isUUID(v) → boolean`.
  - `session.js`: `IDLE_MS`, `createSession({fetch, now, idleMs}) → {signIn(key) → Promise<{ok} | {ok:false, error, detail}>, signOut(), signedIn(), expired(), touch(), authorization() → 'Bearer …', me() → {principalId, tenantId, roles} | null, hasAny(roles | null) → boolean}`.
  - `api.js`: `ROUTES` (array of `[name, method, path, queryKeys]`), `buildPath(name, params, query) → {method, url}`, `createClient({session, fetch, onUnauthorized}) → {call(name, {params, query, body}) → Promise<{ok:true, status, data} | {ok:false, status, error, detail}>}`.
  - `confirm.js`: `canConfirm({reason, typed}, reasonValue, typedValue) → boolean`, `ask({title, lines, reason: 'none'|'required', typed: string|null, danger, confirmLabel}) → Promise<{reason} | null>`.

- [ ] **Step 1: Write the failing tests**

`internal/ui/package.json` (lets Node load `static/*.js` as ES modules; it is outside `static/`, so it is never embedded or served):

```json
{
  "private": true,
  "type": "module",
  "description": "EACP operator console: plain ES modules, no dependencies, no build. Tests: node --test jstest/*.test.mjs"
}
```

`internal/ui/jstest/fakedom.mjs`:

```js
// A minimal DOM for unit tests: elements keep attributes, listeners and
// children; text nodes keep their data verbatim.
class Node {
  constructor() { this.childNodes = []; }
  append(...children) {
    for (const c of children) this.childNodes.push(typeof c === 'string' ? new Text(c) : c);
  }
  replaceChildren(...children) { this.childNodes = []; this.append(...children); }
  get textContent() { return this.childNodes.map(c => c.textContent).join(''); }
}

export class Text extends Node {
  constructor(data) { super(); this.data = data; }
  get textContent() { return this.data; }
}

export class Element extends Node {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.attributes = new Map();
    this.listeners = {};
    this.open = false;
    this.value = '';
  }
  setAttribute(k, v) { this.attributes.set(k, String(v)); }
  getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); }
  dispatch(type, event = {}) { for (const fn of this.listeners[type] ?? []) fn({preventDefault() {}, ...event}); }
  showModal() { this.open = true; }
  close() { this.open = false; }
  remove() { this.removed = true; }
  focus() {}
}

export function installFakeDOM() {
  globalThis.document = {
    body: new Element('body'),
    activeElement: null,
    createElement: tag => new Element(tag),
    createTextNode: data => new Text(data),
  };
}
```

`internal/ui/jstest/dom.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM, Text} from './fakedom.mjs';
import {h, replace, fmtTime, cls, badge, table, select, kv} from '../static/dom.js';

installFakeDOM();

test('text children stay text, markup included', () => {
  const p = h('p', {}, '<img src=x onerror=alert(1)>', 3, null, false, ['a', ['b']]);
  assert.ok(p.childNodes.every(c => c instanceof Text));
  assert.equal(p.textContent, '<img src=x onerror=alert(1)>3ab');
});

test('replace skips null and false children', () => {
  const el = h('div', {}, 'old');
  replace(el, 'a', null, false, h('span', {}, 'b'));
  assert.equal(el.childNodes.length, 2);
  assert.equal(el.textContent, 'ab');
});

test('refuses elements that can run or load code', () => {
  for (const tag of ['script', 'iframe', 'object', 'embed', 'style', 'link', 'base', 'meta', 'img', 'svg']) {
    assert.throws(() => h(tag), /not allowed/, tag);
  }
});

test('refuses attributes that can run or load code', () => {
  for (const name of ['style', 'src', 'srcdoc', 'formaction', 'action', 'onerror']) {
    assert.throws(() => h('div', {[name]: 'x'}), /not allowed|must be a function/, name);
  }
  assert.throws(() => h('button', {onclick: 'alert(1)'}), /must be a function/);
});

test('links stay inside the console', () => {
  for (const href of ['javascript:alert(1)', 'https://example.org', '//example.org', '#x', '/v1/me']) {
    assert.throws(() => h('a', {href}), /inside the console/, href);
  }
  assert.equal(h('a', {href: '#/incidents'}).getAttribute('href'), '#/incidents');
});

test('events are listeners and empty attributes are skipped', () => {
  const f = () => {};
  const b = h('button', {onclick: f, disabled: false, title: null, type: 'button'});
  assert.deepEqual(b.listeners.click, [f]);
  assert.equal(b.getAttribute('disabled'), null);
  assert.equal(b.getAttribute('type'), 'button');
  assert.equal(h('input', {required: true}).getAttribute('required'), '');
});

test('formatting helpers', () => {
  assert.equal(fmtTime('2026-09-26T10:11:12.345Z'), '2026-09-26 10:11:12Z');
  assert.equal(fmtTime(null), '—');
  assert.equal(fmtTime('not a time'), 'not a time');
  assert.equal(cls('Critical <x>'), 'criticalx');
  assert.equal(badge('high').getAttribute('class'), 'badge badge-high');
});

test('table, select and kv', () => {
  assert.equal(table([['A', r => r.a]], []).getAttribute('class'), 'empty');
  const t = table([['A', r => r.a]], [{a: '<b>'}]);
  assert.equal(t.getAttribute('class'), 'table-wrap');
  assert.ok(t.textContent.includes('<b>'));
  const s = select('state', ['', 'OPEN'], 'OPEN');
  assert.equal(s.childNodes[0].textContent, 'any');
  assert.equal(s.childNodes[1].getAttribute('selected'), '');
  assert.equal(kv([['K', 'v'], null]).childNodes.length, 2);
});
```

`internal/ui/jstest/router.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {AREAS, parse, format, isUUID} from '../static/router.js';

const ID = '0b5e3c1a-7f2d-4c8e-9a61-3d2f5e7a9b10';
const HOME = {area: 'overview', parts: [], query: {}};

test('parses an area, its parts and its query', () => {
  assert.deepEqual(parse(`#/incidents/${ID}?state=OPEN&severity=critical`),
    {area: 'incidents', parts: [ID], query: {state: 'OPEN', severity: 'critical'}});
});

test('falls back to the overview for unknown areas and bad segments', () => {
  for (const hash of ['', '#', '#/', '#/nope', '#/incidents/<img>', '#/incidents/a%20b', '#/inventory/../x',
    '#/inventory/./x']) {
    assert.deepEqual(parse(hash), HOME, hash);
  }
});

test('drops query keys and values outside the allow-list', () => {
  assert.deepEqual(parse('#/incidents?state=<script>&Bad=1&kind=mcp_drift&x=%E0%A4%A').query, {kind: 'mcp_drift'});
});

test('format round-trips through parse and drops empty values', () => {
  const hash = format('fleet', [], {op: 'pause', agents: 'a-bot,b-bot', empty: '', none: undefined});
  assert.equal(hash, '#/fleet?op=pause&agents=a-bot%2Cb-bot');
  assert.deepEqual(parse(hash), {area: 'fleet', parts: [], query: {op: 'pause', agents: 'a-bot,b-bot'}});
  assert.equal(format('inventory', ['tools', ID]), `#/inventory/tools/${ID}`);
});

test('isUUID', () => {
  assert.ok(isUUID(ID));
  assert.ok(isUUID(ID.toUpperCase()));
  for (const v of ['', 'x', `${ID}/`, `../${ID}`, undefined, null]) assert.ok(!isUUID(v), String(v));
});

test('areas are lowercase words', () => {
  assert.ok(AREAS.every(a => /^[a-z]+$/.test(a)));
  assert.equal(AREAS[0], 'overview');
});
```

`internal/ui/jstest/session.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createSession, IDLE_MS} from '../static/session.js';

function fetchReturning(status, body, calls = []) {
  return async (url, init) => {
    calls.push({url, init});
    return {status, ok: status >= 200 && status < 300, json: async () => body};
  };
}
const ME = {principal_id: 'p-1', tenant_id: 't-1', roles: ['operator']};

test('keeps the key and roles only after /v1/me accepts the key', async () => {
  const calls = [];
  const s = createSession({fetch: fetchReturning(200, ME, calls), now: () => 1000});
  assert.equal(s.signedIn(), false);
  assert.throws(() => s.authorization(), /signed out/);
  assert.deepEqual(await s.signIn('  eacp_key  '), {ok: true});
  assert.equal(calls[0].url, '/v1/me');
  assert.equal(calls[0].init.headers.Authorization, 'Bearer eacp_key');
  assert.equal(calls[0].init.credentials, 'omit');
  assert.equal(calls[0].init.redirect, 'error');
  assert.equal(s.authorization(), 'Bearer eacp_key');
  assert.deepEqual(s.me(), {principalId: 'p-1', tenantId: 't-1', roles: ['operator']});
  assert.ok(s.hasAny(['admin', 'operator']));
  assert.ok(!s.hasAny(['approver']));
  assert.ok(s.hasAny(null));
});

test('a rejected key is not kept', async () => {
  const s = createSession({fetch: fetchReturning(401, {error: 'unauthenticated'})});
  const r = await s.signIn('bad');
  assert.equal(r.ok, false);
  assert.equal(r.error, 'unauthenticated');
  assert.equal(s.signedIn(), false);
});

test('an agent key cannot sign in', async () => {
  const s = createSession({fetch: fetchReturning(403, {error: 'forbidden'})});
  const r = await s.signIn('agent-key');
  assert.equal(r.ok, false);
  assert.match(r.detail, /principal key/);
  assert.equal(s.signedIn(), false);
});

test('an empty key is refused without a request', async () => {
  const calls = [];
  const s = createSession({fetch: fetchReturning(200, ME, calls)});
  assert.equal((await s.signIn('   ')).ok, false);
  assert.equal(calls.length, 0);
});

test('an unreachable control plane is reported', async () => {
  const s = createSession({fetch: async () => { throw new TypeError('offline'); }});
  assert.equal((await s.signIn('k')).error, 'network');
});

test('an expired session refuses to authorize; touch extends it; sign-out clears it', async () => {
  let t = 0;
  const s = createSession({fetch: fetchReturning(200, ME), now: () => t});
  await s.signIn('k');
  t = IDLE_MS - 1;
  assert.equal(s.expired(), false);
  s.touch();
  t += IDLE_MS - 1;
  assert.equal(s.expired(), false);
  assert.equal(s.authorization(), 'Bearer k');
  t += 1;
  assert.equal(s.expired(), true);
  assert.throws(() => s.authorization(), /signed out/);
  s.signOut();
  assert.equal(s.signedIn(), false);
  assert.equal(s.me(), null);
  assert.equal(s.expired(), false);
});

test('me() returns a copy', async () => {
  const s = createSession({fetch: fetchReturning(200, ME)});
  await s.signIn('k');
  s.me().roles.push('admin');
  assert.ok(!s.hasAny(['admin']));
});
```

`internal/ui/jstest/api.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {ROUTES, buildPath, createClient} from '../static/api.js';

const ID = '0b5e3c1a-7f2d-4c8e-9a61-3d2f5e7a9b10';
const session = {authorization: () => 'Bearer k-1'};

function fakeFetch(status, body, calls = []) {
  return async (url, init) => {
    calls.push({url, init});
    return {status, ok: status >= 200 && status < 300,
      json: async () => { if (body === undefined) throw new SyntaxError('no body'); return body; }};
  };
}

test('route names are unique and every path is under /v1', () => {
  const names = ROUTES.map(r => r[0]);
  assert.equal(new Set(names).size, names.length);
  for (const [name, method, path, query] of ROUTES) {
    assert.match(name, /^[a-z]+\.[a-z]+$|^me$/, name);
    assert.ok(['GET', 'POST'].includes(method), name);
    assert.ok(path.startsWith('/v1/'), name);
    assert.ok(Array.isArray(query), name);
  }
});

test('buildPath substitutes and validates path parameters', () => {
  assert.deepEqual(buildPath('incident.ack', {id: ID}), {method: 'POST', url: `/v1/incidents/${ID}/acknowledge`});
  assert.deepEqual(buildPath('agent.get', {ref: 'invoice-bot'}), {method: 'GET', url: '/v1/agents/invoice-bot'});
  assert.throws(() => buildPath('incident.ack', {id: '../soc/summary'}), /invalid id/);
  assert.throws(() => buildPath('incident.ack', {}), /invalid id/);
  assert.throws(() => buildPath('agent.get', {ref: 'a/b'}), /invalid ref/);
  assert.throws(() => buildPath('nope'), /unknown route/);
});

test('buildPath keeps only allowed, non-empty query parameters', () => {
  assert.equal(buildPath('incident.list', {}, {state: 'OPEN', kind: '', limit: 20}).url,
    '/v1/incidents?state=OPEN&limit=20');
  assert.throws(() => buildPath('incident.list', {}, {evil: 'x'}), /not allowed/);
  assert.throws(() => buildPath('incident.list', {}, {state: 'a\nb'}), /invalid state/);
});

test('call sends the bearer key, no cookies, and parses JSON', async () => {
  const calls = [];
  const c = createClient({session, fetch: fakeFetch(200, {x: 1}, calls)});
  assert.deepEqual(await c.call('soc.summary'), {ok: true, status: 200, data: {x: 1}});
  const {url, init} = calls[0];
  assert.equal(url, '/v1/soc/summary');
  assert.equal(init.method, 'GET');
  assert.equal(init.headers.Authorization, 'Bearer k-1');
  assert.equal(init.credentials, 'omit');
  assert.equal(init.redirect, 'error');
  assert.equal(init.cache, 'no-store');
  assert.equal(init.body, undefined);
  assert.equal(init.headers['Content-Type'], undefined);
});

test('call sends a JSON body for writes', async () => {
  const calls = [];
  const c = createClient({session, fetch: fakeFetch(200, {}, calls)});
  await c.call('incident.ack', {params: {id: ID}, body: {reason: 'on it'}});
  assert.equal(calls[0].init.method, 'POST');
  assert.equal(calls[0].init.headers['Content-Type'], 'application/json');
  assert.equal(calls[0].init.body, '{"reason":"on it"}');
});

test('call maps API errors verbatim and signs out only on 401', async () => {
  let signedOut = 0;
  const c = createClient({session, fetch: fakeFetch(409, {error: 'conflict', detail: 'two-person rule'}),
    onUnauthorized: () => signedOut++});
  assert.deepEqual(await c.call('incident.resolve', {params: {id: ID}, body: {}}),
    {ok: false, status: 409, error: 'conflict', detail: 'two-person rule'});
  assert.equal(signedOut, 0);
  const c401 = createClient({session, fetch: fakeFetch(401, {error: 'unauthenticated'}), onUnauthorized: () => signedOut++});
  assert.equal((await c401.call('soc.summary')).status, 401);
  assert.equal(signedOut, 1);
  const c500 = createClient({session, fetch: fakeFetch(500, undefined)});
  assert.deepEqual(await c500.call('soc.summary'), {ok: false, status: 500, error: 'http_500', detail: ''});
});

test('network failure and empty answers', async () => {
  const c = createClient({session, fetch: async () => { throw new TypeError('offline'); }});
  assert.deepEqual(await c.call('soc.summary'), {ok: false, status: 0, error: 'network', detail: ''});
  const c204 = createClient({session, fetch: fakeFetch(204, undefined)});
  assert.deepEqual(await c204.call('incident.note', {params: {id: ID}, body: {text: 'x'}}),
    {ok: true, status: 204, data: null});
});
```

`internal/ui/jstest/confirm.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM} from './fakedom.mjs';
import {canConfirm, ask} from '../static/confirm.js';

installFakeDOM();

test('a required reason must be non-blank', () => {
  assert.equal(canConfirm({reason: 'required'}, '   '), false);
  assert.equal(canConfirm({reason: 'required'}, 'contained'), true);
  assert.equal(canConfirm({reason: 'none'}), true);
});

test('a typed word must match exactly', () => {
  assert.equal(canConfirm({typed: 'tenant'}, '', 'Tenant'), false);
  assert.equal(canConfirm({typed: 'tenant'}, '', 'tenant'), true);
  assert.equal(canConfirm({reason: 'required', typed: 'tenant'}, '', 'tenant'), false);
});

test('only one dialog at a time; cancel resolves null', async () => {
  const first = ask({title: 'Kill scope tenant', lines: ['Scope: tenant'], typed: 'tenant'});
  assert.equal(await ask({title: 'second'}), null);
  const dialog = document.body.childNodes.at(-1);
  assert.equal(dialog.tagName, 'DIALOG');
  assert.equal(dialog.open, true);
  dialog.dispatch('cancel');
  assert.equal(await first, null);
  assert.equal(dialog.open, false);
  const third = ask({title: 'again'});
  document.body.childNodes.at(-1).dispatch('cancel');
  assert.equal(await third, null);
});
```

- [ ] **Step 2: Run them to verify they fail**

Run: `cd internal/ui && node --test jstest/dom.test.mjs jstest/router.test.mjs jstest/session.test.mjs jstest/api.test.mjs jstest/confirm.test.mjs`
Expected: FAIL — `Cannot find module '…/static/dom.js'`.

- [ ] **Step 3: Implement the modules**

`internal/ui/static/dom.js`:

```js
// dom.js builds the console's DOM from elements and text nodes only. No
// string is ever parsed as markup: API data (tool definitions, MCP hints,
// action targets) may be hostile and is always rendered as text. Elements
// and attributes that can load or run code are refused.

const TAGS = new Set(['a', 'button', 'code', 'dd', 'details', 'dialog', 'div', 'dl', 'dt', 'em', 'form', 'h1',
  'h2', 'h3', 'input', 'label', 'li', 'option', 'p', 'pre', 'section', 'select', 'small', 'span', 'strong',
  'summary', 'table', 'tbody', 'td', 'textarea', 'th', 'thead', 'tr', 'ul']);

const ATTRS = new Set(['autocomplete', 'checked', 'class', 'disabled', 'for', 'hidden', 'href', 'id', 'maxlength',
  'method', 'name', 'placeholder', 'required', 'role', 'rows', 'scope', 'selected', 'size', 'spellcheck', 'title',
  'type', 'value']);

const EVENTS = new Set(['onchange', 'onclick', 'oninput', 'onsubmit']);

export function h(tag, attrs, ...children) {
  if (!TAGS.has(tag)) throw new Error(`element <${tag}> is not allowed`);
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs ?? {})) {
    if (EVENTS.has(name)) {
      if (typeof value !== 'function') throw new Error(`${name} must be a function`);
      el.addEventListener(name.slice(2), value);
      continue;
    }
    if (!ATTRS.has(name) && !/^(aria|data)-[a-z-]+$/.test(name)) throw new Error(`attribute ${name} is not allowed`);
    if (value === undefined || value === null || value === false) continue;
    if (name === 'href' && !String(value).startsWith('#/')) throw new Error('links stay inside the console');
    el.setAttribute(name, value === true ? '' : String(value));
  }
  return append(el, children);
}

export function append(el, children) {
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    el.append(typeof child === 'object' ? child : document.createTextNode(String(child)));
  }
  return el;
}

// replace swaps an element's children, skipping null and false like h().
export function replace(el, ...children) {
  el.replaceChildren();
  return append(el, children);
}

export function fmtTime(value) {
  if (!value) return '—';
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? String(value) : `${d.toISOString().replace('T', ' ').slice(0, 19)}Z`;
}

export const cls = value => String(value ?? '').toLowerCase().replace(/[^a-z0-9_-]/g, '');

export const badge = value => h('span', {class: `badge badge-${cls(value)}`}, value ?? '—');

export const link = (label, hash) => h('a', {href: hash}, label);

export function button(label, onclick, {kind = '', disabled = false, type = 'button'} = {}) {
  return h('button', {type, class: kind || null, disabled, onclick}, label);
}

export function table(columns, rows, empty = 'Nothing to show.') {
  if (!rows || rows.length === 0) return h('p', {class: 'empty'}, empty);
  return h('div', {class: 'table-wrap'}, h('table', {},
    h('thead', {}, h('tr', {}, columns.map(([label]) => h('th', {scope: 'col'}, label)))),
    h('tbody', {}, rows.map(row => h('tr', {}, columns.map(([, cell]) => h('td', {}, cell(row))))))));
}

export function kv(pairs) {
  return h('dl', {class: 'kv'}, pairs.filter(Boolean).map(([k, v]) => [h('dt', {}, k), h('dd', {}, v ?? '—')]));
}

export const json = value => h('pre', {class: 'json'}, JSON.stringify(value ?? null, null, 2));

export const section = (title, ...children) => h('section', {class: 'panel'}, h('h2', {}, title), ...children);

const LEADS = {0: 'The control plane is unreachable.', 403: 'Your roles do not allow this.', 404: 'Not found.',
  409: 'Conflict: the server refused this change.'};

export function notice(res) {
  const lead = LEADS[res.status] ?? `Request failed (${res.status}).`;
  return h('div', {class: 'notice error', role: 'alert'}, h('strong', {}, lead), ' ',
    [res.error, res.detail].filter(Boolean).join(': '));
}

export const ok = message => h('div', {class: 'notice ok', role: 'status'}, message);

export const field = (label, control) => h('label', {class: 'field'}, h('span', {}, label), control);

export const input = (name, attrs = {}) => h('input', {name, autocomplete: 'off', spellcheck: 'false', ...attrs});

export function select(name, options, value = '') {
  return h('select', {name}, options.map(o => {
    const [v, label] = Array.isArray(o) ? o : [o, o === '' ? 'any' : o];
    return h('option', {value: v, selected: v === value}, label);
  }));
}

export function values(form) {
  const out = {};
  for (const el of form.elements) {
    if (el.name) out[el.name] = el.type === 'checkbox' ? el.checked : String(el.value).trim();
  }
  return out;
}

export function stat(label, value, hash) {
  return h('div', {class: 'stat'}, h('span', {class: 'stat-label'}, hash ? link(label, hash) : label),
    h('span', {class: 'stat-value'}, String(value ?? 0)));
}
```

`internal/ui/static/router.js`:

```js
// router.js maps the location hash to an area, path parts and a query. The
// hash carries ids and filters only, never a key or personal data.

export const AREAS = ['overview', 'incidents', 'inventory', 'fleet', 'security', 'approvals', 'execution',
  'dependencies', 'cost'];

const SEGMENT = /^[A-Za-z0-9._-]{1,128}$/;
const KEY = /^[a-z_]{1,32}$/;
const VALUE = /^[A-Za-z0-9._:,/@-]{0,1024}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const isUUID = value => typeof value === 'string' && UUID.test(value);

export function parse(hash) {
  const raw = String(hash ?? '').replace(/^#\/?/, '');
  const [path, qs = ''] = raw.split('?', 2);
  const parts = path.split('/').filter(Boolean);
  const area = parts.shift();
  const segmentsOK = parts.every(p => SEGMENT.test(p) && p !== '.' && p !== '..');
  if (!AREAS.includes(area) || !segmentsOK) return {area: 'overview', parts: [], query: {}};
  const query = {};
  for (const pair of qs.split('&').filter(Boolean)) {
    const [k, v = ''] = pair.split('=', 2);
    let value;
    try {
      value = decodeURIComponent(v);
    } catch {
      continue;
    }
    if (KEY.test(k) && VALUE.test(value)) query[k] = value;
  }
  return {area, parts, query};
}

export function format(area, parts = [], query = {}) {
  const qs = Object.entries(query)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => `${k}=${encodeURIComponent(String(v))}`).join('&');
  return `#/${[area, ...parts.map(p => encodeURIComponent(String(p)))].join('/')}${qs ? `?${qs}` : ''}`;
}
```

`internal/ui/static/session.js`:

```js
// session.js holds the operator's API key in this closure only (ADR-028):
// never in browser storage, a cookie, a URL or a log. A reload, sign-out, a
// 401 answer or IDLE_MS without input drops it.

export const IDLE_MS = 30 * 60 * 1000;

export function createSession({fetch: f = (...a) => globalThis.fetch(...a), now = () => Date.now(), idleMs = IDLE_MS} = {}) {
  let key = null;
  let me = null;
  let last = 0;
  const expired = () => key !== null && now() - last >= idleMs;
  const session = {
    async signIn(value) {
      const candidate = String(value ?? '').trim();
      if (!candidate) return {ok: false, error: 'empty', detail: 'Enter an API key.'};
      let res;
      try {
        res = await f('/v1/me', {method: 'GET', credentials: 'omit', cache: 'no-store', redirect: 'error',
          headers: {Authorization: `Bearer ${candidate}`, Accept: 'application/json'}});
      } catch {
        return {ok: false, error: 'network', detail: 'The control plane is unreachable.'};
      }
      if (res.status === 200) {
        const body = await res.json();
        key = candidate;
        me = {principalId: body.principal_id, tenantId: body.tenant_id, roles: [...(body.roles ?? [])]};
        last = now();
        return {ok: true};
      }
      if (res.status === 401) return {ok: false, error: 'unauthenticated', detail: 'The key was not accepted.'};
      if (res.status === 403) {
        return {ok: false, error: 'forbidden', detail: 'A principal key is required; agent keys cannot sign in.'};
      }
      return {ok: false, error: `http_${res.status}`, detail: 'Sign-in failed.'};
    },
    signOut() {
      key = null;
      me = null;
      last = 0;
    },
    signedIn: () => key !== null,
    expired,
    touch() {
      if (key !== null && !expired()) last = now();
    },
    authorization() {
      if (key === null || expired()) throw new Error('signed out');
      return `Bearer ${key}`;
    },
    me: () => (me ? {...me, roles: [...me.roles]} : null),
    hasAny: roles => me !== null && (roles == null || roles.some(r => me.roles.includes(r))),
  };
  return session;
}
```

`internal/ui/static/api.js`:

```js
// api.js is the one list of every call the console makes. A Go test checks
// each entry against the real API mux, so the console cannot call a route
// that does not exist. The console is a client: the API and PostgreSQL
// decide every request.
import {isUUID} from './router.js';

// [name, method, path, allowed query parameters]
export const ROUTES = [
  ['me', 'GET', '/v1/me', []],
  ['soc.summary', 'GET', '/v1/soc/summary', []],
  ['incident.list', 'GET', '/v1/incidents', ['state', 'severity', 'kind', 'limit']],
  ['incident.get', 'GET', '/v1/incidents/{id}', []],
  ['incident.open', 'POST', '/v1/incidents', []],
  ['incident.ack', 'POST', '/v1/incidents/{id}/acknowledge', []],
  ['incident.assign', 'POST', '/v1/incidents/{id}/assign', []],
  ['incident.note', 'POST', '/v1/incidents/{id}/notes', []],
  ['incident.link', 'POST', '/v1/incidents/{id}/links', []],
  ['incident.resolve', 'POST', '/v1/incidents/{id}/resolve', []],
  ['agent.list', 'GET', '/v1/agents', []],
  ['agent.get', 'GET', '/v1/agents/{ref}', []],
  ['connector.list', 'GET', '/v1/connectors', []],
  ['connector.tools', 'GET', '/v1/connectors/{id}/tools', []],
  ['connector.circuit', 'GET', '/v1/connectors/{id}/circuit', []],
  ['circuit.disable', 'POST', '/v1/connectors/{id}/circuit/disable', []],
  ['circuit.enable', 'POST', '/v1/connectors/{id}/circuit/enable', []],
  ['tool.get', 'GET', '/v1/tools/{id}', []],
  ['tool.definitions', 'GET', '/v1/tools/{id}/definitions', []],
  ['tool.quarantine', 'POST', '/v1/tools/{id}/quarantine', []],
  ['tool.release', 'POST', '/v1/tools/{id}/release', []],
  ['kill.list', 'GET', '/v1/killswitch', []],
  ['kill.set', 'POST', '/v1/killswitch', []],
  ['fleet.health', 'GET', '/v1/fleet/health', ['environment', 'risk_class', 'health', 'window']],
  ['fleet.agents', 'GET', '/v1/fleet/agents', ['environment', 'risk_class', 'health', 'window']],
  ['fleet.apply', 'POST', '/v1/fleet/operations', []],
  ['fleet.operation', 'GET', '/v1/fleet/operations/{id}', []],
  ['approval.list', 'GET', '/v1/approvals', []],
  ['approval.get', 'GET', '/v1/approvals/{id}', []],
  ['approval.vote', 'POST', '/v1/approvals/{id}/votes', []],
  ['action.list', 'GET', '/v1/actions', ['state', 'limit']],
  ['action.get', 'GET', '/v1/actions/{id}', []],
  ['action.evidence', 'GET', '/v1/actions/{id}/evidence', []],
  ['dependency.blast', 'GET', '/v1/dependencies/blast-radius', ['kind', 'id', 'name']],
  ['finops.dashboard', 'GET', '/v1/finops/dashboard', []],
  ['finops.alerts', 'GET', '/v1/finops/alerts', ['open', 'limit']],
];

const BY_NAME = new Map(ROUTES.map(([name, method, path, query]) => [name, {method, path, query}]));
const PARAMS = {id: isUUID, ref: v => typeof v === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(v)};
const QUERY_VALUE = /^[^\u0000-\u001f\u007f]{1,256}$/;

export function buildPath(name, params = {}, query = {}) {
  const route = BY_NAME.get(name);
  if (!route) throw new Error(`unknown route ${name}`);
  const path = route.path.replace(/\{(\w+)\}/g, (_, p) => {
    const value = params[p];
    if (!PARAMS[p] || !PARAMS[p](value)) throw new Error(`invalid ${p} for ${name}`);
    return encodeURIComponent(value);
  });
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v === undefined || v === null || v === '') continue;
    if (!route.query.includes(k)) throw new Error(`query ${k} is not allowed for ${name}`);
    if (!QUERY_VALUE.test(String(v))) throw new Error(`invalid ${k} for ${name}`);
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return {method: route.method, url: s ? `${path}?${s}` : path};
}

export function createClient({session, fetch: f = (...a) => globalThis.fetch(...a), onUnauthorized = () => {}}) {
  return {
    async call(name, {params, query, body} = {}) {
      const {method, url} = buildPath(name, params, query);
      const headers = {Authorization: session.authorization(), Accept: 'application/json'};
      const init = {method, headers, credentials: 'omit', cache: 'no-store', redirect: 'error'};
      if (body !== undefined) {
        headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(body);
      }
      let res;
      try {
        res = await f(url, init);
      } catch {
        return {ok: false, status: 0, error: 'network', detail: ''};
      }
      let data = null;
      if (res.status !== 204) {
        try {
          data = await res.json();
        } catch {
          data = null;
        }
      }
      if (res.ok) return {ok: true, status: res.status, data};
      if (res.status === 401) onUnauthorized();
      return {ok: false, status: res.status, error: String(data?.error ?? data?.code ?? `http_${res.status}`),
        detail: String(data?.detail ?? data?.message ?? '')};
    },
  };
}
```

`internal/ui/static/confirm.js`:

```js
// confirm.js is the one dialog every write passes through (spec §4.4). It
// restates the target as text, collects a reason where the API takes one
// and, for the widest scopes, asks the operator to type a word. Only one
// dialog is open at a time, so a double click never sends twice.
import {h} from './dom.js';

let busy = false;

export function canConfirm({reason = 'none', typed = null}, reasonValue = '', typedValue = '') {
  if (reason === 'required' && String(reasonValue).trim() === '') return false;
  if (typed !== null && typed !== undefined && typedValue !== typed) return false;
  return true;
}

export function ask({title, lines = [], reason = 'none', typed = null, danger = false, confirmLabel = 'Confirm'}) {
  if (busy) return Promise.resolve(null);
  busy = true;
  return new Promise(resolve => {
    const opener = document.activeElement;
    const reasonBox = reason === 'required'
      ? h('textarea', {id: 'confirm-reason', name: 'reason', rows: 3, maxlength: 1024, required: true}) : null;
    const typedBox = typed !== null
      ? h('input', {id: 'confirm-typed', name: 'typed', autocomplete: 'off', spellcheck: 'false'}) : null;
    const confirmButton = h('button', {type: 'submit', class: danger ? 'danger' : 'primary', disabled: true}, confirmLabel);
    const current = () => canConfirm({reason, typed}, reasonBox?.value ?? '', typedBox?.value ?? '');
    let dialog = null;
    const finish = value => {
      if (!busy || dialog === null) return;
      busy = false;
      dialog.close();
      dialog.remove();
      if (opener && typeof opener.focus === 'function') opener.focus();
      resolve(value);
    };
    const form = h('form', {
      method: 'dialog',
      oninput: () => { confirmButton.disabled = !current(); },
      onsubmit: e => {
        e.preventDefault();
        if (current()) finish({reason: reasonBox ? reasonBox.value.trim() : ''});
      },
    },
    h('h2', {id: 'confirm-title'}, title),
    h('ul', {}, lines.filter(Boolean).map(line => h('li', {}, line))),
    reasonBox ? h('label', {class: 'field', for: 'confirm-reason'}, 'Reason (journaled; never paste a secret)') : null,
    reasonBox,
    typedBox ? h('label', {class: 'field', for: 'confirm-typed'}, `Type ${typed} to confirm`) : null,
    typedBox,
    h('div', {class: 'actions'}, confirmButton, h('button', {type: 'button', onclick: () => finish(null)}, 'Cancel')));
    dialog = h('dialog', {class: 'confirm', 'aria-labelledby': 'confirm-title'}, form);
    dialog.addEventListener('cancel', e => {
      e.preventDefault();
      finish(null);
    });
    document.body.append(dialog);
    dialog.showModal();
    confirmButton.disabled = !current();
  });
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd internal/ui && node --test jstest/dom.test.mjs jstest/router.test.mjs jstest/session.test.mjs jstest/api.test.mjs jstest/confirm.test.mjs`
Expected: PASS, all tests (0 failures).

- [ ] **Step 5: Commit**

```bash
git add internal/ui/package.json internal/ui/static internal/ui/jstest
git commit -m "feat(ui): console core modules - text-only DOM, hash router, in-memory session, route table, confirm dialog"
```

---

### Task 3: Serve the console — `internal/ui`, the shell, the overview, and the Go guard tests

**Files:**
- Create: `internal/ui/ui.go`
- Create: `internal/ui/ui_test.go`, `internal/ui/lint_test.go`, `internal/ui/contract_test.go`, `internal/ui/jstest_test.go`
- Create: `internal/ui/static/index.html`, `app.css`, `app.js`, `views/common.js`, `views/overview.js`
- Modify: `cmd/controlplane-api/main.go` (mount the console)
- Create: `cmd/controlplane-api/ui_test.go`

**Interfaces:**
- Consumes: Task 1 `config.Config.UI`; Task 2 modules.
- Produces: `ui.Register(mux *http.ServeMux)`, `ui.Handler() http.Handler`, `ui.Files fs.FS`, `ui.CSP string`; `views/common.js`: `back(label, hash)`, `mapText(map) → string`, `linkBack(ctx, kind, id) → Element | null`; the view contract `render(ctx) → Promise<Element>` with `ctx = {client, session, route, go(hash), refresh()}`; `app.js` `NAV` and `VIEWS` (later tasks add entries).

- [ ] **Step 1: Write the failing Go tests**

`internal/ui/ui_test.go`:

```go
package ui_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"eacp/internal/ui"
)

// consoleFiles is every file the console serves. Adding a served file is a
// deliberate change: add it here too.
var consoleFiles = []string{
	"api.js", "app.css", "app.js", "confirm.js", "dom.js", "index.html", "router.js", "session.js",
	"views/common.js", "views/overview.js",
}

var wantHeaders = map[string]string{
	"Content-Security-Policy":      ui.CSP,
	"X-Content-Type-Options":       "nosniff",
	"Referrer-Policy":              "no-referrer",
	"X-Frame-Options":              "DENY",
	"Cache-Control":                "no-store",
	"Cross-Origin-Opener-Policy":   "same-origin",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Permissions-Policy":           "camera=(), microphone=(), geolocation=()",
}

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	ui.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	for k, v := range wantHeaders {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s: %s = %q, want %q", path, k, got, v)
		}
	}
	return rec
}

func TestCSPIsStrict(t *testing.T) {
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'",
		"frame-ancestors 'none'", "form-action 'none'", "require-trusted-types-for 'script'"} {
		if !strings.Contains(ui.CSP, want) {
			t.Errorf("CSP lacks %q", want)
		}
	}
	if strings.Contains(ui.CSP, "unsafe") {
		t.Errorf("CSP allows an unsafe source: %s", ui.CSP)
	}
}

func TestServesTheConsoleWithItsHeaders(t *testing.T) {
	cases := map[string]string{
		"/ui/":                  "text/html; charset=utf-8",
		"/ui/index.html":        "text/html; charset=utf-8",
		"/ui/app.js":            "text/javascript; charset=utf-8",
		"/ui/app.css":           "text/css; charset=utf-8",
		"/ui/views/overview.js": "text/javascript; charset=utf-8",
	}
	for path, ctype := range cases {
		rec := serve(t, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type = %q, want %q", path, got, ctype)
		}
	}
	if body := serve(t, "/ui/").Body.String(); !strings.Contains(body, `<script type="module" src="app.js"></script>`) {
		t.Errorf("index.html does not load app.js as a module:\n%s", body)
	}
}

func TestRedirectsToTheConsoleRoot(t *testing.T) {
	rec := serve(t, "/ui")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET /ui: %d %q, want 301 /ui/", rec.Code, rec.Header().Get("Location"))
	}
}

func TestUnknownFilesAndDirectoriesAreNotFound(t *testing.T) {
	for _, path := range []string{"/ui/nope.js", "/ui/views", "/ui/views/", "/ui/package.json", "/ui/app.js.map",
		"/ui/jstest/api.test.mjs", "/ui/ui.go", "/ui/%2e%2e/ui.go"} {
		rec := httptest.NewRecorder()
		ui.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ui.CSP {
			t.Errorf("%s: 404 lacks the CSP", path)
		}
	}
}

func TestEmbeddedFilesAreExactlyTheConsole(t *testing.T) {
	var got []string
	err := fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			got = append(got, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(consoleFiles)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("embedded files = %v\nwant %v", got, want)
	}
}
```

`internal/ui/lint_test.go`:

```go
package ui_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"eacp/internal/ui"
)

// banned are sinks and APIs the console must never use (ADR-028): markup
// parsing, code from strings, browser storage for the key, cookies, other
// origins and dynamic imports. Comments count too.
var banned = []string{
	"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "Function(",
	"setTimeout('", `setTimeout("`, "setInterval('", `setInterval("`, "srcdoc", "javascript:", "localStorage",
	"sessionStorage", "indexedDB", "document.cookie", "http://", "https://", "DOMParser", "createContextualFragment",
	"import(", "`/v1",
}

func consoleSource(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".css") {
			return err
		}
		b, err := fs.ReadFile(ui.Files, path)
		out[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConsoleUsesNoDangerousSinks(t *testing.T) {
	for path, src := range consoleSource(t) {
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Errorf("%s contains %q", path, b)
			}
		}
		if strings.Contains(src, "fetch(") && path != "api.js" && path != "session.js" {
			t.Errorf("%s calls fetch; only api.js and session.js may", path)
		}
	}
}

func TestEveryCallNamesItsRouteLiterally(t *testing.T) {
	any := regexp.MustCompile(`\.call\(`)
	literal := regexp.MustCompile(`\.call\('[a-z][a-z.]*'`)
	for path, src := range consoleSource(t) {
		if n, m := len(any.FindAllString(src, -1)), len(literal.FindAllString(src, -1)); n != m {
			t.Errorf("%s: %d .call( uses, %d with a literal route name", path, n, m)
		}
	}
}

func TestIndexLoadsOnlyTheConsole(t *testing.T) {
	src := consoleSource(t)["index.html"]
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(src) {
		t.Error("index.html has an inline event handler")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(src) {
		t.Error("index.html has an inline style")
	}
	for _, m := range regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(src, -1) {
		if strings.TrimSpace(m[2]) != "" || !strings.Contains(m[1], `src="app.js"`) {
			t.Errorf("inline or foreign script: %s", m[0])
		}
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(src, -1) {
		if ref := m[1]; !strings.HasPrefix(ref, "#/") && ref != "app.js" && ref != "app.css" {
			t.Errorf("index.html references %q", ref)
		}
	}
}
```

`internal/ui/contract_test.go`:

```go
package ui_test

import (
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"eacp/internal/api"
	"eacp/internal/ui"
)

var (
	routeRE   = regexp.MustCompile(`\[\s*'([a-z][a-z.]*)',\s*'(GET|POST|PUT|DELETE)',\s*'(/v1/[^']*)'`)
	literalRE = regexp.MustCompile(`'(/v1/[^'?]*)`)
	callRE    = regexp.MustCompile(`\.call\('([a-z][a-z.]*)'`)
	paramRE   = regexp.MustCompile(`\{[a-z]+\}`)
)

// TestEveryConsoleCallIsARealRoute proves the console never calls an
// invented route: every ROUTES entry resolves on the real API mux to the
// same pattern, every /v1 literal is a ROUTES path, and every call names a
// ROUTES entry.
func TestEveryConsoleCallIsARealRoute(t *testing.T) {
	src, err := fs.ReadFile(ui.Files, "api.js")
	if err != nil {
		t.Fatal(err)
	}
	routes := routeRE.FindAllStringSubmatch(string(src), -1)
	if len(routes) < 30 {
		t.Fatalf("found %d routes in api.js; the pattern no longer matches the table", len(routes))
	}
	mux := http.NewServeMux()
	api.New(nil, slog.New(slog.DiscardHandler)).Register(mux)
	names, paths := map[string]bool{}, map[string]bool{}
	for _, m := range routes {
		name, method, tmpl := m[1], m[2], m[3]
		if names[name] {
			t.Errorf("route %s is listed twice", name)
		}
		names[name], paths[tmpl] = true, true
		concrete := paramRE.ReplaceAllStringFunc(tmpl, func(p string) string {
			if p == "{ref}" {
				return "sample-agent"
			}
			return "0b5e3c1a-7f2d-4c8e-9a61-3d2f5e7a9b10"
		})
		_, pattern := mux.Handler(httptest.NewRequest(method, concrete, nil))
		if want := method + " " + tmpl; pattern != want {
			t.Errorf("%s: %s %s resolves to %q, want %q", name, method, concrete, pattern, want)
		}
	}
	err = fs.WalkDir(ui.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") {
			return err
		}
		b, err := fs.ReadFile(ui.Files, path)
		for _, m := range literalRE.FindAllStringSubmatch(string(b), -1) {
			if !paths[m[1]] {
				t.Errorf("%s uses %s, which is not in ROUTES", path, m[1])
			}
		}
		for _, m := range callRE.FindAllStringSubmatch(string(b), -1) {
			if !names[m[1]] {
				t.Errorf("%s calls %q, which is not in ROUTES", path, m[1])
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

`internal/ui/jstest_test.go`:

```go
package ui_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestJavaScriptUnitTests runs the console's pure-module tests with node.
// Without node it skips, unless EACP_UI_NODE_REQUIRED=1 makes it fail.
func TestJavaScriptUnitTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("EACP_UI_NODE_REQUIRED") == "1" {
			t.Fatal("node is required (EACP_UI_NODE_REQUIRED=1) but not on PATH")
		}
		t.Skip("node is not on PATH: the console's JavaScript tests did not run (EACP_UI_NODE_REQUIRED=1 fails instead)")
	}
	files, err := filepath.Glob(filepath.Join("jstest", "*.test.mjs"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no JavaScript tests found: %v", err)
	}
	out, err := exec.Command(node, append([]string{"--test"}, files...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node --test: %v\n%s", err, out)
	}
}
```

`cmd/controlplane-api/ui_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"eacp/internal/config"
)

func TestConsoleIsMountedUnlessDisabled(t *testing.T) {
	for _, on := range []bool{true, false} {
		mux := http.NewServeMux()
		mountUI(config.Config{UI: on}, mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/", nil))
		want := http.StatusNotFound
		if on {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("EACP_UI on=%v: GET /ui/ = %d, want %d", on, rec.Code, want)
		}
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ui ./cmd/controlplane-api -run 'TestCSP|TestServes|TestRedirects|TestUnknown|TestEmbedded|TestConsole|TestEvery|TestIndex|TestJavaScript'`
Expected: FAIL — `package eacp/internal/ui` has no non-test Go files / `undefined: mountUI`.

- [ ] **Step 3: Implement the server**

`internal/ui/ui.go`:

```go
// Package ui serves the operator console (ADR-028): plain HTML, CSS and ES
// modules embedded in controlplane-api at /ui/. The console calls the /v1
// API with the operator's own key and holds no authority: every rule is
// enforced by the API and PostgreSQL. Only the files under static/ are
// served, with a strict Content-Security-Policy.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var static embed.FS

// Files is the embedded console, rooted at static/.
var Files = func() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// CSP allows only the console's own scripts, styles and API calls, and
// requires Trusted Types so no string can reach an HTML sink.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
}

func secure(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// Register mounts the console at /ui/ and redirects /ui there.
func Register(mux *http.ServeMux) {
	mux.Handle("GET /ui/", Handler())
	mux.Handle("GET /ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w.Header())
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	}))
}

// Handler serves the embedded files under /ui/. A directory, an unknown
// file or any other extension is 404, never a listing.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w.Header())
		name, found := strings.CutPrefix(r.URL.Path, "/ui/")
		if name == "" {
			name = "index.html"
		}
		ctype := contentTypes[path.Ext(name)]
		if !found || ctype == "" || !fs.ValidPath(name) {
			notFound(w)
			return
		}
		body, err := fs.ReadFile(Files, name)
		if err != nil {
			notFound(w)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}
```

In `cmd/controlplane-api/main.go`: add `"eacp/internal/ui"` to the imports, add a line to the package comment (`// It serves the operator console at /ui/ unless EACP_UI=off (ADR-028).`), call `mountUI(d.Config, mux)` right after `api.New(...).Register(mux)`, and add:

```go
// mountUI serves the operator console unless EACP_UI=off (ADR-028). The
// console is static files calling the same API; it adds no authority.
func mountUI(cfg config.Config, mux *http.ServeMux) {
	if cfg.UI {
		ui.Register(mux)
	}
}
```

- [ ] **Step 4: Implement the shell, the overview and the shared view helpers**

`internal/ui/static/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>EACP operator console</title>
<link rel="stylesheet" href="app.css">
<script type="module" src="app.js"></script>
</head>
<body class="signed-out">
<header class="top">
<a class="brand" href="#/overview">EACP operator console</a>
<div id="who"></div>
</header>
<nav id="nav" aria-label="Areas"></nav>
<main id="main" tabindex="-1"></main>
</body>
</html>
```

`internal/ui/static/app.css`:

```css
:root {
  color-scheme: light dark;
  --bg: #f6f7f9; --panel: #ffffff; --text: #1b1f24; --muted: #5b6573; --border: #d8dde3;
  --accent: #2456c7; --danger: #b3261e; --ok: #1e7a3a;
  --sev-critical: #b3261e; --sev-high: #b35400; --sev-medium: #7a6100; --sev-low: #4b6584;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #111418; --panel: #1a1f25; --text: #e6e9ed; --muted: #9aa4b1; --border: #2c343d;
    --accent: #7aa2ff; --danger: #ff8a80; --ok: #7bd88f;
    --sev-critical: #ff8a80; --sev-high: #ffb74d; --sev-medium: #e6c84f; --sev-low: #a3b8d6;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; font: 14px/1.45 system-ui, sans-serif; background: var(--bg); color: var(--text);
  display: grid; grid-template-columns: 200px minmax(0, 1fr); grid-template-rows: auto 1fr; min-height: 100vh;
}
body.signed-out { grid-template-columns: minmax(0, 1fr); }
body.signed-out nav#nav { display: none; }
header.top {
  grid-column: 1 / -1; display: flex; justify-content: space-between; align-items: center; gap: 12px;
  padding: 8px 16px; border-bottom: 1px solid var(--border); background: var(--panel);
}
.brand { font-weight: 700; color: var(--text); text-decoration: none; }
#who { display: flex; gap: 8px; align-items: center; color: var(--muted); flex-wrap: wrap; }
nav#nav { border-right: 1px solid var(--border); padding: 12px 0; background: var(--panel); }
nav#nav ul { list-style: none; margin: 0; padding: 0; }
nav#nav a { display: block; padding: 6px 16px; color: var(--text); text-decoration: none; border-left: 3px solid transparent; }
nav#nav a[aria-current="page"] { background: var(--bg); border-left-color: var(--accent); font-weight: 600; }
main { padding: 16px; min-width: 0; }
main[aria-busy="true"] { opacity: .7; }
h1 { font-size: 20px; margin: 4px 0 8px; }
h3 { font-size: 14px; margin: 12px 0 4px; }
a { color: var(--accent); }
.panel { background: var(--panel); border: 1px solid var(--border); border-radius: 6px; padding: 12px 16px; margin-bottom: 16px; }
.panel h2 { font-size: 15px; margin: 0 0 8px; }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 16px; }
.grid .panel { margin-bottom: 0; }
.stat { display: flex; justify-content: space-between; gap: 12px; padding: 4px 0; border-bottom: 1px dashed var(--border); }
.stat-value { font-variant-numeric: tabular-nums; font-weight: 600; text-align: right; }
.table-wrap { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--border); vertical-align: top; overflow-wrap: anywhere; }
th { color: var(--muted); font-weight: 600; font-size: 12px; text-transform: uppercase; letter-spacing: .03em; }
.badge { display: inline-block; padding: 1px 8px; border-radius: 10px; border: 1px solid var(--border); font-size: 12px; white-space: nowrap; }
.badge-critical { color: var(--sev-critical); border-color: var(--sev-critical); font-weight: 700; }
.badge-high { color: var(--sev-high); border-color: var(--sev-high); }
.badge-medium { color: var(--sev-medium); border-color: var(--sev-medium); }
.badge-low { color: var(--sev-low); border-color: var(--sev-low); }
.badge-open, .badge-killed, .badge-quarantined, .badge-disabled, .badge-contained, .badge-degraded { color: var(--danger); border-color: var(--danger); }
.badge-resolved, .badge-ok, .badge-closed, .badge-active, .badge-cleared, .badge-executable { color: var(--ok); border-color: var(--ok); }
.chips { display: flex; gap: 6px; flex-wrap: wrap; margin-bottom: 12px; }
.chip { color: var(--muted); }
.kv { display: grid; grid-template-columns: max-content minmax(0, 1fr); gap: 4px 16px; margin: 0; }
.kv dt { color: var(--muted); }
.kv dd { margin: 0; overflow-wrap: anywhere; }
pre.json { background: var(--bg); border: 1px solid var(--border); padding: 8px; overflow: auto; max-height: 360px; font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
form.stack, form.filters { display: flex; flex-wrap: wrap; gap: 8px 12px; align-items: flex-end; margin: 8px 0; }
.field { display: flex; flex-direction: column; gap: 2px; font-size: 12px; color: var(--muted); }
input, select, textarea, button { font: inherit; color: var(--text); background: var(--panel); border: 1px solid var(--border); border-radius: 4px; padding: 5px 8px; max-width: 100%; }
textarea { width: 100%; min-width: min(260px, 100%); }
button { cursor: pointer; }
button:disabled { opacity: .5; cursor: not-allowed; }
button.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
button.danger { background: var(--danger); border-color: var(--danger); color: #fff; }
.actions { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin: 8px 0; }
.notice { padding: 8px 12px; border-radius: 4px; margin: 8px 0; border: 1px solid; }
.notice.error { border-color: var(--danger); color: var(--danger); }
.notice.ok { border-color: var(--ok); color: var(--ok); }
.hint, .empty, .asof { color: var(--muted); }
dialog.confirm { border: 1px solid var(--border); border-radius: 8px; background: var(--panel); color: var(--text); max-width: 560px; width: calc(100% - 32px); }
dialog.confirm::backdrop { background: rgb(0 0 0 / .45); }
dialog.confirm ul { padding-left: 18px; overflow-wrap: anywhere; }
.signin { max-width: 440px; margin: 10vh auto; display: flex; flex-direction: column; gap: 10px; }
code { font-size: 12px; }
@media (max-width: 720px) {
  body { grid-template-columns: minmax(0, 1fr); }
  nav#nav { border-right: 0; border-bottom: 1px solid var(--border); padding: 4px 0; }
  nav#nav ul { display: flex; flex-wrap: wrap; }
  nav#nav a { border-left: 0; border-bottom: 3px solid transparent; padding: 6px 10px; }
  nav#nav a[aria-current="page"] { border-bottom-color: var(--accent); }
}
```

`internal/ui/static/views/common.js`:

```js
// common.js holds helpers shared by the views.
import {h, link, button, ok, notice, replace} from '../dom.js';
import {format, isUUID} from '../router.js';

export const back = (label, hash) => h('p', {}, link(label, hash));

export const mapText = m => Object.entries(m ?? {}).map(([k, v]) => `${k}: ${v}`).join(', ') || '0';

// linkBack offers to record a containment change on the incident the
// operator came from (?incident=<id>). The link is journaled; it changes
// nothing else.
export function linkBack(ctx, kind, id) {
  const incident = ctx.route.query.incident;
  if (!isUUID(incident) || !ctx.session.hasAny(['operator', 'admin'])) return null;
  const out = h('span');
  return h('div', {class: 'actions'},
    button('Link this to the incident', async () => {
      const r = await ctx.client.call('incident.link', {params: {id: incident}, body: {kind, id}});
      replace(out, r.ok ? ok('Linked.') : notice(r));
    }),
    link('Back to the incident', format('incidents', [incident])),
    out);
}
```

`internal/ui/static/views/overview.js`:

```js
// overview.js shows the §55 counters from GET /v1/soc/summary and the open
// incidents.
import {h, section, notice, table, badge, link, fmtTime, stat} from '../dom.js';
import {format} from '../router.js';

export async function render({client}) {
  const [sum, open] = await Promise.all([
    client.call('soc.summary'),
    client.call('incident.list', {query: {state: 'OPEN', limit: 20}}),
  ]);
  if (!sum.ok) return notice(sum);
  const s = sum.data;
  const sev = s.security.open_incidents ?? {};
  const spend = (s.finops.spend_today ?? []).map(u => `${u.amount} ${u.unit}`).join(' · ') || '0';
  const incidents = sev => format('incidents', [], {severity: sev});
  return h('div', {},
    h('p', {class: 'asof'}, `As of ${fmtTime(s.as_of)}; refreshes every 15 seconds.`),
    h('div', {class: 'grid'},
      section('Agents',
        stat('Registered', s.agents.registered, '#/inventory'),
        stat('Production', s.agents.production),
        stat('High risk', s.agents.high_risk),
        stat('Owner disabled', s.agents.owner_disabled),
        stat('Active versions', s.agents.versions.active),
        stat('Suspended versions', s.agents.versions.suspended),
        stat('Quarantined versions', s.agents.versions.quarantined, '#/fleet')),
      section('Security',
        stat('Critical incidents', sev.critical ?? 0, incidents('critical')),
        stat('High incidents', sev.high ?? 0, incidents('high')),
        stat('Medium incidents', sev.medium ?? 0, incidents('medium')),
        stat('Low incidents', sev.low ?? 0, incidents('low')),
        stat('Unacknowledged', s.security.unacknowledged, format('incidents', [], {state: 'OPEN'})),
        stat('Quarantined tools', s.security.quarantined_tools, '#/security'),
        stat('Active kills', s.security.active_kills, '#/security'),
        stat('Open circuits', s.security.open_circuits, '#/security'),
        stat('Disabled circuits', s.security.disabled_circuits, '#/security'),
        stat('Pending approvals', s.security.pending_approvals, '#/approvals')),
      section('Execution',
        stat('Queued', s.execution.queued, format('execution', [], {state: 'QUEUED'})),
        stat('Running', s.execution.running, format('execution', [], {state: 'EXECUTING'})),
        stat('Retry wait', s.execution.retry_wait, format('execution', [], {state: 'RETRY_WAIT'})),
        stat('Unknown outcome', s.execution.unknown_outcome, format('execution', [], {state: 'UNKNOWN_OUTCOME'})),
        stat('Needs a human', s.execution.needs_human, format('execution', [], {state: 'NEEDS_HUMAN_RESOLUTION'}))),
      section('FinOps',
        stat('Spend today', spend, '#/cost'),
        stat('Open alerts', s.finops.open_alerts, '#/cost'))),
    section('Open incidents', open.ok ? table([
      ['Severity', i => badge(i.severity)],
      ['Title', i => link(i.title, format('incidents', [i.id]))],
      ['Kind', i => i.kind],
      ['Opened', i => fmtTime(i.opened_at)],
    ], open.data.incidents, 'No open incidents.') : notice(open)));
}
```

`internal/ui/static/app.js`:

```js
// app.js boots the operator console (ADR-028): sign-in, navigation, the
// idle timer and polling. The console is a client of the /v1 API and holds
// no authority; the API and PostgreSQL decide every request.
import {createSession} from './session.js';
import {createClient} from './api.js';
import {parse, format} from './router.js';
import {h, button} from './dom.js';
import * as overview from './views/overview.js';

const READERS = ['operator', 'auditor', 'admin'];
// NAV lists the areas that have a view: [area, label, roles that may read it].
// The roles only hide links; the API answers 403 to anyone else.
const NAV = [
  ['overview', 'Overview', READERS],
];
const VIEWS = {overview};
const POLLED = new Set(['overview', 'incidents']);
const POLL_MS = 15000;
const IDLE_CHECK_MS = 30000;

const session = createSession();
const client = createClient({session, onUnauthorized: () => signOut('Your key was not accepted; sign in again.')});
const main = document.getElementById('main');
const nav = document.getElementById('nav');
const who = document.getElementById('who');
let generation = 0;

function signOut(message) {
  session.signOut();
  generation++;
  renderSignIn(message);
}

function renderSignIn(message = '') {
  document.body.classList.add('signed-out');
  nav.replaceChildren();
  who.replaceChildren();
  const key = h('input', {id: 'key', name: 'key', type: 'password', autocomplete: 'off', spellcheck: 'false', required: true});
  const status = h('div', {role: 'status'}, message);
  const form = h('form', {class: 'signin', onsubmit: async e => {
    e.preventDefault();
    const res = await session.signIn(key.value);
    key.value = '';
    if (res.ok) {
      render();
      return;
    }
    status.replaceChildren(h('div', {class: 'notice error', role: 'alert'}, res.detail));
  }},
  h('h1', {}, 'Sign in'),
  h('label', {class: 'field', for: 'key'}, 'Principal API key'),
  key,
  h('button', {type: 'submit', class: 'primary'}, 'Sign in'),
  h('p', {class: 'hint'}, 'The key stays in this tab’s memory only. Reloading the page, signing out or 30 minutes without activity forgets it.'),
  status);
  main.replaceChildren(form);
  key.focus();
}

async function render() {
  if (!session.signedIn()) {
    renderSignIn();
    return;
  }
  document.body.classList.remove('signed-out');
  const route = parse(location.hash);
  const area = VIEWS[route.area] ? route.area : 'overview';
  const me = session.me();
  nav.replaceChildren(h('ul', {}, NAV.filter(([, , roles]) => session.hasAny(roles)).map(([a, label]) =>
    h('li', {}, h('a', {href: format(a), 'aria-current': a === area ? 'page' : null}, label)))));
  who.replaceChildren(h('span', {}, me.roles.length ? me.roles.join(', ') : 'no roles'),
    button('Sign out', () => signOut('Signed out.')));
  const mine = ++generation;
  const ctx = {
    client,
    session,
    route: area === route.area ? route : {area, parts: [], query: {}},
    go: hash => {
      if (location.hash === hash) render();
      else location.hash = hash;
    },
    refresh: () => render(),
  };
  main.setAttribute('aria-busy', 'true');
  let node;
  try {
    node = await VIEWS[area].render(ctx);
  } catch (err) {
    node = h('div', {class: 'notice error', role: 'alert'}, 'This page failed to render: ', String(err?.message ?? err));
  }
  if (mine !== generation || !session.signedIn()) return;
  main.removeAttribute('aria-busy');
  main.replaceChildren(node);
}

window.addEventListener('hashchange', () => render());
for (const type of ['pointerdown', 'keydown']) document.addEventListener(type, () => session.touch(), {capture: true});
setInterval(() => {
  if (session.expired()) signOut('Signed out after 30 minutes without activity.');
}, IDLE_CHECK_MS);
setInterval(() => {
  if (!session.signedIn() || document.visibilityState !== 'visible') return;
  const route = parse(location.hash);
  if (!POLLED.has(route.area) || route.parts.length > 0 || document.querySelector('dialog[open]')) return;
  const focused = document.activeElement;
  if (focused && focused.matches('input, select, textarea')) return;
  render();
}, POLL_MS);
renderSignIn();
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go vet ./internal/ui ./cmd/controlplane-api && go test -race ./internal/ui ./cmd/controlplane-api`
Expected: PASS, including `TestJavaScriptUnitTests` (node is on PATH locally; it must not report SKIP — check with `-v -run TestJavaScript`).

- [ ] **Step 6: Commit**

```bash
git add internal/ui cmd/controlplane-api
git commit -m "feat(ui): serve the operator console at /ui/ with a strict CSP; shell, sign-in and overview"
```

---

### Task 4: Incidents view (list, §56 detail, lifecycle, timeline)

**Files:**
- Create: `internal/ui/static/views/incidents.js`
- Create: `internal/ui/jstest/incidents.test.mjs`
- Modify: `internal/ui/static/app.js` (import, `NAV`, `VIEWS`)
- Modify: `internal/ui/ui_test.go` (`consoleFiles` += `"views/incidents.js"`)

**Interfaces:**
- Consumes: `dom.js`, `router.js`, `confirm.js` `ask`, `views/common.js` `back`.
- Produces: `recommendations(incident) → [[label, hash], …]` (exported for tests); `render(ctx)`. Links it creates: `#/fleet?op=pause&(tool=…|agents=…)&incident=<id>`, `#/security?scope=…&target=…&incident=<id>`, `#/security/connectors/<id>?incident=<id>`, `#/inventory/tools/<id>`, `#/execution/<id>`, `#/inventory/agents/<name>`, `#/cost`.

- [ ] **Step 1: Write the failing test**

`internal/ui/jstest/incidents.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {recommendations} from '../static/views/incidents.js';

const ID = '11111111-1111-4111-8111-111111111111';
const TOOL = '22222222-2222-4222-8222-222222222222';
const CONN = '33333333-3333-4333-8333-333333333333';
const ACTION = '44444444-4444-4444-8444-444444444444';

test('MCP drift: pause by the tool, kill the tool, review its definitions', () => {
  const recs = recommendations({id: ID, kind: 'mcp_drift', subject_type: 'tool', subject_id: TOOL,
    detail: {connector: 'erp', tool: 'post_invoice'}, affected: {confirmed: [{agent: 'invoice-bot'}]}});
  assert.deepEqual(recs, [
    ['Pause the agents that use this tool', `#/fleet?op=pause&tool=erp.post_invoice&incident=${ID}`],
    ['Kill the tool', `#/security?scope=tool&target=${TOOL}&incident=${ID}`],
    ['Review the tool definitions', `#/inventory/tools/${TOOL}`],
  ]);
});

test('kill: review the kill state; pause the affected agents by name', () => {
  const recs = recommendations({id: ID, kind: 'kill', subject_type: 'kill_state', subject_id: TOOL,
    detail: {scope: 'agent', target_id: CONN}, affected: {confirmed: [{agent: 'a-bot'}, {agent: 'b-bot'}, {agent: 'a-bot'}]}});
  assert.deepEqual(recs, [
    ['Pause the affected agents', `#/fleet?op=pause&agents=a-bot%2Cb-bot&incident=${ID}`],
    ['Review the kill state', `#/security?scope=agent&target=${CONN}&incident=${ID}`],
  ]);
});

test('too many affected agents are chosen in Fleet', () => {
  const confirmed = Array.from({length: 21}, (_, i) => ({agent: `bot-${i}`}));
  const [first] = recommendations({id: ID, kind: 'kill', detail: {scope: 'tenant', target_id: CONN}, affected: {confirmed}});
  assert.deepEqual(first, ['Pause the affected agents (choose them in Fleet)', `#/fleet?op=pause&incident=${ID}`]);
});

test('circuit, unknown outcome, rollback and FinOps', () => {
  assert.deepEqual(recommendations({id: ID, kind: 'circuit_open', subject_id: CONN, detail: {}, affected: {}}),
    [['Review or disable the connector circuit', `#/security/connectors/${CONN}?incident=${ID}`]]);
  assert.deepEqual(recommendations({id: ID, kind: 'unknown_outcome', subject_id: ACTION, detail: {}, affected: {}}),
    [['Review the action and its evidence', `#/execution/${ACTION}`]]);
  assert.deepEqual(recommendations({id: ID, kind: 'canary_rollback', detail: {agent: 'invoice-bot'}, affected: {}}),
    [['Review the agent and its versions', '#/inventory/agents/invoice-bot']]);
  assert.deepEqual(recommendations({id: ID, kind: 'finops', detail: {}, affected: {}}),
    [['Review spend and alerts', '#/cost']]);
});

test('a manual incident without a subject recommends nothing and never throws', () => {
  assert.deepEqual(recommendations({id: ID, kind: 'manual', detail: {reason: 'x'}, affected: {}}), []);
  assert.deepEqual(recommendations({id: ID, kind: 'mcp_drift', detail: {}, affected: null}), []);
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd internal/ui && node --test jstest/incidents.test.mjs`
Expected: FAIL — `Cannot find module '…/views/incidents.js'`.

- [ ] **Step 3: Implement the view**

`internal/ui/static/views/incidents.js`:

```js
// incidents.js lists incidents and shows one (§56): its detail, the blast
// radius recorded at opening, the recommended containment as links, and the
// journaled timeline. Operators and admins acknowledge, assign, note, link
// and resolve; PostgreSQL enforces the lifecycle and the two-person rule.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back} from './common.js';

const SEVERITIES = ['low', 'medium', 'high', 'critical'];
const STATES = ['OPEN', 'ACKNOWLEDGED', 'RESOLVED'];
const KINDS = ['mcp_drift', 'kill', 'circuit_open', 'unknown_outcome', 'canary_rollback', 'finops', 'manual'];
const SUBJECT_TYPES = ['tool', 'kill_state', 'connector', 'action', 'release', 'finops_alert', 'agent', 'agent_version'];
const LINK_KINDS = ['fleet_operation', 'kill_state', 'tool', 'connector', 'action', 'agent', 'agent_version', 'release',
  'change_set', 'finops_alert'];
const RESOLUTIONS = ['contained', 'false_positive', 'accepted_risk', 'duplicate'];
const WORKERS = ['operator', 'admin'];
const MAX_PREFILLED_AGENTS = 20;

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list(ctx) {
  const {client, session, route, go} = ctx;
  const q = route.query;
  const res = await client.call('incident.list', {query: {state: q.state, severity: q.severity, kind: q.kind, limit: 200}});
  const filters = h('form', {class: 'filters', onsubmit: e => {
    e.preventDefault();
    go(format('incidents', [], values(e.target)));
  }},
  field('State', select('state', ['', ...STATES], q.state ?? '')),
  field('Severity', select('severity', ['', ...SEVERITIES], q.severity ?? '')),
  field('Kind', select('kind', ['', ...KINDS], q.kind ?? '')),
  h('button', {type: 'submit'}, 'Filter'));
  return h('div', {},
    section('Incidents', filters, res.ok ? table([
      ['Severity', i => badge(i.severity)],
      ['State', i => badge(i.state)],
      ['Title', i => link(i.title, format('incidents', [i.id]))],
      ['Kind', i => i.kind],
      ['Opened', i => fmtTime(i.opened_at)],
      ['Assignee', i => i.assignee_id ?? '—'],
    ], res.data.incidents, 'No incidents match.') : notice(res)),
    session.hasAny(WORKERS) ? openForm(ctx) : null);
}

function openForm({client, go}) {
  const out = h('div');
  const form = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = values(form);
    const body = {title: v.title, severity: v.severity};
    if (v.subject_type) body.subject_type = v.subject_type;
    if (v.subject_id) {
      if (!isUUID(v.subject_id)) {
        replace(out, notice({status: 400, error: 'invalid', detail: 'the subject id must be a UUID'}));
        return;
      }
      body.subject_id = v.subject_id;
    }
    const c = await ask({title: 'Open a manual incident', lines: [`${v.severity}: ${v.title}`], reason: 'required'});
    if (!c) return;
    const r = await client.call('incident.open', {body: {...body, reason: c.reason}});
    if (r.ok) go(format('incidents', [r.data.id]));
    else replace(out, notice(r));
  }},
  field('Title', input('title', {required: true, maxlength: 200, size: 40})),
  field('Severity', select('severity', SEVERITIES, 'medium')),
  field('Subject type', select('subject_type', ['', ...SUBJECT_TYPES])),
  field('Subject id', input('subject_id', {placeholder: 'UUID (optional)', size: 38})),
  h('button', {type: 'submit', class: 'primary'}, 'Open incident…'));
  return section('Open a manual incident', form, out);
}

// recommendations turns an incident into containment links. They only
// navigate and pre-fill a form; nothing changes until the operator confirms.
export function recommendations(i) {
  const d = i.detail ?? {};
  const agents = [...new Set((i.affected?.confirmed ?? []).map(c => c.agent).filter(Boolean))];
  const recs = [];
  const incident = i.id;
  if (i.kind === 'mcp_drift') {
    if (d.connector && d.tool) {
      recs.push(['Pause the agents that use this tool',
        format('fleet', [], {op: 'pause', tool: `${d.connector}.${d.tool}`, incident})]);
    }
  } else if (agents.length > MAX_PREFILLED_AGENTS) {
    recs.push(['Pause the affected agents (choose them in Fleet)', format('fleet', [], {op: 'pause', incident})]);
  } else if (agents.length > 0) {
    recs.push(['Pause the affected agents', format('fleet', [], {op: 'pause', agents: agents.join(','), incident})]);
  }
  switch (i.kind) {
    case 'mcp_drift':
      if (isUUID(i.subject_id)) {
        recs.push(['Kill the tool', format('security', [], {scope: 'tool', target: i.subject_id, incident})],
          ['Review the tool definitions', format('inventory', ['tools', i.subject_id])]);
      }
      break;
    case 'kill':
      if (d.scope && isUUID(d.target_id)) {
        recs.push(['Review the kill state', format('security', [], {scope: d.scope, target: d.target_id, incident})]);
      }
      break;
    case 'circuit_open':
      if (isUUID(i.subject_id)) {
        recs.push(['Review or disable the connector circuit', format('security', ['connectors', i.subject_id], {incident})]);
      }
      break;
    case 'unknown_outcome':
      if (isUUID(i.subject_id)) recs.push(['Review the action and its evidence', format('execution', [i.subject_id])]);
      break;
    case 'canary_rollback':
      if (d.agent) recs.push(['Review the agent and its versions', format('inventory', ['agents', d.agent])]);
      break;
    case 'finops':
      recs.push(['Review spend and alerts', '#/cost']);
      break;
    default:
  }
  return recs;
}

async function detail(ctx, id) {
  const {client, session} = ctx;
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an incident id'});
  const res = await client.call('incident.get', {params: {id}});
  if (!res.ok) return notice(res);
  const i = res.data;
  const out = h('div');
  // act runs one write; a confirm dialog comes first when opts is given.
  const act = async (opts, send) => {
    let reason = '';
    if (opts) {
      const c = await ask(opts);
      if (!c) return;
      reason = c.reason;
    }
    const r = await send(reason);
    if (r.ok) ctx.refresh();
    else replace(out, notice(r));
  };
  const worker = session.hasAny(WORKERS);
  const recs = recommendations(i);
  return h('div', {},
    back('← Incidents', '#/incidents'),
    h('h1', {}, i.title),
    h('div', {class: 'chips'}, badge(i.severity), badge(i.state), h('span', {class: 'chip'}, i.kind)),
    out,
    worker && i.state !== 'RESOLVED' ? lifecycle(ctx, i, act) : null,
    section('Summary', summary(i)),
    section('Detail', kv(Object.entries(i.detail ?? {}).map(([k, v]) =>
      [k, v !== null && typeof v === 'object' ? JSON.stringify(v) : String(v)]))),
    section('Affected at opening (blast radius)', affected(i.affected)),
    section('Recommended containment',
      recs.length ? h('ul', {}, recs.map(([label, hash]) => h('li', {}, link(label, hash)))) : h('p', {class: 'empty'}, 'None.'),
      h('p', {class: 'hint'}, 'These links only open a form. Nothing changes until you confirm it, and the operator decides (§57).')),
    section('Timeline', timeline(i.events ?? [])),
    worker ? section('Add to the timeline', noteForm(id, client, act), linkForm(id, client, act, out)) : null);
}

function lifecycle({client, session}, i, act) {
  const me = session.me();
  const id = i.id;
  const parts = [];
  if (i.state === 'OPEN') {
    parts.push(button('Acknowledge…', () => act({title: 'Acknowledge this incident', lines: [i.title], reason: 'required'},
      reason => client.call('incident.ack', {params: {id}, body: {reason}})), {kind: 'primary'}));
  }
  if (i.assignee_id !== me.principalId) {
    parts.push(button('Assign to me', () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: me.principalId}}))));
  }
  if (i.assignee_id) {
    parts.push(button('Unassign', () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: null}}))));
  }
  const assignee = input('assignee_id', {placeholder: 'operator or admin principal UUID', size: 38});
  parts.push(h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const target = assignee.value.trim();
    if (!isUUID(target)) return;
    act(null, () => client.call('incident.assign', {params: {id}, body: {assignee_id: target}}));
  }}, assignee, h('button', {type: 'submit'}, 'Assign')));
  if (i.state === 'ACKNOWLEDGED') {
    const code = select('resolution', RESOLUTIONS, 'contained');
    const critical = i.severity === 'critical';
    parts.push(h('form', {class: 'stack', onsubmit: e => {
      e.preventDefault();
      act({title: 'Resolve this incident', reason: 'required', confirmLabel: 'Resolve',
        lines: [i.title, `Resolution: ${code.value}`,
          critical ? 'A critical incident is resolved by someone other than its acknowledger.' : null]},
      reason => client.call('incident.resolve', {params: {id}, body: {resolution: code.value, reason}}));
    }}, field('Resolution', code), h('button', {type: 'submit', class: 'primary'}, 'Resolve…')));
    if (critical && i.acknowledged_by === me.principalId) {
      parts.push(h('p', {class: 'hint'}, 'You acknowledged this critical incident: a second operator must resolve it.'));
    }
  }
  return section('Work this incident', h('div', {class: 'actions'}, parts));
}

function summary(i) {
  return kv([
    ['Kind', i.kind],
    ['Source', i.source_key ?? 'manual'],
    ['Subject', i.subject_type ? `${i.subject_type} ${i.subject_id ?? ''}` : '—'],
    ['Opened', `${fmtTime(i.opened_at)} by ${i.opened_by ?? 'the incident evaluator'}`],
    ['Acknowledged', i.acknowledged_at ? `${fmtTime(i.acknowledged_at)} by ${i.acknowledged_by}: ${i.ack_reason ?? ''}` : '—'],
    ['Assignee', i.assignee_id ?? '—'],
    ['Resolved', i.resolved_at
      ? `${fmtTime(i.resolved_at)} by ${i.resolved_by} as ${i.resolution}: ${i.resolution_reason ?? ''}` : '—'],
    ['Id', h('code', {}, i.id)],
  ]);
}

function affected(a) {
  if (!a || !Array.isArray(a.confirmed)) return h('p', {class: 'empty'}, 'No blast radius was recorded.');
  const nodes = a.nodes ?? [];
  return h('div', {},
    kv([
      ['Nodes', nodes.length < (a.node_count ?? 0) ? `${a.node_count} (first ${nodes.length} shown)` : String(a.node_count ?? 0)],
      ['Confirmed versions', String(a.confirmed_count ?? a.confirmed.length)],
      ['Possible versions', String(a.possible_count ?? 0)],
      ['Active in production', String(a.production_active ?? 0)],
    ]),
    table([
      ['Agent', c => link(c.agent, format('inventory', ['agents', c.agent]))],
      ['Version', c => String(c.version)],
      ['Environment', c => c.environment],
      ['State', c => badge(c.state)],
    ], a.confirmed, 'No confirmed versions.'),
    nodes.length ? h('p', {class: 'hint'}, `Starting from: ${nodes.join(', ')}`) : null,
    h('p', {class: 'hint'}, 'Unknown or stale evidence widens possible impact; it never proves an agent unaffected (ADR-015).'));
}

function timeline(events) {
  return table([
    ['#', e => String(e.seq)],
    ['At', e => fmtTime(e.at)],
    ['Event', e => e.kind],
    ['By', e => e.actor_id ?? 'system'],
    ['Note or link', e => e.note ?? (e.link_kind ? `${e.link_kind} ${e.link_id}` : '')],
  ], events, 'No events.');
}

function noteForm(id, client, act) {
  const text = h('textarea', {name: 'text', rows: 3, maxlength: 4096, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!text.value.trim()) return;
    act(null, () => client.call('incident.note', {params: {id}, body: {text: text.value}}));
  }}, field('Note (journaled; never paste a secret)', text), h('button', {type: 'submit'}, 'Add note'));
}

function linkForm(id, client, act, out) {
  const kind = select('kind', LINK_KINDS, 'fleet_operation');
  const target = input('id', {placeholder: 'UUID', size: 38, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!isUUID(target.value.trim())) {
      replace(out, notice({status: 400, error: 'invalid', detail: 'the linked id must be a UUID'}));
      return;
    }
    act(null, () => client.call('incident.link', {params: {id}, body: {kind: kind.value, id: target.value.trim()}}));
  }}, field('Link kind', kind), field('Id', target), h('button', {type: 'submit'}, 'Add link'));
}
```


- [ ] **Step 4: Wire the view into the app and the file list**

In `app.js`: add `import * as incidents from './views/incidents.js';` after the overview import; change `NAV` to

```js
const NAV = [
  ['overview', 'Overview', READERS],
  ['incidents', 'Incidents', READERS],
];
```

and `const VIEWS = {overview, incidents};`. In `internal/ui/ui_test.go` add `"views/incidents.js"` to `consoleFiles`.

- [ ] **Step 5: Run all console tests**

Run: `cd internal/ui && node --test jstest/*.test.mjs && cd ../.. && go test -race ./internal/ui`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ui
git commit -m "feat(ui): incidents - list, the §56 incident view, lifecycle, timeline and containment links"
```

---

### Task 5: Read-only views — Inventory, Execution, Dependencies, Cost

**Files:**
- Create: `internal/ui/static/views/inventory.js`, `execution.js`, `dependencies.js`, `cost.js`
- Modify: `internal/ui/static/app.js`, `internal/ui/ui_test.go`

**Interfaces:**
- Consumes: `dom.js`, `router.js`, `views/common.js` (`back`, `mapText`).
- Produces: `inventory.js` exports `toolTable(tools, area)` (used by Security in Task 6) and `render(ctx)`; the others export `render(ctx)`.

- [ ] **Step 1: Implement `views/inventory.js`**

```js
// inventory.js shows agents, their versions and allowlists, connectors and
// tools with their recorded definitions. Everything here is read-only.
import {h, section, notice, table, badge, link, fmtTime, kv, json} from '../dom.js';
import {format, isUUID} from '../router.js';
import {back} from './common.js';

export async function render(ctx) {
  const [kind, id] = ctx.route.parts;
  if (kind === 'agents' && id) return agent(ctx, id);
  if (kind === 'connectors' && id) return connector(ctx, id);
  if (kind === 'tools' && id) return tool(ctx, id);
  return overview(ctx);
}

async function overview({client}) {
  const [agents, connectors] = await Promise.all([client.call('agent.list'), client.call('connector.list')]);
  return h('div', {},
    section('Agents', agents.ok ? table([
      ['Agent', a => link(a.name, format('inventory', ['agents', a.name]))],
      ['Display name', a => a.display_name],
      ['Environment', a => a.environment],
      ['Risk', a => badge(a.risk_class)],
      ['Registered', a => fmtTime(a.created_at)],
    ], agents.data.agents, 'No agents are registered.') : notice(agents)),
    section('Connectors', connectors.ok ? table([
      ['Connector', c => link(c.name, format('inventory', ['connectors', c.id]))],
      ['Protocol', c => c.protocol],
      ['Endpoint', c => c.endpoint],
      ['Tools', c => (c.tools ?? []).join(', ') || '—'],
    ], connectors.data.connectors, 'No connectors are registered.') : notice(connectors)));
}

async function agent({client}, ref) {
  const res = await client.call('agent.get', {params: {ref}});
  if (!res.ok) return notice(res);
  const a = res.data;
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, a.display_name || a.name),
    section('Agent', kv([
      ['Name', a.name], ['Id', h('code', {}, a.id)], ['Environment', a.environment], ['Risk class', badge(a.risk_class)],
      ['Owner principal', a.owner_principal_id ?? '—'], ['Owner group', a.owner_group_id ?? '—'],
      ['Registered', fmtTime(a.created_at)],
    ])),
    section('Versions (newest first)', table([
      ['Version', v => String(v.number)],
      ['State', v => badge(v.state)],
      ['Reason', v => v.state_reason ?? ''],
      ['Runtime', v => v.runtime],
      ['Code', v => v.code_ref],
      ['Allowed tools', v => (v.allowed_tools ?? []).join(', ') || '—'],
      ['Id', v => h('code', {}, v.id)],
    ], a.versions, 'No versions.')),
    h('p', {}, link('Blast radius of the active version', '#/dependencies')));
}

export function toolTable(tools, area = 'inventory') {
  return table([
    ['Tool', t => link(t.name, format(area, ['tools', t.id]))],
    ['Origin', t => t.origin],
    ['Executable', t => badge(t.executable ? 'executable' : 'not executable')],
    ['Contract matches', t => String(t.contract_matches)],
    ['Quarantined', t => (t.quarantined_at ? fmtTime(t.quarantined_at) : 'no')],
    ['Missing since', t => fmtTime(t.missing_since)],
  ], tools, 'No tools.');
}

async function connector({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not a connector id'});
  const [list, tools] = await Promise.all([client.call('connector.list'), client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: 'no such connector'});
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, c.name),
    section('Connector', kv([['Id', h('code', {}, c.id)], ['Protocol', c.protocol], ['Endpoint', c.endpoint],
      ['Secret reference', c.secret_ref || '—']])),
    section('Tools', tools.ok ? toolTable(tools.data.tools) : notice(tools)),
    h('p', {}, link('Circuit and containment', format('security', ['connectors', c.id]))));
}

async function tool({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not a tool id'});
  const [t, defs] = await Promise.all([client.call('tool.get', {params: {id}}),
    client.call('tool.definitions', {params: {id}})]);
  if (!t.ok) return notice(t);
  const x = t.data;
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, x.name),
    section('Tool', kv([
      ['Id', h('code', {}, x.id)],
      ['Connector', link(x.connector_id, format('inventory', ['connectors', x.connector_id]))],
      ['Origin', x.origin], ['Remote name', x.remote_name ?? '—'],
      ['Executable', badge(x.executable ? 'executable' : 'not executable')],
      ['Contract matches', String(x.contract_matches)], ['Active contract', x.active_contract_id ?? '—'],
      ['Quarantined', x.quarantined_at ? `${fmtTime(x.quarantined_at)}: ${x.quarantine_reason ?? ''}` : 'no'],
      ['Missing since', fmtTime(x.missing_since)],
    ])),
    h('p', {}, link('Containment for this tool', format('security', ['tools', x.id]))),
    section('Recorded definitions', defs.ok ? definitions(defs.data.definitions) : notice(defs)));
}

function definitions(list) {
  if (!list || list.length === 0) return h('p', {class: 'empty'}, 'No recorded definitions (HTTP tools have none).');
  return list.map(d => h('details', {},
    h('summary', {}, `#${d.seq} · risk ${d.risk} · ${fmtTime(d.observed_at)} · ${(d.changes ?? []).join(', ') || 'first seen'}`),
    kv([
      ['Fingerprint', h('code', {}, d.fingerprint)],
      ['Display digest', h('code', {}, d.display_digest)],
      ['Server hints (untrusted)', `read_only=${d.read_only} destructive=${d.destructive} idempotent=${d.idempotent} open_world=${d.open_world}`],
      ['Observed by', d.observed_by],
    ]),
    h('h3', {}, 'Definition'), json(d.definition),
    h('h3', {}, 'Server-described display (untrusted)'), json(d.display)));
}
```

- [ ] **Step 2: Implement `views/execution.js`**

```js
// execution.js lists actions by state and shows one with its evidence.
// Resolving an unknown outcome stays in eacpctl (two-person, ADR-004).
import {h, section, notice, table, badge, link, fmtTime, kv, json, field, select, values} from '../dom.js';
import {format, isUUID} from '../router.js';
import {back} from './common.js';

const STATES = ['NEEDS_HUMAN_RESOLUTION', 'UNKNOWN_OUTCOME', 'RECONCILING', 'RETRY_WAIT', 'QUEUED', 'LEASED',
  'EXECUTING', 'PENDING_APPROVAL', 'AUTHORIZED', 'RECEIVED', 'SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED'];

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list({client, route, go}) {
  const state = STATES.includes(route.query.state) ? route.query.state : STATES[0];
  const res = await client.call('action.list', {query: {state, limit: 200}});
  const filter = h('form', {class: 'filters', onsubmit: e => {
    e.preventDefault();
    go(format('execution', [], values(e.target)));
  }}, field('State', select('state', STATES, state)), h('button', {type: 'submit'}, 'Show'));
  return section('Actions', filter, res.ok ? table([
    ['Action', a => link(a.id.slice(0, 8), format('execution', [a.id]))],
    ['State', a => badge(a.state)],
    ['Tool', a => a.tool],
    ['Operation', a => a.operation],
    ['Target', a => a.target],
    ['Agent', a => link(a.agent_id.slice(0, 8), format('inventory', ['agents', a.agent_id]))],
    ['Changed', a => fmtTime(a.state_changed_at)],
    ['Attempts', a => String(a.attempt_count ?? 0)],
  ], res.data.actions, `No actions in ${state}.`) : notice(res));
}

async function detail({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an action id'});
  const [a, ev] = await Promise.all([client.call('action.get', {params: {id}}),
    client.call('action.evidence', {params: {id}})]);
  if (!a.ok) return notice(a);
  const x = a.data;
  return h('div', {},
    back('← Execution', format('execution', [], {state: x.state})),
    h('h1', {}, `${x.tool} · ${x.operation}`),
    h('div', {class: 'chips'}, badge(x.state)),
    section('Action', kv([
      ['Id', h('code', {}, x.id)], ['State reason', x.state_reason ?? '—'],
      ['Agent', link(x.agent_id, format('inventory', ['agents', x.agent_id]))], ['Agent version', x.agent_version_id],
      ['Subject', x.subject], ['Target', x.target], ['Resource', x.resource], ['Idempotency key', x.idempotency_key],
      ['Input digest', h('code', {}, x.input_digest)],
      ['Enforced digest', x.enforced_digest ? h('code', {}, x.enforced_digest) : '—'],
      ['Policy version', x.policy_version ?? '—'], ['Approval request', x.approval_request_id ?? '—'],
      ['Created', fmtTime(x.created_at)], ['State changed', fmtTime(x.state_changed_at)],
      ['Not after', fmtTime(x.not_after)], ['Attempts', String(x.attempt_count ?? 0)],
      ['Next attempt', fmtTime(x.next_attempt_at)],
    ])),
    h('p', {}, link('Kill this action', format('security', [], {scope: 'action', target: x.id}))),
    section('Enforced payload', x.enforced_payload ? json(x.enforced_payload) : h('p', {class: 'empty'}, 'None.')),
    section('Evidence: attempts, reconciliation, resolutions', ev.ok ? json(ev.data) : notice(ev)),
    h('p', {class: 'hint'}, 'Resolving an unknown outcome stays in eacpctl (two-person).'));
}
```

- [ ] **Step 3: Implement `views/dependencies.js`**

```js
// dependencies.js runs the ADR-015 blast-radius query. Observed evidence
// only: an undeclared dependency is never proven absent.
import {h, section, notice, table, link, kv, field, input, select, values} from '../dom.js';
import {format} from '../router.js';

const KINDS = ['tool', 'mcp', 'agent_version', 'model', 'system'];
const BY_NAME = new Set(['model', 'system']);

export async function render({client, route, go}) {
  const q = route.query;
  const form = h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const v = values(e.target);
    go(format('dependencies', [], {kind: v.kind, [BY_NAME.has(v.kind) ? 'name' : 'id']: v.target}));
  }},
  field('Kind', select('kind', KINDS, KINDS.includes(q.kind) ? q.kind : 'tool')),
  field('Id (tool, MCP connector, agent version) or name (model, system)',
    input('target', {value: q.id ?? q.name ?? '', required: true, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, 'Show blast radius'));
  const parts = [section('Blast radius', form,
    h('p', {class: 'hint'}, 'Observed evidence only: an undeclared dependency is never proven absent (ADR-015).'))];
  if (q.kind && (q.id || q.name)) {
    const res = await client.call('dependency.blast', {query: {kind: q.kind, id: q.id, name: q.name}});
    parts.push(res.ok ? report(res.data) : notice(res));
  }
  return h('div', {}, parts);
}

function impacts(list) {
  return table([
    ['Agent', i => link(i.agent_name, format('inventory', ['agents', i.agent_name]))],
    ['Version', i => String(i.version)],
    ['Environment', i => i.environment],
    ['Team', i => i.team || '—'],
    ['Owner', i => i.owner_principal ?? '—'],
  ], list, 'None.');
}

function report(r) {
  return h('div', {},
    section('Summary', kv([
      ['Target', `${r.target.kind} ${r.target.name || r.target.id || ''}`],
      ['Coverage', r.coverage],
      ['Affected teams', r.affected_teams.join(', ') || '—'],
      ['Data classes', r.data_classes.join(', ') || '—'],
      ['Actions in the last 24 hours', String(r.recent_actions_24h)],
    ])),
    section(`Confirmed (${r.confirmed_agents.length})`, impacts(r.confirmed_agents)),
    section(`Possible (${r.possible_agents.length})`, impacts(r.possible_agents)));
}
```

- [ ] **Step 4: Implement `views/cost.js`**

```js
// cost.js shows the FinOps dashboard and open alerts. FinOps observes and
// never blocks (ADR-025); acknowledging alerts stays in eacpctl.
import {h, section, notice, table, badge, fmtTime, kv} from '../dom.js';
import {mapText} from './common.js';

export async function render({client}) {
  const [d, alerts] = await Promise.all([client.call('finops.dashboard'),
    client.call('finops.alerts', {query: {open: 'true', limit: 100}})]);
  if (!d.ok) return notice(d);
  const x = d.data;
  return h('div', {},
    h('p', {class: 'asof'}, `As of ${fmtTime(x.as_of)}; days and months are UTC.`),
    section('Spend by unit', table([
      ['Unit', u => u.unit],
      ['Today', u => String(u.today)],
      ['Month to date', u => String(u.month_to_date)],
      ['Top agents', u => (u.top_agents ?? []).map(a => `${a.name} ${a.total}`).join(' · ') || '—'],
    ], x.units, 'No spend recorded.')),
    section('Today', kv([
      ['Hard budget blocks', mapText(x.hard_blocks_today)],
      ['Open alerts by kind', mapText(x.open_alerts)],
      ['Unpriced tokens', String(x.unpriced_tokens_today)],
    ])),
    section('Open alerts', alerts.ok ? table([
      ['Kind', a => badge(a.kind)],
      ['Subject', a => `${a.subject_type} ${a.subject_id}`],
      ['Unit', a => a.unit ?? '—'],
      ['Observed', a => a.observed ?? '—'],
      ['Threshold', a => a.threshold ?? '—'],
      ['Period', a => fmtTime(a.period_start)],
      ['Raised', a => fmtTime(a.created_at)],
    ], alerts.data.alerts, 'No open alerts.') : notice(alerts)),
    h('p', {class: 'hint'}, 'Acknowledging alerts stays in eacpctl.'));
}
```

- [ ] **Step 5: Wire them in**

In `app.js`, add the imports

```js
import * as inventory from './views/inventory.js';
import * as execution from './views/execution.js';
import * as dependencies from './views/dependencies.js';
import * as cost from './views/cost.js';
```

set

```js
const NAV = [
  ['overview', 'Overview', READERS],
  ['incidents', 'Incidents', READERS],
  ['inventory', 'Inventory', null],
  ['execution', 'Execution', ['operator', 'auditor']],
  ['dependencies', 'Dependencies', ['operator', 'auditor']],
  ['cost', 'Cost', READERS],
];
const VIEWS = {overview, incidents, inventory, execution, dependencies, cost};
```

and add `"views/cost.js", "views/dependencies.js", "views/execution.js", "views/inventory.js"` to `consoleFiles`.

- [ ] **Step 6: Run the console tests**

Run: `go test -race ./internal/ui && cd internal/ui && node --test jstest/*.test.mjs`
Expected: PASS (the lint, contract and file-set tests cover the new files).

- [ ] **Step 7: Commit**

```bash
git add internal/ui
git commit -m "feat(ui): inventory, execution, dependencies and cost views (read-only)"
```

---

### Task 6: Security view — kill switch, circuits, tool quarantine

**Files:**
- Create: `internal/ui/static/views/security.js`
- Create: `internal/ui/jstest/security.test.mjs`
- Modify: `internal/ui/static/app.js`, `internal/ui/ui_test.go`

**Interfaces:**
- Consumes: `inventory.js` `toolTable(tools, 'security')`; `common.js` `back`, `linkBack`; `confirm.js` `ask`.
- Produces: `circuitState(circuit) → 'disabled'|'open'|'closed'`, `killLines(scope, target, code) → string[]`; `render(ctx)` for `#/security`, `#/security/tools/<id>`, `#/security/connectors/<id>` (all accept `?incident=<id>`; `#/security` accepts `?scope=&target=`).

- [ ] **Step 1: Write the failing test**

`internal/ui/jstest/security.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {circuitState, killLines} from '../static/views/security.js';

test('circuit state: disabled wins over open', () => {
  assert.equal(circuitState({disabled: true, open: true}), 'disabled');
  assert.equal(circuitState({disabled: false, open: true}), 'open');
  assert.equal(circuitState({disabled: false, open: false}), 'closed');
});

test('the kill dialog restates scope, target and effect', () => {
  const lines = killLines('tenant', 't-1', 'security_incident');
  assert.deepEqual(lines.slice(0, 3), ['Scope: tenant', 'Target: t-1', 'Reason code: security_incident']);
  assert.match(lines[3], /stops/);
  assert.match(lines[3], /second operator/);
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd internal/ui && node --test jstest/security.test.mjs`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement `views/security.js`**

```js
// security.js is the containment console: kill states (ADR-016), connector
// circuits (ADR-022) and tool quarantine (ADR-023). Every write names its
// target in a confirm dialog with a reason; PostgreSQL enforces roles, the
// second operator for clearing a kill, and every other rule.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back, linkBack} from './common.js';
import {toolTable} from './inventory.js';

const SCOPES = ['tool', 'connector', 'agent_version', 'agent', 'team', 'action', 'tenant'];
const CODES = ['security_incident', 'policy_violation', 'operator_request', 'error_budget_exhausted'];
const MAX_CONNECTORS = 50;

export const circuitState = c => (c.disabled ? 'disabled' : c.open ? 'open' : 'closed');

export function killLines(scope, target, code) {
  return [`Scope: ${scope}`, `Target: ${target}`, `Reason code: ${code}`,
    'New dispatch in this scope stops at once; calls in flight see the epoch change and settle as unknown outcomes. A second operator must clear it.'];
}

export async function render(ctx) {
  const [kind, id] = ctx.route.parts;
  if (kind === 'tools' && id) return toolPage(ctx, id);
  if (kind === 'connectors' && id) return connectorPage(ctx, id);
  return overview(ctx);
}

const notAnID = what => notice({status: 404, error: 'not_found', detail: `not a ${what} id`});

async function overview(ctx) {
  const {client, session} = ctx;
  const operator = session.hasAny(['operator']);
  const [kills, conns] = await Promise.all([client.call('kill.list'), client.call('connector.list')]);
  const result = h('div');
  const parts = [section('Kill switch',
    kills.ok ? killTable(ctx, kills.data.kills, result) : notice(kills),
    operator ? killForm(ctx, result) : null,
    result)];
  if (!conns.ok) return h('div', {}, parts, notice(conns));
  const shown = conns.data.connectors.slice(0, MAX_CONNECTORS);
  const rows = await Promise.all(shown.map(async c => {
    const [circuit, tools] = await Promise.all([client.call('connector.circuit', {params: {id: c.id}}),
      client.call('connector.tools', {params: {id: c.id}})]);
    return {c, circuit, tools};
  }));
  parts.push(section('Connectors and circuits', table([
    ['Connector', r => link(r.c.name, format('security', ['connectors', r.c.id]))],
    ['Protocol', r => r.c.protocol],
    ['Circuit', r => (r.circuit.ok ? badge(circuitState(r.circuit.data)) : '—')],
    ['Quarantined tools', r => (r.tools.ok ? String(r.tools.data.tools.filter(t => t.quarantined_at).length) : '—')],
  ], rows, 'No connectors.'),
  conns.data.connectors.length > MAX_CONNECTORS
    ? h('p', {class: 'hint'}, `Showing ${MAX_CONNECTORS} of ${conns.data.connectors.length} connectors; the rest are in Inventory.`)
    : null));
  const quarantined = rows.flatMap(r => (r.tools.ok
    ? r.tools.data.tools.filter(t => t.quarantined_at).map(t => ({...t, connector: r.c.name})) : []));
  parts.push(section('Quarantined tools', table([
    ['Tool', t => link(`${t.connector}.${t.name}`, format('security', ['tools', t.id]))],
    ['Quarantined', t => fmtTime(t.quarantined_at)],
    ['Reason', t => t.quarantine_reason ?? '—'],
  ], quarantined, 'No quarantined tools.')));
  return h('div', {}, parts);
}

function killTable(ctx, kills, result) {
  const operator = ctx.session.hasAny(['operator']);
  return table([
    ['Scope', k => k.scope],
    ['Target', k => h('code', {}, k.target_id)],
    ['State', k => badge(k.killed ? 'killed' : 'cleared')],
    ['Code', k => k.reason_code],
    ['Reason', k => k.reason],
    ['Epoch', k => String(k.epoch)],
    ['Changed', k => fmtTime(k.changed_at)],
    ['', k => (operator && k.killed ? button('Clear…', () => clearKill(ctx, k, result)) : '')],
  ], kills, 'No kill states.');
}

async function clearKill(ctx, k, result) {
  const c = await ask({title: 'Clear a kill', reason: 'required', confirmLabel: 'Clear kill',
    lines: [`Scope ${k.scope}, target ${k.target_id}`,
      'Dispatch in this scope resumes. The operator who set the kill cannot clear it.']});
  if (!c) return;
  const r = await ctx.client.call('kill.set', {body: {scope: k.scope, target_id: k.target_id, killed: false,
    reason_code: 'operator_request', reason: c.reason}});
  if (r.ok) ctx.refresh();
  else replace(result, notice(r));
}

function killForm(ctx, result) {
  const q = ctx.route.query;
  const me = ctx.session.me();
  const initial = SCOPES.includes(q.scope) ? q.scope : 'tool';
  const target = input('target_id', {value: isUUID(q.target) ? q.target : (initial === 'tenant' ? me.tenantId : ''),
    placeholder: 'target UUID', required: true, size: 38});
  const scope = h('select', {name: 'scope', onchange: () => {
    if (scope.value === 'tenant') target.value = me.tenantId;
  }}, SCOPES.map(s => h('option', {value: s, selected: s === initial}, s)));
  const form = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = values(form);
    if (!isUUID(v.target_id)) {
      replace(result, notice({status: 400, error: 'invalid', detail: 'the target must be a UUID'}));
      return;
    }
    const c = await ask({title: `Kill scope ${v.scope}`, danger: true, reason: 'required', confirmLabel: 'Activate kill',
      typed: v.scope === 'tenant' ? 'tenant' : null, lines: killLines(v.scope, v.target_id, v.reason_code)});
    if (!c) return;
    const r = await ctx.client.call('kill.set', {body: {scope: v.scope, target_id: v.target_id, killed: true,
      reason_code: v.reason_code, reason: c.reason}});
    if (!r.ok) {
      replace(result, notice(r));
      return;
    }
    replace(result, ok(`Kill active: ${r.data.scope} ${r.data.target_id}, epoch ${r.data.epoch}.`),
      linkBack(ctx, 'kill_state', r.data.id), button('Refresh', ctx.refresh));
  }},
  field('Scope', scope), field('Target', target), field('Reason code', select('reason_code', CODES, 'security_incident')),
  h('button', {type: 'submit', class: 'danger'}, 'Kill…'));
  return form;
}

async function toolPage(ctx, id) {
  if (!isUUID(id)) return notAnID('tool');
  const res = await ctx.client.call('tool.get', {params: {id}});
  if (!res.ok) return notice(res);
  const t = res.data;
  const quarantined = Boolean(t.quarantined_at);
  const result = h('div');
  const acts = [];
  if (!quarantined && ctx.session.hasAny(['operator', 'registry_approver'])) {
    acts.push(button('Quarantine…', () => quarantine(ctx, t, result), {kind: 'danger'}));
  }
  if (quarantined && ctx.session.hasAny(['registry_approver'])) {
    acts.push(button('Release…', () => release(ctx, t, result)));
  }
  return h('div', {},
    back('← Security', '#/security'),
    h('h1', {}, t.name),
    h('div', {class: 'chips'}, badge(quarantined ? 'quarantined' : 'not quarantined'),
      badge(t.executable ? 'executable' : 'not executable')),
    section('Tool', kv([
      ['Id', h('code', {}, t.id)],
      ['Connector', link(t.connector_id, format('security', ['connectors', t.connector_id]))],
      ['Origin', t.origin], ['Contract matches', String(t.contract_matches)],
      ['Quarantined', quarantined ? fmtTime(t.quarantined_at) : 'no'], ['Quarantine reason', t.quarantine_reason ?? '—'],
    ])),
    h('div', {class: 'actions'}, acts,
      link('Kill this tool', format('security', [], {scope: 'tool', target: t.id, incident: ctx.route.query.incident})),
      link('Recorded definitions', format('inventory', ['tools', t.id]))),
    result);
}

async function quarantine(ctx, t, result) {
  const c = await ask({title: `Quarantine ${t.name}`, danger: true, reason: 'required', confirmLabel: 'Quarantine',
    lines: ['The tool stops executing for every agent until a registry approver releases it.']});
  if (!c) return;
  const r = await ctx.client.call('tool.quarantine', {params: {id: t.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Quarantined.'), linkBack(ctx, 'tool', t.id), button('Refresh', ctx.refresh));
}

async function release(ctx, t, result) {
  const c = await ask({title: `Release ${t.name}`, reason: 'required', confirmLabel: 'Release',
    lines: ['Release does not recertify the tool: its contract must still match its current definition (ADR-023).']});
  if (!c) return;
  const r = await ctx.client.call('tool.release', {params: {id: t.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Released.'), linkBack(ctx, 'tool', t.id), button('Refresh', ctx.refresh));
}

async function connectorPage(ctx, id) {
  if (!isUUID(id)) return notAnID('connector');
  const [list, circuit, tools] = await Promise.all([ctx.client.call('connector.list'),
    ctx.client.call('connector.circuit', {params: {id}}), ctx.client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: 'no such connector'});
  const result = h('div');
  let circuitPart = notice(circuit);
  if (circuit.ok) {
    const s = circuit.data;
    const by = s.changed_by ?? (s.changed_by_worker ? `worker ${s.changed_by_worker}` : '—');
    circuitPart = h('div', {},
      kv([['State', badge(circuitState(s))], ['Open until', fmtTime(s.open_until)], ['Reason', s.reason || '—'],
        ['Changed', fmtTime(s.changed_at)], ['Changed by', by]]),
      ctx.session.hasAny(['operator']) ? h('div', {class: 'actions'}, s.disabled
        ? button('Enable…', () => enableCircuit(ctx, c, result))
        : button('Disable…', () => disableCircuit(ctx, c, result), {kind: 'danger'})) : null);
  }
  return h('div', {},
    back('← Security', '#/security'),
    h('h1', {}, c.name),
    section('Circuit', circuitPart),
    section('Tools', tools.ok ? toolTable(tools.data.tools, 'security') : notice(tools)),
    result);
}

async function disableCircuit(ctx, c, result) {
  const x = await ask({title: `Disable connector ${c.name}`, danger: true, reason: 'required', confirmLabel: 'Disable',
    lines: ['New dispatch to this connector stops until an operator enables it. Calls in flight are not interrupted.']});
  if (!x) return;
  const r = await ctx.client.call('circuit.disable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Disabled.'), linkBack(ctx, 'connector', c.id), button('Refresh', ctx.refresh));
}

async function enableCircuit(ctx, c, result) {
  const x = await ask({title: `Enable connector ${c.name}`, reason: 'required', confirmLabel: 'Enable',
    lines: ['Dispatch to this connector resumes (a breaker a worker opened still holds until it expires).']});
  if (!x) return;
  const r = await ctx.client.call('circuit.enable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Enabled.'), linkBack(ctx, 'connector', c.id), button('Refresh', ctx.refresh));
}
```

- [ ] **Step 4: Wire it in**

`app.js`: `import * as security from './views/security.js';`, insert `['security', 'Security', ['operator', 'auditor', 'registry_approver']],` after the incidents NAV row, and add `security` to `VIEWS`. `ui_test.go`: add `"views/security.js"`.

- [ ] **Step 5: Run the console tests**

Run: `cd internal/ui && node --test jstest/*.test.mjs && cd ../.. && go test -race ./internal/ui`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ui
git commit -m "feat(ui): security view - kill switch, circuits and tool quarantine behind the confirm dialog"
```

---

### Task 7: Fleet operations and approval votes

**Files:**
- Create: `internal/ui/static/views/fleet.js`, `internal/ui/static/views/approvals.js`
- Create: `internal/ui/jstest/fleet.test.mjs`
- Modify: `internal/ui/static/app.js`, `internal/ui/ui_test.go`

**Interfaces:**
- Consumes: `common.js` (`back`, `linkBack`, `mapText`), `confirm.js` `ask`.
- Produces: `fleet.js` `request(formValues) → fleet request body` (exported for tests); `render(ctx)` for `#/fleet?op=&agents=&tool=&environment=&source=&incident=` and `#/fleet/operations/<id>`; `approvals.js` `render(ctx)` for `#/approvals` and `#/approvals/<id>`.

- [ ] **Step 1: Write the failing test**

`internal/ui/jstest/fleet.test.mjs`:

```js
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {request} from '../static/views/fleet.js';

test('pause and quarantine carry only the filled selector fields and never select all', () => {
  assert.deepEqual(request({kind: 'pause', reason: 'drift', agents: ' a-bot, ,b-bot ', tool: 'erp.post_invoice',
    environment: '', risk_class: 'high', source_operation_id: 'ignored'}),
  {kind: 'pause', reason: 'drift', selector: {agents: ['a-bot', 'b-bot'], tool: 'erp.post_invoice', risk_class: 'high'}});
  const empty = request({kind: 'quarantine', reason: 'r', agents: '', tool: '', environment: '', risk_class: ''});
  assert.deepEqual(empty, {kind: 'quarantine', reason: 'r', selector: {}});
  assert.equal('all' in empty.selector, false);
});

test('resume and release undo a source operation and select nothing else', () => {
  assert.deepEqual(request({kind: 'resume', reason: 'fixed', agents: 'a-bot', source_operation_id: 'op-1'}),
    {kind: 'resume', reason: 'fixed', source_operation_id: 'op-1'});
  assert.deepEqual(request({kind: 'release', reason: 'ok', source_operation_id: 'op-2'}),
    {kind: 'release', reason: 'ok', source_operation_id: 'op-2'});
});

test('the request never carries dry_run; the preview adds it', () => {
  assert.equal('dry_run' in request({kind: 'pause', reason: 'r', agents: 'a'}), false);
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd internal/ui && node --test jstest/fleet.test.mjs`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement `views/fleet.js`**

```js
// fleet.js shows fleet health and runs fleet operations (ADR-024). Every
// operation is previewed with dry_run first; the confirmed request is the
// previewed one without dry_run, and PostgreSQL re-checks each transition.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back, linkBack, mapText} from './common.js';

const OPS = ['pause', 'quarantine', 'resume', 'release'];

export async function render(ctx) {
  if (ctx.route.parts[0] === 'operations' && ctx.route.parts[1]) return operation(ctx, ctx.route.parts[1]);
  const {client, session} = ctx;
  const [health, agents] = await Promise.all([client.call('fleet.health'), client.call('fleet.agents')]);
  return h('div', {},
    health.ok ? healthPanel(health.data) : notice(health),
    session.hasAny(['operator', 'registry_approver']) ? operationForm(ctx) : null,
    section('Agents', agents.ok ? agentTable(agents.data.agents) : notice(agents)));
}

function healthPanel(s) {
  return section(`Fleet health (last ${s.window})`,
    kv([
      ['Agents', String(s.agents)], ['By health', mapText(s.by_health)], ['By environment', mapText(s.by_environment)],
      ['Quarantined', String(s.quarantined)], ['Unknown owner', String(s.unknown_owner)],
      ['Capability drift', String(s.capability_drift)], ['Circuit issues', String(s.circuit_issues)],
      ['Active kills', String(s.active_kills)], ['Open actions', mapText(s.open_actions)],
    ]),
    h('p', {class: 'hint'}, `Coverage: ${s.coverage}. The fleet view observes; it never decides.`));
}

function agentTable(list) {
  return table([
    ['Agent', a => link(a.name, format('inventory', ['agents', a.name]))],
    ['Environment', a => a.environment],
    ['Risk', a => badge(a.risk_class)],
    ['Health', a => badge(a.health)],
    ['Reasons', a => (a.reasons ?? []).join(', ') || '—'],
    ['Active version', a => (a.active_version ? `v${a.active_version.number}` : '—')],
    ['Canary', a => (a.canary ? `v${a.canary.version.number} at ${a.canary.canary_bp / 100}%` : '—')],
    ['Kills', a => String((a.kills ?? []).length)],
    ['Drift', a => String((a.capability_drift ?? []).length)],
  ], list, 'No agents.');
}

export function request(v) {
  const r = {kind: v.kind, reason: v.reason};
  if (v.kind === 'resume' || v.kind === 'release') {
    r.source_operation_id = v.source_operation_id;
    return r;
  }
  const selector = {};
  const agents = String(v.agents ?? '').split(',').map(s => s.trim()).filter(Boolean);
  if (agents.length) selector.agents = agents;
  if (v.tool) selector.tool = v.tool;
  if (v.environment) selector.environment = v.environment;
  if (v.risk_class) selector.risk_class = v.risk_class;
  r.selector = selector;
  return r;
}

function operationForm(ctx) {
  const q = ctx.route.query;
  const result = h('div');
  const form = h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const v = values(form);
    if ((v.kind === 'resume' || v.kind === 'release') && !isUUID(v.source_operation_id)) {
      replace(result, notice({status: 400, error: 'invalid', detail: `${v.kind} needs the source operation's UUID`}));
      return;
    }
    preview(ctx, request(v), result);
  }},
  field('Operation', select('kind', OPS, OPS.includes(q.op) ? q.op : 'pause')),
  field('Agents (slugs, comma separated)', input('agents', {value: q.agents ?? '', size: 40})),
  field('Tool on the allowlist (connector.tool)', input('tool', {value: q.tool ?? '', size: 28})),
  field('Environment', select('environment', ['', 'development', 'staging', 'production'], q.environment ?? '')),
  field('Risk class', select('risk_class', ['', 'low', 'medium', 'high', 'critical'], '')),
  field('Source operation (resume, release)', input('source_operation_id', {value: q.source ?? '', size: 38})),
  field('Reason', input('reason', {required: true, maxlength: 1024, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, 'Preview (dry run)'));
  return section('Fleet operation',
    h('p', {class: 'hint'}, 'Every operation is previewed first. Pause and quarantine need a selector; the console never selects the whole fleet.'),
    form, result);
}

function targetTable(targets) {
  return table([
    ['Agent', t => t.agent_name],
    ['Version', t => String(t.version)],
    ['From', t => badge(t.from)],
    ['To', t => badge(t.to)],
  ], targets, 'No targets.');
}

async function preview(ctx, req, result) {
  const dry = await ctx.client.call('fleet.apply', {body: {...req, dry_run: true}});
  if (!dry.ok) {
    replace(result, notice(dry));
    return;
  }
  const targets = dry.data.targets ?? [];
  const skipped = dry.data.skipped ?? [];
  replace(result, 
    h('h3', {}, `Dry run: ${targets.length} version(s) would change, ${skipped.length} skipped`),
    targetTable(targets),
    skipped.length ? table([['Agent', s => s.agent_name], ['Skipped because', s => s.reason]], skipped) : null,
    targets.length
      ? button(`Apply ${req.kind}…`, () => apply(ctx, req, targets, result), {kind: 'danger'})
      : h('p', {class: 'hint'}, 'Nothing to change.'));
}

async function apply(ctx, req, targets, result) {
  const listed = targets.slice(0, 10).map(t => `${t.agent_name} v${t.version} ${t.from}→${t.to}`).join(', ');
  const c = await ask({title: `Apply fleet ${req.kind}`, danger: true, confirmLabel: `Apply ${req.kind}`,
    lines: [`${targets.length} version(s): ${listed}${targets.length > 10 ? ', …' : ''}`, `Reason: ${req.reason}`,
      'PostgreSQL re-checks every transition: the applied set can differ from the preview if the fleet changed.']});
  if (!c) return;
  const r = await ctx.client.call('fleet.apply', {body: req});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(`Applied operation ${r.data.id}: ${r.data.targets.length} version(s) changed.`),
    h('div', {class: 'actions'}, link('Open the operation', format('fleet', ['operations', r.data.id]))),
    linkBack(ctx, 'fleet_operation', r.data.id));
}

async function operation({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an operation id'});
  const res = await client.call('fleet.operation', {params: {id}});
  if (!res.ok) return notice(res);
  const op = res.data;
  const undo = op.kind === 'pause' ? 'resume' : op.kind === 'quarantine' ? 'release' : null;
  return h('div', {},
    back('← Fleet', '#/fleet'),
    h('h1', {}, `Fleet ${op.kind}`),
    section('Operation', kv([
      ['Id', h('code', {}, op.id)], ['Reason', op.reason], ['Created', fmtTime(op.created_at)],
      ['Created by', op.created_by ?? '—'], ['Source operation', op.source_operation_id ?? '—'],
      ['Selector', JSON.stringify(op.selector ?? {})],
    ]),
    undo ? h('p', {}, link(`Prepare a ${undo} of this operation`, format('fleet', [], {op: undo, source: op.id}))) : null),
    section('Targets', targetTable(op.targets ?? [])),
    (op.skipped ?? []).length ? section('Skipped', table([['Agent', s => s.agent_name], ['Reason', s => s.reason]], op.skipped)) : null);
}
```

- [ ] **Step 4: Implement `views/approvals.js`**

```js
// approvals.js lists the approvals waiting for the caller's vote and casts
// one. The vote binds to the enforced payload digest; the server checks
// eligibility, separation of duties and quorum (ADR-005).
import {h, section, notice, table, link, fmtTime, kv, json, button, ok, badge, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back} from './common.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list({client}) {
  const res = await client.call('approval.list');
  return section('Approvals waiting for your vote', res.ok ? table([
    ['Request', a => link(a.id.slice(0, 8), format('approvals', [a.id]))],
    ['Action', a => h('code', {}, a.action_id)],
    ['Enforced digest', a => h('code', {}, `${a.enforced_digest.slice(0, 16)}…`)],
    ['Quorum', a => String(a.required_quorum)],
    ['Policy', a => `v${a.policy_version}`],
    ['Expires', a => fmtTime(a.expires_at)],
  ], res.data.items, 'Nothing waits for your vote.') : notice(res));
}

async function detail(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an approval id'});
  const res = await ctx.client.call('approval.get', {params: {id}});
  if (!res.ok) return notice(res);
  const a = res.data;
  const result = h('div');
  return h('div', {},
    back('← Approvals', '#/approvals'),
    h('h1', {}, 'Approval request'),
    h('div', {class: 'chips'}, badge(a.state)),
    section('Request', kv([
      ['Id', h('code', {}, a.id)], ['Action', h('code', {}, a.action_id)],
      ['Enforced digest', h('code', {}, a.enforced_digest)], ['Policy version', String(a.policy_version)],
      ['Quorum', String(a.required_quorum)], ['Expires', fmtTime(a.expires_at)],
    ])),
    section('Enforced payload (what executes if approved)', json(a.enforced_payload)),
    a.state === 'PENDING' ? h('div', {class: 'actions'},
      button('Approve…', () => vote(ctx, a, 'APPROVE', result), {kind: 'primary'}),
      button('Deny…', () => vote(ctx, a, 'DENY', result), {kind: 'danger'})) : null,
    result);
}

async function vote(ctx, a, decision, result) {
  const approve = decision === 'APPROVE';
  const c = await ask({title: approve ? 'Approve this action' : 'Deny this action', danger: !approve,
    reason: 'required', confirmLabel: approve ? 'Approve' : 'Deny',
    lines: [`Action ${a.action_id}`, `Enforced digest ${a.enforced_digest}`,
      'Your vote binds to this exact payload digest. The server checks eligibility and separation of duties.']});
  if (!c) return;
  const r = await ctx.client.call('approval.vote', {params: {id: a.id}, body: {decision, reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(`Vote recorded; the request is ${r.data.request_state}.`), button('Refresh', ctx.refresh));
}
```

- [ ] **Step 5: Wire them in**

`app.js`: add `import * as fleet from './views/fleet.js';` and `import * as approvals from './views/approvals.js';`; the final `NAV` and `VIEWS`:

```js
const NAV = [
  ['overview', 'Overview', READERS],
  ['incidents', 'Incidents', READERS],
  ['security', 'Security', ['operator', 'auditor', 'registry_approver']],
  ['fleet', 'Fleet', ['operator', 'auditor', 'registry_approver']],
  ['approvals', 'Approvals', ['approver']],
  ['execution', 'Execution', ['operator', 'auditor']],
  ['inventory', 'Inventory', null],
  ['dependencies', 'Dependencies', ['operator', 'auditor']],
  ['cost', 'Cost', READERS],
];
const VIEWS = {overview, incidents, security, fleet, approvals, execution, inventory, dependencies, cost};
```

`ui_test.go`: add `"views/approvals.js", "views/fleet.js"` to `consoleFiles`.

- [ ] **Step 6: Run the console tests**

Run: `cd internal/ui && node --test jstest/*.test.mjs && cd ../.. && go vet ./internal/ui && go test -race ./internal/ui ./cmd/controlplane-api`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/ui
git commit -m "feat(ui): fleet operations (dry run, then confirm) and approval votes"
```

---

### Task 8: End-to-end check in the browser on the Slice C demo stack

**Files:**
- Modify (only if the check finds a defect): the affected `internal/ui/static/**` file, with a unit test for the defect where one can be written.
- Create: `docs/reviews/2026-09-26-phase22b-console-e2e.md` (the recorded walk-through).

**Interfaces:**
- Consumes: everything above; `scripts/demo.sh` (`KEEP=1 DEMO=C` leaves the isolated stack on port 18080); the demo's principal keys (read how `scripts/demo.sh` and `test/demo` create them, and print the operator keys from the kept stack the same way — never commit a key).

- [ ] **Step 1: Bring up the demo stack with the console**

Run: `KEEP=1 DEMO=C scripts/demo.sh` (Git Bash; see AGENTS.md for the Fake ERP token prerequisite). Expected: the demo passes and leaves `eacp-demo` running; `curl -s localhost:18080/ui/ | head -3` shows the console HTML, and `curl -sI localhost:18080/ui/app.js` shows the CSP header.

- [ ] **Step 2: Walk the §1 success flow in the built-in browser**

Open `http://localhost:18080/ui/` with `mcp__Claude_Browser__preview_start` (url). Using the first operator's key:
1. Sign in; the overview shows the MCP drift and kill incidents as open, with non-zero Security counters.
2. Open the drift incident; the Affected table lists the affected agent; the Recommended containment lists "Pause the agents that use this tool".
3. Follow it: the Fleet form is pre-filled (`op=pause`, `tool=…`); Preview shows the targets; Apply opens the confirm dialog; confirm; "Link this to the incident" records the link.
4. Back on the incident: Acknowledge (reason), then Resolve as the same operator → the server's refusal is shown (critical incident, two-person).
5. Sign out; sign in as the second operator; Resolve → the incident is `RESOLVED` and the timeline shows opened, acknowledged, linked and resolved.
6. Check `read_console_messages` for CSP or Trusted Types violations: there must be none.
7. Resize to the mobile preset and check the overview and an incident page have no horizontal page scroll; then reset to desktop.

- [ ] **Step 3: Check hostile text**

As the operator, open a manual incident with the title `<img src=x onerror=alert(1)>` and a note `<script>alert(1)</script>`; both must display as literal text (use `get_page_text`), and no dialog or console error may appear.

- [ ] **Step 4: Check the idle and 401 paths**

In the browser, run `javascript_tool` only to inspect (never to change behaviour): confirm `localStorage.length === 0`, `sessionStorage.length === 0` and `document.cookie === ''` after signing in. Reload the page: the sign-in form returns.

- [ ] **Step 5: Record and clean up**

Write `docs/reviews/2026-09-26-phase22b-console-e2e.md` with each step's outcome (no keys, no ids that identify a person). Fix any defect found (test first where possible), then rerun `go test -race ./internal/ui` and node tests. Tear the stack down: `docker compose -p eacp-demo down -v --remove-orphans`.

- [ ] **Step 6: Commit**

```bash
git add docs/reviews/2026-09-26-phase22b-console-e2e.md internal/ui
git commit -m "test(ui): end-to-end console walk-through on the Slice C demo stack"
```

---

### Task 9: Documentation, full verification and final review

**Files:**
- Create: `docs/adr/ADR-028-operator-console.md`
- Modify: `docs/adr/README.md` (index row), `docs/MASTER_PLAN.md` §94 status, `AGENTS.md` (status line, a console rule, `internal/ui` in the layout), `README.md` (how to open the console), `docs/DEMO.md` (the console step for Slice C)
- Create: `docs/reviews/2026-09-26-phase22b-code-review.md`

- [ ] **Step 1: Write ADR-028**

`docs/adr/ADR-028-operator-console.md` — Status Accepted (Rev 1.0, 2026-09-26), scope Phase 22b (MASTER_PLAN §55–§57, §94). Sections: Context (22a gave the API; operators need one screen); Decision: 1. a client, never an authority (no new routes, tables or roles; role checks only hide controls); 2. serving (`internal/ui`, `go:embed`, `/ui/`, `EACP_UI`, the exact CSP and headers, `nosniff` on API JSON); 3. key custody (closure only, idle 30 min, 401, reload; no cookies so no CSRF); 4. hostile data is text (`dom.js`, the lint list, Trusted Types); 5. every write is deliberate (reason, confirm dialog, typed `tenant`, fleet dry run first, one dialog at a time, never `selector.all`); 6. the route table and its contract test; 7. freshness (15 s polling, no NATS or websocket); Consequences; Unresolved assumptions (conservative choices) table: key persistence across reloads → none; idle timeout → 30 min; tenant kill → typed word; fleet `all` → never from the UI; action resolution, FinOps ack, release management → stay in eacpctl; principals shown as ids (no directory route). Add the row to `docs/adr/README.md`.

- [ ] **Step 2: Update the plan, AGENTS.md, README and DEMO**

- `docs/MASTER_PLAN.md` §94 status: "22a and 22b delivered (ADR-027, ADR-028). The operator console at `/ui/` covers the §94 areas; it calls the existing API with the operator's own key and holds no authority."
- `AGENTS.md` status line: Phase 22 complete (22a incidents, 22b operator console, ADR-028). Add the rule: "The operator console is a client, never an authority (ADR-028). No new routes for it; every route it calls is in `internal/ui/static/api.js` ROUTES, and `TestEveryConsoleCallIsARealRoute` keeps them real. DOM only through `dom.js`; `TestConsoleUsesNoDangerousSinks` bans HTML sinks, storage and other origins. The key lives in memory only. Every write goes through `confirm.js`." Add `internal/ui  operator console: embedded static files, strict CSP (ADR-028); node --test jstest/*.test.mjs` to the layout, and `cd internal/ui && node --test jstest/*.test.mjs` to Commands.
- `README.md`: a short "Operator console" section: open `http://localhost:8080/ui/`, sign in with a principal key, `EACP_UI=off` to disable.
- `docs/DEMO.md`: after the Slice C steps, "Open the console" with the §1 flow.

- [ ] **Step 3: Full verification**

Run (Git Bash, from the repo root):

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
docker compose up -d postgres
export EACP_TEST_ADMIN_DSN="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
export EACP_UI_NODE_REQUIRED=1
go vet ./... && go test -race ./...
```

Expected: every package `ok`; `internal/ui` includes `TestJavaScriptUnitTests` (it fails rather than skips when node is missing). Also `gofmt -l .` prints nothing, and `go test ./test/invariants` passes.

- [ ] **Step 4: Final review**

Review the whole branch diff since `304759d` against the spec and ADR-028 (self-review, or a fresh reviewer subagent on the most capable model; not Codex). Record findings and their fixes in `docs/reviews/2026-09-26-phase22b-code-review.md`. Fix every Critical/Important finding with a test first, rerun Step 3.

- [ ] **Step 5: Commit**

```bash
git add docs AGENTS.md README.md internal cmd
git commit -m "docs: ADR-028 operator console; Phase 22b delivered"
```

- [ ] **Step 6: Report and stop**

Update the project memory (`eacp-project.md`: Phase 22b done, commits, facts to keep). Report to the owner and stop: MASTER_PLAN §107 forbids starting Phase 23 automatically.
