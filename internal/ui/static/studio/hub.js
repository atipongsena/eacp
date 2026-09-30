// hub.js is the Agent Hub (Phase 27b, ADR-033 Rev 1.3): the listings the
// signed-in person may see, searched by text, tag and department, and one
// listing with what it does, a form to run it and, for authors, a copy of
// their own. PostgreSQL decides who sees, runs, clones and retires a
// listing; the page only leaves out buttons that would be refused.
import {h, section, notice, table, link, badge, input, field, kv, relTime, replace, emptyState, button, ok, select,
  values as formValues} from '../dom.js';
import {format, isUUID} from '../router.js';
import {ask} from '../confirm.js';
import {t} from '../i18n.js';
import {definitionView, effects, scopeBadge, stateBadge} from './common.js';
import {runForm} from './agents.js';

export async function render(ctx) {
  return ctx.route.parts.length > 0 ? detail(ctx, ctx.route.parts[0]) : list(ctx);
}

async function list(ctx) {
  const q = ctx.route.query;
  const query = Object.fromEntries(['q', 'tag', 'department'].filter(k => q[k]).map(k => [k, q[k]]));
  const res = await ctx.client.call('hub.list', {query});
  if (!res.ok) return notice(res);
  const groups = ctx.session.me().groups ?? [];
  const f = h('form', {class: 'filters', role: 'search', onsubmit: e => {
    e.preventDefault();
    const v = formValues(f);
    ctx.go(format('hub', [], {q: v.q, tag: v.tag, department: v.department}));
  }},
  field(t('Search'), input('q', {value: q.q ?? '', type: 'search', placeholder: t('name or description')})),
  field(t('Tag'), input('tag', {value: q.tag ?? '', placeholder: t('for example leave')})),
  field(t('Department'), select('department', [['', t('any')], ...groups.map(g => [g.id, g.displayName])], q.department ?? '')),
  h('button', {type: 'submit'}, t('Search')));
  const filtered = Object.keys(query).length > 0;
  const empty = filtered
    ? emptyState(t('No listing matches.'), t('Try another word, tag or department.'), 'empty')
    : emptyState(t('The Hub is empty for you.'),
      t('An agent appears here once a department lead or an approver publishes it to your department or the organisation.'), 'globe');
  return h('div', {}, f, section(t('Agents you can use'), table([
    [t('Agent'), l => h('span', {}, link(l.display_name, format('hub', [l.id])), ' ', h('code', {}, l.name))],
    [t('Department'), l => l.department_name],
    [t('Shared with'), l => scopeBadge(l)],
    [t('Tags'), l => (l.tags.length ? h('span', {class: 'chips'}, l.tags.map(x => badge('tag', x))) : '—')],
    [t('Runs'), l => String(l.run_count)],
    [t('State'), l => [stateBadge(l), l.runnable ? null : [' ', h('span', {class: 'hint'}, t('not runnable now'))]]],
  ], res.data.listings, empty)));
}

async function detail(ctx, id) {
  if (!isUUID(id)) return notice({status: 404, error: 'not_found', detail: t('not a listing id')});
  const res = await ctx.client.call('hub.get', {params: {id}});
  if (!res.ok) return notice(res);
  const l = res.data;
  const me = ctx.session.me();
  return h('div', {},
    h('p', {}, link(t('← Hub'), format('hub'))),
    h('h1', {}, l.display_name),
    h('div', {class: 'chips'}, h('code', {}, l.name), badge('version', `v${l.version}`), scopeBadge(l), stateBadge(l)),
    l.description ? h('p', {class: 'hint'}, l.description) : null,
    h('p', {class: 'status-line', role: 'status'}, availability(l)),
    l.runnable ? runForm(ctx, {agentId: l.agent_id, displayName: l.display_name, version: l.version, definition: l.definition})
      : null,
    l.state === 'PUBLISHED' && ctx.session.hasAny(['studio_author']) ? cloneForm(ctx, l) : null,
    canRetire(ctx.session, me, l) ? retireActions(ctx, l) : null,
    section(t('What it does'), definitionView(l.definition),
      h('h3', {}, t('Tools')), h('ul', {}, l.tools.map(x => h('li', {}, h('code', {}, x.ref), ' — ', effects(x.side_effects))))),
    section(t('Record'), kv([
      [t('Department'), l.department_name],
      [t('Tags'), l.tags.join(', ') || '—'],
      [t('Runs'), String(l.run_count)],
      [t('Published'), relTime(l.published_at, Date.now(), {node: true})],
      [t('Version id'), h('code', {}, l.published_version_id)],
    ])));
}

