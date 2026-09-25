// fleet.js shows fleet health and runs fleet operations (ADR-024). Every
// operation is previewed with dry_run first; the confirmed request is the
// previewed one without dry_run, and PostgreSQL re-checks each transition.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back, linkBack, mapText} from './common.js';

const OPS = ['pause', 'quarantine', 'resume', 'release'];

export async function render(ctx) {
  if (ctx.route.parts[0] === 'operations' && ctx.route.parts[1]) return operation(ctx, ctx.route.parts[1]);
  const {client, session} = ctx;
  const [health, agents] = await Promise.all([client.call('fleet.health'), client.call('fleet.agents')]);
  return h('div', {},
    health.ok ? healthPanel(health.data) : notice(health),
    session.hasAny(['operator', 'registry_approver']) ? operationForm(ctx) : null,
    section('Agents', agents.ok ? agentTable(agents.data.agents) : notice(agents)));
}

function healthPanel(s) {
  return section(`Fleet health (last ${s.window})`,
    kv([
      ['Agents', String(s.agents)], ['By health', mapText(s.by_health)], ['By environment', mapText(s.by_environment)],
      ['Quarantined', String(s.quarantined)], ['Unknown owner', String(s.unknown_owner)],
      ['Capability drift', String(s.capability_drift)], ['Circuit issues', String(s.circuit_issues)],
      ['Active kills', String(s.active_kills)], ['Open actions', mapText(s.open_actions)],
    ]),
    h('p', {class: 'hint'}, `Coverage: ${s.coverage}. The fleet view observes; it never decides.`));
}

function agentTable(list) {
  return table([
    ['Agent', a => link(a.name, format('inventory', ['agents', a.name]))],
    ['Environment', a => a.environment],
    ['Risk', a => badge(a.risk_class)],
    ['Health', a => badge(a.health)],
    ['Reasons', a => (a.reasons ?? []).join(', ') || '—'],
    ['Active version', a => (a.active_version ? `v${a.active_version.number}` : '—')],
    ['Canary', a => (a.canary ? `v${a.canary.version.number} at ${a.canary.canary_bp / 100}%` : '—')],
    ['Kills', a => String((a.kills ?? []).length)],
    ['Drift', a => String((a.capability_drift ?? []).length)],
  ], list, 'No agents.');
}

export function request(v) {
  const r = {kind: v.kind, reason: v.reason};
  if (v.kind === 'resume' || v.kind === 'release') {
    r.source_operation_id = v.source_operation_id;
    return r;
  }
  const selector = {};
  const agents = String(v.agents ?? '').split(',').map(s => s.trim()).filter(Boolean);
  if (agents.length) selector.agents = agents;
  if (v.tool) selector.tool = v.tool;
  if (v.environment) selector.environment = v.environment;
  if (v.risk_class) selector.risk_class = v.risk_class;
  r.selector = selector;
  return r;
}

function operationForm(ctx) {
  const q = ctx.route.query;
  const result = h('div');
  const form = h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const v = values(form);
    if ((v.kind === 'resume' || v.kind === 'release') && !isUUID(v.source_operation_id)) {
      replace(result, notice({status: 400, error: 'invalid', detail: `${v.kind} needs the source operation's UUID`}));
      return;
    }
    preview(ctx, request(v), result);
  }},
  field('Operation', select('kind', OPS, OPS.includes(q.op) ? q.op : 'pause')),
  field('Agents (slugs, comma separated)', input('agents', {value: q.agents ?? '', size: 40})),
  field('Tool on the allowlist (connector.tool)', input('tool', {value: q.tool ?? '', size: 28})),
  field('Environment', select('environment', ['', 'development', 'staging', 'production'], q.environment ?? '')),
  field('Risk class', select('risk_class', ['', 'low', 'medium', 'high', 'critical'], '')),
  field('Source operation (resume, release)', input('source_operation_id', {value: q.source ?? '', size: 38})),
  field('Reason', input('reason', {required: true, maxlength: 1024, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, 'Preview (dry run)'));
  return section('Fleet operation',
    h('p', {class: 'hint'}, 'Every operation is previewed first. Pause and quarantine need a selector; the console never selects the whole fleet.'),
    form, result);
}

function targetTable(targets) {
  return table([
    ['Agent', t => t.agent_name],
    ['Version', t => String(t.version)],
    ['From', t => badge(t.from)],
    ['To', t => badge(t.to)],
  ], targets, 'No targets.');
}

async function preview(ctx, req, result) {
  const dry = await ctx.client.call('fleet.apply', {body: {...req, dry_run: true}});
  if (!dry.ok) {
    replace(result, notice(dry));
    return;
  }
  const targets = dry.data.targets ?? [];
  const skipped = dry.data.skipped ?? [];
  replace(result, 
    h('h3', {}, `Dry run: ${targets.length} version(s) would change, ${skipped.length} skipped`),
    targetTable(targets),
    skipped.length ? table([['Agent', s => s.agent_name], ['Skipped because', s => s.reason]], skipped) : null,
    targets.length
      ? button(`Apply ${req.kind}…`, () => apply(ctx, req, targets, result), {kind: 'danger'})
      : h('p', {class: 'hint'}, 'Nothing to change.'));
}

async function apply(ctx, req, targets, result) {
  const listed = targets.slice(0, 10).map(t => `${t.agent_name} v${t.version} ${t.from}→${t.to}`).join(', ');
  const c = await ask({title: `Apply fleet ${req.kind}`, danger: true, confirmLabel: `Apply ${req.kind}`,
    lines: [`${targets.length} version(s): ${listed}${targets.length > 10 ? ', …' : ''}`, `Reason: ${req.reason}`,
      'PostgreSQL re-checks every transition: the applied set can differ from the preview if the fleet changed.']});
  if (!c) return;
  const r = await ctx.client.call('fleet.apply', {body: req});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(`Applied operation ${r.data.id}: ${r.data.targets.length} version(s) changed.`),
    h('div', {class: 'actions'}, link('Open the operation', format('fleet', ['operations', r.data.id]))),
    linkBack(ctx, 'fleet_operation', r.data.id));
}

async function operation({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an operation id'});
  const res = await client.call('fleet.operation', {params: {id}});
  if (!res.ok) return notice(res);
  const op = res.data;
  const undo = op.kind === 'pause' ? 'resume' : op.kind === 'quarantine' ? 'release' : null;
  return h('div', {},
    back('← Fleet', '#/fleet'),
    h('h1', {}, `Fleet ${op.kind}`),
    section('Operation', kv([
      ['Id', h('code', {}, op.id)], ['Reason', op.reason], ['Created', fmtTime(op.created_at)],
      ['Created by', op.created_by ?? '—'], ['Source operation', op.source_operation_id ?? '—'],
      ['Selector', JSON.stringify(op.selector ?? {})],
    ]),
    undo ? h('p', {}, link(`Prepare a ${undo} of this operation`, format('fleet', [], {op: undo, source: op.id}))) : null),
    section('Targets', targetTable(op.targets ?? [])),
    (op.skipped ?? []).length ? section('Skipped', table([['Agent', s => s.agent_name], ['Reason', s => s.reason]], op.skipped)) : null);
}
