import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readdirSync, readFileSync, statSync} from 'node:fs';
import {join, relative} from 'node:path';
import {fileURLToPath} from 'node:url';
import {t, setLang, getLang, detectLang} from '../static/i18n.js';
import th, {sameOnPurpose} from '../static/messages.th.js';

const STATIC = fileURLToPath(new URL('../static/', import.meta.url));

function sources(dir = STATIC) {
  const out = {};
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) Object.assign(out, sources(path));
    else if (name.endsWith('.js')) out[relative(STATIC, path).replaceAll('\\', '/')] = readFileSync(path, 'utf8');
  }
  return out;
}

const placeholders = text => [...text.matchAll(/\{([a-z][a-zA-Z0-9]*)\}/g)].map(m => m[1]).sort();

test('English is the default and the key itself', () => {
  setLang('en');
  assert.equal(getLang(), 'en');
  assert.equal(t('Sign in'), 'Sign in');
});

test('Thai returns the catalogue text and falls back to English for an unknown key', () => {
  setLang('th');
  assert.equal(t('Sign in'), th['Sign in']);
  assert.notEqual(th['Sign in'], 'Sign in');
  assert.equal(t('a sentence nobody translated'), 'a sentence nobody translated');
  setLang('en');
});

test('parameters are filled in, and a missing parameter stays visible', () => {
  setLang('en');
  assert.equal(t('As of {time}; refreshes every 15 seconds.', {time: '10:00'}), 'As of 10:00; refreshes every 15 seconds.');
  assert.equal(t('As of {time}; refreshes every 15 seconds.', {}), 'As of {time}; refreshes every 15 seconds.');
  setLang('th');
  assert.match(t('As of {time}; refreshes every 15 seconds.', {time: '10:00'}), /10:00/);
  setLang('en');
});

test('a parameter is text: markup and placeholders inside it are not interpreted', () => {
  setLang('en');
  assert.equal(t('As of {time}; refreshes every 15 seconds.', {time: '{time}<b>'}), 'As of {time}<b>; refreshes every 15 seconds.');
});

test('an unsupported language is refused, never guessed', () => {
  setLang('en');
  assert.throws(() => setLang('fr'), /unsupported language/);
  assert.equal(getLang(), 'en');
});

test('detectLang: an explicit ?lang= wins, then the browser, then English', () => {
  assert.equal(detectLang({search: '?lang=th', languages: ['en-US']}), 'th');
  assert.equal(detectLang({search: '?lang=en', languages: ['th-TH']}), 'en');
  assert.equal(detectLang({search: '', languages: ['th-TH', 'en']}), 'th');
  assert.equal(detectLang({search: '', languages: ['fr-FR', 'th']}), 'th');
  assert.equal(detectLang({search: '', languages: ['fr-FR']}), 'en');
  assert.equal(detectLang({search: '?lang=xx', languages: ['th']}), 'th');
  assert.equal(detectLang({search: '?lang=', languages: undefined}), 'en');
});

test('every t() call in the console names its text with a string literal', () => {
  for (const [path, src] of Object.entries(sources())) {
    if (path === 'i18n.js' || path.startsWith('messages.')) continue;
    const calls = [...src.matchAll(/(?<![\w.$])t\(/g)].length;
    const literal = [...src.matchAll(/(?<![\w.$])t\('(?:[^'\\\n]|\\.)*'/g)].length;
    assert.equal(calls, literal, `${path}: ${calls} t( calls, ${literal} with a literal key`);
  }
});

test('every literal key has a Thai text with the same placeholders', () => {
  const missing = [];
  const mismatched = [];
  for (const [path, src] of Object.entries(sources())) {
    if (path === 'i18n.js' || path.startsWith('messages.')) continue;
    for (const m of src.matchAll(/(?<![\w.$])t\('((?:[^'\\\n]|\\.)*)'/g)) {
      const key = m[1].replaceAll("\\'", "'");
      if (!(key in th)) missing.push(`${path}: ${key}`);
      else if (placeholders(key).join() !== placeholders(th[key]).join()) mismatched.push(`${path}: ${key}`);
    }
  }
  assert.deepEqual(missing, [], 'keys with no Thai text');
  assert.deepEqual(mismatched, [], 'keys whose Thai text has other placeholders');
});

test('the Thai catalogue holds no orphan and no untranslated copy', () => {
  const used = new Set();
  for (const [path, src] of Object.entries(sources())) {
    if (path === 'i18n.js' || path.startsWith('messages.')) continue;
    for (const m of src.matchAll(/(?<![\w.$])t\('((?:[^'\\\n]|\\.)*)'/g)) used.add(m[1].replaceAll("\\'", "'"));
  }
  const orphans = Object.keys(th).filter(k => !used.has(k));
  assert.deepEqual(orphans, [], 'catalogue keys that no t() call uses');
  const copies = Object.entries(th).filter(([k, v]) => k === v && !sameOnPurpose.includes(k)).map(([k]) => k);
  assert.deepEqual(copies, [], 'Thai text identical to its English key');
});
