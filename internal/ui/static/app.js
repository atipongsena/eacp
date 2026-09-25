// app.js boots the operator console (ADR-028): sign-in, navigation, the
// idle timer and polling. The console is a client of the /v1 API and holds
// no authority; the API and PostgreSQL decide every request.
import {createSession} from './session.js';
import {createClient} from './api.js';
import {parse, format} from './router.js';
import {h, button} from './dom.js';
import * as overview from './views/overview.js';
import * as incidents from './views/incidents.js';
import * as inventory from './views/inventory.js';
import * as execution from './views/execution.js';
import * as dependencies from './views/dependencies.js';
import * as cost from './views/cost.js';
import * as security from './views/security.js';

const READERS = ['operator', 'auditor', 'admin'];
// NAV lists the areas that have a view: [area, label, roles that may read it].
// The roles only hide links; the API answers 403 to anyone else.
const NAV = [
  ['overview', 'Overview', READERS],
  ['incidents', 'Incidents', READERS],
  ['security', 'Security', ['operator', 'auditor', 'registry_approver']],
  ['inventory', 'Inventory', null],
  ['execution', 'Execution', ['operator', 'auditor']],
  ['dependencies', 'Dependencies', ['operator', 'auditor']],
  ['cost', 'Cost', READERS],
];
const VIEWS = {overview, incidents, security, inventory, execution, dependencies, cost};
const POLLED = new Set(['overview', 'incidents']);
const POLL_MS = 15000;
const IDLE_CHECK_MS = 30000;

const session = createSession();
const client = createClient({session, onUnauthorized: () => signOut('Your key was not accepted; sign in again.')});
const main = document.getElementById('main');
const nav = document.getElementById('nav');
const who = document.getElementById('who');
let generation = 0;

function signOut(message) {
  session.signOut();
  generation++;
  renderSignIn(message);
}

function renderSignIn(message = '') {
  document.body.classList.add('signed-out');
  nav.replaceChildren();
  who.replaceChildren();
  const key = h('input', {id: 'key', name: 'key', type: 'password', autocomplete: 'off', spellcheck: 'false', required: true});
  const status = h('div', {role: 'status'}, message);
  const form = h('form', {class: 'signin', onsubmit: async e => {
    e.preventDefault();
    const res = await session.signIn(key.value);
    key.value = '';
    if (res.ok) {
      render();
      return;
    }
    status.replaceChildren(h('div', {class: 'notice error', role: 'alert'}, res.detail));
  }},
  h('h1', {}, 'Sign in'),
  h('label', {class: 'field', for: 'key'}, 'Principal API key'),
  key,
  h('button', {type: 'submit', class: 'primary'}, 'Sign in'),
  h('p', {class: 'hint'}, 'The key stays in this tab’s memory only. Reloading the page, signing out or 30 minutes without activity forgets it.'),
  status);
  main.replaceChildren(form);
  key.focus();
}

async function render() {
  if (!session.signedIn()) {
    renderSignIn();
    return;
  }
  document.body.classList.remove('signed-out');
  const route = parse(location.hash);
  const area = VIEWS[route.area] ? route.area : 'overview';
  const me = session.me();
  nav.replaceChildren(h('ul', {}, NAV.filter(([, , roles]) => session.hasAny(roles)).map(([a, label]) =>
    h('li', {}, h('a', {href: format(a), 'aria-current': a === area ? 'page' : null}, label)))));
  who.replaceChildren(h('span', {}, me.roles.length ? me.roles.join(', ') : 'no roles'),
    button('Sign out', () => signOut('Signed out.')));
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
  main.setAttribute('aria-busy', 'true');
  let node;
  try {
    node = await VIEWS[area].render(ctx);
  } catch (err) {
    node = h('div', {class: 'notice error', role: 'alert'}, 'This page failed to render: ', String(err?.message ?? err));
  }
  if (mine !== generation || !session.signedIn()) return;
  main.removeAttribute('aria-busy');
  main.replaceChildren(node);
}

window.addEventListener('hashchange', () => render());
for (const type of ['pointerdown', 'keydown']) document.addEventListener(type, () => session.touch(), {capture: true});
setInterval(() => {
  if (session.expired()) signOut('Signed out after 30 minutes without activity.');
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
