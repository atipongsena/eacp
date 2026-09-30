// signin.js builds the sign-in form both pages use (ADR-028): the operator
// console and the Agent Studio page. The key goes to session.js only; the
// field is emptied as soon as it is read, whatever the answer.
import {h} from './dom.js';
import {t} from './i18n.js';

// signInForm returns the form and its key field (the caller focuses it).
// top is the brand and language switch; onSignedIn runs after a success.
export function signInForm({session, top, onSignedIn, message = ''}) {
  const key = h('input', {id: 'key', name: 'key', type: 'password', autocomplete: 'off', spellcheck: 'false', required: true});
  const status = h('div', {role: 'status'}, message);
  const form = h('form', {class: 'signin', onsubmit: async e => {
    e.preventDefault();
    const res = await session.signIn(key.value);
    key.value = '';
    if (res.ok) {
      onSignedIn();
      return;
    }
    status.replaceChildren(h('div', {class: 'notice error', role: 'alert'}, res.detail));
  }},
  h('div', {class: 'signin-top'}, top),
  h('h1', {}, t('Sign in')),
  h('label', {class: 'field', for: 'key'}, t('Principal API key')),
  key,
  h('button', {type: 'submit', class: 'primary'}, t('Sign in')),
  h('p', {class: 'hint'}, t('The key stays in this tab’s memory only. Reloading the page, signing out or 30 minutes without activity forgets it.')),
  status);
  return {form, key};
}
