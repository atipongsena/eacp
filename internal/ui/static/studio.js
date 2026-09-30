// studio.js boots the Agent Studio page at /studio/ (ADR-028 Rev 1.2,
// ADR-033): an employee saves an agent from a form, sees where it is on the
// way to running, and runs it; a registry approver approves requests and the
// runtime's keys; everyone finds, runs and copies agents in the Hub, and a
// department lead or an approver publishes them there (Phase 27b). It shares the console's modules and rules and holds no
// authority: the API and PostgreSQL decide every request. The key lives in
// session.js memory only, so this page signs in on its own.
import {createSession} from './session.js';
import {createClient} from './api.js';
import {parse, format, STUDIO_AREAS} from './router.js';
import {h, button, icon, loading, pageHeader, replace} from './dom.js';
import {t, setLang, getLang, detectLang, LANGS} from './i18n.js';
import {signInForm} from './signin.js';
import * as agents from './studio/agents.js';
import * as form from './studio/form.js';
import * as requests from './studio/requests.js';
import * as runs from './studio/run.js';
import * as hub from './studio/hub.js';
import {hasPageBar} from './studio/common.js';

const AUTHORS = ['studio_author'];
const READERS = ['studio_author', 'registry_approver', 'auditor'];
// NAV hides links only; the API answers 403 to anyone else.
const NAV = [
  [() => t('Build'), [
    ['agents', () => t('Agents'), 'fleet', READERS],
    ['new', () => t('New agent'), 'run', AUTHORS],
    ['runs', () => t('Runs'), 'execution', null],
  ]],
  [() => t('Share'), [
    ['hub', () => t('Hub'), 'globe', null],
  ]],
  [() => t('Review'), [
    // Everyone: a department lead has no role, and the server lists only
    // what each person may decide.
    ['requests', () => t('Requests'), 'approvals', null],
  ]],
];
const VIEWS = {agents, new: form, hub, requests, runs};
const IDLE_CHECK_MS = 30000;

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
// Runs started in this tab, newest first: there is no run list in the API,
// and nothing here is stored.
const started = [];
let generation = 0;
let shownArea = null;
let poll = null; // {every, next}: the view asked to be shown again

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

const brandContent = () => [h('span', {class: 'brand-mark'}, icon('run')),
  h('span', {}, 'EACP', h('small', {}, t('Agent Studio')))];

// home is where a signed-in person starts: their agents, or the Hub.
const home = () => (session.hasAny(READERS) ? 'agents' : 'hub');

function signOut(message) {
  session.signOut();
  started.length = 0;
  generation++;
  renderSignIn(message);
}

function renderSignIn(message = '') {
  document.body.classList.add('signed-out');
  replace(nav);
  replace(who);
  replace(pagebar);
  replace(langBox);
  replace(brand, brandContent());
  shownArea = null;
  poll = null;
  const {form: f, key} = signInForm({session, message, onSignedIn: () => render(),
    top: [h('div', {class: 'brand'}, brandContent()), h('div', {class: 'lang'}, languageSwitch())]});
  main.replaceChildren(f);
  key.focus();
}

function renderShell(area, route, label) {
  replace(brand, brandContent());
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
  replace(pagebar, hasPageBar(route) ? pageHeader(label()) : null);
}

async function render() {
  if (!session.signedIn()) {
    renderSignIn();
    return;
  }
  document.body.classList.remove('signed-out');
  const route = parse(location.hash, {areas: STUDIO_AREAS, home: home()});
  const area = route.area;
  const label = NAV.flatMap(([, items]) => items).find(([a]) => a === area)[1];
  renderShell(area, route, label);
  const mine = ++generation;
  poll = null;
  const ctx = {
    client,
    session,
    route,
    started,
    go: hash => {
      if (location.hash === hash) render();
      else location.hash = hash;
    },
    refresh: () => render(),
    // pollEvery asks to be shown again every ms while this view is open.
    pollEvery: ms => {
      if (mine === generation) poll = {every: ms, next: Date.now() + ms};
    },
  };
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
// A view that waits on the server (a run, an approval) asks to be polled.
// Nothing is re-rendered under an open dialog or a field being edited.
setInterval(() => {
  if (!poll || !session.signedIn() || document.visibilityState !== 'visible' || Date.now() < poll.next) return;
  const focused = document.activeElement;
  if (document.querySelector('dialog[open]') || (focused && focused.matches('input, select, textarea'))) return;
  render();
}, 1000);
renderSignIn();
