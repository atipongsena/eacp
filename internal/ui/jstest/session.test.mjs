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
  assert.deepEqual(s.me(), {principalId: 'p-1', tenantId: 't-1', roles: ['operator'], groups: []});
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

test('keeps the caller’s groups from /v1/me, as a copy', async () => {
  const body = {...ME, groups: [{id: 'g-1', name: 'hr', display_name: 'HR', lead: true}, {id: 'g-2', name: 'it', display_name: 'IT'}]};
  const s = createSession({fetch: fetchReturning(200, body)});
  await s.signIn('k');
  const want = [{id: 'g-1', name: 'hr', displayName: 'HR', lead: true}, {id: 'g-2', name: 'it', displayName: 'IT', lead: false}];
  assert.deepEqual(s.me().groups, want);
  s.me().groups[0].name = 'x';
  s.me().groups.push({id: 'g-3'});
  assert.deepEqual(s.me().groups, want);
});
