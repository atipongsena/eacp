// approvals.js lists the approvals waiting for the caller's vote and casts
// one. The vote binds to the enforced payload digest; the server checks
// eligibility, separation of duties and quorum (ADR-005).
import {h, section, notice, table, link, fmtTime, kv, json, button, ok, badge, replace} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {back} from './common.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list({client}) {
  const res = await client.call('approval.list');
  return section('Approvals waiting for your vote', res.ok ? table([
    ['Request', a => link(a.id.slice(0, 8), format('approvals', [a.id]))],
    ['Action', a => h('code', {}, a.action_id)],
    ['Enforced digest', a => h('code', {}, `${a.enforced_digest.slice(0, 16)}…`)],
    ['Quorum', a => String(a.required_quorum)],
    ['Policy', a => `v${a.policy_version}`],
    ['Expires', a => fmtTime(a.expires_at)],
  ], res.data.items, 'Nothing waits for your vote.') : notice(res));
}

async function detail(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: 'not an approval id'});
  const res = await ctx.client.call('approval.get', {params: {id}});
  if (!res.ok) return notice(res);
  const a = res.data;
  const result = h('div');
  return h('div', {},
    back('← Approvals', '#/approvals'),
    h('h1', {}, 'Approval request'),
    h('div', {class: 'chips'}, badge(a.state)),
    section('Request', kv([
      ['Id', h('code', {}, a.id)], ['Action', h('code', {}, a.action_id)],
      ['Enforced digest', h('code', {}, a.enforced_digest)], ['Policy version', String(a.policy_version)],
      ['Quorum', String(a.required_quorum)], ['Expires', fmtTime(a.expires_at)],
    ])),
    section('Enforced payload (what executes if approved)', json(a.enforced_payload)),
    a.state === 'PENDING' ? h('div', {class: 'actions'},
      button('Approve…', () => vote(ctx, a, 'APPROVE', result), {kind: 'primary'}),
      button('Deny…', () => vote(ctx, a, 'DENY', result), {kind: 'danger'})) : null,
    result);
}

async function vote(ctx, a, decision, result) {
  const approve = decision === 'APPROVE';
  const c = await ask({title: approve ? 'Approve this action' : 'Deny this action', danger: !approve,
    reason: 'required', confirmLabel: approve ? 'Approve' : 'Deny',
    lines: [`Action ${a.action_id}`, `Enforced digest ${a.enforced_digest}`,
      'Your vote binds to this exact payload digest. The server checks eligibility and separation of duties.']});
  if (!c) return;
  const r = await ctx.client.call('approval.vote', {params: {id: a.id}, body: {decision, reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(`Vote recorded; the request is ${r.data.request_state}.`), button('Refresh', ctx.refresh));
}
