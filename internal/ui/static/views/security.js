// security.js is the containment console: kill states (ADR-016), connector
// circuits (ADR-022) and tool quarantine (ADR-023). Every write names its
// target in a confirm dialog with a reason; PostgreSQL enforces roles, the
// second operator for clearing a kill, and every other rule.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back, linkBack} from './common.js';
import {toolTable} from './inventory.js';

const SCOPES = ['tool', 'connector', 'agent_version', 'agent', 'team', 'action', 'tenant'];
const CODES = ['security_incident', 'policy_violation', 'operator_request', 'error_budget_exhausted'];
const MAX_CONNECTORS = 50;

export const circuitState = c => (c.disabled ? 'disabled' : c.open ? 'open' : 'closed');

export function killLines(scope, target, code) {
  return [`Scope: ${scope}`, `Target: ${target}`, `Reason code: ${code}`,
    'New dispatch in this scope stops at once; calls in flight see the epoch change and settle as unknown outcomes. A second operator must clear it.'];
}

export async function render(ctx) {
  const [kind, id] = ctx.route.parts;
  if (kind === 'tools' && id) return toolPage(ctx, id);
  if (kind === 'connectors' && id) return connectorPage(ctx, id);
  return overview(ctx);
}

const notAnID = what => notice({status: 404, error: 'not_found', detail: `not a ${what} id`});

async function overview(ctx) {
  const {client, session} = ctx;
  const operator = session.hasAny(['operator']);
  const [kills, conns] = await Promise.all([client.call('kill.list'), client.call('connector.list')]);
  const result = h('div');
  const parts = [section('Kill switch',
    kills.ok ? killTable(ctx, kills.data.kills, result) : notice(kills),
    operator ? killForm(ctx, result) : null,
    result)];
  if (!conns.ok) return h('div', {}, parts, notice(conns));
  const shown = conns.data.connectors.slice(0, MAX_CONNECTORS);
  const rows = await Promise.all(shown.map(async c => {
    const [circuit, tools] = await Promise.all([client.call('connector.circuit', {params: {id: c.id}}),
      client.call('connector.tools', {params: {id: c.id}})]);
    return {c, circuit, tools};
  }));
  parts.push(section('Connectors and circuits', table([
    ['Connector', r => link(r.c.name, format('security', ['connectors', r.c.id]))],
    ['Protocol', r => r.c.protocol],
    ['Circuit', r => (r.circuit.ok ? badge(circuitState(r.circuit.data)) : '—')],
    ['Quarantined tools', r => (r.tools.ok ? String(r.tools.data.tools.filter(t => t.quarantined_at).length) : '—')],
  ], rows, 'No connectors.'),
  conns.data.connectors.length > MAX_CONNECTORS
    ? h('p', {class: 'hint'}, `Showing ${MAX_CONNECTORS} of ${conns.data.connectors.length} connectors; the rest are in Inventory.`)
    : null));
  const quarantined = rows.flatMap(r => (r.tools.ok
    ? r.tools.data.tools.filter(t => t.quarantined_at).map(t => ({...t, connector: r.c.name})) : []));
  parts.push(section('Quarantined tools', table([
    ['Tool', t => link(`${t.connector}.${t.name}`, format('security', ['tools', t.id]))],
    ['Quarantined', t => fmtTime(t.quarantined_at)],
    ['Reason', t => t.quarantine_reason ?? '—'],
  ], quarantined, 'No quarantined tools.')));
  return h('div', {}, parts);
}

function killTable(ctx, kills, result) {
  const operator = ctx.session.hasAny(['operator']);
  return table([
    ['Scope', k => k.scope],
    ['Target', k => h('code', {}, k.target_id)],
    ['State', k => badge(k.killed ? 'killed' : 'cleared')],
    ['Code', k => k.reason_code],
    ['Reason', k => k.reason],
    ['Epoch', k => String(k.epoch)],
    ['Changed', k => fmtTime(k.changed_at)],
    ['', k => (operator && k.killed ? button('Clear…', () => clearKill(ctx, k, result)) : '')],
  ], kills, 'No kill states.');
}

async function clearKill(ctx, k, result) {
  const c = await ask({title: 'Clear a kill', reason: 'required', confirmLabel: 'Clear kill',
    lines: [`Scope ${k.scope}, target ${k.target_id}`,
      'Dispatch in this scope resumes. The operator who set the kill cannot clear it.']});
  if (!c) return;
  const r = await ctx.client.call('kill.set', {body: {scope: k.scope, target_id: k.target_id, killed: false,
    reason_code: 'operator_request', reason: c.reason}});
  if (r.ok) ctx.refresh();
  else replace(result, notice(r));
}

function killForm(ctx, result) {
  const q = ctx.route.query;
  const me = ctx.session.me();
  const initial = SCOPES.includes(q.scope) ? q.scope : 'tool';
  const target = input('target_id', {value: isUUID(q.target) ? q.target : (initial === 'tenant' ? me.tenantId : ''),
    placeholder: 'target UUID', required: true, size: 38});
  const scope = h('select', {name: 'scope', onchange: () => {
    if (scope.value === 'tenant') target.value = me.tenantId;
  }}, SCOPES.map(s => h('option', {value: s, selected: s === initial}, s)));
  const form = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = values(form);
    if (!isUUID(v.target_id)) {
      replace(result, notice({status: 400, error: 'invalid', detail: 'the target must be a UUID'}));
      return;
    }
    const c = await ask({title: `Kill scope ${v.scope}`, danger: true, reason: 'required', confirmLabel: 'Activate kill',
      typed: v.scope === 'tenant' ? 'tenant' : null, lines: killLines(v.scope, v.target_id, v.reason_code)});
    if (!c) return;
    const r = await ctx.client.call('kill.set', {body: {scope: v.scope, target_id: v.target_id, killed: true,
      reason_code: v.reason_code, reason: c.reason}});
    if (!r.ok) {
      replace(result, notice(r));
      return;
    }
    replace(result, ok(`Kill active: ${r.data.scope} ${r.data.target_id}, epoch ${r.data.epoch}.`),
      linkBack(ctx, 'kill_state', r.data.id), button('Refresh', ctx.refresh));
  }},
  field('Scope', scope), field('Target', target), field('Reason code', select('reason_code', CODES, 'security_incident')),
  h('button', {type: 'submit', class: 'danger'}, 'Kill…'));
  return form;
}

