// A minimal DOM for unit tests: elements keep attributes, listeners and
// children; text nodes keep their data verbatim.
class Node {
  constructor() { this.childNodes = []; }
  append(...children) {
    for (const c of children) this.childNodes.push(typeof c === 'string' ? new Text(c) : c);
  }
  replaceChildren(...children) { this.childNodes = []; this.append(...children); }
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
    this.value = '';
  }
  setAttribute(k, v) { this.attributes.set(k, String(v)); }
  getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; }
  addEventListener(type, fn) { (this.listeners[type] ??= []).push(fn); }
  dispatch(type, event = {}) { for (const fn of this.listeners[type] ?? []) fn({preventDefault() {}, ...event}); }
  showModal() { this.open = true; }
  close() { this.open = false; }
  remove() { this.removed = true; }
  focus() {}
}

export function installFakeDOM() {
  globalThis.document = {
    body: new Element('body'),
    activeElement: null,
    createElement: tag => new Element(tag),
    createTextNode: data => new Text(data),
  };
}
