import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {fileURLToPath} from 'node:url';

// The design tokens are read from the stylesheet the console really serves,
// so the contrast rules below hold for what an operator sees.
const css = readFileSync(fileURLToPath(new URL('../static/app.css', import.meta.url)), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '');

function block(source, from) {
  const open = source.indexOf('{', from);
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++;
    if (source[i] === '}' && --depth === 0) return source.slice(open + 1, i);
  }
  throw new Error('unbalanced braces');
}

function tokens(body) {
  const out = {};
  for (const m of body.matchAll(/--([a-z0-9-]+)\s*:\s*([^;]+);/g)) out[m[1]] = m[2].trim();
  return out;
}

const light = tokens(block(css, css.indexOf(':root')));
const darkAt = css.indexOf('@media (prefers-color-scheme: dark)');
const darkMedia = darkAt < 0 ? '' : block(css, darkAt);
const dark = darkMedia.includes(':root') ? tokens(block(darkMedia, darkMedia.indexOf(':root'))) : {};

const REQUIRED = ['page', 'surface', 'surface-2', 'surface-hover', 'border', 'border-strong', 'text', 'text-muted',
  'accent', 'accent-hover', 'on-accent', 'accent-soft', 'accent-text',
  'danger', 'danger-hover', 'on-danger', 'danger-soft', 'danger-text',
  'warning-soft', 'warning-text', 'success-soft', 'success-text', 'info-soft', 'info-text',
  'neutral-soft', 'neutral-text', 'ring'];

const TEXT = [ // [foreground, background]: WCAG 2.x AA for text, 4.5:1
  ['text', 'page'], ['text', 'surface'], ['text', 'surface-2'], ['text-muted', 'page'], ['text-muted', 'surface'],
  ['text-muted', 'surface-2'], ['accent-text', 'surface'], ['accent-text', 'page'], ['accent-text', 'accent-soft'],
  ['on-accent', 'accent'], ['on-accent', 'accent-hover'], ['on-danger', 'danger'], ['on-danger', 'danger-hover'],
  ['danger-text', 'surface'], ['danger-text', 'danger-soft'], ['warning-text', 'warning-soft'],
  ['success-text', 'success-soft'], ['info-text', 'info-soft'], ['neutral-text', 'neutral-soft'],
];

const UI = [ // WCAG 2.x 1.4.11 non-text contrast, 3:1
  ['border-strong', 'surface'], ['border-strong', 'page'], ['ring', 'surface'], ['ring', 'page'],
];

function luminance(hex) {
  const c = [1, 3, 5].map(i => parseInt(hex.slice(i, i + 2), 16) / 255)
    .map(v => (v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}

const ratio = (a, b) => {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};

test('the stylesheet defines a dark theme under prefers-color-scheme', () => {
  assert.ok(darkAt >= 0, 'no @media (prefers-color-scheme: dark)');
  assert.ok(Object.keys(dark).length > 0, 'the dark block defines no token');
  assert.match(css, /color-scheme:\s*light dark/);
});

for (const [name, theme] of [['light', light], ['dark', dark]]) {
  test(`${name}: every semantic token exists and is a #rrggbb colour`, () => {
    for (const token of REQUIRED) {
      assert.ok(token in theme, `--${token} is missing`);
      assert.match(theme[token], /^#[0-9a-f]{6}$/i, `--${token} = ${theme[token]}`);
    }
  });

  test(`${name}: text pairs reach 4.5:1`, () => {
    const low = TEXT.map(([fg, bg]) => [fg, bg, ratio(theme[fg], theme[bg])]).filter(([, , r]) => !(r >= 4.5))
      .map(([fg, bg, r]) => `--${fg} on --${bg}: ${Number.isFinite(r) ? r.toFixed(2) : 'n/a'}`);
    assert.deepEqual(low, []);
  });

  test(`${name}: field borders and the focus ring reach 3:1`, () => {
    const low = UI.map(([fg, bg]) => [fg, bg, ratio(theme[fg], theme[bg])]).filter(([, , r]) => !(r >= 3))
      .map(([fg, bg, r]) => `--${fg} on --${bg}: ${Number.isFinite(r) ? r.toFixed(2) : 'n/a'}`);
    assert.deepEqual(low, []);
  });
}

test('the elevation order holds: cards sit above the page in both themes', () => {
  for (const theme of [light, dark]) {
    assert.notEqual(theme.page, theme.surface);
    assert.notEqual(theme.surface, theme['surface-2']);
  }
});

test('motion is optional and focus is visible', () => {
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)/);
  assert.match(css, /:focus-visible/);
  assert.doesNotMatch(css, /!important/);
});

test('the stylesheet loads nothing from elsewhere', () => {
  assert.doesNotMatch(css, /@import|url\(\s*['"]?(?:https?:|\/\/|data:)/i);
});
