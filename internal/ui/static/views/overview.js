// overview.js shows the §55 counters from GET /v1/soc/summary and the open
// incidents.
import {h, section, notice, table, badge, link, fmtTime, stat} from '../dom.js';
import {format} from '../router.js';

export async function render({client}) {
  const [sum, open] = await Promise.all([
    client.call('soc.summary'),
    client.call('incident.list', {query: {state: 'OPEN', limit: 20}}),
  ]);
  if (!sum.ok) return notice(sum);
  const s = sum.data;
  const sev = s.security.open_incidents ?? {};
  const spend = (s.finops.spend_today ?? []).map(u => `${u.amount} ${u.unit}`).join(' · ') || '0';
  const incidents = sev => format('incidents', [], {severity: sev});
  return h('div', {},
    h('p', {class: 'asof'}, `As of ${fmtTime(s.as_of)}; refreshes every 15 seconds.`),
    h('div', {class: 'grid'},
      section('Agents',
        stat('Registered', s.agents.registered, '#/inventory'),
        stat('Production', s.agents.production),
        stat('High risk', s.agents.high_risk),
        stat('Owner disabled', s.agents.owner_disabled),
        stat('Active versions', s.agents.versions.active),
        stat('Suspended versions', s.agents.versions.suspended),
        stat('Quarantined versions', s.agents.versions.quarantined, '#/fleet')),
      section('Security',
        stat('Critical incidents', sev.critical ?? 0, incidents('critical')),
        stat('High incidents', sev.high ?? 0, incidents('high')),
        stat('Medium incidents', sev.medium ?? 0, incidents('medium')),
        stat('Low incidents', sev.low ?? 0, incidents('low')),
        stat('Unacknowledged', s.security.unacknowledged, format('incidents', [], {state: 'OPEN'})),
        stat('Quarantined tools', s.security.quarantined_tools, '#/security'),
        stat('Active kills', s.security.active_kills, '#/security'),
        stat('Open circuits', s.security.open_circuits, '#/security'),
        stat('Disabled circuits', s.security.disabled_circuits, '#/security'),
        stat('Pending approvals', s.security.pending_approvals, '#/approvals')),
      section('Execution',
        stat('Queued', s.execution.queued, format('execution', [], {state: 'QUEUED'})),
        stat('Running', s.execution.running, format('execution', [], {state: 'EXECUTING'})),
        stat('Retry wait', s.execution.retry_wait, format('execution', [], {state: 'RETRY_WAIT'})),
        stat('Unknown outcome', s.execution.unknown_outcome, format('execution', [], {state: 'UNKNOWN_OUTCOME'})),
        stat('Needs a human', s.execution.needs_human, format('execution', [], {state: 'NEEDS_HUMAN_RESOLUTION'}))),
      section('FinOps',
        stat('Spend today', spend, '#/cost'),
        stat('Open alerts', s.finops.open_alerts, '#/cost'))),
    section('Open incidents', open.ok ? table([
      ['Severity', i => badge(i.severity)],
      ['Title', i => link(i.title, format('incidents', [i.id]))],
      ['Kind', i => i.kind],
      ['Opened', i => fmtTime(i.opened_at)],
    ], open.data.incidents, 'No open incidents.') : notice(open)));
}
