import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM, Text} from './fakedom.mjs';
import {h, replace, fmtTime, cls, badge, table, select, kv, notice} from '../static/dom.js';

installFakeDOM();

test('text children stay text, markup included', () => {
  const p = h('p', {}, '<img src=x onerror=alert(1)>', 3, null, false, ['a', ['b']]);
  assert.ok(p.childNodes.every(c => c instanceof Text));
  assert.equal(p.textContent, '<img src=x onerror=alert(1)>3ab');
});

test('replace skips null and false children', () => {
  const el = h('div', {}, 'old');
  replace(el, 'a', null, false, h('span', {}, 'b'));
  assert.equal(el.childNodes.length, 2);
  assert.equal(el.textContent, 'ab');
});

test('refuses elements that can run or load code', () => {
  for (const tag of ['script', 'iframe', 'object', 'embed', 'style', 'link', 'base', 'meta', 'img', 'svg']) {
    assert.throws(() => h(tag), /not allowed/, tag);
  }
});

test('refuses attributes that can run or load code', () => {
  for (const name of ['style', 'src', 'srcdoc', 'formaction', 'action', 'onerror']) {
    assert.throws(() => h('div', {[name]: 'x'}), /not allowed|must be a function/, name);
  }
  assert.throws(() => h('button', {onclick: 'alert(1)'}), /must be a function/);
});

test('links stay inside the console', () => {
  for (const href of ['javascript:alert(1)', 'https://example.org', '//example.org', '#x', '/v1/me']) {
    assert.throws(() => h('a', {href}), /inside the console/, href);
  }
  assert.equal(h('a', {href: '#/incidents'}).getAttribute('href'), '#/incidents');
});

test('events are listeners and empty attributes are skipped', () => {
  const f = () => {};
  const b = h('button', {onclick: f, disabled: false, title: null, type: 'button'});
  assert.deepEqual(b.listeners.click, [f]);
  assert.equal(b.getAttribute('disabled'), null);
  assert.equal(b.getAttribute('type'), 'button');
  assert.equal(h('input', {required: true}).getAttribute('required'), '');
});

test('formatting helpers', () => {
  assert.equal(fmtTime('2026-09-26T10:11:12.345Z'), '2026-09-26 10:11:12Z');
  assert.equal(fmtTime(null), '—');
  assert.equal(fmtTime('not a time'), 'not a time');
  assert.equal(cls('Critical <x>'), 'criticalx');
  assert.equal(badge('high').getAttribute('class'), 'badge badge-high');
});

test('table, select and kv', () => {
  assert.equal(table([['A', r => r.a]], []).getAttribute('class'), 'empty');
  const t = table([['A', r => r.a]], [{a: '<b>'}]);
  assert.equal(t.getAttribute('class'), 'table-wrap');
  assert.ok(t.textContent.includes('<b>'));
  const s = select('state', ['', 'OPEN'], 'OPEN');
  assert.equal(s.childNodes[0].textContent, 'any');
  assert.equal(s.childNodes[1].getAttribute('selected'), '');
  assert.equal(kv([['K', 'v'], null]).childNodes.length, 2);
});

test('a 403 says the server refused, not that a role is missing: two-person rules answer 403 too', () => {
  const n = notice({status: 403, error: 'forbidden', detail: 'two-person rule: the actor cannot also be the acknowledger'});
  assert.match(n.textContent, /^The server refused this\./);
  assert.doesNotMatch(n.textContent, /roles/);
  assert.match(n.textContent, /forbidden: two-person rule/);
});
