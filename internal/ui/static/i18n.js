// i18n.js translates the console's own words (ADR-028 Rev 1.1). A text is
// named by its English sentence: t('Sign in'). English is the key and the
// fallback, so an untranslated text shows English and never blank. Only the
// console's chrome and sentences are translated; protocol terms that come from
// the API (states such as UNKNOWN_OUTCOME, ids, tool names) are shown as sent.
// The language is never stored: it comes from ?lang= in the address, else the
// browser's languages.
import th from './messages.th.js';

export const LANGS = ['en', 'th'];
const CATALOGS = {en: null, th};
let lang = 'en';

export function setLang(next) {
  if (!LANGS.includes(next)) throw new Error(`unsupported language ${next}`);
  lang = next;
}

export const getLang = () => lang;

export function detectLang({search = '', languages = []} = {}) {
  const asked = new URLSearchParams(search).get('lang');
  if (LANGS.includes(asked)) return asked;
  for (const tag of languages ?? []) {
    const base = String(tag).toLowerCase().split('-')[0];
    if (LANGS.includes(base)) return base;
  }
  return 'en';
}

// t fills {name} placeholders in one pass, so a value that itself contains
// braces or markup is inserted as plain text and never re-read.
export function t(key, params = {}) {
  const text = CATALOGS[lang]?.[key] ?? key;
  return text.replace(/\{([a-z][a-zA-Z0-9]*)\}/g, (whole, name) => (Object.hasOwn(params, name) ? String(params[name]) : whole));
}
