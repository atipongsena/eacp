// agents.js lists the caller's Studio agents (all of them for approvers and
// auditors) and shows one: where it is on the way to running, what it does,
// and, once it is ready, a form to run it.
import {h, section, notice, table, link, badge, input, field, kv, relTime, replace, emptyState, button, ok, select,
  values as formValues} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {STAGES, stageIndex, statusSentence} from './status.js';
import {tools, models} from './definition.js';
import {approvedPreview} from './preview.js';
import {definitionView, stageBadge, scopeBadge, stateBadge, approverOf} from './common.js';

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
  const hub = mine ? await hubSection(ctx, agent, v) : null;
  return h('div', {},
    h('p', {}, link(t('← Agents'), format('agents'))),
    h('h1', {}, agent.display_name),
    h('div', {class: 'chips'}, h('code', {}, agent.name), badge('version', `v${v.version}`), stageBadge(v)),
    agent.description ? h('p', {class: 'hint'}, agent.description) : null,
    stepper(v),
    h('p', {class: 'status-line', role: 'status'}, statusSentence(v)),
    mine ? h('div', {class: 'actions'}, link(t('Save a new version…'), format('new', [], {agent: agent.id}))) : null,
    v.status === 'ready' ? runForm(ctx, {agentId: agent.id, displayName: agent.display_name, version: v.version,
      definition: v.definition}) : null,
    mine && v.status === 'ready' ? approvedPreview(ctx, {versionId: v.id, agentId: agent.id, definition: v.definition}) : null,
    hub,
    runs.length ? section(t('Runs you started in this tab'), table([
      [t('Run'), r => link(r.id.slice(0, 8), format('runs', [r.id]))],
      [t('Started'), r => relTime(r.at, Date.now(), {node: true})],
    ], runs)) : null,
    section(t('What it does'), definitionView(v.definition)),
    section(t('Record'), kv([
      [t('Version id'), h('code', {}, v.id)],
      [t('Digest'), h('code', {}, v.digest)],
      [t('Tools it may use'), v.capability.join(', ') || '—'],
      [t('Models it may use'), (v.models ?? []).join(', ') || '—'],
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

// runForm asks for a version's declared inputs and starts a run of the agent
// as the signed-in person. The server checks who may run it (the owner, or
// someone its Hub listing reaches) and the inputs.
export function runForm(ctx, {agentId, displayName, version, definition}) {
  const inputs = Object.entries(definition?.inputs ?? {});
  const out = h('div');
  const f = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const given = formValues(f);
    const values = Object.fromEntries(inputs.map(([name]) => [name, given[name] ?? '']));
    const c = await ask({title: t('Run {agent}', {agent: displayName}), confirmLabel: t('Run'),
      lines: [t('It runs as you, with version {version}.', {version}),
        t('Models: {models}', {models: models(definition).join(', ') || '—'}),
        t('It may call: {tools}.', {tools: tools(definition).join(', ') || '—'}),
        t('Tool steps are governed actions; model steps use the gateway and the agent’s budget. Kill switches apply to both.')]});
    if (!c) return;
    const r = await ctx.client.call('studio.runstart', {params: {id: agentId}, body: {inputs: values}});
    if (!r.ok) {
      replace(out, notice(r));
      return;
    }
    ctx.started.unshift({id: r.data.id, agentId, at: new Date().toISOString()});
    ctx.go(format('runs', [r.data.id]));
  }},
  inputs.map(([name, spec]) => field(name, input(name, {required: true, maxlength: spec.max_length}))),
  h('button', {type: 'submit', class: 'primary'}, t('Run…')));
  return section(t('Run it'), f, out);
}

// hubSection shows the owner where their agent stands in the Hub (Phase
// 27b): its listing, its open or last proposal, and, for a ready version
// with nothing open, a form to propose it to the department or the
// organisation. A lead or an approver other than the owner decides.
async function hubSection(ctx, agent, v) {
  const res = await ctx.client.call('hub.listing', {params: {id: agent.id}});
  if (!res.ok) return section(t('Hub'), notice(res));
  const {listing, proposal} = res.data ?? {};
  const out = h('div');
  const open = proposal != null && proposal.decision == null;
  const lines = [];
  if (listing) {
    lines.push(h('p', {}, t('In the Hub:'), ' ', scopeBadge(listing), ' ', stateBadge(listing), ' ',
      t('version {version}', {version: listing.version}), ' · ', link(t('Open its listing'), format('hub', [listing.id]))));
  } else {
    lines.push(h('p', {class: 'hint'}, t('Not in the Hub: only you run it.')));
  }
  if (open) {
    lines.push(h('p', {class: 'status-line', role: 'status'}, t('Waiting for {who} to publish version {version}.',
      {who: approverOf(proposal.scope), version: proposal.version})),
    h('div', {class: 'actions'}, button(t('Cancel proposal…'), async () => {
      const c = await ask({title: t('Cancel the proposal'), reason: 'required', confirmLabel: t('Cancel proposal'),
        lines: [t('Nothing is published; you can propose again later.')]});
      if (!c) return;
      const r = await ctx.client.call('hub.cancel', {params: {id: proposal.id}, body: {reason: c.reason}});
      replace(out, r.ok ? ok(t('Cancelled.')) : notice(r));
      if (r.ok) ctx.refresh();
    })));
  } else if (proposal?.decision === 'rejected') {
    lines.push(h('p', {class: 'hint'}, t('Your last proposal was rejected: {reason}', {reason: proposal.decision_reason ?? ''})));
  }
  const f = !open && v.status === 'ready' ? h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const given = formValues(f);
    const tags = given.tags.split(',').map(x => x.trim().toLowerCase()).filter(Boolean);
    const c = await ask({title: t('Publish {agent} to the Hub', {agent: agent.display_name}), confirmLabel: t('Propose'),
      lines: [t('Version {version} is proposed; {who} decides.', {version: v.version, who: approverOf(given.scope)}),
        t('Once published, whoever it reaches can run it as themselves and copy it; nobody else can change it.')]});
    if (!c) return;
    const r = await ctx.client.call('hub.propose', {params: {id: agent.id},
      body: {version_id: v.id, scope: given.scope, tags, note: given.note}});
    replace(out, r.ok ? ok(t('Proposed.')) : notice(r));
    if (r.ok) ctx.refresh();
  }},
  h('div', {class: 'form-grid'},
    field(t('Share with'), select('scope', [['DEPARTMENT', t('my department')], ['ORG', t('the whole organisation')]], 'DEPARTMENT')),
    field(t('Tags, separated by commas'), input('tags', {placeholder: t('for example leave, hr')})),
    field(t('Note for the approver'), input('note', {maxlength: 500}))),
  h('button', {type: 'submit'}, t('Publish to the Hub…'))) : null;
  return section(t('Hub'), lines, f, out);
}
