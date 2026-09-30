// requests.js is the queue of what the signed-in person may decide
// (ADR-033): for a registry approver, each Studio version waiting for a
// decision, with the tools it asks for in plain words, and each key the
// agent runtime proposed; for anyone, the Hub proposals they may publish
// (Phase 27b: a department lead, or an admin or approver for the
// organisation). PostgreSQL enforces the two-person rule; the page only
// says so before asking.
import {h, section, notice, table, button, ok, replace, relTime, emptyState, badge} from '../dom.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {definitionView, effects, scopeBadge} from './common.js';

export async function render(ctx) {
  const listings = await ctx.client.call('hub.requests');
  if (!listings.ok) return notice(listings);
  ctx.pollEvery(15000);
  const proposals = listings.data?.proposals ?? [];
  const hub = section(t('Hub listings waiting for your decision'), proposals.length === 0
    ? emptyState(t('No listing waits for your decision.'),
      t('A lead of a department publishes its agents to the department; an admin or a registry approver to the organisation.'), 'globe')
    : proposals.map(p => proposal(ctx, p)));
  if (!ctx.session.hasAny(['registry_approver'])) return h('div', {}, hub);
  const res = await ctx.client.call('studio.requests');
  if (!res.ok) return h('div', {}, notice(res), hub);
  const me = ctx.session.me().principalId;
  const {requests, keys} = res.data;
  return h('div', {},
    section(t('Agents waiting for a decision'), requests.length === 0
      ? emptyState(t('Nothing waits for your decision.'), t('A saved Studio agent appears here until a registry approver decides it.'), 'approvals')
      : requests.map(r => request(ctx, r, r.created_by === me))),
    section(t('Runtime keys waiting for approval'), table([
      [t('Agent'), k => `${k.agent_name} v${k.version}`],
      [t('Tools'), k => k.capability.join(', ') || '—'],
      [t('Proposed'), k => [relTime(k.proposed_at, Date.now(), {node: true}), ' ',
        k.proposed_by_runtime ? t('by the agent runtime') : t('by a person')]],
      [t('Master'), k => k.master_version || '—'],
      [t('Expires'), k => relTime(k.expires_at, Date.now(), {node: true})],
      ['', k => keyButton(ctx, k)],
    ], keys, emptyState(t('No key waits for approval.'),
      t('After an agent is approved, the agent runtime proposes its key here within a minute.'), 'shield-ok'))),
    hub);
}

// proposal is one Hub proposal the signed-in person may decide: the server
// lists only those, and refuses anyone else.
function proposal(ctx, p) {
  const out = h('div');
  const go = async approve => {
    const c = await ask({title: approve ? t('Publish {agent} v{version}', {agent: p.agent_name, version: p.version})
      : t('Reject publishing {agent} v{version}', {agent: p.agent_name, version: p.version}),
    reason: 'required', danger: !approve, confirmLabel: approve ? t('Publish') : t('Reject'),
    lines: [approve ? (p.scope === 'ORG' ? t('Everyone in the organisation can then run it as themselves and copy it.')
      : t('Everyone in {department} can then run it as themselves and copy it.', {department: p.department_name}))
      : t('The owner sees your reason; nothing is published.'),
    t('It may use: {tools}.', {tools: p.tools.map(x => x.ref).join(', ') || '—'})]});
    if (!c) return;
    const req = {params: {id: p.id}, body: {reason: c.reason}};
    const r = approve ? await ctx.client.call('hub.approve', req) : await ctx.client.call('hub.reject', req);
    replace(out, r.ok ? ok(approve ? t('Published.') : t('Rejected.')) : notice(r), r.ok ? button(t('Refresh'), ctx.refresh) : null);
  };
  return h('div', {class: 'request'},
    h('h3', {}, `${p.display_name} (${p.agent_name}) v${p.version}`),
    h('p', {class: 'chips'}, t('Share with'), ' ', scopeBadge(p), ' ', t('from {department}', {department: p.department_name}),
      p.tags.length ? [' ', p.tags.map(x => badge('tag', x))] : null),
    p.note ? h('p', {class: 'hint'}, t('Note: {note}', {note: p.note})) : null,
    h('ul', {}, p.tools.map(x => h('li', {}, h('code', {}, x.ref), ' — ', effects(x.side_effects)))),
    h('div', {class: 'actions'},
      button(t('Publish…'), () => go(true), {kind: 'primary'}),
      button(t('Reject…'), () => go(false), {kind: 'danger'})),
    out);
}

function request(ctx, r, own) {
  const out = h('div');
  return h('div', {class: 'request'},
    h('h3', {}, `${r.agent_name} v${r.version}`),
    h('ul', {}, r.tools.map(x => h('li', {}, h('code', {}, x.ref), ' — ', effects(x.side_effects)))),
    h('details', {}, h('summary', {}, t('What it does')), definitionView(r.definition)),
    own ? h('p', {class: 'hint'}, t('You saved this agent, so another registry approver must decide it.')) : h('div', {class: 'actions'},
      button(t('Approve…'), () => decide(ctx, r, true, out), {kind: 'primary'}),
      button(t('Reject…'), () => decide(ctx, r, false, out), {kind: 'danger'})),
    out);
}

async function decide(ctx, r, approve, out) {
  const c = await ask({title: approve ? t('Approve {agent} v{version}', {agent: r.agent_name, version: r.version})
    : t('Reject {agent} v{version}', {agent: r.agent_name, version: r.version}),
  reason: 'required', danger: !approve, confirmLabel: approve ? t('Approve') : t('Reject'),
  lines: [t('It may use: {tools}.', {tools: r.capability.join(', ') || '—'}),
    approve ? t('Approval activates this version with exactly those tools and retires its previous version.')
      : t('The author sees your reason.')]});
  if (!c) return;
  const req = {params: {id: r.id}, body: {reason: c.reason}};
  const r2 = approve ? await ctx.client.call('studio.approve', req) : await ctx.client.call('studio.reject', req);
  replace(out, r2.ok ? ok(approve ? t('Approved.') : t('Rejected.')) : notice(r2), r2.ok ? button(t('Refresh'), ctx.refresh) : null);
}

function keyButton(ctx, k) {
  const out = h('span');
  return h('span', {}, button(t('Approve key…'), async () => {
    const c = await ask({title: t('Approve the key for {agent} v{version}', {agent: k.agent_name, version: k.version}),
      confirmLabel: t('Approve'),
      lines: [t('The agent runtime derived this key; nobody sees it. Approving lets the agent act with it until it expires.'),
        t('It may use: {tools}.', {tools: k.capability.join(', ') || '—'})]});
    if (!c) return;
    const r = await ctx.client.call('credential.approve', {params: {id: k.id}});
    replace(out, r.ok ? ok(t('Approved.')) : notice(r));
    if (r.ok) ctx.refresh();
  }), out);
}
