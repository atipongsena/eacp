// app.js boots the operator console (ADR-028): sign-in, navigation, the
// idle timer and polling. The console is a client of the /v1 API and holds
// no authority; the API and PostgreSQL decide every request.
import {createSession} from './session.js';
import {createClient} from './api.js';
import {parse, format} from './router.js';
import {h, button, icon, loading, pageHeader, replace} from './dom.js';
import {t, setLang, getLang, detectLang, LANGS} from './i18n.js';
import {signInForm} from './signin.js';
import * as overview from './views/overview.js';
import * as incidents from './views/incidents.js';
import * as inventory from './views/inventory.js';
import * as execution from './views/execution.js';
import * as dependencies from './views/dependencies.js';
import * as cost from './views/cost.js';
import * as security from './views/security.js';
import * as fleet from './views/fleet.js';
import * as approvals from './views/approvals.js';

const READERS = ['operator', 'auditor', 'admin'];
// NAV lists the areas that have a view, in groups: [group label, [area, label,
// icon, roles that may read it]]. The roles only hide links; the API answers
// 403 to anyone else. Labels are functions so they follow the language.
const NAV = [
  [() => t('Monitor'), [
    ['overview', () => t('Overview'), 'overview', READERS],
    ['incidents', () => t('Incidents'), 'incidents', READERS],
    ['security', () => t('Security'), 'security', ['operator', 'auditor', 'registry_approver']],
  ]],
  [() => t('Operate'), [
    ['fleet', () => t('Fleet'), 'fleet', ['operator', 'auditor', 'registry_approver']],
    ['execution', () => t('Execution'), 'execution', ['operator', 'auditor']],
  ]],
  [() => t('Govern'), [
    ['approvals', () => t('Approvals'), 'approvals', ['approver']],
    ['inventory', () => t('Inventory'), 'inventory', null],
    ['dependencies', () => t('Dependencies'), 'dependencies', ['operator', 'auditor']],
    ['cost', () => t('Cost'), 'cost', READERS],
  ]],
];
const VIEWS = {overview, incidents, security, fleet, approvals, execution, inventory, dependencies, cost};
const POLLED = new Set(['overview', 'incidents']);
const POLL_MS = 15000;
const IDLE_CHECK_MS = 30000;

// The language is never stored: ?lang= in the address wins, else the browser.
setLang(detectLang({search: location.search, languages: navigator.languages}));
document.documentElement.lang = getLang();

const session = createSession();
const client = createClient({session, onUnauthorized: () => signOut(t('Your key was not accepted; sign in again.'))});
const main = document.getElementById('main');
const nav = document.getElementById('nav');
const who = document.getElementById('who');
const pagebar = document.getElementById('pagebar');
const brand = document.getElementById('brand');
const langBox = document.getElementById('lang');
let generation = 0;
let shownArea = null;

function chooseLanguage(next) {
  if (next === getLang()) return;
  setLang(next);
  document.documentElement.lang = next;
  const url = new URL(location.href);
  url.searchParams.set('lang', next);
  history.replaceState(null, '', url);
  render();
}

const languageSwitch = () => LANGS.map(code => h('button', {
  type: 'button', 'aria-pressed': code === getLang() ? 'true' : 'false', lang: code === 'th' ? 'th' : null,
  onclick: () => chooseLanguage(code),
}, code === 'th' ? 'ไทย' : 'EN'));

const brandContent = () => [h('span', {class: 'brand-mark'}, icon('shield-ok')),
  h('span', {}, 'EACP', h('small', {}, t('Operator console')))];

const renderBrand = () => replace(brand, brandContent());

function signOut(message) {
  session.signOut();
  generation++;
  renderSignIn(message);
}

function renderSignIn(message = '') {
  document.body.classList.add('signed-out');
  replace(nav);
  replace(who);
  replace(pagebar);
  replace(langBox);
  renderBrand();
  shownArea = null;
  const {form, key} = signInForm({session, message, onSignedIn: () => render(),
    top: [h('div', {class: 'brand'}, brandContent()), h('div', {class: 'lang'}, languageSwitch())]});
  main.replaceChildren(form);
  key.focus();
}

function renderShell(area, route, label) {
  renderBrand();
  const me = session.me();
  replace(nav, NAV.map(([group, items]) => {
    const visible = items.filter(([, , , roles]) => session.hasAny(roles));
    if (visible.length === 0) return null;
    return [h('div', {class: 'nav-group'}, group()), visible.map(([a, name, ic]) =>
      h('a', {href: format(a), 'aria-current': a === area ? 'page' : null}, icon(ic), name()))];
  }));
  replace(who, h('span', {class: 'role'}, me.roles.length ? me.roles.join(', ') : t('no roles')),
    button(t('Sign out'), () => signOut(t('Signed out.')), {kind: 'ghost'}));
  replace(langBox, languageSwitch());
  // A detail page has its own h1 and back link; only a list gets the area title.
  replace(pagebar, route.parts.length === 0 ? pageHeader(label()) : null);
}

async function render() {
  if (!session.signedIn()) {
    renderSignIn();
    return;
  }
  document.body.classList.remove('signed-out');
  const route = parse(location.hash);
  const area = VIEWS[route.area] ? route.area : 'overview';
  const label = NAV.flatMap(([, items]) => items).find(([a]) => a === area)[1];
  renderShell(area, route, label);
  const mine = ++generation;
  const ctx = {
    client,
    session,
    route: area === route.area ? route : {area, parts: [], query: {}},
    go: hash => {
      if (location.hash === hash) render();
      else location.hash = hash;
    },
    refresh: () => render(),
  };
  // A new area shows placeholders at once; a refresh keeps what is on screen.
  if (shownArea !== area) main.replaceChildren(loading());
  main.setAttribute('aria-busy', 'true');
  let node;
  try {
    node = await VIEWS[area].render(ctx);
  } catch (err) {
    node = h('div', {class: 'notice error', role: 'alert'}, t('This page failed to render: {reason}', {reason: String(err?.message ?? err)}));
  }
  if (mine !== generation || !session.signedIn()) return;
  main.removeAttribute('aria-busy');
  main.replaceChildren(node);
  shownArea = area;
}

window.addEventListener('hashchange', () => render());
for (const type of ['pointerdown', 'keydown']) document.addEventListener(type, () => session.touch(), {capture: true});
setInterval(() => {
  if (session.expired()) signOut(t('Signed out after 30 minutes without activity.'));
}, IDLE_CHECK_MS);
setInterval(() => {
  if (!session.signedIn() || document.visibilityState !== 'visible') return;
  const route = parse(location.hash);
  if (!POLLED.has(route.area) || route.parts.length > 0 || document.querySelector('dialog[open]')) return;
  const focused = document.activeElement;
  if (focused && focused.matches('input, select, textarea')) return;
  render();
}, POLL_MS);
renderSignIn();
