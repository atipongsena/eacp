// inventory.js shows agents, their versions and allowlists, connectors and
// tools with their recorded definitions. Everything here is read-only.
import {h, section, notice, table, badge, link, fmtTime, kv, json, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {t} from '../i18n.js';
import {back} from './common.js';

export async function render(ctx) {
  const [kind, id] = ctx.route.parts;
  if (kind === 'agents' && id) return agent(ctx, id);
  if (kind === 'connectors' && id) return connector(ctx, id);
  if (kind === 'tools' && id) return tool(ctx, id);
  return overview(ctx);
}

async function overview({client}) {
  const [agents, connectors] = await Promise.all([client.call('agent.list'), client.call('connector.list')]);
  return h('div', {},
    section(t('Agents'), agents.ok ? table([
      [t('Agent'), a => link(a.name, format('inventory', ['agents', a.name]))],
      [t('Display name'), a => a.display_name],
      [t('Environment'), a => a.environment],
      [t('Risk'), a => badge(a.risk_class)],
      [t('Registered'), a => fmtTime(a.created_at)],
    ], agents.data.agents, emptyState(t('No agents are registered'), t('Register an agent with eacpctl to see it here.'), 'fleet')) : notice(agents)),
    section(t('Connectors'), connectors.ok ? table([
      [t('Connector'), c => link(c.name, format('inventory', ['connectors', c.id]))],
      [t('Protocol'), c => c.protocol],
      [t('Endpoint'), c => c.endpoint],
      [t('Tools'), c => (c.tools ?? []).join(', ') || '—'],
    ], connectors.data.connectors, emptyState(t('No connectors are registered'), t('Connectors give agents governed access to enterprise systems.'), 'inventory')) : notice(connectors)));
}

async function agent({client}, ref) {
  const res = await client.call('agent.get', {params: {ref}});
  if (!res.ok) return notice(res);
  const a = res.data;
  return h('div', {},
    back(t('← Inventory'), '#/inventory'),
    h('h1', {}, a.display_name || a.name),
    section(t('Agent'), kv([
      [t('Name'), a.name], [t('Id'), h('code', {}, a.id)], [t('Environment'), a.environment], [t('Risk class'), badge(a.risk_class)],
      [t('Owner principal'), a.owner_principal_id ?? '—'], [t('Owner group'), a.owner_group_id ?? '—'],
      [t('Registered'), fmtTime(a.created_at)],
    ])),
    section(t('Versions (newest first)'), table([
      [t('Version'), v => String(v.number)],
      [t('State'), v => badge(v.state)],
      [t('Reason'), v => v.state_reason ?? ''],
      [t('Runtime'), v => v.runtime],
      [t('Code'), v => v.code_ref],
      [t('Allowed tools'), v => (v.allowed_tools ?? []).join(', ') || '—'],
      [t('Id'), v => h('code', {}, v.id)],
    ], a.versions, t('No versions.'))),
    h('p', {}, link(t('Blast radius of the active version'), '#/dependencies')));
}

export function toolTable(tools, area = 'inventory') {
  return table([
    [t('Tool'), x => link(x.name, format(area, ['tools', x.id]))],
    [t('Origin'), x => x.origin],
    [t('Executable'), x => badge(x.executable ? 'executable' : 'not executable')],
    [t('Contract matches'), x => String(x.contract_matches)],
    [t('Quarantined'), x => (x.quarantined_at ? fmtTime(x.quarantined_at) : t('no'))],
    [t('Missing since'), x => fmtTime(x.missing_since)],
  ], tools, t('No tools.'));
}

async function connector({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not a connector id')});
  const [list, tools] = await Promise.all([client.call('connector.list'), client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: t('no such connector')});
  return h('div', {},
    back(t('← Inventory'), '#/inventory'),
    h('h1', {}, c.name),
    section(t('Connector'), kv([[t('Id'), h('code', {}, c.id)], [t('Protocol'), c.protocol], [t('Endpoint'), c.endpoint],
      [t('Secret reference'), c.secret_ref || '—']])),
    section(t('Tools'), tools.ok ? toolTable(tools.data.tools) : notice(tools)),
    h('p', {}, link(t('Circuit and containment'), format('security', ['connectors', c.id]))));
}

async function tool({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not a tool id')});
  const [res, defs] = await Promise.all([client.call('tool.get', {params: {id}}),
    client.call('tool.definitions', {params: {id}})]);
  if (!res.ok) return notice(res);
  const x = res.data;
  return h('div', {},
    back(t('← Inventory'), '#/inventory'),
    h('h1', {}, x.name),
    section(t('Tool'), kv([
      [t('Id'), h('code', {}, x.id)],
      [t('Connector'), link(x.connector_id, format('inventory', ['connectors', x.connector_id]))],
      [t('Origin'), x.origin], [t('Remote name'), x.remote_name ?? '—'],
      [t('Executable'), badge(x.executable ? 'executable' : 'not executable')],
      [t('Contract matches'), String(x.contract_matches)], [t('Active contract'), x.active_contract_id ?? '—'],
      [t('Quarantined'), x.quarantined_at ? `${fmtTime(x.quarantined_at)}: ${x.quarantine_reason ?? ''}` : t('no')],
      [t('Missing since'), fmtTime(x.missing_since)],
    ])),
    h('p', {}, link(t('Containment for this tool'), format('security', ['tools', x.id]))),
    section(t('Recorded definitions'), defs.ok ? definitions(defs.data.definitions) : notice(defs)));
}

function definitions(list) {
  if (!list || list.length === 0) return emptyState(t('No recorded definitions (HTTP tools have none).'));
  return list.map(d => h('details', {},
    h('summary', {}, t('#{seq} · risk {risk} · {time} · {changes}', {seq: d.seq, risk: d.risk, time: fmtTime(d.observed_at),
      changes: (d.changes ?? []).join(', ') || t('first seen')})),
    kv([
      [t('Fingerprint'), h('code', {}, d.fingerprint)],
      [t('Display digest'), h('code', {}, d.display_digest)],
      [t('Server hints (untrusted)'), `read_only=${d.read_only} destructive=${d.destructive} idempotent=${d.idempotent} open_world=${d.open_world}`],
      [t('Observed by'), d.observed_by],
    ]),
    h('h3', {}, t('Definition')), json(d.definition),
    h('h3', {}, t('Server-described display (untrusted)')), json(d.display)));
}
