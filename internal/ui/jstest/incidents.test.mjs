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
