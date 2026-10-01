// security.js is the containment console: kill states (ADR-016), connector
// circuits (ADR-022) and tool quarantine (ADR-023). Every write names its
// target in a confirm dialog with a reason; PostgreSQL enforces roles, the
// second operator for clearing a kill, and every other rule.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, ok, replace, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {back, linkBack} from './common.js';
import {toolTable} from './inventory.js';

// run is one Studio run (ADR-016 Rev 1.1); global waits for platform authority.
export const SCOPES = ['tool', 'connector', 'agent_version', 'agent', 'team', 'action', 'run', 'tenant'];
const CODES = ['security_incident', 'policy_violation', 'operator_request', 'error_budget_exhausted'];
const MAX_CONNECTORS = 50;

export const circuitState = c => (c.disabled ? 'disabled' : c.open ? 'open' : 'closed');

export function killLines(scope, target, code) {
  return [t('Scope: {scope}', {scope}), t('Target: {target}', {target}), t('Reason code: {code}', {code}),
    t('New dispatch in this scope stops at once; calls in flight see the epoch change and settle as unknown outcomes. A second operator must clear it.')];
}

export async function render(ctx) {
  const [kind, id] = ctx.route.parts;
  if (kind === 'tools' && id) return toolPage(ctx, id);
  if (kind === 'connectors' && id) return connectorPage(ctx, id);
  return overview(ctx);
}

const notAnID = what => notice({status: 404, error: 'not_found', detail: t('not a {what} id', {what})});

async function overview(ctx) {
  const {client, session} = ctx;
  const operator = session.hasAny(['operator']);
  const [kills, conns] = await Promise.all([client.call('kill.list'), client.call('connector.list')]);
  const result = h('div');
  const parts = [section(t('Kill switch'),
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
  parts.push(section(t('Connectors and circuits'), table([
    [t('Connector'), r => link(r.c.name, format('security', ['connectors', r.c.id]))],
    [t('Protocol'), r => r.c.protocol],
    [t('Circuit'), r => (r.circuit.ok ? badge(circuitState(r.circuit.data)) : '—')],
    [t('Quarantined tools'), r => (r.tools.ok ? String(r.tools.data.tools.filter(x => x.quarantined_at).length) : '—')],
  ], rows, t('No connectors.')),
  conns.data.connectors.length > MAX_CONNECTORS
    ? h('p', {class: 'hint'}, t('Showing {shown} of {total} connectors; the rest are in Inventory.',
      {shown: MAX_CONNECTORS, total: conns.data.connectors.length}))
    : null));
  const quarantined = rows.flatMap(r => (r.tools.ok
    ? r.tools.data.tools.filter(x => x.quarantined_at).map(x => ({...x, connector: r.c.name})) : []));
  parts.push(section(t('Quarantined tools'), table([
    [t('Tool'), x => link(`${x.connector}.${x.name}`, format('security', ['tools', x.id]))],
    [t('Quarantined'), x => fmtTime(x.quarantined_at)],
    [t('Reason'), x => x.quarantine_reason ?? '—'],
  ], quarantined, emptyState(t('No quarantined tools'), t('A tool is quarantined when its definition changes.'), 'shield-ok'))));
  return h('div', {}, parts);
}

function killTable(ctx, kills, result) {
  const operator = ctx.session.hasAny(['operator']);
  return table([
    [t('Scope'), k => k.scope],
    [t('Target'), k => h('code', {}, k.target_id)],
    [t('State'), k => badge(k.killed ? 'killed' : 'cleared')],
    [t('Code'), k => k.reason_code],
    [t('Reason'), k => k.reason],
    [t('Epoch'), k => String(k.epoch)],
    [t('Changed'), k => fmtTime(k.changed_at)],
    ['', k => (operator && k.killed ? button(t('Clear…'), () => clearKill(ctx, k, result)) : '')],
  ], kills, emptyState(t('No kill states'), t('Nothing is stopped right now.'), 'shield-ok'));
}

async function clearKill(ctx, k, result) {
  const c = await ask({title: t('Clear a kill'), reason: 'required', confirmLabel: t('Clear kill'),
    lines: [t('Scope {scope}, target {target}', {scope: k.scope, target: k.target_id}),
      t('Dispatch in this scope resumes. The operator who set the kill cannot clear it.')]});
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
    placeholder: t('target UUID'), required: true, size: 38});
  const scope = h('select', {name: 'scope', onchange: () => {
    if (scope.value === 'tenant') target.value = me.tenantId;
  }}, SCOPES.map(s => h('option', {value: s, selected: s === initial}, s)));
  const form = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = values(form);
    if (!isUUID(v.target_id)) {
      replace(result, notice({status: 400, error: 'invalid', detail: t('the target must be a UUID')}));
      return;
    }
    const c = await ask({title: t('Kill scope {scope}', {scope: v.scope}), danger: true, reason: 'required', confirmLabel: t('Activate kill'),
      typed: v.scope === 'tenant' ? 'tenant' : null, lines: killLines(v.scope, v.target_id, v.reason_code)});
    if (!c) return;
    const r = await ctx.client.call('kill.set', {body: {scope: v.scope, target_id: v.target_id, killed: true,
      reason_code: v.reason_code, reason: c.reason}});
    if (!r.ok) {
      replace(result, notice(r));
      return;
    }
    replace(result, ok(t('Kill active: {scope} {target}, epoch {epoch}.', {scope: r.data.scope, target: r.data.target_id, epoch: r.data.epoch})),
      linkBack(ctx, 'kill_state', r.data.id), button(t('Refresh'), ctx.refresh));
  }},
  field(t('Scope'), scope), field(t('Target'), target), field(t('Reason code'), select('reason_code', CODES, 'security_incident')),
  h('button', {type: 'submit', class: 'danger'}, t('Kill…')));
  return form;
}

