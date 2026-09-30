import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {parse, format, STUDIO_AREAS} from '../static/router.js';
import {setLang, t} from '../static/i18n.js';
import {TEMPLATES, template} from '../static/studio/templates.js';
import {emptyForm, fromDefinition, toDefinition} from '../static/studio/definition.js';
import {hasPageBar} from '../static/studio/common.js';
import {STAGES, stageIndex, statusSentence, failureSentence, runSentence, FAILURES} from '../static/studio/status.js';

const FIXTURE = fileURLToPath(new URL('../../../test/demo/testdata/leave-balance.json', import.meta.url));
const ID = '0b5e3c1a-7f2d-4c8e-9a61-3d2f5e7a9b10';

test('the leave-balance template is the demo fixture, value for value', () => {
  const fixture = JSON.parse(readFileSync(FIXTURE, 'utf8'));
  assert.deepEqual(template('leave-balance').definition, fixture);
  assert.equal(TEMPLATES.length, 1);
  assert.equal(template('nope'), null);
});

test('the form round-trips the template to the same definition', () => {
  const def = template('leave-balance').definition;
  const form = fromDefinition(def);
  assert.equal(form.steps.length, 2);
  assert.equal(form.steps[0].kind, 'tool_call');
  assert.equal(form.steps[1].kind, 'respond');
  assert.equal(typeof form.steps[0].payload, 'string');
  const back = toDefinition(form);
  assert.equal(back.ok, true, JSON.stringify(back.errors));
  assert.deepEqual(back.definition, def);
});

test('the form builds exactly the server shape: no empty inputs, respond last, limits', () => {
  const form = emptyForm();
  form.inputs = [{name: 'employee_id', maxLength: '64'}, {name: '', maxLength: '10'}];
  form.steps = [
    {kind: 'tool_call', id: 'lookup', tool: 'hr-mcp.get_leave_balance', toolSchemaVersion: '1', operation: 'lookup',
      target: 'hr', resource: 'leave_balance', payload: '{"employee_id": "{{inputs.employee_id}}"}'},
    {kind: 'respond', id: 'answer', text: 'Done.'},
  ];
  form.timeoutSeconds = '60';
  const r = toDefinition(form);
  assert.equal(r.ok, true, JSON.stringify(r.errors));
  assert.deepEqual(r.definition, {schema_version: 1, kind: 'agent',
    inputs: {employee_id: {type: 'string', max_length: 64}},
    steps: [{id: 'lookup', kind: 'tool_call', tool: 'hr-mcp.get_leave_balance', tool_schema_version: '1',
      operation: 'lookup', target: 'hr', resource: 'leave_balance', payload: {employee_id: '{{inputs.employee_id}}'}},
    {id: 'answer', kind: 'respond', text: 'Done.'}],
    limits: {timeout_seconds: 60}});
});

test('the form names what it cannot build and leaves every other rule to the server', () => {
  const form = fromDefinition(template('leave-balance').definition);
  form.steps[0].payload = '[1, 2]';
  form.inputs[0].maxLength = 'ten';
  form.timeoutSeconds = '';
  const r = toDefinition(form);
  assert.equal(r.ok, false);
  const fields = r.errors.map(e => e.field).sort();
  assert.deepEqual(fields, ['inputs.0.max_length', 'limits.timeout_seconds', 'steps.0.payload']);
  for (const e of r.errors) assert.ok(e.message.length > 0);
  // Not JSON at all is named too.
  form.steps[0].payload = '{"employee_id": ';
  form.inputs[0].maxLength = '64';
  form.timeoutSeconds = '120';
  assert.deepEqual(toDefinition(form).errors.map(e => e.field), ['steps.0.payload']);
});

test('the stepper places each status', () => {
  assert.equal(STAGES.length, 4);
  assert.equal(stageIndex({status: 'waiting_for_approval'}), 1);
  assert.equal(stageIndex({status: 'waiting_for_credential'}), 2);
  assert.equal(stageIndex({status: 'ready'}), 4);
  for (const s of ['rejected', 'replaced', 'retired', 'suspended', 'something_new']) {
    assert.equal(stageIndex({status: s}), -1, s);
  }
});

