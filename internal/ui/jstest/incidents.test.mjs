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

// ---- rendered views with a fake client ----
import {installFakeDOM, all, byText} from './fakedom.mjs';
import {render} from '../static/views/incidents.js';
import {linkBack} from '../static/views/common.js';

installFakeDOM();

function fakeClient(responses = {}) {
  const calls = [];
  return {calls, call(name, opts) {
    calls.push({name, opts});
    const res = responses[name] ?? {ok: true, status: 200, data: {}};
    return new Promise(resolve => setTimeout(() => resolve(typeof res === 'function' ? res(opts) : res), 5));
  }};
}
const session = {me: () => ({principalId: 'p-1', tenantId: 't-1', roles: ['operator']}), hasAny: () => true};
const INCIDENT = {id: ID, title: 'manual', severity: 'high', state: 'OPEN', kind: 'manual', detail: {}, affected: {}, events: []};
const route = (parts, query = {}) => ({area: 'incidents', parts, query});

test('a double submit of a note sends it once', async () => {
  const client = fakeClient({'incident.get': {ok: true, status: 200, data: INCIDENT}});
  const node = await render({client, session, route: route([ID]), go() {}, refresh() {}});
  const form = all(node).find(e => e.tagName === 'FORM' && all(e).some(x => x.name === 'text'));
  all(form).find(e => e.name === 'text').value = 'called the vendor';
  await Promise.all([form.dispatch('submit'), form.dispatch('submit')]);
  await new Promise(r => setTimeout(r, 30));
  assert.equal(client.calls.filter(c => c.name === 'incident.note').length, 1);
});

test('a double click on "Assign to me" assigns once', async () => {
  const client = fakeClient({'incident.get': {ok: true, status: 200, data: INCIDENT}});
  const node = await render({client, session, route: route([ID]), go() {}, refresh() {}});
  const button = byText(node, 'button', 'Assign to me');
  await Promise.all([button.dispatch('click'), button.dispatch('click')]);
  await new Promise(r => setTimeout(r, 30));
  assert.equal(client.calls.filter(c => c.name === 'incident.assign').length, 1);
});

test('"Link this to the incident" links once and then stays disabled', async () => {
  const client = fakeClient();
  const node = linkBack({client, session, route: route([], {incident: ID})}, 'tool', TOOL);
  const button = byText(node, 'button', 'Link this to the incident');
  await Promise.all([button.dispatch('click'), button.dispatch('click')]);
  await new Promise(r => setTimeout(r, 30));
  assert.equal(client.calls.filter(c => c.name === 'incident.link').length, 1);
  assert.equal(button.disabled, true);
  await button.dispatch('click');
  assert.equal(client.calls.filter(c => c.name === 'incident.link').length, 1);
});

test('the polled list holds no form to lose; a manual incident is opened on its own page', async () => {
  const client = fakeClient({'incident.list': {ok: true, status: 200, data: {incidents: []}}});
  const list = await render({client, session, route: route([]), go() {}, refresh() {}});
  assert.ok(!all(list).some(e => e.name === 'title'), 'no open-incident form on the polled list');
  assert.ok(all(list).some(e => e.getAttribute('href') === '#/incidents/new'));
  const page = await render({client, session, route: route(['new']), go() {}, refresh() {}});
  assert.ok(all(page).some(e => e.name === 'title'));
});