async function toolPage(ctx, id) {
  if (!isUUID(id)) return notAnID(t('tool'));
  const res = await ctx.client.call('tool.get', {params: {id}});
  if (!res.ok) return notice(res);
  const x = res.data;
  const quarantined = Boolean(x.quarantined_at);
  const result = h('div');
  const acts = [];
  if (!quarantined && ctx.session.hasAny(['operator', 'registry_approver'])) {
    acts.push(button(t('Quarantine…'), () => quarantine(ctx, x, result), {kind: 'danger'}));
  }
  if (quarantined && ctx.session.hasAny(['registry_approver'])) {
    acts.push(button(t('Release…'), () => release(ctx, x, result)));
  }
  return h('div', {},
    back(t('← Security'), '#/security'),
    h('h1', {}, x.name),
    h('div', {class: 'chips'}, badge(quarantined ? 'quarantined' : 'not quarantined'),
      badge(x.executable ? 'executable' : 'not executable')),
    section(t('Tool'), kv([
      [t('Id'), h('code', {}, x.id)],
      [t('Connector'), link(x.connector_id, format('security', ['connectors', x.connector_id]))],
      [t('Origin'), x.origin], [t('Contract matches'), String(x.contract_matches)],
      [t('Quarantined'), quarantined ? fmtTime(x.quarantined_at) : t('no')], [t('Quarantine reason'), x.quarantine_reason ?? '—'],
    ])),
    h('div', {class: 'actions'}, acts,
      link(t('Kill this tool'), format('security', [], {scope: 'tool', target: x.id, incident: ctx.route.query.incident})),
      link(t('Recorded definitions'), format('inventory', ['tools', x.id]))),
    result);
}

async function quarantine(ctx, x, result) {
  const c = await ask({title: t('Quarantine {name}', {name: x.name}), danger: true, reason: 'required', confirmLabel: t('Quarantine'),
    lines: [t('The tool stops executing for every agent until a registry approver releases it.')]});
  if (!c) return;
  const r = await ctx.client.call('tool.quarantine', {params: {id: x.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Quarantined.')), linkBack(ctx, 'tool', x.id), button(t('Refresh'), ctx.refresh));
}

async function release(ctx, x, result) {
  const c = await ask({title: t('Release {name}', {name: x.name}), reason: 'required', confirmLabel: t('Release'),
    lines: [t('Release does not recertify the tool: its contract must still match its current definition (ADR-023).')]});
  if (!c) return;
  const r = await ctx.client.call('tool.release', {params: {id: x.id}, body: {reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Released.')), linkBack(ctx, 'tool', x.id), button(t('Refresh'), ctx.refresh));
}

async function connectorPage(ctx, id) {
  if (!isUUID(id)) return notAnID(t('connector'));
  const [list, circuit, tools] = await Promise.all([ctx.client.call('connector.list'),
    ctx.client.call('connector.circuit', {params: {id}}), ctx.client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: t('no such connector')});
  const result = h('div');
  let circuitPart = notice(circuit);
  if (circuit.ok) {
    const s = circuit.data;
    const by = s.changed_by ?? (s.changed_by_worker ? t('worker {id}', {id: s.changed_by_worker}) : '—');
    circuitPart = h('div', {},
      kv([[t('State'), badge(circuitState(s))], [t('Open until'), fmtTime(s.open_until)], [t('Reason'), s.reason || '—'],
        [t('Changed'), fmtTime(s.changed_at)], [t('Changed by'), by]]),
      ctx.session.hasAny(['operator']) ? h('div', {class: 'actions'}, s.disabled
        ? button(t('Enable…'), () => enableCircuit(ctx, c, result))
        : button(t('Disable…'), () => disableCircuit(ctx, c, result), {kind: 'danger'})) : null);
  }
  return h('div', {},
    back(t('← Security'), '#/security'),
    h('h1', {}, c.name),
    section(t('Circuit'), circuitPart),
    section(t('Tools'), tools.ok ? toolTable(tools.data.tools, 'security') : notice(tools)),
    result);
}

async function disableCircuit(ctx, c, result) {
  const x = await ask({title: t('Disable connector {name}', {name: c.name}), danger: true, reason: 'required', confirmLabel: t('Disable'),
    lines: [t('New dispatch to this connector stops until an operator enables it. Calls in flight are not interrupted.')]});
  if (!x) return;
  const r = await ctx.client.call('circuit.disable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Disabled.')), linkBack(ctx, 'connector', c.id), button(t('Refresh'), ctx.refresh));
}

async function enableCircuit(ctx, c, result) {
  const x = await ask({title: t('Enable connector {name}', {name: c.name}), reason: 'required', confirmLabel: t('Enable'),
    lines: [t('Dispatch to this connector resumes (a breaker a worker opened still holds until it expires).')]});
  if (!x) return;
  const r = await ctx.client.call('circuit.enable', {params: {id: c.id}, body: {reason: x.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Enabled.')), linkBack(ctx, 'connector', c.id), button(t('Refresh'), ctx.refresh));
}
