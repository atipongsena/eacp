// Draft tests only read samples in memory. They send no model or tool request.
import {ExactNumber, ratio, stringifyJSON} from '../json.js';
import {parseJSON} from '../json.js';
import {h, section, field, button, replace, notice} from '../dom.js';
import {ask} from '../confirm.js';
import {format} from '../router.js';
import {t} from '../i18n.js';

export const successors = (steps, i) => steps.slice(i + 1).map(s => s.id);
const number = v => typeof v === 'number' || v instanceof ExactNumber;
function compare(left, op, right) {
  if (left === null || right === null || (number(left) !== number(right)) || (!number(left) && typeof left !== typeof right)) throw new Error('branch_invalid');
  let c;
  if (number(left)) { const [a, b] = ratio(left); const [x, y] = ratio(right); c = a * y < x * b ? -1 : a * y > x * b ? 1 : 0; }
  else if (['string', 'boolean'].includes(typeof left) && ['eq', 'ne'].includes(op)) c = left === right ? 0 : 1;
  else throw new Error('branch_invalid');
  switch (op) { case 'eq': return c === 0; case 'ne': return c !== 0; case 'lt': return c < 0; case 'le': return c <= 0; case 'gt': return c > 0; case 'ge': return c >= 0; default: throw new Error('branch_invalid'); }
}
function render(value, env) {
  const get = path => {
    let v = env;
    for (const key of path.split('.')) { if (!v || !Object.hasOwn(v, key)) throw new Error('result_unavailable'); v = v[key]; }
    return v;
  };
  if (typeof value === 'string') {
    const whole = /^\{\{([^{}]+)\}\}$/.exec(value);
    if (whole) return get(whole[1]);
    return value.replace(/\{\{([^{}]+)\}\}/g, (_, path) => {
      const v = get(path); if (v === null || (typeof v === 'object' && !(v instanceof ExactNumber))) throw new Error('result_unavailable');
      return String(v);
    });
  }
  if (Array.isArray(value)) return value.map(x => render(x, env));
  if (value && typeof value === 'object' && !(value instanceof ExactNumber)) return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, render(v, env)]));
  return value;
}
export function previewDefinition(def, inputs, samples) {
  const path = [];
  try {
    for (const [k, spec] of Object.entries(def.inputs ?? {})) {
      if (typeof inputs[k] !== 'string' || inputs[k].length > spec.max_length) throw new Error('inputs_invalid');
    }
    const env = {inputs, steps: {}}; let i = 0;
    for (let n = 0; n < 20; n++) {
      const s = def.steps[i]; if (!s) throw new Error('graph_invalid');
      path.push(s.id); let dest = s.next;
      if (s.kind === 'respond') return {ok: true, answer: String(render(s.text, env)), path};
      if (s.kind === 'branch') dest = compare(render(s.condition.left, env), s.condition.operator, render(s.condition.right, env)) ? s.then : s.else;
      else if (['tool_call', 'llm'].includes(s.kind)) {
        render(s.kind === 'llm' ? s.input : s.payload, env);
        if (s.kind === 'llm') render(s.instruction, env);
        if (!Object.hasOwn(samples, s.id)) throw new Error('sample_missing');
        if (stringifyJSON(samples[s.id]).length > 65536) throw new Error('sample_too_large');
        env.steps[s.id] = {output: samples[s.id]};
      } else throw new Error('kind_unsupported');
      const next = def.schema_version === 1 ? i + 1 : def.steps.findIndex(x => x.id === dest);
      if (next <= i) throw new Error('graph_invalid'); i = next;
    }
    throw new Error('graph_invalid');
  } catch (e) { return {ok: false, error: e.message, path}; }
}

// Draft state stays in this panel. A real preview uses only the saved,
// approved version, never an edited definition from the form.
export function testPanel(getDefinition) {
  return samplePanel(getDefinition, async (def, inputs, samples, out) => {
    const r = previewDefinition(def, inputs, samples);
    replace(out, h('p', {}, r.ok ? t('Sample path: {path}', {path: r.path.join(' → ')}) : t('Draft test stopped: {reason}', {reason: r.error})),
      r.ok ? h('pre', {}, r.answer) : null);
  }, t('Test with samples'));
}

export function approvedPreview(ctx, {versionId, agentId, definition}) {
  return samplePanel(() => ({ok: true, definition}), async (def, inputs, samples, out) => {
    const tools = Object.fromEntries(def.steps.filter(s => s.kind === 'tool_call' && Object.hasOwn(samples, s.id)).map(s => [s.id, samples[s.id]]));
    const c = await ask({title: t('Preview the approved version'), confirmLabel: t('Start preview'),
      lines: [t('Tool steps use your samples and send no actions. Model steps are real calls and spend the agent’s budget.'),
        t('The saved version and its runtime key must be approved. Your edited draft is not sent.')]});
    if (!c) return;
    const r = await ctx.client.call('studio.preview', {params: {id: versionId}, body: {inputs, samples: tools}});
    if (!r.ok) { replace(out, notice(r)); return; }
    ctx.started.unshift({id: r.data.id, agentId, at: new Date().toISOString()});
    ctx.go(format('runs', [r.data.id]));
  }, t('Preview approved version…'));
}

function samplePanel(getDefinition, run, label) {
  let inputs = '{}', samples = '{}'; const out = h('div');
  const area = (value, set) => h('textarea', {rows: 4, spellcheck: 'false', oninput: e => set(e.target.value)}, value);
  return section(t('Test'), h('p', {class: 'hint'}, t('Draft tests use sample outputs for every tool and model. They prove no policy, budget or approval. Inputs and samples stay in this tab.')),
    field(t('Test inputs (JSON object)'), area(inputs, v => { inputs = v; })),
    field(t('Sample outputs by step id (JSON object)'), area(samples, v => { samples = v; })),
    button(label, async () => {
      try {
        const built = getDefinition(); if (!built.ok) throw new Error('definition_invalid');
        const given = parseJSON(inputs), values = parseJSON(samples);
        if (!given || !values || Array.isArray(given) || Array.isArray(values) || typeof given !== 'object' || typeof values !== 'object') throw new Error('samples_invalid');
        await run(built.definition, given, values, out);
      } catch { replace(out, h('p', {class: 'notice error'}, t('Enter valid test inputs, samples and definition fields.'))); }
    }), out);
}
