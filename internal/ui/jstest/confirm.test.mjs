import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM} from './fakedom.mjs';
import {canConfirm, ask} from '../static/confirm.js';

installFakeDOM();

test('a required reason must be non-blank', () => {
  assert.equal(canConfirm({reason: 'required'}, '   '), false);
  assert.equal(canConfirm({reason: 'required'}, 'contained'), true);
  assert.equal(canConfirm({reason: 'none'}), true);
});

test('a typed word must match exactly', () => {
  assert.equal(canConfirm({typed: 'tenant'}, '', 'Tenant'), false);
  assert.equal(canConfirm({typed: 'tenant'}, '', 'tenant'), true);
  assert.equal(canConfirm({reason: 'required', typed: 'tenant'}, '', 'tenant'), false);
});

test('only one dialog at a time; cancel resolves null', async () => {
  const first = ask({title: 'Kill scope tenant', lines: ['Scope: tenant'], typed: 'tenant'});
  assert.equal(await ask({title: 'second'}), null);
  const dialog = document.body.childNodes.at(-1);
  assert.equal(dialog.tagName, 'DIALOG');
  assert.equal(dialog.open, true);
  dialog.dispatch('cancel');
  assert.equal(await first, null);
  assert.equal(dialog.open, false);
  const third = ask({title: 'again'});
  document.body.childNodes.at(-1).dispatch('cancel');
  assert.equal(await third, null);
});

test('a danger dialog without a text field starts on Cancel, so a stray Enter confirms nothing', async () => {
  const pending = ask({title: 'Apply fleet pause', danger: true, lines: ['1 version']});
  const dialog = document.body.childNodes.at(-1);
  assert.equal(document.activeElement.textContent, 'Cancel');
  dialog.dispatch('cancel');
  assert.equal(await pending, null);
});

test('a dialog with a reason starts in the reason field', async () => {
  const pending = ask({title: 'Kill scope tool', danger: true, reason: 'required'});
  const dialog = document.body.childNodes.at(-1);
  assert.equal(document.activeElement.tagName, 'TEXTAREA');
  dialog.dispatch('cancel');
  assert.equal(await pending, null);
});
