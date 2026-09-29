// incidents.js lists incidents and shows one (§56): its detail, the blast
// radius recorded at opening, the recommended containment as links, and the
// journaled timeline. Operators and admins acknowledge, assign, note, link
// and resolve; PostgreSQL enforces the lifecycle and the two-person rule.
import {h, section, notice, table, badge, link, fmtTime, relTime, kv, field, input, select, values, button, replace, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {back, severity} from './common.js';

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
  const [id] = ctx.route.parts;
  if (id === 'new') return h('div', {}, back(t('← Incidents'), '#/incidents'), openForm(ctx));
  return id ? detail(ctx, id) : list(ctx);
}

async function list(ctx) {
  const {client, session, route, go} = ctx;
  const q = route.query;
  const res = await client.call('incident.list', {query: {state: q.state, severity: q.severity, kind: q.kind, limit: 200}});
  const filters = h('form', {class: 'filters', onsubmit: e => {
    e.preventDefault();
    go(format('incidents', [], values(e.target)));
  }},
  field(t('State'), select('state', ['', ...STATES], q.state ?? '')),
  field(t('Severity'), select('severity', ['', ...SEVERITIES], q.severity ?? '')),
  field(t('Kind'), select('kind', ['', ...KINDS], q.kind ?? '')),
  h('button', {type: 'submit'}, t('Filter')));
  return h('div', {},
    section(t('Incidents'), filters, res.ok ? table([
      [t('Severity'), i => severity(i.severity)],
      [t('State'), i => badge(i.state)],
      [t('Title'), i => link(i.title, format('incidents', [i.id]))],
      [t('Kind'), i => i.kind],
      [t('Opened'), i => relTime(i.opened_at, Date.now(), {node: true})],
      [t('Assignee'), i => i.assignee_id ?? '—'],
    ], res.data.incidents, emptyState(t('No incidents match'), t('Change the filters to look at other incidents.'), 'ok')) : notice(res)),
    // The list is polled, so the manual-incident form lives on its own page:
    // a poll never discards what the operator typed or the server's answer.
    session.hasAny(WORKERS) ? h('p', {}, link(t('Open a manual incident…'), '#/incidents/new')) : null);
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
        replace(out, notice({status: 400, error: 'invalid', detail: t('the subject id must be a UUID')}));
        return;
      }
      body.subject_id = v.subject_id;
    }
    const c = await ask({title: t('Open a manual incident'), lines: [`${v.severity}: ${v.title}`], reason: 'required'});
    if (!c) return;
    const r = await client.call('incident.open', {body: {...body, reason: c.reason}});
    if (r.ok) go(format('incidents', [r.data.id]));
    else replace(out, notice(r));
  }},
  field(t('Title'), input('title', {required: true, maxlength: 200, size: 40})),
  field(t('Severity'), select('severity', SEVERITIES, 'medium')),
  field(t('Subject type'), select('subject_type', ['', ...SUBJECT_TYPES])),
  field(t('Subject id'), input('subject_id', {placeholder: t('UUID (optional)'), size: 38})),
  h('button', {type: 'submit', class: 'primary'}, t('Open incident…')));
  return section(t('Open a manual incident'), form, out);
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
      recs.push([t('Pause the agents that use this tool'),
        format('fleet', [], {op: 'pause', tool: `${d.connector}.${d.tool}`, incident})]);
    }
  } else if (agents.length > MAX_PREFILLED_AGENTS) {
    recs.push([t('Pause the affected agents (choose them in Fleet)'), format('fleet', [], {op: 'pause', incident})]);
  } else if (agents.length > 0) {
    recs.push([t('Pause the affected agents'), format('fleet', [], {op: 'pause', agents: agents.join(','), incident})]);
  }
  switch (i.kind) {
    case 'mcp_drift':
      if (isUUID(i.subject_id)) {
        recs.push([t('Kill the tool'), format('security', [], {scope: 'tool', target: i.subject_id, incident})],
          [t('Review the tool definitions'), format('inventory', ['tools', i.subject_id])]);
      }
      break;
    case 'kill':
      if (d.scope && isUUID(d.target_id)) {
        recs.push([t('Review the kill state'), format('security', [], {scope: d.scope, target: d.target_id, incident})]);
      }
      break;
    case 'circuit_open':
      if (isUUID(i.subject_id)) {
        recs.push([t('Review or disable the connector circuit'), format('security', ['connectors', i.subject_id], {incident})]);
      }
      break;
    case 'unknown_outcome':
      if (isUUID(i.subject_id)) recs.push([t('Review the action and its evidence'), format('execution', [i.subject_id])]);
      break;
    case 'canary_rollback':
      if (d.agent) recs.push([t('Review the agent and its versions'), format('inventory', ['agents', d.agent])]);
      break;
    case 'finops':
      recs.push([t('Review spend and alerts'), '#/cost']);
      break;
    default:
  }
  return recs;
}