// availability says in one sentence whether the listing runs, and why not.
export function availability(l) {
  if (l.state === 'WITHDRAWN') return t('Withdrawn: it starts no new run. Only its owner sees it.');
  if (!l.runnable) return t('Not runnable now: its owner saved a newer version, which runs here once it is published again.');
  if (l.state === 'DEPRECATED') return t('Deprecated: it still runs, but its owner or an approver advises against new use.');
  return t('Published: you can run it. It runs as you, and every step goes through the action path.');
}

// canRetire leaves the deprecate and withdraw buttons to those the server
// accepts: the owner, an admin, a lead of the department for a department
// listing, a registry approver for an organisation listing.
export function canRetire(session, me, l) {
  if (l.state === 'WITHDRAWN') return false;
  if (me.principalId === l.owner_id || session.hasAny(['admin'])) return true;
  if (l.scope === 'ORG') return session.hasAny(['registry_approver']);
  return (me.groups ?? []).some(g => g.id === l.department_id && g.lead);
}

function retireActions(ctx, l) {
  const out = h('div');
  const go = async deprecate => {
    const c = await ask({title: deprecate ? t('Deprecate {agent}', {agent: l.display_name}) : t('Withdraw {agent}', {agent: l.display_name}),
      reason: 'required', danger: !deprecate, confirmLabel: deprecate ? t('Deprecate') : t('Withdraw'),
      lines: [deprecate ? t('It stays in the Hub and still runs, marked as deprecated.')
        : t('It leaves the Hub and starts no new run; runs in progress finish. Publishing it again needs a new approval.')]});
    if (!c) return;
    const req = {params: {id: l.id}, body: {reason: c.reason}};
    const r = deprecate ? await ctx.client.call('hub.deprecate', req) : await ctx.client.call('hub.withdraw', req);
    replace(out, r.ok ? ok(deprecate ? t('Deprecated.') : t('Withdrawn.')) : notice(r));
    if (r.ok) ctx.refresh();
  };
  return section(t('Manage this listing'), h('div', {class: 'actions'},
    l.state === 'PUBLISHED' ? button(t('Deprecate…'), () => go(true)) : null,
    button(t('Withdraw…'), () => go(false), {kind: 'danger'})), out);
}

// cloneForm copies the published definition into a new agent of the
// author's own department. The copy waits for its own approval.
function cloneForm(ctx, l) {
  const groups = ctx.session.me().groups ?? [];
  const out = h('div');
  if (groups.length === 0) {
    return section(t('Make your own copy'), h('p', {class: 'hint'},
      t('You are in no group, so you have no department to save a copy into. Ask an administrator.')));
  }
  const f = h('form', {class: 'stack', onsubmit: async e => {
    e.preventDefault();
    const v = formValues(f);
    const c = await ask({title: t('Copy {agent}', {agent: l.display_name}), confirmLabel: t('Copy'),
      lines: [t('The copy is a new agent of yours. It carries no permission: a registry approver must approve its tools before it runs.')]});
    if (!c) return;
    const r = await ctx.client.call('hub.clone', {params: {id: l.id},
      body: {name: v.name, display_name: v.display_name, department_id: v.department}});
    if (!r.ok) {
      replace(out, notice(r));
      return;
    }
    ctx.go(format('agents', [r.data.agent_id]));
  }},
  h('div', {class: 'form-grid'},
    field(t('Name (lower case, digits and hyphens)'), input('name', {required: true, value: `${l.name}-copy`.slice(0, 63)})),
    field(t('Display name'), input('display_name', {required: true, value: l.display_name})),
    field(t('Department'), select('department', groups.map(g => [g.id, `${g.displayName} (${g.name})`]), groups[0].id))),
  h('button', {type: 'submit'}, t('Copy…')));
  return section(t('Make your own copy'), h('p', {class: 'hint'},
    t('A copy lets you change it for your team. It needs its own approval.')), f, out);
}
