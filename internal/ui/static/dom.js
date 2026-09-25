// dom.js builds the console's DOM from elements and text nodes only. No
// string is ever parsed as markup: API data (tool definitions, MCP hints,
// action targets) may be hostile and is always rendered as text. Elements
// and attributes that can load or run code are refused.

const TAGS = new Set(['a', 'button', 'code', 'dd', 'details', 'dialog', 'div', 'dl', 'dt', 'em', 'form', 'h1',
  'h2', 'h3', 'input', 'label', 'li', 'option', 'p', 'pre', 'section', 'select', 'small', 'span', 'strong',
  'summary', 'table', 'tbody', 'td', 'textarea', 'th', 'thead', 'tr', 'ul']);

const ATTRS = new Set(['autocomplete', 'checked', 'class', 'disabled', 'for', 'hidden', 'href', 'id', 'maxlength',
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

export const cls = value => String(value ?? '').toLowerCase().replace(/[^a-z0-9_-]/g, '');

export const badge = value => h('span', {class: `badge badge-${cls(value)}`}, value ?? '—');

export const link = (label, hash) => h('a', {href: hash}, label);

export function button(label, onclick, {kind = '', disabled = false, type = 'button'} = {}) {
  return h('button', {type, class: kind || null, disabled, onclick}, label);
}

export function table(columns, rows, empty = 'Nothing to show.') {
  if (!rows || rows.length === 0) return h('p', {class: 'empty'}, empty);
  return h('div', {class: 'table-wrap'}, h('table', {},
    h('thead', {}, h('tr', {}, columns.map(([label]) => h('th', {scope: 'col'}, label)))),
    h('tbody', {}, rows.map(row => h('tr', {}, columns.map(([, cell]) => h('td', {}, cell(row))))))));
}

export function kv(pairs) {
  return h('dl', {class: 'kv'}, pairs.filter(Boolean).map(([k, v]) => [h('dt', {}, k), h('dd', {}, v ?? '—')]));
}

export const json = value => h('pre', {class: 'json'}, JSON.stringify(value ?? null, null, 2));

export const section = (title, ...children) => h('section', {class: 'panel'}, h('h2', {}, title), ...children);

const LEADS = {0: 'The control plane is unreachable.', 403: 'Your roles do not allow this.', 404: 'Not found.',
  409: 'Conflict: the server refused this change.'};

export function notice(res) {
  const lead = LEADS[res.status] ?? `Request failed (${res.status}).`;
  return h('div', {class: 'notice error', role: 'alert'}, h('strong', {}, lead), ' ',
    [res.error, res.detail].filter(Boolean).join(': '));
}

export const ok = message => h('div', {class: 'notice ok', role: 'status'}, message);

export const field = (label, control) => h('label', {class: 'field'}, h('span', {}, label), control);

export const input = (name, attrs = {}) => h('input', {name, autocomplete: 'off', spellcheck: 'false', ...attrs});

export function select(name, options, value = '') {
  return h('select', {name}, options.map(o => {
    const [v, label] = Array.isArray(o) ? o : [o, o === '' ? 'any' : o];
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

export function stat(label, value, hash) {
  return h('div', {class: 'stat'}, h('span', {class: 'stat-label'}, hash ? link(label, hash) : label),
    h('span', {class: 'stat-value'}, String(value ?? 0)));
}
