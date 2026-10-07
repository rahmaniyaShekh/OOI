// OOI rendezvous: a Cloudflare Worker + Durable Object that swaps one sealed
// offer and one sealed answer between two peers, then leaves the path. It never
// sees the code, the SDP, or either peer's IP in plaintext.
//
// Deployed under a path prefix (share.mdarif.online/ooi) via a Worker route, so
// it strips BASE before routing. The joiner page and the API both live below it.
//
// Relay: some pairs of networks can never reach each other directly (a
// carrier-grade NAT that picks a new public port per destination on one side,
// a router that drops packets from unexpected ports on the other). For those,
// the host and the sharing page each open a WebSocket here, paired by the
// handshake session, and every message is forwarded verbatim to the other
// side. The page puts a fresh AES-256-GCM key inside its code-sealed answer, so
// this Worker forwards ciphertext it has no key for. Hosts advertise the
// ability with caps:["relay"] on publish, and a page only tries it when it is
// there. Direct sessions never touch it.
//
//   GET /api/room/:id/relay?session=&role=host|viewer[&owner=]  -> WebSocket
//   GET /api/health                                             -> {ok, service}

const BASE = '/ooi';                 // path prefix this Worker owns
const TTL_SECONDS = 600;             // idle rooms evaporate after 10 min
const MAX_BODY = 16 * 1024;          // a sealed SDP is ~1 KB
const PREFIX = 'OOI1:';              // our sealed-blob marker
// Relay message caps. The page sends the video, so its largest message is a
// keyframe fragment (it splits frames at 240 KiB); Cloudflare accepts messages
// up to 1 MiB. The host only sends small control messages.
const RELAY_MAX_VIEWER = 1024 * 1024;
const RELAY_MAX_HOST = 64 * 1024;
// Capabilities a host may advertise; anything else is dropped.
const CAPS = ['relay'];
const capsOf = c => Array.isArray(c) ? c.filter(x => CAPS.includes(x)) : [];

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
// A random secret only the publishing host knows. Optional, so hosts that
// predate it keep publishing; only the relay's host role requires it.
const isOwner = s => typeof s === 'string' && /^[0-9a-f]{32,64}$/.test(s);
const isBlob = s => typeof s === 'string' && s.length > 16 && s.length < MAX_BODY &&
  s.startsWith(PREFIX) && /^[A-Za-z0-9_\-:]+$/.test(s);

async function sha256hex(s) {
  const h = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s));
  return [...new Uint8Array(h)].map(b => b.toString(16).padStart(2, '0')).join('');
}

async function readJson(req) {
  const raw = await req.text();
  if (raw.length > MAX_BODY) return null;
  try { return JSON.parse(raw); } catch { return null; }
}

export class Room {
  constructor(state) {
    this.state = state;
    // Answered by the runtime itself, so an idle relay's keepalive neither
    // wakes nor bills the object.
    state.setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'));
  }

  // Any activity extends the room's life, but the alarm is only rewritten past
  // half-life: the host polls every 2s, and resetting on every poll would be
  // tens of thousands of storage writes a day to move a deadline by 2 seconds.
  async touch() {
    const now = Date.now();
    const cur = await this.state.storage.getAlarm();
    if (cur && cur - now > (TTL_SECONDS * 1000) / 2) return;
    await this.state.storage.setAlarm(now + TTL_SECONDS * 1000);
  }
  async alarm() {
    // A live relay keeps its room: relayed frames deliberately never touch
    // storage, so they do not extend the alarm by themselves.
    if (this.state.getWebSockets().length > 0) {
      await this.state.storage.setAlarm(Date.now() + TTL_SECONDS * 1000);
      return;
    }
    await this.state.storage.deleteAll();
  }

