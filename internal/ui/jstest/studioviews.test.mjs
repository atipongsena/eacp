// The Studio page's views, rendered with a fake client (Phase 27a-3b).
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {installFakeDOM, all, byText} from './fakedom.mjs';
import {setLang} from '../static/i18n.js';
import * as form from '../static/studio/form.js';
import * as agents from '../static/studio/agents.js';
import * as requests from '../static/studio/requests.js';
import * as runs from '../static/studio/run.js';
import * as hub from '../static/studio/hub.js';

installFakeDOM();
setLang('en');

const FIXTURE = JSON.parse(readFileSync(fileURLToPath(new URL('../../../test/demo/testdata/leave-balance.json', import.meta.url)), 'utf8'));
const ME = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const OTHER = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const GROUP = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const AGENT = 'dddddddd-dddd-4ddd-8ddd-dddddddddddd';
const VERSION = 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee';
const RUN = 'ffffffff-ffff-4fff-8fff-ffffffffffff';
const CONN = '12121212-1212-4121-8121-121212121212';

function fakeClient(responses = {}) {
  const calls = [];
  return {calls, call(name, opts) {
    calls.push({name, opts});
    const res = responses[name] ?? {ok: true, status: 200, data: {}};
    return Promise.resolve(typeof res === 'function' ? res(opts) : res);
  }};
}
const session = (groups = [{id: GROUP, name: 'hr', displayName: 'HR'}], roles = ['studio_author']) => ({
  me: () => ({principalId: ME, tenantId: 't', roles, groups}),
  hasAny: r => r == null || r.some(x => roles.includes(x)),
});
function ctxFor(area, parts, query, client, sess = session()) {
  const ctx = {client, session: sess, route: {area, parts, query}, started: [], went: [], polls: [],
    go: h => ctx.went.push(h), refresh() {}, pollEvery: ms => ctx.polls.push(ms)};
  return ctx;
}
// confirmDialog submits the confirm.js dialog that is open, with a reason.
async function confirmDialog(reason = '') {
  await new Promise(r => setTimeout(r, 0));
  const dialog = document.body.childNodes.at(-1);
  assert.equal(dialog.tagName, 'DIALOG');
  const box = all(dialog).find(e => e.name === 'reason');
  if (box) box.value = reason;
  await all(dialog).find(e => e.tagName === 'FORM').dispatch('submit');
}
const settle = () => new Promise(r => setTimeout(r, 10));
const catalogue = {
  'model.list': {ok: true, status: 200, data: {models: [{name: 'triage', provider: 'openai'}]}},
  'connector.list': {ok: true, status: 200, data: {connectors: [{id: CONN, name: 'hr-mcp', tools: ['get_leave_balance']},
    {id: '34343434-3434-4343-8343-343434343434', name: 'empty', tools: []}]}},
  'connector.tools': {ok: true, status: 200, data: {tools: [{name: 'get_leave_balance', executable: true,
    definition: {read_only: true, risk: 'low'}}]}},
};

test('a stale new-version save keeps the edited form and offers a separately confirmed copy', async () => {
  const client = fakeClient({...catalogue, 'studio.agents': agentList('ready'), 'studio.version': version('ready'),
    'studio.newversion': {ok: false, status: 409, error: 'conflict', detail: 'studio_version_stale'},
    'studio.save': {ok: true, status: 201, data: {agent_id: AGENT}}});
  const node = await form.render(ctxFor('new', [], {agent: AGENT}, client));
  const answer = all(node).find(e => e.tagName === 'TEXTAREA' && e.textContent.startsWith('You have'));
  answer.value = 'My edited answer'; await answer.dispatch('input');
  let click = byText(node, 'button', 'Save the new version…').dispatch('click');
  assert.equal(client.calls.some(c => c.name === 'studio.newversion'), false);
  await confirmDialog(); await click;
  const write = client.calls.find(c => c.name === 'studio.newversion');
  assert.equal(write.opts.body.expected_version_id, VERSION);
  assert.equal(write.opts.body.definition.steps[1].text, 'My edited answer');
  assert.equal(answer.value, 'My edited answer');
  await byText(node, 'button', 'Save as copy').dispatch('click');
  const name = all(node).find(e => e.tagName === 'INPUT' && e.value === 'leave-bot-copy');
  name.value = 'copied'; await name.dispatch('input');
  click = byText(node, 'button', 'Save…').dispatch('click'); await confirmDialog(); await click;
  assert.equal(client.calls.find(c => c.name === 'studio.save').opts.body.definition.steps[1].text, 'My edited answer');
});

