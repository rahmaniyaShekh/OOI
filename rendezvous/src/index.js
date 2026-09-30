// OOI rendezvous: a Cloudflare Worker + Durable Object that swaps one sealed
// offer and one sealed answer between two peers, then leaves the path. It never
// sees the code, the SDP, or either peer's IP in plaintext.
//
// Deployed under a path prefix (share.mdarif.online/ooi) via a Worker route, so
// it strips BASE before routing. The joiner page and the API both live below it.

const BASE = '/ooi';                 // path prefix this Worker owns
const TTL_SECONDS = 600;             // idle rooms evaporate after 10 min
const MAX_BODY = 16 * 1024;          // a sealed SDP is ~1 KB
const PREFIX = 'OOI1:';              // our sealed-blob marker

const CORS = {
  'access-control-allow-origin': '*',
  'access-control-allow-methods': 'GET, POST, DELETE, OPTIONS',
  'access-control-allow-headers': 'content-type',
  'access-control-max-age': '86400',
};
const json = (o, status = 200) => new Response(JSON.stringify(o), {
  status, headers: { 'content-type': 'application/json', 'cache-control': 'no-store', ...CORS },
});
const empty = status => new Response(null, { status, headers: { ...CORS, 'cache-control': 'no-store' } });

// Validate shapes so callers cannot address arbitrary objects or store junk.
const isRoomId = s => typeof s === 'string' && /^[0-9a-f]{64}$/.test(s);
const isSession = s => typeof s === 'string' && /^[0-9a-f]{16}$/.test(s);
const isBlob = s => typeof s === 'string' && s.length > 16 && s.length < MAX_BODY &&
  s.startsWith(PREFIX) && /^[A-Za-z0-9_\-:]+$/.test(s);

async function readJson(req) {
  const raw = await req.text();
  if (raw.length > MAX_BODY) return null;
  try { return JSON.parse(raw); } catch { return null; }
}

export class Room {
  constructor(state) { this.state = state; }

  // Any activity extends the room's life, but the alarm is only rewritten past
  // half-life: the host polls every 2s, and resetting on every poll would be
  // tens of thousands of storage writes a day to move a deadline by 2 seconds.
  async touch() {
    const now = Date.now();
    const cur = await this.state.storage.getAlarm();
    if (cur && cur - now > (TTL_SECONDS * 1000) / 2) return;
    await this.state.storage.setAlarm(now + TTL_SECONDS * 1000);
  }
  async alarm() { await this.state.storage.deleteAll(); }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.searchParams.get('op');
    const s = this.state.storage;

    if (op === 'publish') {
      // Overwrite is deliberate: after a drop the host republishes under the
      // SAME code with a NEW session, so the joiner rejoins with no new code.
      const { offer, session } = await request.json();
      await s.put('room', { offer, session, at: Date.now() });
      const old = await s.list({ prefix: 'ans:' });
      if (old.size) await s.delete([...old.keys()]);
      await this.touch();
      return json({ ok: true, expiresIn: TTL_SECONDS }, 201);
    }
    if (op === 'offer') {
      const room = await s.get('room');
      return room ? json({ offer: room.offer, session: room.session })
                  : json({ error: 'unknown or expired code' }, 404);
    }
    if (op === 'answer-post') {
      const { answer, session } = await request.json();
      const room = await s.get('room');
      if (!room) return json({ error: 'unknown or expired code' }, 404);
      // Tell the joiner at once that it answered a replaced offer, instead of
      // letting it burn a full ICE timeout finding out.
      if (room.session !== session) return json({ error: 'stale', session: room.session }, 409);
      await s.put(`ans:${session}`, answer);
      await this.touch();
      return empty(204);
    }
    if (op === 'answer-get') {
      await this.touch(); // a polling host is alive: never expire under it
      const key = `ans:${url.searchParams.get('session')}`;
      const answer = await s.get(key);
      if (!answer) return empty(204);
      await s.delete(key);
      return json({ answer });
    }
    if (op === 'delete') {
      await s.deleteAll();
      await s.deleteAlarm();
      return empty(204);
    }
    return json({ error: 'not found' }, 404);
  }
}

const room = (env, id, op, init = {}) =>
  env.ROOMS.get(env.ROOMS.idFromName(id)).fetch(`https://room/?op=${op}${init.qs || ''}`, init.req);

// stripBase removes the route prefix, returning the app-relative path.
function stripBase(pathname) {
  if (pathname === BASE) return '/';
  if (pathname.startsWith(BASE + '/')) return pathname.slice(BASE.length);
  return pathname; // requests without the prefix (should not happen on the route)
}

function securityHeaders(resp) {
  const out = new Response(resp.body, resp);
  out.headers.set('content-security-policy',
    "default-src 'none'; connect-src 'self'; media-src blob:; img-src 'self' data:; " +
    "style-src 'unsafe-inline'; script-src 'unsafe-inline'; " +
    "base-uri 'none'; form-action 'none'; frame-ancestors 'none'");
  out.headers.set('referrer-policy', 'no-referrer');
  out.headers.set('x-content-type-options', 'nosniff');
  // getDisplayMedia is a powerful feature; allow it only for this origin.
  out.headers.set('permissions-policy', 'display-capture=(self), fullscreen=(self)');
  return out;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === 'OPTIONS') return empty(204);

    const path = stripBase(url.pathname);

    // API.
    if (path.startsWith('/api/')) {
      if (path === '/api/room' && request.method === 'POST') {
        const b = await readJson(request);
        if (!b || !isRoomId(b.id) || !isBlob(b.offer) || !isSession(b.session))
          return json({ error: 'bad request' }, 400);
        return room(env, b.id, 'publish',
          { req: { method: 'POST', body: JSON.stringify({ offer: b.offer, session: b.session }) } });
      }
      const m = path.match(/^\/api\/room\/([0-9a-f]{64})(\/answer)?$/);
      if (m) {
        const [, id, isAnswer] = m;
        if (!isAnswer && request.method === 'GET') return room(env, id, 'offer');
        if (!isAnswer && request.method === 'DELETE') return room(env, id, 'delete');
        if (isAnswer && request.method === 'POST') {
          const b = await readJson(request);
          if (!b || !isBlob(b.answer) || !isSession(b.session)) return json({ error: 'bad request' }, 400);
          return room(env, id, 'answer-post',
            { req: { method: 'POST', body: JSON.stringify({ answer: b.answer, session: b.session }) } });
        }
        if (isAnswer && request.method === 'GET') {
          const session = url.searchParams.get('session');
          if (!isSession(session)) return json({ error: 'bad request' }, 400);
          return room(env, id, 'answer-get', { qs: `&session=${session}` });
        }
      }
      return json({ error: 'not found' }, 404);
    }

    // Everything else is the joiner page. Assets are addressed by the stripped
    // path; a deep link like /ooi/K7Q-4MX resolves to the SPA index.
    const assetURL = new URL(request.url);
    assetURL.pathname = path === '/' ? '/index.html' : path;
    let asset = await env.ASSETS.fetch(new Request(assetURL, request));
    if (asset.status === 404) {
      assetURL.pathname = '/index.html';
      asset = await env.ASSETS.fetch(new Request(assetURL, request));
    }
    return securityHeaders(asset);
  },
};
