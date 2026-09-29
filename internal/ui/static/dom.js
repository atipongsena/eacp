// dom.js builds the console's DOM from elements and text nodes only. No
// string is ever parsed as markup: API data (tool definitions, MCP hints,
// action targets) may be hostile and is always rendered as text. Elements
// and attributes that can load or run code are refused.
import {t} from './i18n.js';

const TAGS = new Set(['a', 'button', 'code', 'dd', 'details', 'dialog', 'div', 'dl', 'dt', 'em', 'form', 'h1',
  'h2', 'h3', 'input', 'label', 'li', 'option', 'p', 'pre', 'section', 'select', 'small', 'span', 'strong',
  'summary', 'table', 'tbody', 'td', 'textarea', 'th', 'thead', 'tr', 'ul']);

const ATTRS = new Set(['autocomplete', 'checked', 'class', 'disabled', 'for', 'hidden', 'href', 'id', 'lang', 'maxlength',
  'method', 'name', 'placeholder', 'required', 'role', 'rows', 'scope', 'selected', 'size', 'spellcheck', 'title',
  'type', 'value']);

const EVENTS = new Set(['onchange', 'onclick', 'oninput', 'onsubmit']);

export function h(tag, attrs, ...children) {
  if (!TAGS.has(tag)) throw new Error(`element <${tag}> is not allowed`);
  const el = document.createElement(tag);
  for (const [name, value] of Object.entries(attrs ?? {})) {
    if (EVENTS.has(name)) {
      if (typeof value !== 'function') throw new Error(`${name} must be a function`);
      el.addEventListener(name.slice(2), value);
      continue;
    }
    if (!ATTRS.has(name) && !/^(aria|data)-[a-z-]+$/.test(name)) throw new Error(`attribute ${name} is not allowed`);
    if (value === undefined || value === null || value === false) continue;
    if (name === 'href' && !String(value).startsWith('#/')) throw new Error('links stay inside the console');
    el.setAttribute(name, value === true ? '' : String(value));
  }
  return append(el, children);
}

export function append(el, children) {
  for (const child of children.flat(Infinity)) {
    if (child === undefined || child === null || child === false) continue;
    el.append(typeof child === 'object' ? child : document.createTextNode(String(child)));
  }
  return el;
}

// replace swaps an element's children, skipping null and false like h().
export function replace(el, ...children) {
  el.replaceChildren();
  return append(el, children);
}

export function fmtTime(value) {
  if (!value) return '—';
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? String(value) : `${d.toISOString().replace('T', ' ').slice(0, 19)}Z`;
}

// relTime says how long ago a moment was ("6 min ago"); beyond 30 days, or for
// a moment in the future, it gives the exact time. With {node: true} it returns
// an element whose title is the exact time.
export function relTime(value, now = Date.now(), {node = false} = {}) {
  const d = value ? new Date(value) : null;
  let text;
  if (!value) text = '—';
  else if (Number.isNaN(d.getTime())) text = String(value);
  else {
    const seconds = (now - d.getTime()) / 1000;
    if (seconds < 0 || seconds >= 30 * 86400) text = fmtTime(value);
    else if (seconds < 45) text = t('just now');
    else if (seconds < 3600) text = t('{n} min ago', {n: Math.max(1, Math.round(seconds / 60))});
    else if (seconds < 86400) text = t('{n} h ago', {n: Math.floor(seconds / 3600)});
    else text = t('{n} d ago', {n: Math.floor(seconds / 86400)});
  }
  return node ? h('span', {title: value ? fmtTime(value) : null}, text) : text;
}

export const cls = value => String(value ?? '').toLowerCase().replace(/[^a-z0-9_-]/g, '');

// ICONS are the classes app.css draws (Bootstrap Icons through clip-path). A
// name outside this list is refused, so a class can never be injected.
export const ICONS = ['overview', 'incidents', 'security', 'fleet', 'approvals', 'execution', 'inventory',
  'dependencies', 'cost', 'signout', 'user', 'alert', 'arrow', 'ok', 'info', 'empty', 'refresh', 'globe', 'run',
  'shield-ok', 'pulse'];

export function icon(name) {
  if (!ICONS.includes(name)) throw new Error(`unknown icon ${name}`);
  return h('span', {class: `icon i-${name}`, 'aria-hidden': 'true'});
}

// badge shows an API state or severity; the class follows the value and the
// text may be a translation of it.
export const badge = (value, label = value) => h('span', {class: `badge badge-${cls(value)}`}, label ?? '—');

