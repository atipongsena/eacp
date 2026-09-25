// api.js is the one list of every call the console makes. A Go test checks
// each entry against the real API mux, so the console cannot call a route
// that does not exist. The console is a client: the API and PostgreSQL
// decide every request.
import {isUUID} from './router.js';

// [name, method, path, allowed query parameters]
export const ROUTES = [
  ['me', 'GET', '/v1/me', []],
  ['soc.summary', 'GET', '/v1/soc/summary', []],
  ['incident.list', 'GET', '/v1/incidents', ['state', 'severity', 'kind', 'limit']],
  ['incident.get', 'GET', '/v1/incidents/{id}', []],
  ['incident.open', 'POST', '/v1/incidents', []],
  ['incident.ack', 'POST', '/v1/incidents/{id}/acknowledge', []],
  ['incident.assign', 'POST', '/v1/incidents/{id}/assign', []],
  ['incident.note', 'POST', '/v1/incidents/{id}/notes', []],
  ['incident.link', 'POST', '/v1/incidents/{id}/links', []],
  ['incident.resolve', 'POST', '/v1/incidents/{id}/resolve', []],
  ['agent.list', 'GET', '/v1/agents', []],
  ['agent.get', 'GET', '/v1/agents/{ref}', []],
  ['connector.list', 'GET', '/v1/connectors', []],
  ['connector.tools', 'GET', '/v1/connectors/{id}/tools', []],
  ['connector.circuit', 'GET', '/v1/connectors/{id}/circuit', []],
  ['circuit.disable', 'POST', '/v1/connectors/{id}/circuit/disable', []],
  ['circuit.enable', 'POST', '/v1/connectors/{id}/circuit/enable', []],
  ['tool.get', 'GET', '/v1/tools/{id}', []],
  ['tool.definitions', 'GET', '/v1/tools/{id}/definitions', []],
  ['tool.quarantine', 'POST', '/v1/tools/{id}/quarantine', []],
  ['tool.release', 'POST', '/v1/tools/{id}/release', []],
  ['kill.list', 'GET', '/v1/killswitch', []],
  ['kill.set', 'POST', '/v1/killswitch', []],
  ['fleet.health', 'GET', '/v1/fleet/health', ['environment', 'risk_class', 'health', 'window']],
  ['fleet.agents', 'GET', '/v1/fleet/agents', ['environment', 'risk_class', 'health', 'window']],
  ['fleet.apply', 'POST', '/v1/fleet/operations', []],
  ['fleet.operation', 'GET', '/v1/fleet/operations/{id}', []],
  ['approval.list', 'GET', '/v1/approvals', []],
  ['approval.get', 'GET', '/v1/approvals/{id}', []],
  ['approval.vote', 'POST', '/v1/approvals/{id}/votes', []],
  ['action.list', 'GET', '/v1/actions', ['state', 'limit']],
  ['action.get', 'GET', '/v1/actions/{id}', []],
  ['action.evidence', 'GET', '/v1/actions/{id}/evidence', []],
  ['dependency.blast', 'GET', '/v1/dependencies/blast-radius', ['kind', 'id', 'name']],
  ['finops.dashboard', 'GET', '/v1/finops/dashboard', []],
  ['finops.alerts', 'GET', '/v1/finops/alerts', ['open', 'limit']],
];

const BY_NAME = new Map(ROUTES.map(([name, method, path, query]) => [name, {method, path, query}]));
const PARAMS = {id: isUUID,
  ref: v => typeof v === 'string' && /^[A-Za-z0-9._-]{1,128}$/.test(v) && v !== '.' && v !== '..'};
const QUERY_VALUE = /^[^\u0000-\u001f\u007f]{1,256}$/;

export function buildPath(name, params = {}, query = {}) {
  const route = BY_NAME.get(name);
  if (!route) throw new Error(`unknown route ${name}`);
  const path = route.path.replace(/\{(\w+)\}/g, (_, p) => {
    const value = params[p];
    if (!PARAMS[p] || !PARAMS[p](value)) throw new Error(`invalid ${p} for ${name}`);
    return encodeURIComponent(value);
  });
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v === undefined || v === null || v === '') continue;
    if (!route.query.includes(k)) throw new Error(`query ${k} is not allowed for ${name}`);
    if (!QUERY_VALUE.test(String(v))) throw new Error(`invalid ${k} for ${name}`);
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return {method: route.method, url: s ? `${path}?${s}` : path};
}

export function createClient({session, fetch: f = (...a) => globalThis.fetch(...a), onUnauthorized = () => {}}) {
  return {
    async call(name, {params, query, body} = {}) {
      const {method, url} = buildPath(name, params, query);
      const headers = {Authorization: session.authorization(), Accept: 'application/json'};
      const init = {method, headers, credentials: 'omit', cache: 'no-store', redirect: 'error'};
      if (body !== undefined) {
        headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(body);
      }
      let res;
      try {
        res = await f(url, init);
      } catch {
        return {ok: false, status: 0, error: 'network', detail: ''};
      }
      let data = null;
      if (res.status !== 204) {
        try {
          data = await res.json();
        } catch {
          data = null;
        }
      }
      if (res.ok) return {ok: true, status: res.status, data};
      if (res.status === 401) onUnauthorized();
      return {ok: false, status: res.status, error: String(data?.error ?? data?.code ?? `http_${res.status}`),
        detail: String(data?.detail ?? data?.message ?? '')};
    },
  };
}
