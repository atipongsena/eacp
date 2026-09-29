// fleet.js shows fleet health and runs fleet operations (ADR-024). Every
// operation is previewed with dry_run first; the confirmed request is the
// previewed one without dry_run, and PostgreSQL re-checks each transition.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {back, linkBack, mapText} from './common.js';

const OPS = ['pause', 'quarantine', 'resume', 'release'];

export async function render(ctx) {
  if (ctx.route.parts[0] === 'operations' && ctx.route.parts[1]) return operation(ctx, ctx.route.parts[1]);
  const {client, session} = ctx;
  const [health, agents] = await Promise.all([client.call('fleet.health'), client.call('fleet.agents')]);
  return h('div', {},
    health.ok ? healthPanel(health.data) : notice(health),
    session.hasAny(['operator', 'registry_approver']) ? operationForm(ctx) : null,
    section(t('Agents'), agents.ok ? agentTable(agents.data.agents) : notice(agents)));
}

function healthPanel(s) {
  return section(t('Fleet health (last {window})', {window: s.window}),
    kv([
      [t('Agents'), String(s.agents)], [t('By health'), mapText(s.by_health)], [t('By environment'), mapText(s.by_environment)],
      [t('Quarantined'), String(s.quarantined)], [t('Unknown owner'), String(s.unknown_owner)],
      [t('Capability drift'), String(s.capability_drift)], [t('Circuit issues'), String(s.circuit_issues)],
      [t('Active kills'), String(s.active_kills)], [t('Open actions'), mapText(s.open_actions)],
    ]),
    h('p', {class: 'hint'}, t('Coverage: {coverage}. The fleet view observes; it never decides.', {coverage: s.coverage})));
}

function agentTable(list) {
  return table([
    [t('Agent'), a => link(a.name, format('inventory', ['agents', a.name]))],
    [t('Environment'), a => a.environment],
    [t('Risk'), a => badge(a.risk_class)],
    [t('Health'), a => badge(a.health)],
    [t('Reasons'), a => (a.reasons ?? []).join(', ') || '—'],
    [t('Active version'), a => (a.active_version ? `v${a.active_version.number}` : '—')],
    [t('Canary'), a => (a.canary ? t('v{number} at {percent}%', {number: a.canary.version.number, percent: a.canary.canary_bp / 100}) : '—')],
    [t('Kills'), a => String((a.kills ?? []).length)],
    [t('Drift'), a => String((a.capability_drift ?? []).length)],
  ], list, emptyState(t('No agents'), t('Register an agent with eacpctl to see it here.'), 'fleet'));
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
      replace(result, notice({status: 400, error: 'invalid', detail: t('{kind} needs the source operation’s UUID', {kind: v.kind})}));
      return;
    }
    preview(ctx, request(v), result);
  }},
  field(t('Operation'), select('kind', OPS, OPS.includes(q.op) ? q.op : 'pause')),
  field(t('Agents (slugs, comma separated)'), input('agents', {value: q.agents ?? '', size: 40})),
  field(t('Tool on the allowlist (connector.tool)'), input('tool', {value: q.tool ?? '', size: 28})),
  field(t('Environment'), select('environment', ['', 'development', 'staging', 'production'], q.environment ?? '')),
  field(t('Risk class'), select('risk_class', ['', 'low', 'medium', 'high', 'critical'], '')),
  field(t('Source operation (resume, release)'), input('source_operation_id', {value: q.source ?? '', size: 38})),
  field(t('Reason'), input('reason', {required: true, maxlength: 1024, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, t('Preview (dry run)')));
  return section(t('Fleet operation'),
    h('p', {class: 'hint'}, t('Every operation is previewed first. Pause and quarantine need a selector; the console never selects the whole fleet.')),
    form, result);
}

function targetTable(targets) {
  return table([
    [t('Agent'), x => x.agent_name],
    [t('Version'), x => String(x.version)],
    [t('From'), x => badge(x.from)],
    [t('To'), x => badge(x.to)],
  ], targets, t('No targets.'));
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
    h('h3', {}, t('Dry run: {changed} version(s) would change, {skipped} skipped', {changed: targets.length, skipped: skipped.length})),
    targetTable(targets),
    skipped.length ? table([[t('Agent'), s => s.agent_name], [t('Skipped because'), s => s.reason]], skipped) : null,
    targets.length
      ? button(t('Apply {kind}…', {kind: req.kind}), () => apply(ctx, req, targets, result), {kind: 'danger'})
      : h('p', {class: 'hint'}, t('Nothing to change.')));
}

async function apply(ctx, req, targets, result) {
  const listed = targets.slice(0, 10).map(x => `${x.agent_name} v${x.version} ${x.from}→${x.to}`).join(', ');
  const c = await ask({title: t('Apply fleet {kind}', {kind: req.kind}), danger: true, confirmLabel: t('Apply {kind}', {kind: req.kind}),
    lines: [t('{n} version(s): {list}', {n: targets.length, list: `${listed}${targets.length > 10 ? ', …' : ''}`}),
      t('Reason: {reason}', {reason: req.reason}),
      t('PostgreSQL re-checks every transition: the applied set can differ from the preview if the fleet changed.')]});
  if (!c) return;
  const r = await ctx.client.call('fleet.apply', {body: req});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Applied operation {id}: {n} version(s) changed.', {id: r.data.id, n: r.data.targets.length})),
    h('div', {class: 'actions'}, link(t('Open the operation'), format('fleet', ['operations', r.data.id]))),
    linkBack(ctx, 'fleet_operation', r.data.id));
}

async function operation({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not an operation id')});
  const res = await client.call('fleet.operation', {params: {id}});
  if (!res.ok) return notice(res);
  const op = res.data;
  const undo = op.kind === 'pause' ? 'resume' : op.kind === 'quarantine' ? 'release' : null;
  return h('div', {},
    back(t('← Fleet'), '#/fleet'),
    h('h1', {}, t('Fleet {kind}', {kind: op.kind})),
    section(t('Operation'), kv([
      [t('Id'), h('code', {}, op.id)], [t('Reason'), op.reason], [t('Created'), fmtTime(op.created_at)],
      [t('Created by'), op.created_by ?? '—'], [t('Source operation'), op.source_operation_id ?? '—'],
      [t('Selector'), JSON.stringify(op.selector ?? {})],
    ]),
    undo ? h('p', {}, link(t('Prepare a {kind} of this operation', {kind: undo}), format('fleet', [], {op: undo, source: op.id}))) : null),
    section(t('Targets'), targetTable(op.targets ?? [])),
    (op.skipped ?? []).length ? section(t('Skipped'), table([[t('Agent'), s => s.agent_name], [t('Reason'), s => s.reason]], op.skipped)) : null);
}