test('an approved owner preview confirms real model spend and sends only tool samples', async () => {
  const client = fakeClient({'studio.agents': agentList('ready'), 'studio.version': version('ready'),
    'studio.preview': {ok: true, status: 201, data: {id: RUN}}});
  const ctx = ctxFor('agents', [AGENT], {}, client);
  const node = await agents.render(ctx);
  const areas = all(node).filter(e => e.tagName === 'TEXTAREA');
  assert.equal(areas.length, 2);
  areas[0].value = '{"employee_id":"E-1"}'; await areas[0].dispatch('input');
  areas[1].value = '{"lookup":{"structuredContent":{"days":12}},"unused":{"x":1}}'; await areas[1].dispatch('input');
  const click = byText(node, 'button', 'Preview approved version…').dispatch('click');
  assert.equal(client.calls.some(c => c.name === 'studio.preview'), false);
  assert.match(document.body.childNodes.at(-1).textContent, /real calls/);
  await confirmDialog(); await click;
  assert.deepEqual(client.calls.find(c => c.name === 'studio.preview').opts, {params: {id: VERSION}, body: {inputs: {employee_id: 'E-1'}, samples: {lookup: {structuredContent: {days: 12}}}}});
  assert.deepEqual(ctx.went, [`#/runs/${RUN}`]);
});

test('the template form saves the fixture definition into the author’s department', async () => {
  const client = fakeClient({...catalogue,
    'studio.save': {ok: true, status: 201, data: {id: VERSION, agent_id: AGENT}}});
  const ctx = ctxFor('new', [], {template: 'leave-balance'}, client);
  const node = await form.render(ctx);
  assert.equal(client.calls.filter(c => c.name === 'connector.tools').length, 1, 'only connectors with tools are listed');
  const option = all(node).find(e => e.tagName === 'OPTION' && e.getAttribute('value') === 'hr-mcp.get_leave_balance'
    && e.getAttribute('selected') !== null);
  assert.ok(option, 'the template’s tool is chosen in the picker');
  assert.match(option.textContent, /read-only/);
  const save = byText(node, 'button', 'Save…');
  const clicked = save.dispatch('click');
  await confirmDialog();
  await clicked;
  const call = client.calls.find(c => c.name === 'studio.save');
  assert.ok(call, 'saved');
  assert.deepEqual(call.opts.body, {name: 'leave-bot', display_name: 'Leave balance',
    description: 'Tells an employee how many days of leave they have left.', department_id: GROUP, definition: FIXTURE});
  assert.deepEqual(ctx.went, [`#/agents/${AGENT}`]);
});

test('a payload that is not a JSON object is named and nothing is sent', async () => {
  const client = fakeClient(catalogue);
  const node = await form.render(ctxFor('new', [], {template: 'leave-balance'}, client));
  const payload = all(node).find(e => e.tagName === 'TEXTAREA' && e.textContent.includes('employee_id'));
  payload.value = '["not", "an object"]';
  await payload.dispatch('input');
  await byText(node, 'button', 'Save…').dispatch('click');
  assert.equal(client.calls.filter(c => c.name === 'studio.save').length, 0);
  assert.match(node.textContent, /steps\.0\.payload/);
  assert.equal(document.body.childNodes.filter(n => n.tagName === 'DIALOG' && n.open).length, 0);
});

test('an author in no group is told why they cannot save', async () => {
  const node = await form.render(ctxFor('new', [], {}, fakeClient(catalogue), session([])));
  assert.equal(byText(node, 'button', 'Save…').disabled, true);
  assert.match(node.textContent, /no group/);
});

test('a refusal shows the server’s reason', async () => {
  const client = fakeClient({...catalogue, 'studio.save': {ok: false, status: 400, error: 'invalid',
    detail: 'definition: unknown tool hr-mcp.get_leave_balance'}});
  const node = await form.render(ctxFor('new', [], {template: 'leave-balance'}, client));
  const clicked = byText(node, 'button', 'Save…').dispatch('click');
  await confirmDialog();
  await clicked;
  await settle();
  assert.match(node.textContent, /unknown tool hr-mcp\.get_leave_balance/);
});

const agentList = (status, owner = ME) => ({ok: true, status: 200, data: {agents: [{id: AGENT, name: 'leave-bot',
  display_name: 'Leave balance', description: '', owner_id: owner, latest: {id: VERSION, version: 1, status}}]}});
const version = status => ({ok: true, status: 200, data: {id: VERSION, agent_id: AGENT, version: 1, status,
  waiting_on: status === 'waiting_for_approval' ? 'registry_approver' : '', digest: 'd', capability: ['hr-mcp.get_leave_balance'],
  definition: FIXTURE, created_at: '2026-09-30T10:00:00Z'}});

