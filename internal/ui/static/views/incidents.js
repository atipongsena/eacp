// incidents.js lists incidents and shows one (§56): its detail, the blast
// radius recorded at opening, the recommended containment as links, and the
// journaled timeline. Operators and admins acknowledge, assign, note, link
// and resolve; PostgreSQL enforces the lifecycle and the two-person rule.
import {h, section, notice, table, badge, link, fmtTime, kv, field, input, select, values, button, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back} from './common.js';

const SEVERITIES = ['low', 'medium', 'high', 'critical'];
const STATES = ['OPEN', 'ACKNOWLEDGED', 'RESOLVED'];
const KINDS = ['mcp_drift', 'kill', 'circuit_open', 'unknown_outcome', 'canary_rollback', 'finops', 'manual'];
const SUBJECT_TYPES = ['tool', 'kill_state', 'connector', 'action', 'release', 'finops_alert', 'agent', 'agent_version'];
const LINK_KINDS = ['fleet_operation', 'kill_state', 'tool', 'connector', 'action', 'agent', 'agent_version', 'release',
  'change_set', 'finops_alert'];
const RESOLUTIONS = ['contained', 'false_positive', 'accepted_risk', 'duplicate'];
const WORKERS = ['operator', 'admin'];
const MAX_PREFILLED_AGENTS = 20;

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list(ctx) {
  const {client, session, route, go} = ctx;
  const q = route.query;
  const res = await client.call('incident.list', {query: {state: q.state, severity: q.severity, kind: q.kind, limit: 200}});
  const filters = h('form', {class: 'filters', onsubmit: e => {
    e.preventDefault();
    go(format('incidents', [], values(e.target)));
  }},
  field('State', select('state', ['', ...STATES], q.state ?? '')),
  field('Severity', select('severity', ['', ...SEVERITIES], q.severity ?? '')),
  field('Kind', select('kind', ['', ...KINDS], q.kind ?? '')),
  h('button', {type: 'submit'}, 'Filter'));
  return h('div', {},
    section('Incidents', filters, res.ok ? table([
      ['Severity', i => badge(i.severity)],
      ['State', i => badge(i.state)],
      ['Title', i => link(i.title, format('incidents', [i.id]))],
      ['Kind', i => i.kind],
      ['Opened', i => fmtTime(i.opened_at)],
      ['Assignee', i => i.assignee_id ?? '—'],
    ], res.data.incidents, 'No incidents match.') : notice(res)),
    session.hasAny(WORKERS) ? openForm(ctx) : null);
}

function openForm({client, go}) {
  const out = h('div');
  const form = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = values(form);
    const body = {title: v.title, severity: v.severity};
    if (v.subject_type) body.subject_type = v.subject_type;
    if (v.subject_id) {
      if (!isUUID(v.subject_id)) {
        replace(out, notice({status: 400, error: 'invalid', detail: 'the subject id must be a UUID'}));
        return;
      }
      body.subject_id = v.subject_id;
    }
    const c = await ask({title: 'Open a manual incident', lines: [`${v.severity}: ${v.title}`], reason: 'required'});
    if (!c) return;
    const r = await client.call('incident.open', {body: {...body, reason: c.reason}});
    if (r.ok) go(format('incidents', [r.data.id]));
    else replace(out, notice(r));
  }},
  field('Title', input('title', {required: true, maxlength: 200, size: 40})),
  field('Severity', select('severity', SEVERITIES, 'medium')),
  field('Subject type', select('subject_type', ['', ...SUBJECT_TYPES])),
  field('Subject id', input('subject_id', {placeholder: 'UUID (optional)', size: 38})),
  h('button', {type: 'submit', class: 'primary'}, 'Open incident…'));
  return section('Open a manual incident', form, out);
}