  async fetch(request) {
    const url = new URL(request.url);
    const op = url.searchParams.get('op');
    const s = this.state.storage;

    if (op === 'publish') {
      // Overwrite is deliberate: after a drop the host republishes under the
      // SAME code with a NEW session, so the joiner rejoins with no new code.
      // The owner is kept as a hash and belongs to this publish: whoever
      // published the offer is the only one who may join its relay as host.
      const { offer, session, owner, caps } = await request.json();
      await s.put('room', {
        offer, session, at: Date.now(), caps: capsOf(caps),
        owner: isOwner(owner) ? await sha256hex(owner) : null,
      });
      const old = await s.list({ prefix: 'ans:' });
      if (old.size) await s.delete([...old.keys()]);
      await this.touch();
      return json({ ok: true, expiresIn: TTL_SECONDS }, 201);
    }
    if (op === 'offer') {
      const room = await s.get('room');
      return room ? json({ offer: room.offer, session: room.session, caps: room.caps || [] })
                  : json({ error: 'unknown or expired code' }, 404);
    }
    if (op === 'relay') {
      if (request.headers.get('upgrade') !== 'websocket') return json({ error: 'expected a websocket' }, 426);
      const session = url.searchParams.get('session');
      const role = url.searchParams.get('role');
      if (!isSession(session) || (role !== 'host' && role !== 'viewer')) return json({ error: 'bad request' }, 400);
      const room = await s.get('room');
      if (!room) return json({ error: 'unknown or expired code' }, 404);
      if (role === 'host') {
        const owner = url.searchParams.get('owner');
        if (!room.owner || !isOwner(owner) || (await sha256hex(owner)) !== room.owner)
          return json({ error: 'forbidden' }, 403);
      }
      // Both sides join the CURRENT offer's session: once the host has moved on
      // to a new offer, this one will never be served.
      if (room.session !== session) return json({ error: 'stale', session: room.session }, 409);
      const tag = `${role === 'host' ? 'rh' : 'rv'}:${session}`;
      // One socket per side: a reconnect replaces its own stale socket.
      for (const old of this.state.getWebSockets(tag)) { try { old.close(4000, 'replaced'); } catch {} }
      const pair = new WebSocketPair();
      this.state.acceptWebSocket(pair[1], [tag]);
      pair[1].serializeAttachment({ relay: true, role, session });
      await this.touch();
      return new Response(null, { status: 101, webSocket: pair[0] });
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
      for (const ws of this.state.getWebSockets()) { try { ws.close(4002, 'host left'); } catch {} }
      return empty(204);
    }
    return json({ error: 'not found' }, 404);
  }

  // --- relay sockets (hibernation API) --------------------------------------
  // Forwarded verbatim: the payload is sealed with a key this Worker never sees.
  async webSocketMessage(ws, msg) {
    const att = ws.deserializeAttachment() || {};
    if (!att.relay) return;
    const size = typeof msg === 'string' ? msg.length : msg.byteLength;
    if (size > (att.role === 'host' ? RELAY_MAX_HOST : RELAY_MAX_VIEWER)) return;
    const peer = `${att.role === 'host' ? 'rv' : 'rh'}:${att.session}`;
    for (const p of this.state.getWebSockets(peer)) { try { p.send(msg); } catch {} }
  }

  // One side left: close the other, so it notices now and reconnects instead
  // of waiting on a pipe nobody is at the other end of.
  async webSocketClose(ws, code) {
    const att = ws.deserializeAttachment() || {};
    try { ws.close(code === 1005 || code === 1006 ? 1000 : code, 'closed'); } catch {}
    if (!att.relay) return;
    // A socket replaced by its own side's reconnect is not that side leaving.
    const own = `${att.role === 'host' ? 'rh' : 'rv'}:${att.session}`;
    if (code === 4000 || this.state.getWebSockets(own).some(s => s !== ws)) return;
    const peer = `${att.role === 'host' ? 'rv' : 'rh'}:${att.session}`;
    for (const p of this.state.getWebSockets(peer)) { try { p.close(4001, 'peer left'); } catch {} }
  }

  async webSocketError(ws) { await this.webSocketClose(ws, 1011); }
}

const room = (env, id, op, init = {}) =>
  env.ROOMS.get(env.ROOMS.idFromName(id)).fetch(`https://room/?op=${op}${init.qs || ''}`, init.req);

// stripBase removes the route prefix, returning the app-relative path.
function stripBase(pathname) {
  if (pathname === BASE) return '/';
  if (pathname.startsWith(BASE + '/')) return pathname.slice(BASE.length);
  return pathname; // requests without the prefix (should not happen on the route)
}

function securityHeaders(resp, host) {
  const out = new Response(resp.body, resp);
  // The relay is a WebSocket to this same host. It is named explicitly because
  // older browsers do not let 'self' cover wss:.
  out.headers.set('content-security-policy',
    `default-src 'none'; connect-src 'self' wss://${host}; media-src blob:; img-src 'self' data:; ` +
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
    if (path === '/api/health') return json({ ok: true, service: 'ooi' });
    if (path.startsWith('/api/')) {
      if (path === '/api/room' && request.method === 'POST') {
        const b = await readJson(request);
        if (!b || !isRoomId(b.id) || !isBlob(b.offer) || !isSession(b.session))
          return json({ error: 'bad request' }, 400);
        return room(env, b.id, 'publish', { req: { method: 'POST', body: JSON.stringify({
          offer: b.offer, session: b.session, owner: b.owner, caps: b.caps }) } });
      }
      const r = path.match(/^\/api\/room\/([0-9a-f]{64})\/relay$/);
      if (r && request.method === 'GET') {
        // The upgrade request itself goes to the room, which accepts the socket.
        const qs = new URLSearchParams({ op: 'relay',
          session: url.searchParams.get('session') || '', role: url.searchParams.get('role') || '',
          owner: url.searchParams.get('owner') || '' });
        return env.ROOMS.get(env.ROOMS.idFromName(r[1])).fetch(new Request(`https://room/?${qs}`, request));
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
    return securityHeaders(asset, url.host);
  },
};
