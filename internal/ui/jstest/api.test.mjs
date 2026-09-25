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
  assert.throws(() => buildPath('agent.get', {ref: '..'}), /invalid ref/);
  assert.throws(() => buildPath('agent.get', {ref: '.'}), /invalid ref/);
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
