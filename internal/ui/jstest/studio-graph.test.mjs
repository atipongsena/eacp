import {test} from 'node:test';
import assert from 'node:assert/strict';
import {installFakeDOM, all} from './fakedom.mjs';
import {layout, connect, graphView} from '../static/studio/graph.js';
import {geometry, h} from '../static/dom.js';

installFakeDOM();
const steps = () => [
  {id: 'model', kind: 'llm', model: 'triage', next: 'choose'},
  {id: 'choose', kind: 'branch', then: 'yes', else: 'no'},
  {id: 'yes', kind: 'respond', text: 'private answer'},
  {id: 'no', kind: 'respond', text: 'other private answer'},
];
const port = (root, index, name) => all(root).find(e => e.getAttribute('data-node') === String(index) && e.getAttribute('data-port') === name);

test('layout separates both branch arms and keeps invalid nodes visible without changing the definition', () => {
  const s = steps(), before = structuredClone(s), g = layout(s);
  assert.equal(g.edges.length, 3);
  assert.equal(g.nodes[2].y, g.nodes[3].y);
  assert.notEqual(g.nodes[2].x, g.nodes[3].x);
  assert.ok(g.nodes[1].y > g.nodes[0].y);
  s[1].else = 'missing'; s[2].next = 'model';
  const bad = layout(s);
  assert.equal(bad.nodes.length, 4);
  assert.ok(bad.issues.length >= 1);
  assert.deepEqual(before, steps());
  assert.equal(s[1].else, 'missing');
});

test('connections change only the named forward port and refuse cycles, ambiguous ids and unknown ports', () => {
  const s = steps();
  assert.equal(connect(s, 1, 'else', 2), true);
  assert.equal(s[1].else, 'yes'); assert.equal(s[1].then, 'yes');
  const before = structuredClone(s);
  for (const args of [[1, 'then', 0], [1, 'then', 1], [0, 'then', 2], [2, 'next', 3], [0, 'next', 99]]) {
    assert.equal(connect(s, ...args), false);
  }
  assert.deepEqual(s, before);
  s[3].id = 'yes'; assert.equal(connect(s, 1, 'else', 3), false);
});

test('keyboard-compatible click connections and local drag connections have the same effect; external drops do nothing', async () => {
  const s = steps(); let writes = 0, selected = -1;
  const view = graphView(s, {selected: 0, onSelect: i => { selected = i; }, onChange: () => { writes++; }});
  await port(view, 1, 'else').dispatch('click'); await port(view, 2, 'input').dispatch('click');
  assert.equal(s[1].else, 'yes'); assert.equal(writes, 1);
  await port(view, 3, 'input').dispatch('drop'); assert.equal(writes, 1);
  await port(view, 1, 'else').dispatch('dragstart', {dataTransfer: {setData() {}}});
  await port(view, 3, 'input').dispatch('drop');
  assert.equal(s[1].else, 'no'); assert.equal(writes, 2);
  await all(view).find(e => e.getAttribute('data-select-node') === '3').dispatch('click');
  assert.equal(selected, 3);
  assert.doesNotMatch(view.textContent, /private answer/);
});

test('a saved graph is read-only, preserves hostile ids as text and reports broken edges', () => {
  const s = steps(); s[0].id = '<img onerror=x>'; s[1].else = 'missing';
  const v = graphView(s);
  assert.match(v.textContent, /<img onerror=x>/);
  assert.match(v.textContent, /missing/);
  assert.equal(all(v).some(e => e.getAttribute('draggable') === 'true'), false);
  assert.equal(all(v).some(e => e.tagName === 'IMG'), false);
});

test('geometry accepts bounded numbers only, never arbitrary CSS and validates atomically', () => {
  const el = h('div'); geometry(el, {x: 12, y: 0, width: 240, height: 150, scale: 1});
  assert.equal(el.style.getPropertyValue('--graph-x'), '12px');
  for (const value of [NaN, Infinity, -1, 8193, '1px', 'url(secret)']) assert.throws(() => geometry(el, {x: value}));
  for (const values of [{color: 'red'}, {scale: 4}, {x: 20, y: Infinity}]) assert.throws(() => geometry(el, values));
  assert.equal(el.style.getPropertyValue('--graph-x'), '12px');
  assert.throws(() => h('div', {style: 'color:red'}));
});
