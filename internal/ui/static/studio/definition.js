// definition.js maps the Studio form to an agent definition and back
// (ADR-033, schema version 1: tool_call steps and a final respond). The form
// checks only what it must to build JSON (a payload is a JSON object, a
// number is a number); PostgreSQL validates everything else and derives the
// capability, and the page shows its answer.
import {t} from '../i18n.js';

const toolStep = (id = '') => ({kind: 'tool_call', id, tool: '', toolSchemaVersion: '1', operation: '', target: '',
  resource: '', payload: '{}'});

export const newToolStep = toolStep;

// emptyForm is a blank agent: one input, one tool call and the answer.
export function emptyForm() {
  return {
    inputs: [{name: '', maxLength: '64'}],
    steps: [toolStep('lookup'), {kind: 'respond', id: 'answer', text: ''}],
    timeoutSeconds: '120',
  };
}

// fromDefinition fills the form from a saved definition. Values stay as the
// server sent them; a payload becomes indented JSON text.
export function fromDefinition(def) {
  const inputs = Object.entries(def?.inputs ?? {}).map(([name, spec]) => ({name, maxLength: String(spec?.max_length ?? '')}));
  const steps = (def?.steps ?? []).map(s => (s.kind === 'respond'
    ? {kind: 'respond', id: s.id ?? '', text: s.text ?? ''}
    : {kind: 'tool_call', id: s.id ?? '', tool: s.tool ?? '', toolSchemaVersion: s.tool_schema_version ?? '',
      operation: s.operation ?? '', target: s.target ?? '', resource: s.resource ?? '',
      payload: JSON.stringify(s.payload ?? {}, null, 2)}));
  return {inputs: inputs.length ? inputs : [{name: '', maxLength: '64'}], steps,
    timeoutSeconds: String(def?.limits?.timeout_seconds ?? '')};
}

const integer = value => (/^\d{1,6}$/.test(String(value).trim()) ? Number(String(value).trim()) : null);

// toDefinition builds the definition, or names the fields it cannot build:
// {ok: true, definition} or {ok: false, errors: [{field, message}]}. An input
// row without a name is skipped.
export function toDefinition(form) {
  const errors = [];
  const inputs = {};
  (form.inputs ?? []).forEach((row, i) => {
    const name = String(row.name ?? '').trim();
    if (name === '') return;
    const max = integer(row.maxLength);
    if (max === null) errors.push({field: `inputs.${i}.max_length`, message: t('Maximum length must be a whole number.')});
    inputs[name] = {type: 'string', max_length: max ?? 0};
  });
  const steps = (form.steps ?? []).map((s, i) => {
    if (s.kind === 'respond') return {id: String(s.id).trim(), kind: 'respond', text: String(s.text ?? '')};
    let payload = null;
    try {
      payload = JSON.parse(String(s.payload ?? ''));
    } catch {
      payload = null;
    }
    if (payload === null || typeof payload !== 'object' || Array.isArray(payload)) {
      errors.push({field: `steps.${i}.payload`, message: t('The payload must be a JSON object, such as {"employee_id": "{{inputs.employee_id}}"}.')});
    }
    return {id: String(s.id).trim(), kind: 'tool_call', tool: String(s.tool).trim(),
      tool_schema_version: String(s.toolSchemaVersion).trim(), operation: String(s.operation).trim(),
      target: String(s.target).trim(), resource: String(s.resource).trim(), payload: payload ?? {}};
  });
  const timeout = integer(form.timeoutSeconds);
  if (timeout === null) errors.push({field: 'limits.timeout_seconds', message: t('The time limit must be a whole number of seconds.')});
  if (errors.length) return {ok: false, errors};
  return {ok: true, definition: {schema_version: 1, kind: 'agent', inputs, steps, limits: {timeout_seconds: timeout}}};
}

// tools lists the tools a definition's steps call, in order and once each:
// what the save asks a registry approver to allow.
export const tools = def => [...new Set((def?.steps ?? []).filter(s => s.kind === 'tool_call').map(s => s.tool))];