test('a waiting agent says who acts next, polls, and offers no run', async () => {
  const client = fakeClient({'studio.agents': agentList('waiting_for_approval'), 'studio.version': version('waiting_for_approval')});
  const ctx = ctxFor('agents', [AGENT], {}, client);
  const node = await agents.render(ctx);
  assert.match(node.textContent, /Waiting for a registry approver/);
  assert.deepEqual(ctx.polls, [5000]);
  assert.equal(byText(node, 'button', 'Run…'), undefined);
  const current = all(node).find(e => e.getAttribute('aria-current') === 'step');
  assert.match(current.textContent, /Approved by a second person/);
});

test('a ready agent runs with its declared inputs and remembers the run in this tab only', async () => {
  const client = fakeClient({'studio.agents': agentList('ready'), 'studio.version': version('ready'),
    'studio.runstart': {ok: true, status: 201, data: {id: RUN, state: 'QUEUED'}}});
  const ctx = ctxFor('agents', [AGENT], {}, client);
  const node = await agents.render(ctx);
  assert.deepEqual(ctx.polls, []);
  const field = all(node).find(e => e.name === 'employee_id');
  field.value = 'E-1';
  const f = all(node).find(e => e.tagName === 'FORM' && all(e).includes(field));
  const submitted = f.dispatch('submit');
  await confirmDialog();
  await submitted;
  const call = client.calls.find(c => c.name === 'studio.runstart');
  assert.deepEqual(call.opts, {params: {id: AGENT}, body: {inputs: {employee_id: 'E-1'}}});
  assert.deepEqual(ctx.started.map(r => r.id), [RUN]);
  assert.deepEqual(ctx.went, [`#/runs/${RUN}`]);
});

test('only the owner is offered a new version', async () => {
  const client = fakeClient({'studio.agents': agentList('ready', OTHER), 'studio.version': version('ready')});
  const node = await agents.render(ctxFor('agents', [AGENT], {}, client, session(undefined, ['registry_approver'])));
  assert.ok(!all(node).some(e => e.getAttribute('href')?.startsWith('#/new?agent=')));
});

const run = extra => ({ok: true, status: 200, data: {id: RUN, agent_id: AGENT, version_id: VERSION, requested_by: ME,
  created_at: '2026-09-30T10:00:00Z', deadline: '2026-09-30T10:02:00Z', steps: [], ...extra}});

test('a run in progress is polled; a finished one shows its answer or its reason', async () => {
  let ctx = ctxFor('runs', [RUN], {}, fakeClient({'studio.run': run({state: 'RUNNING'})}));
  await runs.render(ctx);
  assert.deepEqual(ctx.polls, [2000]);

  ctx = ctxFor('runs', [RUN], {}, fakeClient({'studio.run': run({state: 'SUCCEEDED',
    answer: 'You have 12 days of leave left.', answer_expires_at: '2026-09-30T11:00:00Z',
    steps: [{index: 0, step_id: 'lookup', action_id: VERSION, action_state: 'SUCCEEDED'}]})}));
  let node = await runs.render(ctx);
  assert.deepEqual(ctx.polls, []);
  assert.match(node.textContent, /You have 12 days of leave left\./);

  node = await runs.render(ctxFor('runs', [RUN], {}, fakeClient({'studio.run': run({state: 'FAILED',
    failure_reason: 'credential_pending'})})));
  assert.match(node.textContent, /no approved key yet/);
  assert.match(node.textContent, /credential_pending/);
});

const queue = createdBy => ({ok: true, status: 200, data: {
  requests: [{id: VERSION, agent_id: AGENT, agent_name: 'leave-bot', version: 1, created_by: createdBy,
    capability: ['hr-mcp.get_leave_balance'], definition: FIXTURE,
    tools: [{ref: 'hr-mcp.get_leave_balance', protocol: 'mcp', side_effects: ['READ_ONLY']}]}],
  keys: [{id: RUN, version_id: VERSION, agent_name: 'leave-bot', version: 1, capability: ['hr-mcp.get_leave_balance'],
    master_version: 'v1', proposed_by_runtime: true, proposed_at: '2026-09-30T10:00:00Z', expires_at: '2026-12-29T10:00:00Z'}]}});

