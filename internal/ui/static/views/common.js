// common.js holds helpers shared by the views.
import {h, link, button, ok, notice, replace} from '../dom.js';
import {format, isUUID} from '../router.js';

export const back = (label, hash) => h('p', {}, link(label, hash));

export const mapText = m => Object.entries(m ?? {}).map(([k, v]) => `${k}: ${v}`).join(', ') || '0';

// linkBack offers to record a containment change on the incident the
// operator came from (?incident=<id>). The link is journaled; it changes
// nothing else.
export function linkBack(ctx, kind, id) {
  const incident = ctx.route.query.incident;
  if (!isUUID(incident) || !ctx.session.hasAny(['operator', 'admin'])) return null;
  const out = h('span');
  return h('div', {class: 'actions'},
    button('Link this to the incident', async () => {
      const r = await ctx.client.call('incident.link', {params: {id: incident}, body: {kind, id}});
      replace(out, r.ok ? ok('Linked.') : notice(r));
    }),
    link('Back to the incident', format('incidents', [incident])),
    out);
}
