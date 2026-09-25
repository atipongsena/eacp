// router.js maps the location hash to an area, path parts and a query. The
// hash carries ids and filters only, never a key or personal data.

export const AREAS = ['overview', 'incidents', 'inventory', 'fleet', 'security', 'approvals', 'execution',
  'dependencies', 'cost'];

const SEGMENT = /^[A-Za-z0-9._-]{1,128}$/;
const KEY = /^[a-z_]{1,32}$/;
const VALUE = /^[A-Za-z0-9._:,/@-]{0,1024}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const isUUID = value => typeof value === 'string' && UUID.test(value);

export function parse(hash) {
  const raw = String(hash ?? '').replace(/^#\/?/, '');
  const [path, qs = ''] = raw.split('?', 2);
  const parts = path.split('/').filter(Boolean);
  const area = parts.shift();
  const segmentsOK = parts.every(p => SEGMENT.test(p) && p !== '.' && p !== '..');
  if (!AREAS.includes(area) || !segmentsOK) return {area: 'overview', parts: [], query: {}};
  const query = {};
  for (const pair of qs.split('&').filter(Boolean)) {
    const [k, v = ''] = pair.split('=', 2);
    let value;
    try {
      value = decodeURIComponent(v);
    } catch {
      continue;
    }
    if (KEY.test(k) && VALUE.test(value)) query[k] = value;
  }
  return {area, parts, query};
}

export function format(area, parts = [], query = {}) {
  const qs = Object.entries(query)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => `${k}=${encodeURIComponent(String(v))}`).join('&');
  return `#/${[area, ...parts.map(p => encodeURIComponent(String(p)))].join('/')}${qs ? `?${qs}` : ''}`;
}