test('an approver approves someone else’s agent with a reason, and a runtime key', async () => {
  const client = fakeClient({'studio.requests': queue(OTHER), 'studio.approve': {ok: true, status: 200, data: {}},
    'credential.approve': {ok: true, status: 204, data: null}});
  const node = await requests.render(ctxFor('requests', [], {}, client, session([], ['registry_approver'])));
  assert.match(node.textContent, /reads data and changes nothing/);
  let clicked = byText(node, 'button', 'Approve…').dispatch('click');
  await confirmDialog('reads leave balances only');
  await clicked;
  assert.deepEqual(client.calls.find(c => c.name === 'studio.approve').opts,
    {params: {id: VERSION}, body: {reason: 'reads leave balances only'}});
  clicked = byText(node, 'button', 'Approve key…').dispatch('click');
  await confirmDialog();
  await clicked;
  assert.deepEqual(client.calls.find(c => c.name === 'credential.approve').opts, {params: {id: RUN}});
});

test('an approver’s own agent is not offered for their decision', async () => {
  const node = await requests.render(ctxFor('requests', [], {}, fakeClient({'studio.requests': queue(ME)}),
    session([], ['registry_approver', 'studio_author'])));
  assert.equal(byText(node, 'button', 'Approve…'), undefined);
  assert.match(node.textContent, /another registry approver must decide it/);
});

// ---------------------------------------------------------------- the Hub (Phase 27b)

const LISTING = '56565656-5656-4565-8565-565656565656';
const PROPOSAL = '78787878-7878-4787-8787-787878787878';
const listing = extra => ({id: LISTING, agent_id: AGENT, name: 'leave-bot', display_name: 'Leave balance',
  description: 'Days of leave left.', department_id: GROUP, department_name: 'HR', owner_id: OTHER, scope: 'DEPARTMENT',
  state: 'PUBLISHED', published_version_id: VERSION, version: 1, tags: ['leave'], runnable: true, run_count: 3,
  published_at: '2026-09-30T10:00:00Z', definition: FIXTURE,
  tools: [{ref: 'hr-mcp.get_leave_balance', protocol: 'mcp', side_effects: ['READ_ONLY']}], ...extra});

test('the Hub lists what the server returns and searches through the route', async () => {
  const client = fakeClient({'hub.list': {ok: true, status: 200, data: {listings: [listing()]}}});
  const ctx = ctxFor('hub', [], {q: 'leave'}, client, session(undefined, []));
  const node = await hub.render(ctx);
  assert.deepEqual(client.calls[0], {name: 'hub.list', opts: {query: {q: 'leave'}}});
  assert.match(node.textContent, /Leave balance/);
  assert.match(node.textContent, /department/);
  all(node).find(e => e.name === 'q').value = 'payroll';
  all(node).find(e => e.name === 'tag').value = 'hr';
  await all(node).find(e => e.tagName === 'FORM').dispatch('submit');
  assert.deepEqual(ctx.went, ['#/hub?q=payroll&tag=hr']);
});

test('a runnable listing runs as the person; one that is not runnable offers no run', async () => {
  const client = fakeClient({'hub.get': {ok: true, status: 200, data: listing()},
    'studio.runstart': {ok: true, status: 201, data: {id: RUN, state: 'QUEUED'}}});
  const ctx = ctxFor('hub', [LISTING], {}, client, session(undefined, []));
  const node = await hub.render(ctx);
  assert.equal(byText(node, 'button', 'Copy…'), undefined, 'only an author copies');
  assert.equal(byText(node, 'button', 'Withdraw…'), undefined, 'a member does not manage the listing');
  all(node).find(e => e.name === 'employee_id').value = 'E-1';
  const submitted = all(node).find(e => e.tagName === 'FORM' && all(e).some(x => x.name === 'employee_id')).dispatch('submit');
  await confirmDialog();
  await submitted;
  assert.deepEqual(client.calls.find(c => c.name === 'studio.runstart').opts,
    {params: {id: AGENT}, body: {inputs: {employee_id: 'E-1'}}});
  assert.deepEqual(ctx.went, [`#/runs/${RUN}`]);

  const stale = await hub.render(ctxFor('hub', [LISTING], {}, fakeClient({'hub.get': {ok: true, status: 200,
    data: listing({runnable: false})}}), session(undefined, [])));
  assert.equal(byText(stale, 'button', 'Run…'), undefined);
  assert.match(stale.textContent, /newer version/);
});

test('an author copies a listing into their department, and the copy waits for approval', async () => {
  const client = fakeClient({'hub.get': {ok: true, status: 200, data: listing()},
    'hub.clone': {ok: true, status: 201, data: {id: VERSION, agent_id: AGENT}}});
  const ctx = ctxFor('hub', [LISTING], {}, client);
  const node = await hub.render(ctx);
  const clicked = byText(node, 'button', 'Copy…');
  const f = all(node).find(e => e.tagName === 'FORM' && all(e).includes(clicked));
  const submitted = f.dispatch('submit');
  await confirmDialog();
  await submitted;
  assert.deepEqual(client.calls.find(c => c.name === 'hub.clone').opts, {params: {id: LISTING},
    body: {name: 'leave-bot-copy', display_name: 'Leave balance', department_id: GROUP}});
  assert.deepEqual(ctx.went, [`#/agents/${AGENT}`]);
});

