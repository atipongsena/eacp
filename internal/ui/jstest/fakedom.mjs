// A minimal DOM for unit tests: elements keep attributes, listeners and
// children; text nodes keep their data verbatim. Forms expose their named
// elements, selects their selected value, and events bubble to ancestors.
class Node {
  constructor() { this.childNodes = []; this.parent = null; }
  append(...children) {
    for (let c of children) {
      if (typeof c === 'string') c = new Text(c);
      c.parent = this;
      this.childNodes.push(c);
    }
  }
  replaceChildren(...children) {
    for (const c of this.childNodes) c.parent = null;
    this.childNodes = [];
    this.append(...children);
  }
  get textContent() { return this.childNodes.map(c => c.textContent).join(''); }
}

export class Text extends Node {
  constructor(data) { super(); this.data = data; }
  get textContent() { return this.data; }
}

export class Element extends Node {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase();
    this.attributes = new Map();
    this.listeners = {};
    this.open = false;
    this._value = null;
    this._disabled = null;
  }
  setAttribute(k, v) { this.attributes.set(k, String(v)); }
  getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; }
  removeAttribute(k) { this.attributes.delete(k); }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); }
  // dispatch runs the listeners of this element and its ancestors and
  // resolves when the (possibly async) listeners have returned.
  async dispatch(type, event = {}) {
    const ev = {type, target: this, preventDefault() {}, ...event};
    const running = [];
    for (let n = this; n; n = n.parent) for (const fn of n.listeners?.[type] ?? []) running.push(fn(ev));
    await Promise.all(running);
  }
  get name() { return this.getAttribute('name'); }
  get type() { return this.getAttribute('type') ?? (this.tagName === 'SELECT' ? 'select-one' : 'text'); }
  get value() {
    if (this.tagName === 'SELECT') {
      const options = all(this).filter(e => e.tagName === 'OPTION');
      const chosen = this._value !== null
        ? options.find(o => o.getAttribute('value') === this._value)
        : options.find(o => o.getAttribute('selected') !== null);
      return (chosen ?? options[0])?.getAttribute('value') ?? '';
    }
    return this._value ?? this.getAttribute('value') ?? '';
  }
  set value(v) { this._value = String(v); }
  get disabled() { return this._disabled ?? this.getAttribute('disabled') !== null; }
  set disabled(v) { this._disabled = Boolean(v); }
  get checked() { return this.getAttribute('checked') !== null; }
  get elements() { return all(this).filter(e => e !== this && e.name); }
  get isConnected() {
    let n = this;
    while (n.parent) n = n.parent;
    return n === globalThis.document?.body;
  }
  showModal() { this.open = true; }
  close() { this.open = false; }
  remove() {
    if (this.parent) this.parent.childNodes = this.parent.childNodes.filter(c => c !== this);
    this.parent = null;
  }
  focus() { globalThis.document.activeElement = this; }
}

// all returns root and every element below it, in document order.
export function all(root) {
  const out = [];
  const walk = n => {
    if (n instanceof Element) out.push(n);
    for (const c of n.childNodes ?? []) walk(c);
  };
  walk(root);
  return out;
}

export const byText = (root, tag, text) =>
  all(root).find(e => e.tagName === tag.toUpperCase() && e.textContent.trim() === text);

export function installFakeDOM() {
  globalThis.document = {
    body: new Element('body'),
    activeElement: null,
    createElement: tag => new Element(tag),
    createTextNode: data => new Text(data),
  };
}
