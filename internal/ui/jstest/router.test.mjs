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