export const link = (label, hash) => h('a', {href: hash}, label);

export function button(label, onclick, {kind = '', disabled = false, type = 'button'} = {}) {
  return h('button', {type, class: kind || null, disabled, onclick}, label);
}

// emptyState is an invitation, not an apology: it names the space and, when it
// can, says what to do next.
export const emptyState = (title, hint, name = 'empty') =>
  h('div', {class: 'empty'}, icon(name), h('strong', {}, title), hint ? h('span', {}, hint) : null);

export function table(columns, rows, empty = t('Nothing to show.')) {
  if (!rows || rows.length === 0) return typeof empty === 'string' ? emptyState(empty) : empty;
  return h('div', {class: 'table-wrap'}, h('table', {},
    h('thead', {}, h('tr', {}, columns.map(([label]) => h('th', {scope: 'col'}, label)))),
    h('tbody', {}, rows.map(row => h('tr', {}, columns.map(([, cell]) => h('td', {}, cell(row))))))));
}

export function kv(pairs) {
  return h('dl', {class: 'kv'}, pairs.filter(Boolean).map(([k, v]) => [h('dt', {}, k), h('dd', {}, v ?? '—')]));
}

export const json = value => h('pre', {class: 'json'}, JSON.stringify(value ?? null, null, 2));

export const section = (title, ...children) => h('section', {class: 'panel'}, h('h2', {}, title), ...children);

const lead = status => {
  if (status === 0) return t('The control plane is unreachable.');
  if (status === 403) return t('The server refused this.');
  if (status === 404) return t('Not found.');
  if (status === 409) return t('Conflict: the server refused this change.');
  return t('Request failed ({status}).', {status});
};

export function notice(res) {
  const lead_ = lead(res.status);
  return h('div', {class: 'notice error', role: 'alert'}, h('strong', {}, lead_), ' ',
    [res.error, res.detail].filter(Boolean).join(': '));
}

export const ok = message => h('div', {class: 'notice ok', role: 'status'}, message);

export const field = (label, control) => h('label', {class: 'field'}, h('span', {}, label), control);

export const input = (name, attrs = {}) => h('input', {name, autocomplete: 'off', spellcheck: 'false', ...attrs});

export function select(name, options, value = '') {
  return h('select', {name}, options.map(o => {
    const [v, label] = Array.isArray(o) ? o : [o, o === '' ? t('any') : o];
    return h('option', {value: v, selected: v === value}, label);
  }));
}

export function values(form) {
  const out = {};
  for (const el of form.elements) {
    if (el.name) out[el.name] = el.type === 'checkbox' ? el.checked : String(el.value).trim();
  }
  return out;
}

// stat is one counter row. A zero is dimmed so the rows that matter stand out.
export function stat(label, value, hash) {
  const zero = value === 0 || value === '0';
  return h('div', {class: zero ? 'stat stat-zero' : 'stat'}, h('span', {class: 'stat-label'}, hash ? link(label, hash) : label),
    h('span', {class: 'stat-value'}, String(value ?? 0)));
}

const TONES = [undefined, null, 'danger', 'warning'];

// statCard is a headline number: a link to the place that explains it when it
// has a hash, a plain box otherwise.
export function statCard({label, value, sub, hash, icon: name, tone}) {
  if (!TONES.includes(tone)) throw new Error(`unknown tone ${tone}`);
  return h(hash ? 'a' : 'div', {class: 'kpi', href: hash, 'data-tone': tone},
    h('span', {class: 'kpi-label'}, name ? icon(name) : null, label),
    h('span', {class: 'kpi-value'}, String(value ?? 0)),
    sub ? h('span', {class: 'kpi-sub'}, sub) : null);
}

// banner says what needs attention now. It is a polite status, not an alert,
// because the overview re-renders every 15 seconds.
export const banner = (text, hash, linkLabel) =>
  h('div', {class: 'banner', role: 'status'}, icon('alert'), h('span', {}, text), hash ? link(linkLabel, hash) : null);

// loading shows placeholders while a view fetches, and tells a screen reader.
export const loading = () => h('div', {role: 'status', 'aria-live': 'polite'},
  h('span', {class: 'sr-only'}, t('Loading…')),
  [0, 1, 2, 3].map(() => h('span', {class: 'skeleton', 'aria-hidden': 'true'})));

export const pageHeader = (title, asof) =>
  h('div', {}, h('h1', {}, title), asof ? h('p', {class: 'asof'}, asof) : null);
