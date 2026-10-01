// common.js holds what the Studio views share: a definition shown in plain
// form, a stage badge and the side effects of a tool in words. Everything a
// definition holds came from its author and is shown as text only.
import {h, badge, cls} from '../dom.js';
import {t} from '../i18n.js';
import {stringifyJSON} from '../json.js';
import {graphView} from './graph.js';

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
    def?.schema_version === 2 ? graphView(def.steps ?? []) : null,
    h('h3', {}, t('It asks for')),
    inputs.length ? h('ul', {}, inputs.map(([name, spec]) =>
      h('li', {}, h('code', {}, name), ' ', t('(text, at most {n} characters)', {n: spec?.max_length ?? '—'}))))
      : h('p', {class: 'hint'}, t('No inputs.')),
    h('h3', {}, t('Steps')),
    h('ul', {class: 'steps'}, (def?.steps ?? []).map((s, i) => h('li', {},
      h('strong', {}, `${i + 1}. ${s.id}`), ' ',
      s.kind === 'respond'
        ? [t('answers:'), ' ', h('em', {}, s.text)]
        : s.kind === 'llm' ? [t('calls model {model}, capped at {cap} output tokens', {model: s.model, cap: s.max_output_tokens}),
          h('p', {}, s.instruction), h('pre', {class: 'json'}, stringifyJSON(s.input, 2)),
          h('pre', {class: 'json'}, stringifyJSON(s.output_schema, 2)), h('p', {}, t('Next: {next}', {next: s.next}))]
        : s.kind === 'branch' ? [t('fixed choice: true → {yes}, false → {no}', {yes: s.then, no: s.else}),
          h('pre', {class: 'json'}, stringifyJSON(s.condition, 2))]
        : s.kind === 'tool_call' ? [t('calls {tool} ({operation} on {target})', {tool: s.tool, operation: s.operation, target: s.target}),
          h('pre', {class: 'json'}, stringifyJSON(s.payload ?? {}, 2)), s.next ? h('p', {}, t('Next: {next}', {next: s.next})) : null]
        : t('Unsupported step kind: {kind}', {kind: s.kind})))),
    def?.schema_version === 2 ? h('p', {class: 'hint'}, t('Total declared cap: {cap} output tokens.', {cap: def.limits.max_output_tokens})) : null,
    h('p', {class: 'hint'}, t('Time limit: {n} seconds.', {n: def?.limits?.timeout_seconds ?? '—'})));
}

// hasPageBar says whether the shell titles the page: a list has the page
// bar; the form, an agent and a run carry their own heading and back link.
export const hasPageBar = route => route.parts.length === 0 && route.area !== 'new';

const SCOPES = {DEPARTMENT: () => t('department'), ORG: () => t('organisation')};
const LISTING_STATES = {PUBLISHED: () => t('published'), DEPRECATED: () => t('deprecated'), WITHDRAWN: () => t('withdrawn')};

// scopeBadge and stateBadge word a Hub listing's scope and state (Phase
// 27b); an unknown value is shown as sent.
export const scopeBadge = l => badge(`scope-${cls(l.scope)}`, SCOPES[l.scope]?.() ?? l.scope);
export const stateBadge = l => badge(`listing-${cls(l.state)}`, LISTING_STATES[l.state]?.() ?? l.state);

// approverOf names who decides a Hub proposal at scope.
export const approverOf = scope => (scope === 'ORG' ? t('an admin or a registry approver') : t('a lead of the department'));
