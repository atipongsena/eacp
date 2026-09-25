// cost.js shows the FinOps dashboard and open alerts. FinOps observes and
// never blocks (ADR-025); acknowledging alerts stays in eacpctl.
import {h, section, notice, table, badge, fmtTime, kv} from '../dom.js';
import {mapText} from './common.js';

export async function render({client}) {
  const [d, alerts] = await Promise.all([client.call('finops.dashboard'),
    client.call('finops.alerts', {query: {open: 'true', limit: 100}})]);
  if (!d.ok) return notice(d);
  const x = d.data;
  return h('div', {},
    h('p', {class: 'asof'}, `As of ${fmtTime(x.as_of)}; days and months are UTC.`),
    section('Spend by unit', table([
      ['Unit', u => u.unit],
      ['Today', u => String(u.today)],
      ['Month to date', u => String(u.month_to_date)],
      ['Top agents', u => (u.top_agents ?? []).map(a => `${a.name} ${a.total}`).join(' · ') || '—'],
    ], x.units, 'No spend recorded.')),
    section('Today', kv([
      ['Hard budget blocks', mapText(x.hard_blocks_today)],
      ['Open alerts by kind', mapText(x.open_alerts)],
      ['Unpriced tokens', String(x.unpriced_tokens_today)],
    ])),
    section('Open alerts', alerts.ok ? table([
      ['Kind', a => badge(a.kind)],
      ['Subject', a => `${a.subject_type} ${a.subject_id}`],
      ['Unit', a => a.unit ?? '—'],
      ['Observed', a => a.observed ?? '—'],
      ['Threshold', a => a.threshold ?? '—'],
      ['Period', a => fmtTime(a.period_start)],
      ['Raised', a => fmtTime(a.created_at)],
    ], alerts.data.alerts, 'No open alerts.') : notice(alerts)),
    h('p', {class: 'hint'}, 'Acknowledging alerts stays in eacpctl.'));
}
