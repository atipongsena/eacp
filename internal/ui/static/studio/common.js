// common.js holds what the Studio views share: a definition shown in plain
// form, a stage badge and the side effects of a tool in words. Everything a
// definition holds came from its author and is shown as text only.
import {h, badge} from '../dom.js';
import {t} from '../i18n.js';

const STAGE_LABELS = {
  waiting_for_approval: () => t('waiting for approval'),
  waiting_for_credential: () => t('waiting for its key'),
  ready: () => t('ready'),
  rejected: () => t('rejected'),
  replaced: () => t('replaced'),
};

// stageBadge shows a version's status; the class is the server's word.
export const stageBadge = v => badge(v?.status, STAGE_LABELS[v?.status]?.() ?? v?.status);

// sideEffect words one contract side effect for an approver; an unknown one
// is shown as sent.
export function sideEffect(e) {
  switch (e) {
  case 'READ_ONLY': return t('reads data and changes nothing');
  case 'REVERSIBLE_WRITE': return t('changes data in a way that can be undone');
  case 'IRREVERSIBLE_WRITE': return t('changes data in a way that cannot be undone');
  case 'EXTERNAL_COMMUNICATION': return t('sends messages outside the system');
  case 'FINANCIAL': return t('moves money or makes a financial commitment');
  case 'ADMINISTRATIVE': return t('changes accounts, permissions or settings');
  default: return String(e);
  }
}

// effects words a tool's side effects; no contract means it cannot run.
export const effects = list => (list?.length ? list.map(sideEffect).join('; ') : t('has no active contract, so it cannot run'));

// definitionView shows what an agent asks for and does, step by step.
export function definitionView(def) {
  const inputs = Object.entries(def?.inputs ?? {});
  return h('div', {class: 'definition'},
    h('h3', {}, t('It asks for')),
    inputs.length ? h('ul', {}, inputs.map(([name, spec]) =>
      h('li', {}, h('code', {}, name), ' ', t('(text, at most {n} characters)', {n: spec?.max_length ?? '—'}))))
      : h('p', {class: 'hint'}, t('No inputs.')),
    h('h3', {}, t('Steps')),
    h('ul', {class: 'steps'}, (def?.steps ?? []).map((s, i) => h('li', {},
      h('strong', {}, `${i + 1}. ${s.id}`), ' ',
      s.kind === 'respond'
        ? [t('answers:'), ' ', h('em', {}, s.text)]
        : [t('calls {tool} ({operation} on {target})', {tool: s.tool, operation: s.operation, target: s.target}),
          h('pre', {class: 'json'}, JSON.stringify(s.payload ?? {}, null, 2))]))),
    h('p', {class: 'hint'}, t('Time limit: {n} seconds.', {n: def?.limits?.timeout_seconds ?? '—'})));
}

// hasPageBar says whether the shell titles the page: a list has the page
// bar; the form, an agent and a run carry their own heading and back link.
export const hasPageBar = route => route.parts.length === 0 && route.area !== 'new';
