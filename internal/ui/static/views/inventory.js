// inventory.js shows agents, their versions and allowlists, connectors and
// tools with their recorded definitions. Everything here is read-only.
import {h, section, notice, table, badge, link, fmtTime, kv, json} from '../dom.js';
import {format, isUUID} from '../router.js';
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
    section('Agents', agents.ok ? table([
      ['Agent', a => link(a.name, format('inventory', ['agents', a.name]))],
      ['Display name', a => a.display_name],
      ['Environment', a => a.environment],
      ['Risk', a => badge(a.risk_class)],
      ['Registered', a => fmtTime(a.created_at)],
    ], agents.data.agents, 'No agents are registered.') : notice(agents)),
    section('Connectors', connectors.ok ? table([
      ['Connector', c => link(c.name, format('inventory', ['connectors', c.id]))],
      ['Protocol', c => c.protocol],
      ['Endpoint', c => c.endpoint],
      ['Tools', c => (c.tools ?? []).join(', ') || '—'],
    ], connectors.data.connectors, 'No connectors are registered.') : notice(connectors)));
}

async function agent({client}, ref) {
  const res = await client.call('agent.get', {params: {ref}});
  if (!res.ok) return notice(res);
  const a = res.data;
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, a.display_name || a.name),
    section('Agent', kv([
      ['Name', a.name], ['Id', h('code', {}, a.id)], ['Environment', a.environment], ['Risk class', badge(a.risk_class)],
      ['Owner principal', a.owner_principal_id ?? '—'], ['Owner group', a.owner_group_id ?? '—'],
      ['Registered', fmtTime(a.created_at)],
    ])),
    section('Versions (newest first)', table([
      ['Version', v => String(v.number)],
      ['State', v => badge(v.state)],
      ['Reason', v => v.state_reason ?? ''],
      ['Runtime', v => v.runtime],
      ['Code', v => v.code_ref],
      ['Allowed tools', v => (v.allowed_tools ?? []).join(', ') || '—'],
      ['Id', v => h('code', {}, v.id)],
    ], a.versions, 'No versions.')),
    h('p', {}, link('Blast radius of the active version', '#/dependencies')));
}

export function toolTable(tools, area = 'inventory') {
  return table([
    ['Tool', t => link(t.name, format(area, ['tools', t.id]))],
    ['Origin', t => t.origin],
    ['Executable', t => badge(t.executable ? 'executable' : 'not executable')],
    ['Contract matches', t => String(t.contract_matches)],
    ['Quarantined', t => (t.quarantined_at ? fmtTime(t.quarantined_at) : 'no')],
    ['Missing since', t => fmtTime(t.missing_since)],
  ], tools, 'No tools.');
}

async function connector({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not a connector id'});
  const [list, tools] = await Promise.all([client.call('connector.list'), client.call('connector.tools', {params: {id}})]);
  if (!list.ok) return notice(list);
  const c = list.data.connectors.find(x => x.id === id.toLowerCase());
  if (!c) return notice({status: 404, error: 'not_found', detail: 'no such connector'});
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, c.name),
    section('Connector', kv([['Id', h('code', {}, c.id)], ['Protocol', c.protocol], ['Endpoint', c.endpoint],
      ['Secret reference', c.secret_ref || '—']])),
    section('Tools', tools.ok ? toolTable(tools.data.tools) : notice(tools)),
    h('p', {}, link('Circuit and containment', format('security', ['connectors', c.id]))));
}

async function tool({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not a tool id'});
  const [t, defs] = await Promise.all([client.call('tool.get', {params: {id}}),
    client.call('tool.definitions', {params: {id}})]);
  if (!t.ok) return notice(t);
  const x = t.data;
  return h('div', {},
    back('← Inventory', '#/inventory'),
    h('h1', {}, x.name),
    section('Tool', kv([
      ['Id', h('code', {}, x.id)],
      ['Connector', link(x.connector_id, format('inventory', ['connectors', x.connector_id]))],
      ['Origin', x.origin], ['Remote name', x.remote_name ?? '—'],
      ['Executable', badge(x.executable ? 'executable' : 'not executable')],
      ['Contract matches', String(x.contract_matches)], ['Active contract', x.active_contract_id ?? '—'],
      ['Quarantined', x.quarantined_at ? `${fmtTime(x.quarantined_at)}: ${x.quarantine_reason ?? ''}` : 'no'],
      ['Missing since', fmtTime(x.missing_since)],
    ])),
    h('p', {}, link('Containment for this tool', format('security', ['tools', x.id]))),
    section('Recorded definitions', defs.ok ? definitions(defs.data.definitions) : notice(defs)));
}

function definitions(list) {
  if (!list || list.length === 0) return h('p', {class: 'empty'}, 'No recorded definitions (HTTP tools have none).');
  return list.map(d => h('details', {},
    h('summary', {}, `#${d.seq} · risk ${d.risk} · ${fmtTime(d.observed_at)} · ${(d.changes ?? []).join(', ') || 'first seen'}`),
    kv([
      ['Fingerprint', h('code', {}, d.fingerprint)],
      ['Display digest', h('code', {}, d.display_digest)],
      ['Server hints (untrusted)', `read_only=${d.read_only} destructive=${d.destructive} idempotent=${d.idempotent} open_world=${d.open_world}`],
      ['Observed by', d.observed_by],
    ]),
    h('h3', {}, 'Definition'), json(d.definition),
    h('h3', {}, 'Server-described display (untrusted)'), json(d.display)));
}
