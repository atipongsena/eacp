// session.js holds the operator's API key in this closure only (ADR-028):
// never in browser storage, a cookie, a URL or a log. A reload, sign-out, a
// 401 answer or IDLE_MS without input drops it.

import {t} from './i18n.js';

export const IDLE_MS = 30 * 60 * 1000;

export function createSession({fetch: f = (...a) => globalThis.fetch(...a), now = () => Date.now(), idleMs = IDLE_MS} = {}) {
  let key = null;
  let me = null;
  let last = 0;
  const expired = () => key !== null && now() - last >= idleMs;
  const session = {
    async signIn(value) {
      const candidate = String(value ?? '').trim();
      if (!candidate) return {ok: false, error: 'empty', detail: t('Enter an API key.')};
      let res;
      try {
        res = await f('/v1/me', {method: 'GET', credentials: 'omit', cache: 'no-store', redirect: 'error',
          headers: {Authorization: `Bearer ${candidate}`, Accept: 'application/json'}});
      } catch {
        return {ok: false, error: 'network', detail: t('The control plane is unreachable.')};
      }
      if (res.status === 200) {
        const body = await res.json();
        key = candidate;
        me = {principalId: body.principal_id, tenantId: body.tenant_id, roles: [...(body.roles ?? [])],
          groups: (body.groups ?? []).map(g => ({id: g.id, name: g.name, displayName: g.display_name, lead: g.lead === true}))};
        last = now();
        return {ok: true};
      }
      if (res.status === 401) return {ok: false, error: 'unauthenticated', detail: t('The key was not accepted.')};
      if (res.status === 403) {
        return {ok: false, error: 'forbidden', detail: t('A principal key is required; agent keys cannot sign in.')};
      }
      return {ok: false, error: `http_${res.status}`, detail: t('Sign-in failed.')};
    },
    signOut() {
      key = null;
      me = null;
      last = 0;
    },
    signedIn: () => key !== null,
    expired,
    touch() {
      if (key !== null && !expired()) last = now();
    },
    authorization() {
      if (key === null || expired()) throw new Error('signed out');
      return `Bearer ${key}`;
    },
    me: () => (me ? {...me, roles: [...me.roles], groups: me.groups.map(g => ({...g}))} : null),
    hasAny: roles => me !== null && (roles == null || roles.some(r => me.roles.includes(r))),
  };
  return session;
}
