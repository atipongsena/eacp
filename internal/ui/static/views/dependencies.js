// dependencies.js runs the ADR-015 blast-radius query. Observed evidence
// only: an undeclared dependency is never proven absent.
import {h, section, notice, table, link, kv, field, input, select, values} from '../dom.js';
import {format} from '../router.js';
import {t} from '../i18n.js';

const KINDS = ['tool', 'mcp', 'agent_version', 'model', 'system'];
const BY_NAME = new Set(['model', 'system']);

export async function render({client, route, go}) {
  const q = route.query;
  const form = h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const v = values(e.target);
    go(format('dependencies', [], {kind: v.kind, [BY_NAME.has(v.kind) ? 'name' : 'id']: v.target}));
  }},
  field(t('Kind'), select('kind', KINDS, KINDS.includes(q.kind) ? q.kind : 'tool')),
  field(t('Id (tool, MCP connector, agent version) or name (model, system)'),
    input('target', {value: q.id ?? q.name ?? '', required: true, size: 40})),
  h('button', {type: 'submit', class: 'primary'}, t('Show blast radius')));
  const parts = [section(t('Blast radius'), form,
    h('p', {class: 'hint'}, t('Observed evidence only: an undeclared dependency is never proven absent (ADR-015).')))];
  if (q.kind && (q.id || q.name)) {
    const res = await client.call('dependency.blast', {query: {kind: q.kind, id: q.id, name: q.name}});
    parts.push(res.ok ? report(res.data) : notice(res));
  }
  return h('div', {}, parts);
}

function impacts(list) {
  return table([
    [t('Agent'), i => link(i.agent_name, format('inventory', ['agents', i.agent_name]))],
    [t('Version'), i => String(i.version)],
    [t('Environment'), i => i.environment],
    [t('Team'), i => i.team || '—'],
    [t('Owner'), i => i.owner_principal ?? '—'],
  ], list, t('None.'));
}

function report(r) {
  return h('div', {},
    section(t('Summary'), kv([
      [t('Target'), `${r.target.kind} ${r.target.name || r.target.id || ''}`],
      [t('Coverage'), r.coverage],
      [t('Affected teams'), r.affected_teams.join(', ') || '—'],
      [t('Data classes'), r.data_classes.join(', ') || '—'],
      [t('Actions in the last 24 hours'), String(r.recent_actions_24h)],
    ])),
    section(t('Confirmed ({n})', {n: r.confirmed_agents.length}), impacts(r.confirmed_agents)),
    section(t('Possible ({n})', {n: r.possible_agents.length}), impacts(r.possible_agents)));
}
