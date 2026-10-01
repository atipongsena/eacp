import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {emptyForm, fromDefinition, toDefinition, models} from '../static/studio/definition.js';
import {TEMPLATES} from '../static/studio/templates.js';
import {previewDefinition, successors} from '../static/studio/preview.js';
import {parseJSON, stringifyJSON} from '../static/json.js';
import {createClient} from '../static/api.js';

test('new forms use v2 and known v1 definitions still round trip', () => {
  assert.equal(emptyForm().schemaVersion, 2);
  for (const x of TEMPLATES) {
    const built = toDefinition(fromDefinition(x.definition));
    assert.equal(built.ok, true, JSON.stringify(built.errors));
    assert.deepEqual(built.definition, x.definition);
    assert.deepEqual(x.definition, JSON.parse(readFileSync(new URL(`../../../test/demo/testdata/${x.id}.json`, import.meta.url), 'utf8')));
  }
  assert.deepEqual(TEMPLATES.map(x => x.id), ['leave-balance', 'leave-triage', 'procurement-triage']);
});

test('unknown kinds are kept and refused instead of becoming tool calls', () => {
  const form = fromDefinition({schema_version: 2, steps: [{id: 'x', kind: 'future'}], inputs: {}, limits: {timeout_seconds: 120}});
  assert.equal(form.steps[0].kind, 'future');
  assert.equal(toDefinition(form).ok, false);
});

test('all fixed successors and all model capabilities are visible', () => {
  const d = TEMPLATES.find(x => x.id === 'leave-triage').definition;
  assert.deepEqual(models(d), ['triage']);
  assert.deepEqual(successors(d.steps, 2), ['yes', 'no']);
  assert.deepEqual(successors(d.steps, 4), []);
});

const graph = () => ({schema_version: 2, kind: 'agent', inputs: {request: {type: 'string', max_length: 64}},
  limits: {timeout_seconds: 120, max_output_tokens: 100}, steps: [
    {id: 'classify', kind: 'llm', model: 'triage', instruction: 'Return JSON.', input: {request: '{{inputs.request}}'}, max_output_tokens: 100,
      output_schema: {type: 'object', properties: {eligible: {type: 'boolean'}}, required: ['eligible'], additionalProperties: false}, next: 'choose'},
    {id: 'choose', kind: 'branch', condition: {left: '{{steps.classify.output.eligible}}', operator: 'eq', right: true}, then: 'yes', else: 'no'},
    {id: 'yes', kind: 'respond', text: 'Eligible'}, {id: 'no', kind: 'respond', text: 'Not eligible'}]});

test('draft test is sample-only and follows the chosen fixed path', () => {
  for (const eligible of [true, false]) {
    const r = previewDefinition(graph(), {request: 'x'}, {classify: {eligible}});
    assert.equal(r.ok, true, r.error);
    assert.equal(r.answer, eligible ? 'Eligible' : 'Not eligible');
    assert.deepEqual(r.path, ['classify', 'choose', eligible ? 'yes' : 'no']);
  }
  assert.equal(previewDefinition(graph(), {}, {}).ok, false);
  assert.equal(previewDefinition(graph(), {request: 'x'}, {}).ok, false);
  assert.equal(previewDefinition(graph(), {request: 'x'}, {classify: {eligible: 'true'}}).ok, false);
});

test('draft test refuses missing output references on a skipped producer', () => {
  const d = graph();
  d.steps.unshift({id: 'entry', kind: 'branch', condition: {left: '{{inputs.request}}', operator: 'eq', right: 'skip'}, then: 'choose', else: 'classify'});
  assert.equal(previewDefinition(d, {request: 'skip'}, {}).ok, false);
});

test('JSON and draft numeric comparisons retain decimal and large integer precision', async () => {
  const raw = '{"a":9007199254740993,"b":9007199254740992,"c":0.1000000000000000001}';
  assert.equal(stringifyJSON(parseJSON(raw)), raw);
  const d = graph();
  d.steps = [{id: 'choose', kind: 'branch', condition: {left: parseJSON('9007199254740993'), operator: 'gt', right: parseJSON('9007199254740992')}, then: 'yes', else: 'no'}, ...d.steps.slice(2)];
  assert.equal(previewDefinition(d, {request: 'x'}, {}).answer, 'Eligible');
  const calls = [];
  const client = createClient({session: {authorization: () => 'Bearer fixture'}, fetch: async (url, init) => {
    calls.push(init.body); return {ok: true, status: 200, text: async () => raw};
  }});
  const r = await client.call('studio.save', {body: parseJSON(raw)});
  assert.equal(calls[0], raw);
  assert.equal(stringifyJSON(r.data), raw);
  assert.throws(() => parseJSON('{"a":1,"a":2}'));
  assert.throws(() => parseJSON('{"__proto__":{}}'));
});
