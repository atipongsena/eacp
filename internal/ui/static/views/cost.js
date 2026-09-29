// cost.js shows the FinOps dashboard and open alerts. FinOps observes and
// never blocks (ADR-025); acknowledging alerts stays in eacpctl.
import {h, section, notice, table, badge, fmtTime, relTime, kv, emptyState} from '../dom.js';
import {t} from '../i18n.js';
import {mapText} from './common.js';

export async function render({client}) {
  const [d, alerts] = await Promise.all([client.call('finops.dashboard'),
    client.call('finops.alerts', {query: {open: 'true', limit: 100}})]);
  if (!d.ok) return notice(d);
  const x = d.data;
  return h('div', {},
    h('p', {class: 'asof'}, t('As of {time}; days and months are UTC.', {time: fmtTime(x.as_of)})),
    section(t('Spend by unit'), table([
      [t('Unit'), u => u.unit],
      [t('Today'), u => String(u.today)],
      [t('Month to date'), u => String(u.month_to_date)],
      [t('Top agents'), u => (u.top_agents ?? []).map(a => `${a.name} ${a.total}`).join(' · ') || '—'],
    ], x.units, emptyState(t('No spend recorded'), t('Spend appears here once agents report usage.'), 'cost'))),
    section(t('Today'), kv([
      [t('Hard budget blocks'), mapText(x.hard_blocks_today)],
      [t('Open alerts by kind'), mapText(x.open_alerts)],
      [t('Unpriced tokens'), String(x.unpriced_tokens_today)],
    ])),
    section(t('Open alerts'), alerts.ok ? table([
      [t('Kind'), a => badge(a.kind)],
      [t('Subject'), a => `${a.subject_type} ${a.subject_id}`],
      [t('Unit'), a => a.unit ?? '—'],
      [t('Observed'), a => a.observed ?? '—'],
      [t('Threshold'), a => a.threshold ?? '—'],
      [t('Period'), a => fmtTime(a.period_start)],
      [t('Raised'), a => relTime(a.created_at, Date.now(), {node: true})],
    ], alerts.data.alerts, emptyState(t('No open alerts'), t('Alerts appear here when spend crosses a soft limit.'), 'ok')) : notice(alerts)),
    h('p', {class: 'hint'}, t('Acknowledging alerts stays in eacpctl.')));
}
