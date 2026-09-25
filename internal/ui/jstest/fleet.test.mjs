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
