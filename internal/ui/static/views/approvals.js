// approvals.js lists the approvals waiting for the caller's vote and casts
// one. The vote binds to the enforced payload digest; the server checks
// eligibility, separation of duties and quorum (ADR-005).
import {h, section, notice, table, link, fmtTime, kv, json, button, ok, badge, replace, emptyState} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {back} from './common.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list({client}) {
  const res = await client.call('approval.list');
  return section(t('Approvals waiting for your vote'), res.ok ? table([
    [t('Request'), a => link(a.id.slice(0, 8), format('approvals', [a.id]))],
    [t('Action'), a => h('code', {}, a.action_id)],
    [t('Enforced digest'), a => h('code', {}, `${a.enforced_digest.slice(0, 16)}…`)],
    [t('Quorum'), a => String(a.required_quorum)],
    [t('Policy'), a => `v${a.policy_version}`],
    [t('Expires'), a => fmtTime(a.expires_at)],
  ], res.data.items, emptyState(t('Nothing waits for your vote.'),
    t('Requests that need your decision appear here.'), 'approvals')) : notice(res));
}

async function detail(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not an approval id')});
  const res = await ctx.client.call('approval.get', {params: {id}});
  if (!res.ok) return notice(res);
  const a = res.data;
  const result = h('div');
  return h('div', {},
    back(t('← Approvals'), '#/approvals'),
    h('h1', {}, t('Approval request')),
    h('div', {class: 'chips'}, badge(a.state)),
    section(t('Request'), kv([
      [t('Id'), h('code', {}, a.id)], [t('Action'), h('code', {}, a.action_id)],
      [t('Enforced digest'), h('code', {}, a.enforced_digest)], [t('Policy version'), String(a.policy_version)],
      [t('Quorum'), String(a.required_quorum)], [t('Expires'), fmtTime(a.expires_at)],
    ])),
    section(t('Enforced payload (what executes if approved)'), json(a.enforced_payload)),
    a.state === 'PENDING' ? h('div', {class: 'actions'},
      button(t('Approve…'), () => vote(ctx, a, 'APPROVE', result), {kind: 'primary'}),
      button(t('Deny…'), () => vote(ctx, a, 'DENY', result), {kind: 'danger'})) : null,
    result);
}

async function vote(ctx, a, decision, result) {
  const approve = decision === 'APPROVE';
  const c = await ask({title: approve ? t('Approve this action') : t('Deny this action'), danger: !approve,
    reason: 'required', confirmLabel: approve ? t('Approve') : t('Deny'),
    lines: [t('Action {id}', {id: a.action_id}), t('Enforced digest {digest}', {digest: a.enforced_digest}),
      t('Your vote binds to this exact payload digest. The server checks eligibility and separation of duties.')]});
  if (!c) return;
  const r = await ctx.client.call('approval.vote', {params: {id: a.id}, body: {decision, reason: c.reason}});
  if (!r.ok) {
    replace(result, notice(r));
    return;
  }
  replace(result, ok(t('Vote recorded; the request is {state}.', {state: r.data.request_state})), button(t('Refresh'), ctx.refresh));
}
