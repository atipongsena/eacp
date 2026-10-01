// The graph edits only a local definition. PostgreSQL validates its paths on save.
import {h, replace, geometry, icon, button} from '../dom.js';
import {t} from '../i18n.js';

const ports = s => s.kind === 'branch' ? ['then', 'else'] : ['tool_call', 'llm'].includes(s.kind) ? ['next'] : [];
const label = port => port === 'then' ? t('True') : port === 'else' ? t('False') : t('Next step');
const kind = s => s.kind === 'tool_call' ? t('Tool call') : s.kind === 'llm' ? t('Model call') : s.kind === 'branch' ? t('Fixed branch') : s.kind === 'respond' ? t('Answer') : s.kind;
const glyph = s => s.kind === 'tool_call' ? 'inventory' : s.kind === 'llm' ? 'cost' : s.kind === 'branch' ? 'dependencies' : 'ok';

export function layout(steps) {
  const ranks = steps.map(() => 0), edges = [], issues = [];
  const ids = new Map();
  steps.forEach((s, i) => { const list = ids.get(s.id) ?? []; list.push(i); ids.set(s.id, list); });
  steps.forEach((s, i) => {
    if (!s.id || ids.get(s.id).length !== 1) issues.push(t('Node {id} needs a unique id.', {id: s.id || '—'}));
    for (const port of ports(s)) {
      const found = ids.get(s[port]);
      if (!found || found.length !== 1 || found[0] <= i) {
        issues.push(t('Check connection {from} / {port} → {to}.', {from: s.id || '—', port: label(port), to: s[port] || '—'})); continue;
      }
      const to = found[0]; ranks[to] = Math.max(ranks[to], ranks[i] + 1); edges.push({from: i, to, port});
    }
  });
  const counts = ranks.map(r => ranks.filter(x => x === r).length);
  const width = Math.max(620, Math.max(0, ...counts) * 280 + 80);
  const placed = new Map();
  const nodes = steps.map((s, i) => {
    const r = ranks[i], n = placed.get(r) ?? 0; placed.set(r, n + 1);
    return {index: i, x: (width - counts[i] * 280) / 2 + n * 280 + 20, y: 45 + r * 230};
  });
  return {nodes, edges, issues, width, height: 240 + Math.max(0, ...ranks) * 230};
}

export function connect(steps, from, port, to) {
  if (!Number.isInteger(from) || !Number.isInteger(to) || to <= from || !steps[from] || !steps[to]
    || !ports(steps[from]).includes(port) || !steps[to].id || steps.filter(s => s.id === steps[to].id).length !== 1) return false;
  steps[from][port] = steps[to].id; return true;
}

// Native buttons support keyboard/touch as well as local output-to-input drag.
// External drop data is never read or interpreted.
export function graphView(steps, {selected = -1, onSelect, onChange} = {}) {
  const root = h('div', {class: 'studio-graph', onkeydown: e => { if (e.key === 'Escape') cancel(); }}), status = h('p', {class: 'hint', role: 'status', 'aria-live': 'polite'});
  const viewport = h('div', {class: 'graph-viewport', 'aria-label': t('Agent graph')});
  let pending = null, zoom = 1;
  const editable = typeof onChange === 'function';
  const cancel = () => { pending = null; replace(status, editable ? t('Connect an output to a following node input. Click both ports or drag between them.') : t('Read-only graph')); };
  const start = (i, port) => { pending = {i, port}; replace(status, t('Connecting {id} / {port}: choose a following input.', {id: steps[i].id, port: label(port)})); };
  const finish = i => {
    if (!pending) return;
    const p = pending; pending = null;
    if (!connect(steps, p.i, p.port, i)) { replace(status, t('Connections must point forward to a node with a unique id.')); return; }
    cancel(); draw(); onChange();
  };
  const segment = (x, y, width, height) => geometry(h('span', {class: 'graph-line', 'aria-hidden': 'true'}), {x, y, width, height});
  const draw = () => {
    const g = layout(steps);
    const board = geometry(h('div', {class: 'graph-board'}), {width: g.width, height: g.height, scale: zoom});
    const contents = [];
    for (const e of g.edges) {
      const a = g.nodes[e.from], b = g.nodes[e.to];
      const x = a.x + 120 + (e.port === 'then' ? -55 : e.port === 'else' ? 55 : 0), y = a.y + 150;
      const end = b.x + 120, mid = y + 35 + (e.port === 'else' ? 12 : 0);
      contents.push(segment(x, y, 2, mid - y), segment(Math.min(x, end), mid, Math.max(2, Math.abs(end - x)), 2), segment(end, mid, 2, b.y - mid),
        geometry(h('span', {class: 'graph-arrow', 'aria-hidden': 'true'}, icon('arrow')), {x: end - 7, y: b.y - 14}));
    }
    for (const n of g.nodes) {
      const s = steps[n.index];
      const input = editable ? h('button', {type: 'button', class: 'graph-input', 'data-node': n.index, 'data-port': 'input',
        'aria-label': t('Input of {id}', {id: s.id}), onclick: () => finish(n.index), ondragover: e => { if (pending) e.preventDefault(); },
        ondrop: e => { e.preventDefault(); finish(n.index); }}, t('Input')) : null;
      const title = h(onSelect ? 'button' : 'div', {type: onSelect ? 'button' : null, class: 'graph-node-title',
        'data-select-node': n.index, 'aria-pressed': onSelect ? String(selected === n.index) : null,
        ...(onSelect ? {onclick: () => onSelect(n.index)} : {})}, icon(glyph(s)), h('strong', {}, s.id || '—'), h('span', {}, kind(s)),
        h('small', {}, s.kind === 'llm' ? s.model || '—' : s.kind === 'tool_call' ? s.tool || '—' : s.kind === 'branch' ? t('True / False') : t('End of this path')));
      contents.push(geometry(h('div', {class: `graph-node${selected === n.index ? ' graph-selected' : ''}`}, input, title,
        h('div', {class: 'graph-outputs'}, ports(s).map(port => editable ? h('button', {type: 'button', draggable: 'true',
          'data-node': n.index, 'data-port': port, class: 'graph-output', onclick: () => start(n.index, port),
          ondragstart: e => { start(n.index, port); e.dataTransfer?.setData('text/plain', 'studio-connection'); }, ondragend: cancel}, label(port)) : h('span', {}, label(port))))), {x: n.x, y: n.y}));
    }
    replace(board, ...contents);
    replace(viewport, geometry(h('div', {class: 'graph-space'}, board), {width: g.width * zoom, height: g.height * zoom}));
    replace(issues, g.issues.length ? h('div', {class: 'notice error', role: 'alert'}, h('ul', {}, g.issues.map(x => h('li', {}, x)))) : null,
      h('details', {}, h('summary', {}, t('Connections')), h('ul', {}, g.edges.map(e => h('li', {}, `${steps[e.from].id} / ${label(e.port)} → ${steps[e.to].id}`)))));
  };
  const issues = h('div');
  replace(root, h('div', {class: 'actions'}, ...[0.75, 1, 1.25].map(scale => button(`${Math.round(scale * 100)}%`, () => { zoom = scale; draw(); }, {kind: 'ghost'})),
    editable ? button(t('Cancel connection'), cancel, {kind: 'ghost'}) : null), status, viewport, issues);
  cancel(); draw(); return root;
}
