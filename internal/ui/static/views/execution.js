// execution.js lists actions by state and shows one with its evidence.
// Resolving an unknown outcome stays in eacpctl (two-person, ADR-004).
import {h, section, notice, table, badge, link, fmtTime, kv, json, field, select, values} from '../dom.js';
import {format, isUUID} from '../router.js';
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
  }}, field('State', select('state', STATES, state)), h('button', {type: 'submit'}, 'Show'));
  return section('Actions', filter, res.ok ? table([
    ['Action', a => link(a.id.slice(0, 8), format('execution', [a.id]))],
    ['State', a => badge(a.state)],
    ['Tool', a => a.tool],
    ['Operation', a => a.operation],
    ['Target', a => a.target],
    ['Agent', a => link(a.agent_id.slice(0, 8), format('inventory', ['agents', a.agent_id]))],
    ['Changed', a => fmtTime(a.state_changed_at)],
    ['Attempts', a => String(a.attempt_count ?? 0)],
  ], res.data.actions, `No actions in ${state}.`) : notice(res));
}

async function detail({client}, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an action id'});
  const [a, ev] = await Promise.all([client.call('action.get', {params: {id}}),
    client.call('action.evidence', {params: {id}})]);
  if (!a.ok) return notice(a);
  const x = a.data;
  return h('div', {},
    back('← Execution', format('execution', [], {state: x.state})),
    h('h1', {}, `${x.tool} · ${x.operation}`),
    h('div', {class: 'chips'}, badge(x.state)),
    section('Action', kv([
      ['Id', h('code', {}, x.id)], ['State reason', x.state_reason ?? '—'],
      ['Agent', link(x.agent_id, format('inventory', ['agents', x.agent_id]))], ['Agent version', x.agent_version_id],
      ['Subject', x.subject], ['Target', x.target], ['Resource', x.resource], ['Idempotency key', x.idempotency_key],
      ['Input digest', h('code', {}, x.input_digest)],
      ['Enforced digest', x.enforced_digest ? h('code', {}, x.enforced_digest) : '—'],
      ['Policy version', x.policy_version ?? '—'], ['Approval request', x.approval_request_id ?? '—'],
      ['Created', fmtTime(x.created_at)], ['State changed', fmtTime(x.state_changed_at)],
      ['Not after', fmtTime(x.not_after)], ['Attempts', String(x.attempt_count ?? 0)],
      ['Next attempt', fmtTime(x.next_attempt_at)],
    ])),
    h('p', {}, link('Kill this action', format('security', [], {scope: 'action', target: x.id}))),
    section('Enforced payload', x.enforced_payload ? json(x.enforced_payload) : h('p', {class: 'empty'}, 'None.')),
    section('Evidence: attempts, reconciliation, resolutions', ev.ok ? json(ev.data) : notice(ev)),
    h('p', {class: 'hint'}, 'Resolving an unknown outcome stays in eacpctl (two-person).'));
}
