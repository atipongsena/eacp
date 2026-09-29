// confirm.js is the one dialog every write passes through (spec §4.4). It
// restates the target as text, collects a reason where the API takes one
// and, for the widest scopes, asks the operator to type a word. Only one
// dialog is open at a time, so a double click never sends twice.
import {h} from './dom.js';
import {t} from './i18n.js';

let busy = false;

export function canConfirm({reason = 'none', typed = null}, reasonValue = '', typedValue = '') {
  if (reason === 'required' && String(reasonValue).trim() === '') return false;
  if (typed !== null && typed !== undefined && typedValue !== typed) return false;
  return true;
}

export function ask({title, lines = [], reason = 'none', typed = null, danger = false, confirmLabel = t('Confirm')}) {
  if (busy) return Promise.resolve(null);
  busy = true;
  return new Promise(resolve => {
    const opener = document.activeElement;
    const reasonBox = reason === 'required'
      ? h('textarea', {id: 'confirm-reason', name: 'reason', rows: 3, maxlength: 1024, required: true}) : null;
    const typedBox = typed !== null
      ? h('input', {id: 'confirm-typed', name: 'typed', autocomplete: 'off', spellcheck: 'false'}) : null;
    const confirmButton = h('button', {type: 'submit', class: danger ? 'danger' : 'primary', disabled: true}, confirmLabel);
    const cancelButton = h('button', {type: 'button', onclick: () => finish(null)}, t('Cancel'));
    const current = () => canConfirm({reason, typed}, reasonBox?.value ?? '', typedBox?.value ?? '');
    let dialog = null;
    const finish = value => {
      if (!busy || dialog === null) return;
      busy = false;
      dialog.close();
      dialog.remove();
      if (opener && typeof opener.focus === 'function') opener.focus();
      resolve(value);
    };
    const form = h('form', {
      method: 'dialog',
      oninput: () => { confirmButton.disabled = !current(); },
      onsubmit: e => {
        e.preventDefault();
        if (current()) finish({reason: reasonBox ? reasonBox.value.trim() : ''});
      },
    },
    h('h2', {id: 'confirm-title'}, title),
    h('ul', {}, lines.filter(Boolean).map(line => h('li', {}, line))),
    reasonBox ? h('label', {class: 'field', for: 'confirm-reason'}, t('Reason (journaled; never paste a secret)')) : null,
    reasonBox,
    typedBox ? h('label', {class: 'field', for: 'confirm-typed'}, t('Type {word} to confirm', {word: typed})) : null,
    typedBox,
    h('div', {class: 'actions'}, confirmButton, cancelButton));
    dialog = h('dialog', {class: 'confirm', 'aria-labelledby': 'confirm-title'}, form);
    dialog.addEventListener('cancel', e => {
      e.preventDefault();
      finish(null);
    });
    document.body.append(dialog);
    dialog.showModal();
    confirmButton.disabled = !current();
    // Start where the operator must type; a danger dialog with nothing to
    // type starts on Cancel, so a stray Enter never confirms it.
    (reasonBox ?? typedBox ?? (danger ? cancelButton : null))?.focus();
  });
}