test('only those the server accepts are offered to deprecate or withdraw a listing', () => {
  const s = (roles, groups = []) => ({sess: session(groups, roles), me: {principalId: ME, groups}});
  const check = (who, l) => hub.canRetire(who.sess, who.me, l);
  const lead = [{id: GROUP, name: 'hr', displayName: 'HR', lead: true}];
  const member = [{id: GROUP, name: 'hr', displayName: 'HR', lead: false}];
  assert.equal(check(s([]), listing({owner_id: ME})), true, 'the owner');
  assert.equal(check(s(['admin']), listing()), true, 'an admin');
  assert.equal(check(s([], lead), listing()), true, 'a lead of the department');
  assert.equal(check(s([], member), listing()), false, 'a member');
  assert.equal(check(s(['registry_approver']), listing()), false, 'an approver, for a department listing');
  assert.equal(check(s(['registry_approver']), listing({scope: 'ORG'})), true, 'an approver, for the organisation');
  assert.equal(check(s([], lead), listing({scope: 'ORG'})), false, 'a lead, for the organisation');
  assert.equal(check(s(['admin']), listing({state: 'WITHDRAWN'})), false, 'nothing is left to withdraw');
});

test('the owner proposes a ready agent to the Hub and sees who decides it', async () => {
  const client = fakeClient({'studio.agents': agentList('ready'), 'studio.version': version('ready'),
    'hub.listing': {ok: true, status: 200, data: {listing: null, proposal: null}},
    'hub.propose': {ok: true, status: 201, data: {id: PROPOSAL, scope: 'DEPARTMENT', decision: null}}});
  const ctx = ctxFor('agents', [AGENT], {}, client);
  const node = await agents.render(ctx);
  assert.match(node.textContent, /Not in the Hub/);
  all(node).find(e => e.name === 'tags').value = 'leave, HR ,';
  const clicked = byText(node, 'button', 'Publish to the Hub…');
  const submitted = all(node).find(e => e.tagName === 'FORM' && all(e).includes(clicked)).dispatch('submit');
  await confirmDialog();
  await submitted;
  assert.deepEqual(client.calls.find(c => c.name === 'hub.propose').opts, {params: {id: AGENT},
    body: {version_id: VERSION, scope: 'DEPARTMENT', tags: ['leave', 'hr'], note: ''}});

  const waiting = await agents.render(ctxFor('agents', [AGENT], {}, fakeClient({'studio.agents': agentList('ready'),
    'studio.version': version('ready'), 'hub.listing': {ok: true, status: 200, data: {listing: null,
      proposal: {id: PROPOSAL, scope: 'DEPARTMENT', version: 1, decision: null}}}})));
  assert.match(waiting.textContent, /a lead of the department/);
  assert.equal(byText(waiting, 'button', 'Publish to the Hub…'), undefined, 'one open proposal at a time');
  assert.ok(byText(waiting, 'button', 'Cancel proposal…'));
});

test('a lead decides a Hub proposal, and never loads the approvers’ queue', async () => {
  const lead = [{id: GROUP, name: 'hr', displayName: 'HR', lead: true}];
  const client = fakeClient({'hub.requests': {ok: true, status: 200, data: {proposals: [{id: PROPOSAL, agent_id: AGENT,
    agent_name: 'leave-bot', display_name: 'Leave balance', department_name: 'HR', version_id: VERSION, version: 1,
    scope: 'DEPARTMENT', tags: ['leave'], note: 'for HR', proposed_by: OTHER, decision: null,
    tools: [{ref: 'hr-mcp.get_leave_balance', protocol: 'mcp', side_effects: ['READ_ONLY']}]}]}},
  'hub.approve': {ok: true, status: 200, data: {state: 'PUBLISHED'}}});
  const node = await requests.render(ctxFor('requests', [], {}, client, session(lead, [])));
  assert.equal(client.calls.filter(c => c.name === 'studio.requests').length, 0);
  assert.match(node.textContent, /reads data and changes nothing/);
  const clicked = byText(node, 'button', 'Publish…').dispatch('click');
  await confirmDialog('useful for HR');
  await clicked;
  assert.deepEqual(client.calls.find(c => c.name === 'hub.approve').opts,
    {params: {id: PROPOSAL}, body: {reason: 'useful for HR'}});
});
