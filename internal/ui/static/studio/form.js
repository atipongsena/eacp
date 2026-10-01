// form.js is the one Studio form (program spec 8.3): a new agent, from a
// template or blank, or a new version of the caller's agent. It builds the
// definition; Save sends it through confirm.js, and PostgreSQL validates it,
// derives the tools it may use and waits for a second person's approval.
import {h, section, notice, link, input, field, button, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {successors, testPanel} from './preview.js';
import {stringifyJSON} from '../json.js';
import {TEMPLATES, template} from './templates.js';
import {emptyForm, fromDefinition, toDefinition, tools, models, newToolStep, newModelStep, newBranchStep} from './definition.js';

export async function render(ctx) {
  const agentId = ctx.route.query.agent;
  const [catalogue, modelList, base] = await Promise.all([toolCatalogue(ctx.client), modelCatalogue(ctx.client), agentId ? existing(ctx, agentId) : null]);
  if (base && !base.ok) return base.node;
  const chosen = agentId ? null : template(ctx.route.query.template ?? '');
  const state = base ? {agent: base.agent, expectedVersion: base.agent.latest.id, name: `${base.agent.name}-copy`, displayName: base.agent.display_name, description: base.agent.description, department: base.agent.department_id, form: fromDefinition(base.definition)}
    : {name: chosen?.name ?? '', displayName: chosen?.displayName ?? '', description: chosen?.description ?? '',
      department: ctx.session.me().groups[0]?.id ?? '',
      form: chosen ? fromDefinition(chosen.definition) : emptyForm()};
  return build(ctx, state, catalogue, modelList, chosen);
}

// existing reads the caller's agent and its latest definition.
async function existing(ctx, id) {
  if (!isUUID(id)) return {ok: false, node: notice({status: 404, error: 'not_found', detail: t('not an agent id')})};
  const res = await ctx.client.call('studio.agents');
  if (!res.ok) return {ok: false, node: notice(res)};
  const agent = res.data.agents.find(a => a.id === id);
  if (!agent) return {ok: false, node: notice({status: 404, error: 'not_found', detail: t('no such Studio agent')})};
  const v = await ctx.client.call('studio.version', {params: {id: agent.latest.id}});
  if (!v.ok) return {ok: false, node: notice(v)};
  return {ok: true, agent, definition: v.data.definition};
}

// toolCatalogue lists the tenant's tools with what the picker shows: whether
// each can run now, reads only, and its risk. It is a hint: PostgreSQL decides
// which tools a definition may name.
async function toolCatalogue(client) {
  const res = await client.call('connector.list');
  if (!res.ok) return [];
  const lists = await Promise.all(res.data.connectors.filter(c => c.tools.length > 0).map(async c => {
    const r = await client.call('connector.tools', {params: {id: c.id}});
    return r.ok ? r.data.tools.map(x => ({ref: `${c.name}.${x.name}`, executable: x.executable,
      readOnly: x.definition?.read_only ?? null, risk: x.definition?.risk ?? ''})) : [];
  }));
  return lists.flat().sort((a, b) => a.ref.localeCompare(b.ref));
}

async function modelCatalogue(client) {
  const r = await client.call('model.list');
  return r.ok ? r.data.models : [];
}

function toolLabel(x) {
  const parts = [x.readOnly === true ? t('read-only') : x.readOnly === false ? t('may change data') : null,
    x.risk ? t('risk {risk}', {risk: x.risk}) : null, x.executable ? null : t('cannot run now')].filter(Boolean);
  return parts.length ? `${x.ref} (${parts.join(', ')})` : x.ref;
}

function build(ctx, state, catalogue, modelList, chosen) {
  const root = h('div');
  const errors = h('div');
  const review = h('div');
  const draftTest = testPanel(() => toDefinition(state.form));
  const groups = ctx.session.me().groups;
  const text = (value, set, attrs = {}) => input(undefined, {value, oninput: e => { set(e.target.value); showReview(); }, ...attrs});
  const area = (value, set, rows = 3) => h('textarea', {rows, spellcheck: 'false', oninput: e => { set(e.target.value); showReview(); }}, value);
  const showReview = () => {
    const built = toDefinition(state.form);
    replace(review,
      h('p', {}, t('Tools: {tools}', {tools: built.ok ? tools(built.definition).join(', ') || '—' : '—'})),
      h('p', {}, t('Models: {models}', {models: built.ok ? models(built.definition).join(', ') || '—' : '—'})),
      built.ok ? h('details', {}, h('summary', {}, t('Definition JSON')), h('pre', {}, stringifyJSON(built.definition, 2))) : null);
  };

  const draw = () => {
    const isNew = !state.agent;
    showReview();
    return replace(root,
    h('p', {}, link(isNew ? t('← Agents') : t('← Back to the agent'), isNew ? format('agents') : format('agents', [state.agent.id]))),
    h('h1', {}, isNew ? t('New agent') : t('New version of {agent}', {agent: state.agent.display_name})),
    isNew ? section(t('Start from'),
      h('div', {class: 'stack'}, field(t('Template'), h('select', {onchange: e => ctx.go(format('new', [], {template: e.target.value}))},
        h('option', {value: '', selected: !chosen}, t('Blank')),
        TEMPLATES.map(x => h('option', {value: x.id, selected: chosen?.id === x.id}, x.title()))))),
      chosen ? h('p', {class: 'hint'}, chosen.summary()) : null) : null,
    isNew ? section(t('Basics'),
      groups.length === 0 ? h('div', {class: 'notice error', role: 'alert'},
        t('You are in no group, so you cannot save an agent: an admin adds you to your department’s group.')) : null,
      h('div', {class: 'form-grid'},
        field(t('Name (lower case, digits and hyphens)'), text(state.name, v => { state.name = v; }, {required: true, maxlength: 63})),
        field(t('Display name'), text(state.displayName, v => { state.displayName = v; }, {required: true, maxlength: 200})),
        field(t('Department'), h('select', {onchange: e => { state.department = e.target.value; }},
          groups.map(g => h('option', {value: g.id, selected: g.id === state.department}, `${g.displayName} (${g.name})`))))),
      field(t('Description'), area(state.description, v => { state.description = v; }, 2))) : null,
    section(t('Inputs the person running it gives'),
      h('p', {class: 'hint'}, t('Use an input in a step as {{inputs.NAME}}.')),
      state.form.inputs.map((row, i) => h('div', {class: 'form-row'},
        field(t('Name'), text(row.name, v => { row.name = v; }, {maxlength: 32})),
        field(t('Maximum length'), text(row.maxLength, v => { row.maxLength = v; }, {size: 6})),
        button(t('Remove'), () => { state.form.inputs.splice(i, 1); draw(); }, {kind: 'ghost'}))),
      h('div', {class: 'actions'}, button(t('Add an input'), () => { state.form.inputs.push({name: '', maxLength: '64'}); draw(); }))),
    section(t('Steps, in order'),
      h('p', {class: 'hint'}, t('Each tool call is an action: policy, approvals, budgets and kill switches apply to it. Use an earlier step’s output as {{steps.ID.output...}}.')),
      state.form.steps.map((s, i) => stepEditor(state, s, i, catalogue, modelList, text, area, draw)),
      state.form.schemaVersion === 1 ? button(t('Enable the full builder'), () => {
        state.form.schemaVersion = 2; state.form.maxOutputTokens = '100';
        state.form.steps.forEach((s, i) => { if (s.kind === 'tool_call') s.next = state.form.steps[i + 1]?.id ?? ''; }); draw();
      }) : null,
      h('div', {class: 'actions'}, button(t('Add a tool call'), () => {
        const last = state.form.steps.at(-1)?.id ?? '';
        state.form.steps.splice(Math.max(0, state.form.steps.length - 1), 0, newToolStep(`step${state.form.steps.length}`, last));
        draw();
      }))),
    state.form.schemaVersion === 2 ? h('div', {class: 'actions'},
      button(t('Add a model step'), () => { state.form.steps.splice(Math.max(0, state.form.steps.length - 1), 0, newModelStep(`model${state.form.steps.length}`, state.form.steps.at(-1)?.id ?? '')); draw(); }),
      button(t('Add a branch'), () => { state.form.steps.splice(Math.max(0, state.form.steps.length - 1), 0, newBranchStep(`choose${state.form.steps.length}`, state.form.steps.at(-1)?.id ?? '')); draw(); }),
      button(t('Add an answer'), () => { state.form.steps.push({id: `answer${state.form.steps.length}`, kind: 'respond', text: ''}); draw(); })) : null,
    section(t('Limits'), field(t('Time limit in seconds (10 to 3600)'),
      text(state.form.timeoutSeconds, v => { state.form.timeoutSeconds = v; }, {size: 6})), state.form.schemaVersion === 2 ? field(t('Total declared output token cap'), text(state.form.maxOutputTokens, v => { state.form.maxOutputTokens = v; }, {size: 6})) : null),
    draftTest,
    section(t('Review'),
      review,
      h('p', {class: 'hint'}, t('Model steps require an approved runtime key, an active allowlist, prices and a leaf budget. The cap sums every declared model step, including both branches.')),
      h('p', {class: 'hint'}, t('PostgreSQL checks every path, forward edge and output reference when you save.'))),
    h('div', {class: 'actions'}, button(isNew ? t('Save…') : t('Save the new version…'), () => save(ctx, state, errors, draw),
      {kind: 'primary', disabled: isNew && groups.length === 0})),
    errors);
  };
  draw();
  return root;
}

function stepEditor(state, s, i, catalogue, modelList, text, area, draw) {
  const steps = state.form.steps;
  const v2 = state.form.schemaVersion === 2;
  const move = d => { steps.splice(i + d, 0, ...steps.splice(i, 1)); draw(); };
  const last = i === steps.length - 1;
  const controls = h('div', {class: 'actions'},
    (v2 || s.kind !== 'respond') && i > 0 ? button(t('Move up'), () => move(-1), {kind: 'ghost'}) : null,
    (v2 || s.kind !== 'respond') && i < steps.length - (v2 ? 1 : 2) ? button(t('Move down'), () => move(1), {kind: 'ghost'}) : null,
    v2 || s.kind !== 'respond' ? button(t('Remove'), () => { steps.splice(i, 1); draw(); }, {kind: 'ghost'}) : null);
  const pick = (label, key) => field(label, h('select', {onchange: e => { s[key] = e.target.value; draw(); }},
    h('option', {value: '', selected: !s[key]}, t('Choose a following step')),
    successors(steps, i).map(id => h('option', {value: id, selected: s[key] === id}, id))));
  const identity = () => field(t('Step id'), text(s.id, v => { s.id = v; }, {maxlength: 32, onchange: draw}));
  if (s.kind === 'llm') return h('div', {class: 'step-editor'},
    h('h3', {}, t('Step {n}: call a model', {n: i + 1})), identity(),
    field(t('Model'), h('select', {onchange: e => { s.model = e.target.value; draw(); }},
      h('option', {value: '', selected: !s.model}, t('Choose a model')),
      !modelList.some(m => m.name === s.model) && s.model ? h('option', {value: s.model, selected: true}, s.model) : null,
      modelList.map(m => h('option', {value: m.name, selected: s.model === m.name}, `${m.name} (${m.provider})`)))),
    field(t('Instruction'), area(s.instruction, v => { s.instruction = v; })),
    field(t('Model input (JSON object)'), area(s.input, v => { s.input = v; })),
    field(t('Output token cap'), text(s.maxOutputTokens, v => { s.maxOutputTokens = v; })),
    field(t('Closed output schema (JSON)'), area(s.outputSchema, v => { s.outputSchema = v; }, 5)), pick(t('Next step'), 'next'), controls);
  if (s.kind === 'branch') return h('div', {class: 'step-editor'},
    h('h3', {}, t('Step {n}: fixed choice', {n: i + 1})), identity(),
    field(t('Left value (JSON)'), area(s.left, v => { s.left = v; }, 2)),
    field(t('Comparison'), h('select', {onchange: e => { s.operator = e.target.value; draw(); }}, ['eq', 'ne', 'lt', 'le', 'gt', 'ge'].map(op => h('option', {value: op, selected: op === s.operator}, op)))),
    field(t('Right value (JSON)'), area(s.right, v => { s.right = v; }, 2)),
    h('p', {class: 'hint'}, t('Equality requires matching scalar types. Ordering compares numbers without rounding. Missing or null values stop the run.')),
    pick(t('When true'), 'then'), pick(t('When false'), 'else'), controls);
  if (s.kind !== 'tool_call' && s.kind !== 'respond') return h('div', {class: 'notice error'}, t('Unsupported step kind: {kind}', {kind: s.kind}));
  if (s.kind === 'respond') {
    return h('div', {class: 'step-editor'},
      h('h3', {}, t('Step {n}: answer', {n: i + 1}), last || v2 ? null : ` — ${t('the answer must be the last step')}`),
      identity(), field(t('Answer text'), area(s.text, v => { s.text = v; }, 2)), v2 ? controls : null);
  }
  const known = catalogue.some(x => x.ref === s.tool);
  return h('div', {class: 'step-editor'},
    h('h3', {}, t('Step {n}: call a tool', {n: i + 1})),
    h('div', {class: 'form-grid'},
      identity(),
      field(t('Tool'), h('select', {onchange: e => { s.tool = e.target.value; draw(); }},
        h('option', {value: '', selected: s.tool === ''}, t('Choose a tool')),
        !known && s.tool ? h('option', {value: s.tool, selected: true}, s.tool) : null,
        catalogue.map(x => h('option', {value: x.ref, selected: x.ref === s.tool}, toolLabel(x))))),
      field(t('Tool schema version'), text(s.toolSchemaVersion, v => { s.toolSchemaVersion = v; }, {size: 4})),
      field(t('Operation'), text(s.operation, v => { s.operation = v; }, {maxlength: 128})),
      field(t('Target'), text(s.target, v => { s.target = v; }, {maxlength: 128})),
      field(t('Resource'), text(s.resource, v => { s.resource = v; }, {maxlength: 128}))),
    field(t('Payload (JSON)'), area(s.payload, v => { s.payload = v; }, 4)),
    catalogue.length === 0 ? h('p', {class: 'hint'}, t('No tools are registered yet; ask a registry editor to connect the system.')) : null,
    v2 ? pick(t('Next step'), 'next') : null, controls);
}

async function save(ctx, state, errors, draw) {
  replace(errors);
  const built = toDefinition(state.form);
  if (!built.ok) {
    replace(errors, h('div', {class: 'notice error', role: 'alert'}, h('strong', {}, t('Fix these first:')),
      h('ul', {}, built.errors.map(e => h('li', {}, h('code', {}, e.field), ' ', e.message)))));
    return;
  }
  const isNew = !state.agent;
  const name = isNew ? state.name : state.agent.display_name;
  const c = await ask({title: isNew ? t('Save agent {name}', {name}) : t('Save a new version of {name}', {name}),
    confirmLabel: t('Save'),
    lines: [t('Models: {models}', {models: models(built.definition).join(', ') || '—'}), t('It asks to use: {tools}.', {tools: tools(built.definition).join(', ') || '—'}),
      t('A registry approver who is not you must approve it; until then it cannot run.'),
      t('A saved version never changes: to change it, save a new version.')]});
  if (!c) return;
  const r = isNew
    ? await ctx.client.call('studio.save', {body: {name: state.name.trim(), display_name: state.displayName.trim(),
      description: state.description.trim(), department_id: state.department, definition: built.definition}})
    : await ctx.client.call('studio.newversion', {params: {id: state.agent.id}, body: {definition: built.definition, expected_version_id: state.expectedVersion}});
  if (!r.ok) {
    replace(errors, notice(r), r.detail.includes('studio_version_stale') ? [
      h('p', {}, t('Another version was saved. Your form is kept; open the latest version separately or save this as a new agent.')),
      button(t('Save as copy'), () => { state.agent = null; draw(); })] : null);
    return;
  }
  ctx.go(format('agents', [r.data.agent_id]));
}

