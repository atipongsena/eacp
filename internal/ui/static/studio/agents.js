// agents.js lists the caller's Studio agents (all of them for approvers and
// auditors) and shows one: where it is on the way to running, what it does,
// and, once it is ready, a form to run it.
import {h, section, notice, table, link, badge, input, field, kv, relTime, replace, emptyState, values as formValues} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {STAGES, stageIndex, statusSentence} from './status.js';
import {tools} from './definition.js';
import {definitionView, stageBadge} from './common.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list(ctx) {
  const res = await ctx.client.call('studio.agents');
  if (!res.ok) return notice(res);
  const authoring = ctx.session.hasAny(['studio_author']);
  const empty = emptyState(t('No agents yet.'), authoring
    ? t('Start from the leave-balance template: it is filled in for you.') : t('Agents that authors save appear here.'), 'fleet');
  return h('div', {},
    authoring ? h('div', {class: 'actions'},
      link(t('New agent from the leave-balance template'), format('new', [], {template: 'leave-balance'}))) : null,
    section(t('Agents'), table([
      [t('Agent'), a => h('span', {}, link(a.display_name, format('agents', [a.id])), ' ', h('code', {}, a.name))],
      [t('Version'), a => `v${a.latest.version}`],
      [t('Stage'), a => stageBadge(a.latest)],
      [t('Where it is'), a => statusSentence(a.latest)],
      [t('Saved'), a => relTime(a.latest.created_at, Date.now(), {node: true})],
    ], res.data.agents, empty)));
}

async function detail(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not an agent id')});
  const res = await ctx.client.call('studio.agents');
  if (!res.ok) return notice(res);
  const agent = res.data.agents.find(a => a.id === id);
  if (!agent) return notice({status: 404, error: 'not_found', detail: t('no such Studio agent')});
  const vres = await ctx.client.call('studio.version', {params: {id: agent.latest.id}});
  if (!vres.ok) return notice(vres);
  const v = vres.data;
  // Wait on the approver or the runtime without asking the person to reload.
  if (['waiting_for_approval', 'waiting_for_credential'].includes(v.status)) ctx.pollEvery(5000);
  const mine = ctx.session.me().principalId === agent.owner_id;
  const runs = ctx.started.filter(r => r.agentId === agent.id);
  return h('div', {},
    h('p', {}, link(t('← Agents'), format('agents'))),
    h('h1', {}, agent.display_name),
    h('div', {class: 'chips'}, h('code', {}, agent.name), badge('version', `v${v.version}`), stageBadge(v)),
    agent.description ? h('p', {class: 'hint'}, agent.description) : null,
    stepper(v),
    h('p', {class: 'status-line', role: 'status'}, statusSentence(v)),
    mine ? h('div', {class: 'actions'}, link(t('Save a new version…'), format('new', [], {agent: agent.id}))) : null,
    v.status === 'ready' ? runForm(ctx, agent, v) : null,
    runs.length ? section(t('Runs you started in this tab'), table([
      [t('Run'), r => link(r.id.slice(0, 8), format('runs', [r.id]))],
      [t('Started'), r => relTime(r.at, Date.now(), {node: true})],
    ], runs)) : null,
    section(t('What it does'), definitionView(v.definition)),
    section(t('Record'), kv([
      [t('Version id'), h('code', {}, v.id)],
      [t('Digest'), h('code', {}, v.digest)],
      [t('Tools it may use'), v.capability.join(', ') || '—'],
      [t('Saved'), relTime(v.created_at, Date.now(), {node: true})],
      v.decided_at ? [t('Decided'), relTime(v.decided_at, Date.now(), {node: true})] : null,
    ])));
}

// stepper shows the four stages: done, current or still to come.
function stepper(v) {
  const at = stageIndex(v);
  return h('ul', {class: 'stepper', 'aria-label': t('Progress')}, STAGES.map((label, i) => {
    const state = at < 0 ? 'off' : i < at ? 'done' : i === at ? 'current' : 'todo';
    return h('li', {class: `stage stage-${state}`, 'aria-current': state === 'current' ? 'step' : null},
      state === 'done' ? h('span', {class: 'sr-only'}, t('done: ')) : null, label());
  }));
}

// runForm asks for the version's declared inputs and starts a run as the
// signed-in person. The server checks department membership and the inputs.
function runForm(ctx, agent, v) {
  const inputs = Object.entries(v.definition?.inputs ?? {});
  const out = h('div');
  const f = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const given = formValues(f);
    const values = Object.fromEntries(inputs.map(([name]) => [name, given[name] ?? '']));
    const c = await ask({title: t('Run {agent}', {agent: agent.display_name}), confirmLabel: t('Run'),
      lines: [t('It runs as you, with version {version}.', {version: v.version}),
        t('It may call: {tools}.', {tools: tools(v.definition).join(', ') || '—'}),
        t('Each step is an action: policy, approvals, budgets and kill switches apply to it.')]});
    if (!c) return;
    const r = await ctx.client.call('studio.runstart', {params: {id: agent.id}, body: {inputs: values}});
    if (!r.ok) {
      replace(out, notice(r));
      return;
    }
    ctx.started.unshift({id: r.data.id, agentId: agent.id, at: new Date().toISOString()});
    ctx.go(format('runs', [r.data.id]));
  }},
  inputs.map(([name, spec]) => field(name, input(name, {required: true, maxlength: spec.max_length}))),
  h('button', {type: 'submit', class: 'primary'}, t('Run…')));
  return section(t('Run it'), f, out);
}