async function toolPage(ctx, id) {
  if (!isUUID(id)) return notAnID('tool');
  const res = await ctx.client.call('tool.get', {params: {id}});
  if (!res.ok) return notice(res);
  const t = res.data;
  const quarantined = Boolean(t.quarantined_at);
  const result = h('div');
  const acts = [];
  if (!quarantined && ctx.session.hasAny(['operator', 'registry_approver'])) {
    acts.push(button('Quarantine…', () => quarantine(ctx, t, result), {kind: 'danger'}));
  }
  if (quarantined && ctx.session.hasAny(['registry_approver'])) {
    acts.push(button('Release…', () => release(ctx, t, result)));
  }
  return h('div', {},
    back('← Security', '#/security'),
    h('h1', {}, t.name),
    h('div', {class: 'chips'}, badge(quarantined ? 'quarantined' : 'not quarantined'),
      badge(t.executable ? 'executable' : 'not executable')),
    section('Tool', kv([
      ['Id', h('code', {}, t.id)],
      ['Connector', link(t.connector_id, format('security', ['connectors', t.connector_id]))],
      ['Origin', t.origin], ['Contract matches', String(t.contract_matches)],
      ['Quarantined', quarantined ? fmtTime(t.quarantined_at) : 'no'], ['Quarantine reason', t.quarantine_reason ?? '—'],
    ])),
    h('div', {class: 'actions'}, acts,
      link('Kill this tool', format('security', [], {scope: 'tool', target: t.id, incident: ctx.route.query.incident})),
      link('Recorded definitions', format('inventory', ['tools', t.id]))),
    result);
}

async function quarantine(ctx, t, result) {
  const c = await ask({title: `Quarantine ${t.name}`, danger: true, reason: 'required', confirmLabel: 'Quarantine',
    lines: ['The tool stops executing for every agent until a registry approver releases it.']});
  if (!c) return;
  const r = await ctx.client.call('tool.quarantine', {params: {id: t.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Quarantined.'), linkBack(ctx, 'tool', t.id), button('Refresh', ctx.refresh));
}

async function release(ctx, t, result) {
  const c = await ask({title: `Release ${t.name}`, reason: 'required', confirmLabel: 'Release',
    lines: ['Release does not recertify the tool: its contract must still match its current definition (ADR-023).']});
  if (!c) return;
  const r = await ctx.client.call('tool.release', {params: {id: t.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Released.'), linkBack(ctx, 'tool', t.id), button('Refresh', ctx.refresh));
}

async function connectorPage(ctx, id) {
  if (!isUUID(id)) return notAnID('connector');
  const [list, circuit, tools] = await Promise.all([ctx.client.call('connector.list'),
    ctx.client.call('connector.circuit', {params: {id}}), ctx.client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: 'no such connector'});
  const result = h('div');
  let circuitPart = notice(circuit);
  if (circuit.ok) {
    const s = circuit.data;
    const by = s.changed_by ?? (s.changed_by_worker ? `worker ${s.changed_by_worker}` : '—');
    circuitPart = h('div', {},
      kv([['State', badge(circuitState(s))], ['Open until', fmtTime(s.open_until)], ['Reason', s.reason || '—'],
        ['Changed', fmtTime(s.changed_at)], ['Changed by', by]]),
      ctx.session.hasAny(['operator']) ? h('div', {class: 'actions'}, s.disabled
        ? button('Enable…', () => enableCircuit(ctx, c, result))
        : button('Disable…', () => disableCircuit(ctx, c, result), {kind: 'danger'})) : null);
  }
  return h('div', {},
    back('← Security', '#/security'),
    h('h1', {}, c.name),
    section('Circuit', circuitPart),
    section('Tools', tools.ok ? toolTable(tools.data.tools, 'security') : notice(tools)),
    result);
}

async function disableCircuit(ctx, c, result) {
  const x = await ask({title: `Disable connector ${c.name}`, danger: true, reason: 'required', confirmLabel: 'Disable',
    lines: ['New dispatch to this connector stops until an operator enables it. Calls in flight are not interrupted.']});
  if (!x) return;
  const r = await ctx.client.call('circuit.disable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Disabled.'), linkBack(ctx, 'connector', c.id), button('Refresh', ctx.refresh));
}

async function enableCircuit(ctx, c, result) {
  const x = await ask({title: `Enable connector ${c.name}`, reason: 'required', confirmLabel: 'Enable',
    lines: ['Dispatch to this connector resumes (a breaker a worker opened still holds until it expires).']});
  if (!x) return;
  const r = await ctx.client.call('circuit.enable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok('Enabled.'), linkBack(ctx, 'connector', c.id), button('Refresh', ctx.refresh));
}
