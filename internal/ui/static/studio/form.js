// form.js is the one Studio form (program spec 8.3): a new agent, from a
// template or blank, or a new version of the caller's agent. It builds the
// definition; Save sends it through confirm.js, and PostgreSQL validates it,
// derives the tools it may use and waits for a second person's approval.
import {h, section, notice, link, input, field, button, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {TEMPLATES, template} from './templates.js';
import {emptyForm, fromDefinition, toDefinition, tools, newToolStep} from './definition.js';

export async function render(ctx) {
  const agentId = ctx.route.query.agent;
  const [catalogue, base] = await Promise.all([toolCatalogue(ctx.client), agentId ? existing(ctx, agentId) : null]);
  if (base && !base.ok) return base.node;
  const chosen = agentId ? null : template(ctx.route.query.template ?? '');
  const state = base ? {agent: base.agent, form: fromDefinition(base.definition)}
    : {name: chosen?.name ?? '', displayName: chosen?.displayName ?? '', description: chosen?.description ?? '',
      department: ctx.session.me().groups[0]?.id ?? '',
      form: chosen ? fromDefinition(chosen.definition) : emptyForm()};
  return build(ctx, state, catalogue, chosen);
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

function toolLabel(x) {
  const parts = [x.readOnly === true ? t('read-only') : x.readOnly === false ? t('may change data') : null,
    x.risk ? t('risk {risk}', {risk: x.risk}) : null, x.executable ? null : t('cannot run now')].filter(Boolean);
  return parts.length ? `${x.ref} (${parts.join(', ')})` : x.ref;
}

function build(ctx, state, catalogue, chosen) {
  const root = h('div');
  const errors = h('div');
  const groups = ctx.session.me().groups;
  const isNew = !state.agent;
  const text = (value, set, attrs = {}) => input(undefined, {value, oninput: e => set(e.target.value), ...attrs});
  const area = (value, set, rows = 3) => h('textarea', {rows, spellcheck: 'false', oninput: e => set(e.target.value)}, value);

  const draw = () => replace(root,
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
      state.form.steps.map((s, i) => stepEditor(state, s, i, catalogue, text, area, draw)),
      h('div', {class: 'actions'}, button(t('Add a tool call'), () => {
        state.form.steps.splice(Math.max(0, state.form.steps.length - 1), 0, newToolStep(`step${state.form.steps.length}`));
        draw();
      }))),
    section(t('Limits'), field(t('Time limit in seconds (10 to 3600)'),
      text(state.form.timeoutSeconds, v => { state.form.timeoutSeconds = v; }, {size: 6}))),
    h('div', {class: 'actions'}, button(isNew ? t('Save…') : t('Save the new version…'), () => save(ctx, state, errors),
      {kind: 'primary', disabled: isNew && groups.length === 0})),
    errors);
  draw();
  return root;
}

function stepEditor(state, s, i, catalogue, text, area, draw) {
  const steps = state.form.steps;
  const move = d => { steps.splice(i + d, 0, ...steps.splice(i, 1)); draw(); };
  const last = i === steps.length - 1;
  const controls = h('div', {class: 'actions'},
    s.kind === 'tool_call' && i > 0 ? button(t('Move up'), () => move(-1), {kind: 'ghost'}) : null,
    s.kind === 'tool_call' && i < steps.length - 2 ? button(t('Move down'), () => move(1), {kind: 'ghost'}) : null,
    s.kind === 'tool_call' ? button(t('Remove'), () => { steps.splice(i, 1); draw(); }, {kind: 'ghost'}) : null);
  if (s.kind === 'respond') {
    return h('div', {class: 'step-editor'},
      h('h3', {}, t('Step {n}: answer', {n: i + 1}), last ? null : ` — ${t('the answer must be the last step')}`),
      field(t('Step id'), text(s.id, v => { s.id = v; }, {maxlength: 32})),
      field(t('Answer text'), area(s.text, v => { s.text = v; }, 2)));
  }
  const known = catalogue.some(x => x.ref === s.tool);
  return h('div', {class: 'step-editor'},
    h('h3', {}, t('Step {n}: call a tool', {n: i + 1})),
    h('div', {class: 'form-grid'},
      field(t('Step id'), text(s.id, v => { s.id = v; }, {maxlength: 32})),
      field(t('Tool'), h('select', {onchange: e => { s.tool = e.target.value; }},
        h('option', {value: '', selected: s.tool === ''}, t('Choose a tool')),
        !known && s.tool ? h('option', {value: s.tool, selected: true}, s.tool) : null,
        catalogue.map(x => h('option', {value: x.ref, selected: x.ref === s.tool}, toolLabel(x))))),
      field(t('Tool schema version'), text(s.toolSchemaVersion, v => { s.toolSchemaVersion = v; }, {size: 4})),
      field(t('Operation'), text(s.operation, v => { s.operation = v; }, {maxlength: 128})),
      field(t('Target'), text(s.target, v => { s.target = v; }, {maxlength: 128})),
      field(t('Resource'), text(s.resource, v => { s.resource = v; }, {maxlength: 128}))),
    field(t('Payload (JSON)'), area(s.payload, v => { s.payload = v; }, 4)),
    catalogue.length === 0 ? h('p', {class: 'hint'}, t('No tools are registered yet; ask a registry editor to connect the system.')) : null,
    controls);
}

async function save(ctx, state, errors) {
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
    lines: [t('It asks to use: {tools}.', {tools: tools(built.definition).join(', ') || '—'}),
      t('A registry approver who is not you must approve it; until then it cannot run.'),
      t('A saved version never changes: to change it, save a new version.')]});
  if (!c) return;
  const r = isNew
    ? await ctx.client.call('studio.save', {body: {name: state.name.trim(), display_name: state.displayName.trim(),
      description: state.description.trim(), department_id: state.department, definition: built.definition}})
    : await ctx.client.call('studio.newversion', {params: {id: state.agent.id}, body: {definition: built.definition}});
  if (!r.ok) {
    replace(errors, notice(r));
    return;
  }
  ctx.go(format('agents', [r.data.agent_id]));
}

