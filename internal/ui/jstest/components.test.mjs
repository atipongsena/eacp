import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM, all} from './fakedom.mjs';
import {icon, statCard, stat, emptyState, relTime, loading, pageHeader, notice, table, banner} from '../static/dom.js';
import {setLang} from '../static/i18n.js';
import th from '../static/messages.th.js';

installFakeDOM();

const klass = el => el.getAttribute('class');
const NOW = Date.parse('2026-09-28T17:30:00Z');
const ago = seconds => new Date(NOW - seconds * 1000).toISOString();

test('an icon is a class from a fixed set and never announces itself', () => {
  const el = icon('alert');
  assert.equal(klass(el), 'icon i-alert');
  assert.equal(el.getAttribute('aria-hidden'), 'true');
  for (const bad of ['x" onclick="y', 'unknown', '', undefined, 'alert i-user']) {
    assert.throws(() => icon(bad), /unknown icon/, String(bad));
  }
});

test('a stat card with a link is an anchor to a console route; without one it is a plain box', () => {
  const link = statCard({label: 'Agents', value: 2, sub: '2 active', hash: '#/inventory', icon: 'fleet'});
  assert.equal(link.tagName, 'A');
  assert.equal(link.getAttribute('href'), '#/inventory');
  assert.equal(klass(link), 'kpi');
  assert.match(link.textContent, /Agents/);
  assert.match(link.textContent, /2 active/);
  const plain = statCard({label: 'Spend today', value: '0'});
  assert.equal(plain.tagName, 'DIV');
  assert.throws(() => statCard({label: 'x', value: 1, hash: 'https://example.org'}), /inside the console/);
});

test('a card carries its tone and shows a missing value as zero, never blank', () => {
  assert.equal(statCard({label: 'Incidents', value: 2, tone: 'danger'}).getAttribute('data-tone'), 'danger');
  assert.equal(statCard({label: 'Incidents', value: undefined}).textContent.includes('0'), true);
  assert.throws(() => statCard({label: 'x', value: 1, tone: 'red;x'}), /tone/);
});

test('a zero in a stat row is dimmed so the non-zero rows stand out', () => {
  assert.equal(klass(stat('Queued', 0)), 'stat stat-zero');
  assert.equal(klass(stat('Queued', '0')), 'stat stat-zero');
  assert.equal(klass(stat('Queued', 3)), 'stat');
  assert.equal(klass(stat('Spend today', '251200 THB')), 'stat');
});

test('an empty state names the space, says what to do and keeps the class the table relies on', () => {
  const el = emptyState('No open incidents', 'Incidents appear here when a signal fires.');
  assert.equal(klass(el), 'empty');
  assert.match(el.textContent, /No open incidents/);
  assert.match(el.textContent, /Incidents appear here/);
  assert.ok(all(el).some(e => (klass(e) ?? '').startsWith('icon ')), 'has an icon');
  assert.equal(klass(table([['A', r => r.a]], [])), 'empty');
});

test('a table shows the empty state a view gives it, with its hint', () => {
  const own = emptyState('No open incidents', 'Incidents appear here when a signal fires.');
  assert.equal(table([['A', r => r.a]], [], own), own);
  assert.equal(table([['A', r => r.a]], null, own), own);
  assert.match(table([['A', r => r.a]], [], 'Nothing yet').textContent, /Nothing yet/);
});

test('relative time reads naturally and falls back to the exact time', () => {
  setLang('en');
  assert.equal(relTime(ago(10), NOW), 'just now');
  assert.equal(relTime(ago(6 * 60), NOW), '6 min ago');
  assert.equal(relTime(ago(3 * 3600 + 5), NOW), '3 h ago');
  assert.equal(relTime(ago(2 * 86400), NOW), '2 d ago');
  assert.equal(relTime(ago(45 * 86400), NOW), '2026-08-14 17:30:00Z');
  assert.equal(relTime(new Date(NOW + 60000).toISOString(), NOW), '2026-09-28 17:31:00Z');
  assert.equal(relTime(null, NOW), '—');
  assert.equal(relTime('not a time', NOW), 'not a time');
});

test('the exact time stays reachable as a title', () => {
  const el = relTime(ago(6 * 60), NOW, {node: true});
  assert.equal(el.getAttribute('title'), '2026-09-28 17:24:00Z');
  assert.equal(el.textContent, '6 min ago');
});

test('relative time follows the language', () => {
  setLang('th');
  assert.equal(relTime(ago(6 * 60), NOW), th['{n} min ago'].replace('{n}', '6'));
  assert.equal(relTime(ago(10), NOW), th['just now']);
  setLang('en');
});

test('loading is announced politely and shows placeholders, not a blank page', () => {
  const el = loading();
  assert.equal(el.getAttribute('role'), 'status');
  assert.ok(all(el).filter(e => klass(e) === 'skeleton').length >= 3);
  assert.match(el.textContent, /Loading/);
});

test('a page header has one h1 and an as-of line', () => {
  const el = pageHeader('Overview', 'Updated 17:28:03');
  assert.equal(all(el).filter(e => e.tagName === 'H1').length, 1);
  assert.match(el.textContent, /Overview/);
  assert.match(el.textContent, /Updated 17:28:03/);
});

test('a banner says what needs attention and links to it; it is polite because the overview re-renders every 15 seconds', () => {
  const el = banner('1 critical incident needs attention', '#/incidents?severity=critical', 'Review');
  assert.equal(el.getAttribute('role'), 'status');
  assert.match(el.textContent, /critical incident/);
  assert.ok(all(el).some(e => e.tagName === 'A' && e.getAttribute('href') === '#/incidents?severity=critical'));
});

test('an error notice speaks the operator\'s language', () => {
  setLang('th');
  assert.ok(th['Not found.'], 'the Thai text exists');
  assert.notEqual(th['Not found.'], 'Not found.');
  assert.ok(notice({status: 404}).textContent.includes(th['Not found.']));
  setLang('en');
  assert.match(notice({status: 404}).textContent, /Not found\./);
});
