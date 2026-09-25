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
  // The link is journaled and insert-only: send it once, then keep the
  // button disabled.
  let sending = false;
  const linkButton = button('Link this to the incident', async () => {
    if (sending || linkButton.disabled) return;
    sending = true;
    linkButton.disabled = true;
    try {
      const r = await ctx.client.call('incident.link', {params: {id: incident}, body: {kind, id}});
      linkButton.disabled = r.ok;
      replace(out, r.ok ? ok('Linked.') : notice(r));
    } catch (err) {
      linkButton.disabled = false;
      replace(out, notice({status: 0, error: 'error', detail: String(err?.message ?? err)}));
    } finally {
      sending = false;
    }
  });
  return h('div', {class: 'actions'},
    linkButton,
    link('Back to the incident', format('incidents', [incident])),
    out);
}
