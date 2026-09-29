// overview.js shows the §55 counters from GET /v1/soc/summary and the open
// incidents: what needs attention first, then every counter.
import {h, section, notice, table, link, fmtTime, relTime, stat, statCard, banner, emptyState} from '../dom.js';
import {format} from '../router.js';
import {t} from '../i18n.js';
import {severity} from './common.js';

export async function render({client}) {
  const [sum, open] = await Promise.all([
    client.call('soc.summary'),
    client.call('incident.list', {query: {state: 'OPEN', limit: 20}}),
  ]);
  if (!sum.ok) return notice(sum);
  const s = sum.data;
  const sev = s.security.open_incidents ?? {};
  const critical = sev.critical ?? 0;
  const openTotal = Object.values(sev).reduce((a, n) => a + n, 0);
  const attention = s.execution.needs_human + s.execution.unknown_outcome;
  const spend = (s.finops.spend_today ?? []).map(u => `${u.amount} ${u.unit}`);
  const incidents = level => format('incidents', [], {severity: level});
  return h('div', {},
    h('p', {class: 'asof'}, t('As of {time}; refreshes every 15 seconds.', {time: fmtTime(s.as_of)})),
    critical > 0 ? banner(critical === 1 ? t('One critical incident needs attention') :
      t('{n} critical incidents need attention', {n: critical}), incidents('critical'), t('Review')) : null,
    s.security.active_kills > 0 ? banner(t('Active kill switches: {n}', {n: s.security.active_kills}), '#/security', t('View')) : null,
    h('div', {class: 'kpis'},
      statCard({label: t('Agents'), value: s.agents.registered, hash: '#/inventory', icon: 'fleet',
        sub: t('{active} active versions · {risk} high risk', {active: s.agents.versions.active, risk: s.agents.high_risk})}),
      statCard({label: t('Open incidents'), value: openTotal, hash: format('incidents', [], {state: 'OPEN'}), icon: 'incidents',
        tone: critical > 0 ? 'danger' : ((sev.high ?? 0) > 0 ? 'warning' : undefined),
        sub: t('{critical} critical · {high} high', {critical, high: sev.high ?? 0})}),
      statCard({label: t('Needs attention'), value: attention, icon: 'execution',
        hash: format('execution', [], {state: 'NEEDS_HUMAN_RESOLUTION'}), tone: attention > 0 ? 'warning' : undefined,
        sub: t('{queued} queued · {running} running', {queued: s.execution.queued, running: s.execution.running})}),
      statCard({label: t('Spend today'), value: spend[0] ?? '0', hash: '#/cost', icon: 'cost',
        sub: [...spend.slice(1), t('{n} open alerts', {n: s.finops.open_alerts})].join(' · ')})),
    h('div', {class: 'grid'},
      section(t('Agents'),
        stat(t('Registered'), s.agents.registered, '#/inventory'),
        stat(t('Production'), s.agents.production),
        stat(t('High risk'), s.agents.high_risk),
        stat(t('Owner disabled'), s.agents.owner_disabled),
        stat(t('Active versions'), s.agents.versions.active),
        stat(t('Suspended versions'), s.agents.versions.suspended),
        stat(t('Quarantined versions'), s.agents.versions.quarantined, '#/fleet')),
      section(t('Security'),
        stat(t('Critical incidents'), sev.critical ?? 0, incidents('critical')),
        stat(t('High incidents'), sev.high ?? 0, incidents('high')),
        stat(t('Medium incidents'), sev.medium ?? 0, incidents('medium')),
        stat(t('Low incidents'), sev.low ?? 0, incidents('low')),
        stat(t('Unacknowledged'), s.security.unacknowledged, format('incidents', [], {state: 'OPEN'})),
        stat(t('Quarantined tools'), s.security.quarantined_tools, '#/security'),
        stat(t('Active kills'), s.security.active_kills, '#/security'),
        stat(t('Open circuits'), s.security.open_circuits, '#/security'),
        stat(t('Disabled circuits'), s.security.disabled_circuits, '#/security'),
        stat(t('Pending approvals'), s.security.pending_approvals, '#/approvals')),
      section(t('Execution'),
        stat(t('Queued'), s.execution.queued, format('execution', [], {state: 'QUEUED'})),
        stat(t('Running'), s.execution.running, format('execution', [], {state: 'EXECUTING'})),
        stat(t('Retry wait'), s.execution.retry_wait, format('execution', [], {state: 'RETRY_WAIT'})),
        stat(t('Unknown outcome'), s.execution.unknown_outcome, format('execution', [], {state: 'UNKNOWN_OUTCOME'})),
        stat(t('Needs a human'), s.execution.needs_human, format('execution', [], {state: 'NEEDS_HUMAN_RESOLUTION'}))),
      section(t('FinOps'),
        stat(t('Spend today'), spend.join(' · ') || '0', '#/cost'),
        stat(t('Open alerts'), s.finops.open_alerts, '#/cost'))),
    section(t('Open incidents'), open.ok ? table([
      [t('Severity'), i => severity(i.severity)],
      [t('Title'), i => link(i.title, format('incidents', [i.id]))],
      [t('Kind'), i => i.kind],
      [t('Opened'), i => relTime(i.opened_at, Date.now(), {node: true})],
    ], open.data.incidents, emptyState(t('No open incidents'), t('Incidents appear here when a signal fires.'), 'ok')) : notice(open)));
}