async function detail(ctx, id) {
  const {client, session} = ctx;
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not an incident id')});
  const res = await client.call('incident.get', {params: {id}});
  if (!res.ok) return notice(res);
  const i = res.data;
  const out = h('div');
  // act runs one write at a time; a confirm dialog comes first when opts is
  // given. Notes and links are insert-only journal rows, so a double click
  // must never send twice.
  let sending = false;
  const act = async (opts, send) => {
    if (sending) return;
    sending = true;
    try {
      let reason = '';
      if (opts) {
        const c = await ask(opts);
        if (!c) return;
        reason = c.reason;
      }
      const r = await send(reason);
      if (r.ok) ctx.refresh();
      else replace(out, notice(r));
    } catch (err) {
      replace(out, notice({status: 0, error: 'error', detail: String(err?.message ?? err)}));
    } finally {
      sending = false;
    }
  };
  const worker = session.hasAny(WORKERS);
  const recs = recommendations(i);
  return h('div', {},
    back(t('← Incidents'), '#/incidents'),
    h('h1', {}, i.title),
    h('div', {class: 'chips'}, severity(i.severity), badge(i.state), h('span', {class: 'chip'}, i.kind)),
    out,
    worker && i.state !== 'RESOLVED' ? lifecycle(ctx, i, act) : null,
    section(t('Summary'), summary(i)),
    section(t('Detail'), kv(Object.entries(i.detail ?? {}).map(([k, v]) =>
      [k, v !== null && typeof v === 'object' ? JSON.stringify(v) : String(v)]))),
    section(t('Affected at opening (blast radius)'), affected(i.affected)),
    section(t('Recommended containment'),
      recs.length ? h('ul', {}, recs.map(([label, hash]) => h('li', {}, link(label, hash)))) : emptyState(t('None.')),
      h('p', {class: 'hint'}, t('These links only open a form. Nothing changes until you confirm it, and the operator decides (§57).'))),
    section(t('Timeline'), timeline(i.events ?? [])),
    worker ? section(t('Add to the timeline'), noteForm(id, client, act), linkForm(id, client, act, out)) : null);
}

function lifecycle({client, session}, i, act) {
  const me = session.me();
  const id = i.id;
  const parts = [];
  if (i.state === 'OPEN') {
    parts.push(button(t('Acknowledge…'), () => act({title: t('Acknowledge this incident'), lines: [i.title], reason: 'required'},
      reason => client.call('incident.ack', {params: {id}, body: {reason}})), {kind: 'primary'}));
  }
  if (i.assignee_id !== me.principalId) {
    parts.push(button(t('Assign to me'), () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: me.principalId}}))));
  }
  if (i.assignee_id) {
    parts.push(button(t('Unassign'), () => act(null,
      () => client.call('incident.assign', {params: {id}, body: {assignee_id: null}}))));
  }
  const assignee = input('assignee_id', {placeholder: t('operator or admin principal UUID'), size: 38});
  parts.push(h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    const target = assignee.value.trim();
    if (!isUUID(target)) return;
    act(null, () => client.call('incident.assign', {params: {id}, body: {assignee_id: target}}));
  }}, assignee, h('button', {type: 'submit'}, t('Assign'))));
  if (i.state === 'ACKNOWLEDGED') {
    const code = select('resolution', RESOLUTIONS, 'contained');
    const critical = i.severity === 'critical';
    parts.push(h('form', {class: 'stack', onsubmit: e => {
      e.preventDefault();
      act({title: t('Resolve this incident'), reason: 'required', confirmLabel: t('Resolve'),
        lines: [i.title, t('Resolution: {code}', {code: code.value}),
          critical ? t('A critical incident is resolved by someone other than its acknowledger.') : null]},
      reason => client.call('incident.resolve', {params: {id}, body: {resolution: code.value, reason}}));
    }}, field(t('Resolution'), code), h('button', {type: 'submit', class: 'primary'}, t('Resolve…'))));
    if (critical && i.acknowledged_by === me.principalId) {
      parts.push(h('p', {class: 'hint'}, t('You acknowledged this critical incident: a second operator must resolve it.')));
    }
  }
  return section(t('Work this incident'), h('div', {class: 'actions'}, parts));
}

