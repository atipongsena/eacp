// execution.js lists actions by state and shows one with its evidence.
// Resolving an unknown outcome stays in eacpctl (two-person, ADR-004).
import {h, section, notice, table, badge, link, fmtTime, relTime, kv, json, field, select, values, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {t} from '../i18n.js';
import {back} from './common.js';

const STATES = ['NEEDS_HUMAN_RESOLUTION', 'UNKNOWN_OUTCOME', 'RECONCILING', 'RETRY_WAIT', 'QUEUED', 'LEASED',
  'EXECUTING', 'PENDING_APPROVAL', 'AUTHORIZED', 'RECEIVED', 'SUCCEEDED', 'FAILED', 'DENIED', 'CANCELLED', 'EXPIRED'];

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list({client, route, go}) {
  const state = STATES.includes(route.query.state) ? route.query.state : STATES[0];
  const res = await client.call('action.list', {query: {state, limit: 200}});
  const filter = h('form', {class: 'filters', onsubmit: e => {
    e.preventDefault();
    go(format('execution', [], values(e.target)));
  }}, field(t('State'), select('state', STATES, state)), h('button', {type: 'submit'}, t('Show')));
  return section(t('Actions'), filter, res.ok ? table([
    [t('Action'), a => link(a.id.slice(0, 8), format('execution', [a.id]))],
    [t('State'), a => badge(a.state)],
    [t('Tool'), a => a.tool],
    [t('Operation'), a => a.operation],
    [t('Target'), a => a.target],
    [t('Agent'), a => link(a.agent_id.slice(0, 8), format('inventory', ['agents', a.agent_id]))],
    [t('Changed'), a => relTime(a.state_changed_at, Date.now(), {node: true})],
    [t('Attempts'), a => String(a.attempt_count ?? 0)],
  ], res.data.actions, emptyState(t('No actions in {state}', {state}), t('Choose another state to look elsewhere.'), 'execution')) : notice(res));
}

async function detail({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not an action id')});
  const [a, ev] = await Promise.all([client.call('action.get', {params: {id}}),
    client.call('action.evidence', {params: {id}})]);
  if (!a.ok) return notice(a);
  const x = a.data;
  return h('div', {},
    back(t('← Execution'), format('execution', [], {state: x.state})),
    h('h1', {}, `${x.tool} · ${x.operation}`),
    h('div', {class: 'chips'}, badge(x.state)),
    section(t('Action'), kv([
      [t('Id'), h('code', {}, x.id)], [t('State reason'), x.state_reason ?? '—'],
      [t('Agent'), link(x.agent_id, format('inventory', ['agents', x.agent_id]))], [t('Agent version'), x.agent_version_id],
      [t('Subject'), x.subject], [t('Target'), x.target], [t('Resource'), x.resource], [t('Idempotency key'), x.idempotency_key],
      [t('Input digest'), h('code', {}, x.input_digest)],
      [t('Enforced digest'), x.enforced_digest ? h('code', {}, x.enforced_digest) : '—'],
      [t('Policy version'), x.policy_version ?? '—'], [t('Approval request'), x.approval_request_id ?? '—'],
      [t('Created'), fmtTime(x.created_at)], [t('State changed'), fmtTime(x.state_changed_at)],
      [t('Not after'), fmtTime(x.not_after)], [t('Attempts'), String(x.attempt_count ?? 0)],
      [t('Next attempt'), fmtTime(x.next_attempt_at)],
    ])),
    h('p', {}, link(t('Kill this action'), format('security', [], {scope: 'action', target: x.id}))),
    section(t('Enforced payload'), x.enforced_payload ? json(x.enforced_payload) : emptyState(t('None.'))),
    section(t('Evidence: attempts, reconciliation, resolutions'), ev.ok ? json(ev.data) : notice(ev)),
    h('p', {class: 'hint'}, t('Resolving an unknown outcome stays in eacpctl (two-person).')));
}
