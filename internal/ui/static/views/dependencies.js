// dependencies.js runs the ADR-015 blast-radius query. Observed evidence
// only: an undeclared dependency is never proven absent.
import {h, section, notice, table, link, kv, field, input, select, values} from '../dom.js';
import {format} from '../router.js';

const KINDS = ['tool', 'mcp', 'agent_version', 'model', 'system'];
const BY_NAME = new Set(['model', 'system']);

export async function render({client, route, go}) {
  const q = route.query;
  const form = h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const v = values(e.target);
    go(format('dependencies', [], {kind: v.kind, [BY_NAME.has(v.kind) ? 'name' : 'id']: v.target}));
  }},
  field('Kind', select('kind', KINDS, KINDS.includes(q.kind) ? q.kind : 'tool')),
  field('Id (tool, MCP connector, agent version) or name (model, system)',
    input('target', {value: q.id ?? q.name ?? '', required: true, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, 'Show blast radius'));
  const parts = [section('Blast radius', form,
    h('p', {class: 'hint'}, 'Observed evidence only: an undeclared dependency is never proven absent (ADR-015).'))];
  if (q.kind && (q.id || q.name)) {
    const res = await client.call('dependency.blast', {query: {kind: q.kind, id: q.id, name: q.name}});
    parts.push(res.ok ? report(res.data) : notice(res));
  }
  return h('div', {}, parts);
}

function impacts(list) {
  return table([
    ['Agent', i => link(i.agent_name, format('inventory', ['agents', i.agent_name]))],
    ['Version', i => String(i.version)],
    ['Environment', i => i.environment],
    ['Team', i => i.team || '—'],
    ['Owner', i => i.owner_principal ?? '—'],
  ], list, 'None.');
}

function report(r) {
  return h('div', {},
    section('Summary', kv([
      ['Target', `${r.target.kind} ${r.target.name || r.target.id || ''}`],
      ['Coverage', r.coverage],
      ['Affected teams', r.affected_teams.join(', ') || '—'],
      ['Data classes', r.data_classes.join(', ') || '—'],
      ['Actions in the last 24 hours', String(r.recent_actions_24h)],
    ])),
    section(`Confirmed (${r.confirmed_agents.length})`, impacts(r.confirmed_agents)),
    section(`Possible (${r.possible_agents.length})`, impacts(r.possible_agents)));
}