// recommendations turns an incident into containment links. They only
// navigate and pre-fill a form; nothing changes until the operator confirms.
export function recommendations(i) {
  const d = i.detail ?? {};
  const agents = [...new Set((i.affected?.confirmed ?? []).map(c => c.agent).filter(Boolean))];
  const recs = [];
  const incident = i.id;
  if (i.kind === 'mcp_drift') {
    if (d.connector && d.tool) {
      recs.push(['Pause the agents that use this tool',
        format('fleet', [], {op: 'pause', tool: `${d.connector}.${d.tool}`, incident})]);
    }
  } else if (agents.length > MAX_PREFILLED_AGENTS) {
    recs.push(['Pause the affected agents (choose them in Fleet)', format('fleet', [], {op: 'pause', incident})]);
  } else if (agents.length > 0) {
    recs.push(['Pause the affected agents', format('fleet', [], {op: 'pause', agents: agents.join(','), incident})]);
  }
  switch (i.kind) {
    case 'mcp_drift':
      if (isUUID(i.subject_id)) {
        recs.push(['Kill the tool', format('security', [], {scope: 'tool', target: i.subject_id, incident})],
          ['Review the tool definitions', format('inventory', ['tools', i.subject_id])]);
      }
      break;
    case 'kill':
      if (d.scope && isUUID(d.target_id)) {
        recs.push(['Review the kill state', format('security', [], {scope: d.scope, target: d.target_id, incident})]);
      }
      break;
    case 'circuit_open':
      if (isUUID(i.subject_id)) {
        recs.push(['Review or disable the connector circuit', format('security', ['connectors', i.subject_id], {incident})]);
      }
      break;
    case 'unknown_outcome':
      if (isUUID(i.subject_id)) recs.push(['Review the action and its evidence', format('execution', [i.subject_id])]);
      break;
    case 'canary_rollback':
      if (d.agent) recs.push(['Review the agent and its versions', format('inventory', ['agents', d.agent])]);
      break;
    case 'finops':
      recs.push(['Review spend and alerts', '#/cost']);
      break;
    default:
  }
  return recs;
}

async function detail(ctx, id) {
  const {client, session} = ctx;
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an incident id'});
  const res = await client.call('incident.get', {params: {id}});
  if (!res.ok) return notice(res);
  const i = res.data;
  const out = h('div');
  // act runs one write; a confirm dialog comes first when opts is given.
  const act = async (opts, send) => {
    let reason = '';
    if (opts) {
      const c = await ask(opts);
      if (!c) return;
      reason = c.reason;
    }
    const r = await send(reason);
    if (r.ok) ctx.refresh();
    else replace(out, notice(r));
  };
  const worker = session.hasAny(WORKERS);
  const recs = recommendations(i);
  return h('div', {},
    back('← Incidents', '#/incidents'),
    h('h1', {}, i.title),
    h('div', {class: 'chips'}, badge(i.severity), badge(i.state), h('span', {class: 'chip'}, i.kind)),
    out,
    worker && i.state !== 'RESOLVED' ? lifecycle(ctx, i, act) : null,
    section('Summary', summary(i)),
    section('Detail', kv(Object.entries(i.detail ?? {}).map(([k, v]) =>
      [k, v !== null && typeof v === 'object' ? JSON.stringify(v) : String(v)]))),
    section('Affected at opening (blast radius)', affected(i.affected)),
    section('Recommended containment',
      recs.length ? h('ul', {}, recs.map(([label, hash]) => h('li', {}, link(label, hash)))) : h('p', {class: 'empty'}, 'None.'),
      h('p', {class: 'hint'}, 'These links only open a form. Nothing changes until you confirm it, and the operator decides (§57).')),
    section('Timeline', timeline(i.events ?? [])),
    worker ? section('Add to the timeline', noteForm(id, client, act), linkForm(id, client, act, out)) : null);
}

function lifecycle({client, session}, i, act) {
  const me = session.me();
  const id = i.id;
  const parts = [];
  if (i.state === 'OPEN') {
    parts.push(button('Acknowledge…', () => act({title: 'Acknowledge this incident', lines: [i.title], reason: 'required'},
      reason => client.call('incident.ack', {params: {id}, body: {reason}})), {kind: 'primary'}));
  }
  if (i.assignee_id !== me.principalId) {
    parts.push(button('Assign to me', () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: me.principalId}}))));
  }
  if (i.assignee_id) {
    parts.push(button('Unassign', () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: null}}))));
  }
  const assignee = input('assignee_id', {placeholder: 'operator or admin principal UUID', size: 38});
  parts.push(h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const target = assignee.value.trim();
    if (!isUUID(target)) return;
    act(null, () => client.call('incident.assign', {params: {id}, body: {assignee_id: target}}));
  }}, assignee, h('button', {type: 'submit'}, 'Assign')));
  if (i.state === 'ACKNOWLEDGED') {
    const code = select('resolution', RESOLUTIONS, 'contained');
    const critical = i.severity === 'critical';
    parts.push(h('form', {class: 'stack', onsubmit: e => {
      e.preventDefault();
      act({title: 'Resolve this incident', reason: 'required', confirmLabel: 'Resolve',
        lines: [i.title, `Resolution: ${code.value}`,
          critical ? 'A critical incident is resolved by someone other than its acknowledger.' : null]},
      reason => client.call('incident.resolve', {params: {id}, body: {resolution: code.value, reason}}));
    }}, field('Resolution', code), h('button', {type: 'submit', class: 'primary'}, 'Resolve…')));
    if (critical && i.acknowledged_by === me.principalId) {
      parts.push(h('p', {class: 'hint'}, 'You acknowledged this critical incident: a second operator must resolve it.'));
    }
  }
  return section('Work this incident', h('div', {class: 'actions'}, parts));
}

