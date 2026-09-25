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