function summary(i) {
  return kv([
    [t('Kind'), i.kind],
    [t('Source'), i.source_key ?? t('manual')],
    [t('Subject'), i.subject_type ? `${i.subject_type} ${i.subject_id ?? ''}` : '—'],
    [t('Opened'), t('{time} by {who}', {time: fmtTime(i.opened_at), who: i.opened_by ?? t('the incident evaluator')})],
    [t('Acknowledged'), i.acknowledged_at
      ? t('{time} by {who}: {reason}', {time: fmtTime(i.acknowledged_at), who: i.acknowledged_by, reason: i.ack_reason ?? ''}) : '—'],
    [t('Assignee'), i.assignee_id ?? '—'],
    [t('Resolved'), i.resolved_at
      ? t('{time} by {who} as {resolution}: {reason}', {time: fmtTime(i.resolved_at), who: i.resolved_by, resolution: i.resolution,
        reason: i.resolution_reason ?? ''}) : '—'],
    [t('Id'), h('code', {}, i.id)],
  ]);
}

function affected(a) {
  if (!a || !Array.isArray(a.confirmed)) return emptyState(t('No blast radius was recorded.'));
  const nodes = a.nodes ?? [];
  return h('div', {},
    kv([
      [t('Nodes'), nodes.length < (a.node_count ?? 0) ? t('{count} (first {shown} shown)', {count: a.node_count, shown: nodes.length}) : String(a.node_count ?? 0)],
      [t('Confirmed versions'), String(a.confirmed_count ?? a.confirmed.length)],
      [t('Possible versions'), String(a.possible_count ?? 0)],
      [t('Active in production'), String(a.production_active ?? 0)],
    ]),
    table([
      [t('Agent'), c => link(c.agent, format('inventory', ['agents', c.agent]))],
      [t('Version'), c => String(c.version)],
      [t('Environment'), c => c.environment],
      [t('State'), c => badge(c.state)],
    ], a.confirmed, t('No confirmed versions.')),
    nodes.length ? h('p', {class: 'hint'}, t('Starting from: {nodes}', {nodes: nodes.join(', ')})) : null,
    h('p', {class: 'hint'}, t('Unknown or stale evidence widens possible impact; it never proves an agent unaffected (ADR-015).')));
}

function timeline(events) {
  return table([
    ['#', e => String(e.seq)],
    [t('At'), e => fmtTime(e.at)],
    [t('Event'), e => e.kind],
    [t('By'), e => e.actor_id ?? t('system')],
    [t('Note or link'), e => e.note ?? (e.link_kind ? `${e.link_kind} ${e.link_id}` : '')],
  ], events, t('No events.'));
}

function noteForm(id, client, act) {
  const text = h('textarea', {name: 'text', rows: 3, maxlength: 4096, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!text.value.trim()) return;
    act(null, () => client.call('incident.note', {params: {id}, body: {text: text.value}}));
  }}, field(t('Note (journaled; never paste a secret)'), text), h('button', {type: 'submit'}, t('Add note')));
}

function linkForm(id, client, act, out) {
  const kind = select('kind', LINK_KINDS, 'fleet_operation');
  const target = input('id', {placeholder: 'UUID', size: 38, required: true});
  return h('form', {class: 'stack', onsubmit: e => {
    e.preventDefault();
    if (!isUUID(target.value.trim())) {
      replace(out, notice({status: 400, error: 'invalid', detail: t('the linked id must be a UUID')}));
      return;
    }
    act(null, () => client.call('incident.link', {params: {id}, body: {kind: kind.value, id: target.value.trim()}}));
  }}, field(t('Link kind'), kind), field(t('Id'), target), h('button', {type: 'submit'}, t('Add link')));
}
