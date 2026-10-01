// JSON numbers that the browser cannot round-trip stay exact, including in
// request bodies. No executable source or external schema is accepted.
export class ExactNumber {
  constructor(text) { this.text = text; Object.freeze(this); }
  toString() { return this.text; }
  toJSON() { throw new TypeError('exact number requires stringifyJSON'); }
}

export function ratio(value) {
  const text = value instanceof ExactNumber ? value.text : String(value);
  const m = /^(-?)(\d+)(?:\.(\d+))?(?:[eE]([+-]?\d+))?$/.exec(text);
  if (!m) throw new TypeError('invalid number');
  const exp = Number(m[4] ?? 0) - (m[3]?.length ?? 0);
  if (!Number.isSafeInteger(exp) || Math.abs(exp) > 10000) throw new TypeError('number limit');
  const n = BigInt(`${m[1]}${m[2]}${m[3] ?? ''}`);
  return exp >= 0 ? [n * 10n ** BigInt(exp), 1n] : [n, 10n ** BigInt(-exp)];
}

export function parseJSON(text) {
  if (typeof text !== 'string' || text.length > 1048576) throw new SyntaxError('JSON limit');
  let i = 0;
  const fail = () => { throw new SyntaxError('invalid JSON'); };
  const space = () => { while (/[\t\r\n ]/.test(text[i] ?? '\0')) i++; };
  const string = () => {
    const start = i++;
    while (i < text.length) {
      if (text[i] === '\\') { i += 2; continue; }
      if (text[i++] === '"') return JSON.parse(text.slice(start, i));
    }
    return fail();
  };
  const value = depth => {
    if (depth > 64) return fail();
    space();
    if (text[i] === '"') return string();
    if (text[i] === '{') {
      i++; space(); const out = {}; const keys = new Set();
      if (text[i] === '}') { i++; return out; }
      for (;;) {
        space(); if (text[i] !== '"') return fail();
        const key = string();
        if (keys.has(key) || ['__proto__', 'prototype', 'constructor'].includes(key)) return fail();
        keys.add(key); space(); if (text[i++] !== ':') return fail();
        out[key] = value(depth + 1); space();
        if (text[i] === '}') { i++; return out; }
        if (text[i++] !== ',') return fail();
      }
    }
    if (text[i] === '[') {
      i++; space(); const out = [];
      if (text[i] === ']') { i++; return out; }
      for (;;) {
        out.push(value(depth + 1)); space();
        if (text[i] === ']') { i++; return out; }
        if (text[i++] !== ',') return fail();
      }
    }
    for (const [literal, v] of [['true', true], ['false', false], ['null', null]]) {
      if (text.startsWith(literal, i)) { i += literal.length; return v; }
    }
    const m = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(text.slice(i));
    if (!m) return fail();
    i += m[0].length;
    const n = Number(m[0]);
    const exact = new ExactNumber(m[0]);
    if (!Number.isFinite(n)) return exact;
    const [a, b] = ratio(exact); const [c, d] = ratio(n);
    return a * d === c * b ? n : exact;
  };
  const out = value(0); space(); if (i !== text.length) return fail();
  return out;
}

export function stringifyJSON(value, indent = 0) {
  const seen = new Set(); const pad = n => ' '.repeat(Math.min(indent, 8) * n);
  const encode = (v, depth) => {
    if (v instanceof ExactNumber) return v.text;
    if (v === null || typeof v !== 'object') return JSON.stringify(v);
    if (seen.has(v) || depth > 64) throw new TypeError('JSON limit');
    seen.add(v);
    const array = Array.isArray(v);
    const entries = array ? v.map(x => encode(x, depth + 1) ?? 'null') : Object.entries(v)
      .filter(([, x]) => x !== undefined).map(([k, x]) => `${JSON.stringify(k)}:${indent ? ' ' : ''}${encode(x, depth + 1)}`);
    seen.delete(v);
    const [open, close] = array ? ['[', ']'] : ['{', '}'];
    return indent && entries.length ? `${open}\n${pad(depth + 1)}${entries.join(`,\n${pad(depth + 1)}`)}\n${pad(depth)}${close}`
      : `${open}${entries.join(',')}${close}`;
  };
  return encode(value, 0);
}