test('every status has a sentence that says who acts next; an unknown one is shown as sent', () => {
  setLang('en');
  assert.match(statusSentence({status: 'waiting_for_approval', waiting_on: 'registry_approver'}), /registry approver/);
  assert.match(statusSentence({status: 'waiting_for_credential', waiting_on: 'registry_approver'}), /key/);
  assert.match(statusSentence({status: 'waiting_for_credential', waiting_on: 'studio_runtime'}), /runtime/);
  assert.match(statusSentence({status: 'ready'}), /[Rr]eady/);
  assert.match(statusSentence({status: 'rejected', decision_reason: 'use the portal'}), /use the portal/);
  assert.match(statusSentence({status: 'replaced'}), /newer/);
  assert.match(statusSentence({status: 'mystery'}), /mystery/);
});

test('every failure reason the runtime names has its own sentence; an unknown one is shown as sent', () => {
  setLang('en');
  const reasons = ['credential_pending', 'credential_expired', 'credential_revoked', 'version_replaced',
    'action_denied', 'action_failed', 'action_cancelled', 'action_unknown', 'result_unavailable',
    'answer_too_large', 'deadline_exceeded'];
  assert.deepEqual(Object.keys(FAILURES).sort(), [...reasons].sort());
  const seen = new Set();
  for (const r of reasons) {
    const s = failureSentence(r);
    assert.ok(s && !seen.has(s), r);
    seen.add(s);
  }
  assert.match(failureSentence('new_reason'), /new_reason/);
});

test('run states read plainly', () => {
  setLang('en');
  for (const s of ['QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED']) assert.ok(runSentence(s).length > 0, s);
  assert.match(runSentence('ODD'), /ODD/);
});

test('Thai has its own words for the status sentences', () => {
  setLang('th');
  assert.notEqual(statusSentence({status: 'ready'}), 'Ready: its runs start now.');
  const thai = failureSentence('credential_pending');
  setLang('en');
  assert.notEqual(thai, failureSentence('credential_pending'));
  assert.equal(t('Sign in'), 'Sign in');
});

test('the Studio page has its own areas; anything else falls back to its home', () => {
  assert.deepEqual(STUDIO_AREAS, ['agents', 'new', 'requests', 'runs']);
  const opts = {areas: STUDIO_AREAS, home: 'agents'};
  assert.deepEqual(parse(`#/runs/${ID}`, opts), {area: 'runs', parts: [ID], query: {}});
  assert.deepEqual(parse('#/new?template=leave-balance', opts), {area: 'new', parts: [], query: {template: 'leave-balance'}});
  assert.deepEqual(parse('#/overview', opts), {area: 'agents', parts: [], query: {}});
  assert.deepEqual(parse('#/runs/../x', opts), {area: 'agents', parts: [], query: {}});
  // The console's areas are unchanged.
  assert.deepEqual(parse('#/new'), {area: 'overview', parts: [], query: {}});
  assert.equal(format('agents', [ID]), `#/agents/${ID}`);
});

test('the page bar titles a list only; the form, an agent and a run title themselves', () => {
  assert.equal(hasPageBar(parse('#/agents', {areas: STUDIO_AREAS})), true);
  assert.equal(hasPageBar(parse('#/runs', {areas: STUDIO_AREAS})), true);
  assert.equal(hasPageBar(parse('#/requests', {areas: STUDIO_AREAS})), true);
  assert.equal(hasPageBar(parse(`#/agents/${ID}`, {areas: STUDIO_AREAS})), false);
  assert.equal(hasPageBar(parse('#/new', {areas: STUDIO_AREAS})), false);
  assert.equal(hasPageBar(parse(`#/new?agent=${ID}`, {areas: STUDIO_AREAS})), false);
});
