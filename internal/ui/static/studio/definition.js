// The form builds JSON; PostgreSQL validates and derives the capability.
import {t} from '../i18n.js';
import {ExactNumber, parseJSON, stringifyJSON} from '../json.js';

export const newToolStep = (id = '', next = '') => ({kind: 'tool_call', id, tool: '', toolSchemaVersion: '1', operation: '', target: '', resource: '', payload: '{}', next});
export const newModelStep = (id = '', next = '') => ({kind: 'llm', id, model: '', instruction: 'Return one JSON object.', input: '{}', maxOutputTokens: '100', outputSchema: '{"type":"object","properties":{"eligible":{"type":"boolean"}},"required":["eligible"],"additionalProperties":false}', next});
export const newBranchStep = (id = '', next = '') => ({kind: 'branch', id, left: '"{{inputs.NAME}}"', operator: 'eq', right: '""', then: next, else: next});

export function emptyForm() {
  return {schemaVersion: 2, inputs: [{name: '', maxLength: '64'}],
    steps: [newToolStep('lookup', 'answer'), {kind: 'respond', id: 'answer', text: ''}], timeoutSeconds: '120', maxOutputTokens: '100'};
}

export function fromDefinition(def) {
  const inputs = Object.entries(def?.inputs ?? {}).map(([name, spec]) => ({name, maxLength: String(spec?.max_length ?? '')}));
  const steps = (def?.steps ?? []).map(s => {
    const base = {kind: s.kind, id: s.id ?? '', next: s.next ?? ''};
    switch (s.kind) {
    case 'respond': return {...base, text: s.text ?? ''};
    case 'tool_call': return {...base, tool: s.tool ?? '', toolSchemaVersion: s.tool_schema_version ?? '', operation: s.operation ?? '', target: s.target ?? '', resource: s.resource ?? '', payload: stringifyJSON(s.payload ?? {}, 2)};
    case 'llm': return {...base, model: s.model ?? '', instruction: s.instruction ?? '', input: stringifyJSON(s.input ?? {}, 2), maxOutputTokens: String(s.max_output_tokens ?? ''), outputSchema: stringifyJSON(s.output_schema ?? {}, 2)};
    case 'branch': return {...base, left: stringifyJSON(s.condition?.left), operator: s.condition?.operator ?? '', right: stringifyJSON(s.condition?.right), then: s.then ?? '', else: s.else ?? ''};
    default: return base;
    }
  });
  return {schemaVersion: def?.schema_version, inputs, steps, timeoutSeconds: String(def?.limits?.timeout_seconds ?? ''), maxOutputTokens: String(def?.limits?.max_output_tokens ?? '')};
}

const integer = v => /^\d{1,6}$/.test(String(v).trim()) ? Number(String(v).trim()) : null;
export function toDefinition(form) {
  const errors = []; const inputs = {};
  const bad = field => errors.push({field, message: t('Enter a valid value for this field.')});
  const object = (text, field) => {
    try { const v = parseJSON(String(text)); if (v !== null && typeof v === 'object' && !Array.isArray(v) && !(v instanceof ExactNumber)) return v; } catch {}
    bad(field); return {};
  };
  (form.inputs ?? []).forEach((row, i) => {
    const name = String(row.name ?? '').trim(); if (!name) return;
    const max = integer(row.maxLength); if (max === null) bad(`inputs.${i}.max_length`);
    if (Object.hasOwn(inputs, name) || ['__proto__', 'constructor', 'prototype'].includes(name)) bad(`inputs.${i}.name`);
    else inputs[name] = {type: 'string', max_length: max ?? 0};
  });
  const v2 = form.schemaVersion === 2;
  if (![1, 2].includes(form.schemaVersion)) bad('schema_version');
  const steps = (form.steps ?? []).map((s, i) => {
    const base = {id: String(s.id ?? '').trim(), kind: s.kind};
    switch (s.kind) {
    case 'respond': return {...base, text: String(s.text ?? '')};
    case 'tool_call': return {...base, tool: String(s.tool ?? '').trim(), tool_schema_version: String(s.toolSchemaVersion ?? '').trim(), operation: String(s.operation ?? '').trim(), target: String(s.target ?? '').trim(), resource: String(s.resource ?? '').trim(), payload: object(s.payload, `steps.${i}.payload`), ...(v2 ? {next: s.next} : {})};
    case 'llm': {
      const cap = integer(s.maxOutputTokens); if (cap === null) bad(`steps.${i}.max_output_tokens`);
      return {...base, model: s.model, instruction: s.instruction, input: object(s.input, `steps.${i}.input`), max_output_tokens: cap ?? 0, output_schema: object(s.outputSchema, `steps.${i}.output_schema`), next: s.next};
    }
    case 'branch': {
      const condition = {operator: s.operator};
      for (const side of ['left', 'right']) { try { condition[side] = parseJSON(s[side]); } catch { bad(`steps.${i}.condition.${side}`); } }
      return {...base, condition, then: s.then, else: s.else};
    }
    default: bad(`steps.${i}.kind`); return base;
    }
  });
  const timeout = integer(form.timeoutSeconds); if (timeout === null) bad('limits.timeout_seconds');
  const limits = {timeout_seconds: timeout};
  if (v2) { limits.max_output_tokens = integer(form.maxOutputTokens); if (limits.max_output_tokens === null) bad('limits.max_output_tokens'); }
  return errors.length ? {ok: false, errors} : {ok: true, definition: {schema_version: form.schemaVersion, kind: 'agent', inputs, steps, limits}};
}
export const tools = d => [...new Set((d?.steps ?? []).filter(s => s.kind === 'tool_call').map(s => s.tool))];
export const models = d => [...new Set((d?.steps ?? []).filter(s => s.kind === 'llm').map(s => s.model))];