function summary(i) {
  return kv([
    ['Kind', i.kind],
    ['Source', i.source_key ?? 'manual'],
    ['Subject', i.subject_type ? `${i.subject_type} ${i.subject_id ?? ''}` : '—'],
    ['Opened', `${fmtTime(i.opened_at)} by ${i.opened_by ?? 'the incident evaluator'}`],
    ['Acknowledged', i.acknowledged_at ? `${fmtTime(i.acknowledged_at)} by ${i.acknowledged_by}: ${i.ack_reason ?? ''}` : '—'],
    ['Assignee', i.assignee_id ?? '—'],
    ['Resolved', i.resolved_at
      ? `${fmtTime(i.resolved_at)} by ${i.resolved_by} as ${i.resolution}: ${i.resolution_reason ?? ''}` : '—'],
    ['Id', h('code', {}, i.id)],
  ]);
}

function affected(a) {
  if (!a || !Array.isArray(a.confirmed)) return h('p', {class: 'empty'}, 'No blast radius was recorded.');
  const nodes = a.nodes ?? [];
  return h('div', {},
    kv([
      ['Nodes', nodes.length < (a.node_count ?? 0) ? `${a.node_count} (first ${nodes.length} shown)` : String(a.node_count ?? 0)],
      ['Confirmed versions', String(a.confirmed_count ?? a.confirmed.length)],
      ['Possible versions', String(a.possible_count ?? 0)],
      ['Active in production', String(a.production_active ?? 0)],
    ]),
    table([
      ['Agent', c => link(c.agent, format('inventory', ['agents', c.agent]))],
      ['Version', c => String(c.version)],
      ['Environment', c => c.environment],
      ['State', c => badge(c.state)],
    ], a.confirmed, 'No confirmed versions.'),
    nodes.length ? h('p', {class: 'hint'}, `Starting from: ${nodes.join(', ')}`) : null,
    h('p', {class: 'hint'}, 'Unknown or stale evidence widens possible impact; it never proves an agent unaffected (ADR-015).'));
}

function timeline(events) {
  return table([
    ['#', e => String(e.seq)],
    ['At', e => fmtTime(e.at)],
    ['Event', e => e.kind],
    ['By', e => e.actor_id ?? 'system'],
    ['Note or link', e => e.note ?? (e.link_kind ? `${e.link_kind} ${e.link_id}` : '')],
  ], events, 'No events.');
}

function noteForm(id, client, act) {
  const text = h('textarea', {name: 'text', rows: 3, maxlength: 4096, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!text.value.trim()) return;
    act(null, () => client.call('incident.note', {params: {id}, body: {text: text.value}}));
  }}, field('Note (journaled; never paste a secret)', text), h('button', {type: 'submit'}, 'Add note'));
}

function linkForm(id, client, act, out) {
  const kind = select('kind', LINK_KINDS, 'fleet_operation');
  const target = input('id', {placeholder: 'UUID', size: 38, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!isUUID(target.value.trim())) {
      replace(out, notice({status: 400, error: 'invalid', detail: 'the linked id must be a UUID'}));
      return;
    }
    act(null, () => client.call('incident.link', {params: {id}, body: {kind: kind.value, id: target.value.trim()}}));
  }}, field('Link kind', kind), field('Id', target), h('button', {type: 'submit'}, 'Add link'));
}
