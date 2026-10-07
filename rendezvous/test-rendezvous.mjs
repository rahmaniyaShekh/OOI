// End-to-end test of the deployed rendezvous, simulating both peers.
//   node test-rendezvous.mjs https://share.mdarif.online/ooi
// Asserts the mailbox semantics, read-once answers, stale-session handling,
// republish clearing, and that nothing is stored in plaintext.
import crypto from 'node:crypto';

const base = (process.argv[2] || 'https://share.mdarif.online/ooi').replace(/\/$/, '');
let failures = 0;
const ok = (cond, msg) => { console.log((cond ? 'PASS' : 'FAIL') + '  ' + msg); if (!cond) failures++; };

const PREFIX='OOI1:', MAGIC=[0x4f,0x4f,0x49,0x31], FLAG=1, SALT=16, IV=12, TAG=16, ITER=200000;
const b64u=b=>Buffer.from(b).toString('base64').replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
const cat=(...p)=>{const o=new Uint8Array(p.reduce((a,x)=>a+x.length,0));let k=0;for(const x of p){o.set(x,k);k+=x.length}return o};
async function derive(pass,salt){
  const m=await crypto.subtle.importKey('raw',new TextEncoder().encode(pass),'PBKDF2',false,['deriveKey']);
  return crypto.subtle.deriveKey({name:'PBKDF2',salt,iterations:ITER,hash:'SHA-256'},m,{name:'AES-GCM',length:256},false,['encrypt','decrypt']);
}
async function seal(sdp,code){
  const zlib=await import('node:zlib');
  const body=zlib.deflateRawSync(Buffer.from(sdp));
  const salt=crypto.getRandomValues(new Uint8Array(SALT)),iv=crypto.getRandomValues(new Uint8Array(IV));
  const head=cat(new Uint8Array(MAGIC),new Uint8Array([FLAG]),salt,iv);
  const ct=new Uint8Array(await crypto.subtle.encrypt({name:'AES-GCM',iv,additionalData:head,tagLength:TAG*8},await derive(code,salt),body));
  return PREFIX+b64u(cat(head,ct));
}
const roomIdFor=async code=>[...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(code)))].map(b=>b.toString(16).padStart(2,'0')).join('');
const session=()=>crypto.randomBytes(8).toString('hex');
const sdp='v=0\r\no=- 1 2 IN IP4 192.168.1.5\r\na=candidate:1 1 udp 1 203.0.113.9 5555 typ srflx\r\na=fingerprint:sha-256 AA:BB\r\n';

const run = async () => {
  const code='K7Q4MX', id=await roomIdFor(code), s1=session();
  const offer=await seal(sdp,code);

  // 1. publish -> 201, fetch offer -> same blob
  let r=await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({id,session:s1,offer})});
  ok(r.status===201,'publish returns 201');
  r=await fetch(base+`/api/room/${id}`,{cache:'no-store'}); const got=await r.json();
  ok(r.status===200 && got.offer===offer && got.session===s1,'fetched offer matches, with session');

  // Negative: stored blob leaks nothing.
  ok(!got.offer.includes(code) && !got.offer.includes('192.168.1.5') && !got.offer.includes('candidate'),
     'stored offer contains no code, IP, or SDP text');

  // 2. answer with right session -> 204, host reads once
  const ans=await seal('answer-sdp',code);
  r=await fetch(base+`/api/room/${id}/answer`,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({answer:ans,session:s1})});
  ok(r.status===204,'answer with valid session returns 204');
  r=await fetch(base+`/api/room/${id}/answer?session=${s1}`,{cache:'no-store'});
  ok(r.status===200,'host reads the answer once');
  const consumed=await fetch(base+`/api/room/${id}/answer?session=${s1}`,{cache:'no-store'});
  ok(consumed.status===204,'answer is read-once (second read empty)');

  // 3. stale session -> 409 with current session
  r=await fetch(base+`/api/room/${id}/answer`,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({answer:ans,session:session()})});
  const stale=r.status===409?await r.json():{};
  ok(r.status===409 && stale.session===s1,'stale-session answer returns 409 with current session');

  // 4. republish clears old answers
  const s2=session();
  await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({id,session:s2,offer})});
  await fetch(base+`/api/room/${id}/answer`,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({answer:ans,session:s2})});
  await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({id,session:session(),offer})});
  r=await fetch(base+`/api/room/${id}/answer?session=${s2}`,{cache:'no-store'});
  ok(r.status===204,'republish clears prior answers');

  // 5. consistency: answer visible on the very next read
  const s3=session();
  await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({id,session:s3,offer})});
  await fetch(base+`/api/room/${id}/answer`,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({answer:ans,session:s3})});
  r=await fetch(base+`/api/room/${id}/answer?session=${s3}`,{cache:'no-store'});
  ok(r.status===200,'answer is strongly consistent (visible immediately)');

  // 6. delete -> 404
  r=await fetch(base+`/api/room/${id}`,{method:'DELETE'});
  ok(r.status===204,'delete returns 204');
  r=await fetch(base+`/api/room/${id}`,{cache:'no-store'});
  ok(r.status===404,'deleted room is gone (404)');

  // 7. malformed inputs -> 400
  for (const [name,body] of [
    ['bad id',{id:'xyz',session:session(),offer}],
    ['bad session',{id,session:'zz',offer}],
    ['non-prefixed blob',{id,session:session(),offer:'nope'}],
    ['oversized blob',{id,session:session(),offer:PREFIX+'A'.repeat(20000)}],
  ]){
    r=await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(body)});
    ok(r.status===400,`rejects ${name} with 400`);
  }

  // 8. page + headers
  r=await fetch(base+'/');
  const csp=r.headers.get('content-security-policy')||'';
  ok(r.status===200 && csp.includes("frame-ancestors 'none'"),'joiner page served with CSP header');
  ok((r.headers.get('permissions-policy')||'').includes('display-capture'),'permissions-policy allows display-capture');

  // 9. health
  r=await fetch(base+'/api/health'); const h=await r.json();
  ok(r.status===200 && h.ok===true && h.service==='ooi','health reports ooi');

  // 10. relay: caps round trip, owner check, pairing, forwarding, peer close
  const owner=crypto.randomBytes(16).toString('hex'), rs=session();
  await fetch(base+'/api/room',{method:'POST',headers:{'content-type':'application/json'},
    body:JSON.stringify({id,session:rs,offer,owner,caps:['relay','bogus']})});
  r=await fetch(base+`/api/room/${id}`,{cache:'no-store'}); const withCaps=await r.json();
  ok(JSON.stringify(withCaps.caps)==='["relay"]','caps round trip (whitelisted)');
  const wsBase=base.replace(/^http/,'ws')+`/api/room/${id}/relay?session=${rs}`;
  const open=url=>new Promise(res=>{
    const ws=new WebSocket(url); ws.binaryType='arraybuffer';
    const q=[], waiters=[];
    ws.onmessage=e=>{ const w=waiters.shift(); w?w(e.data):q.push(e.data); };
    ws.next=(ms=5000)=>q.length?Promise.resolve(q.shift()):new Promise(r=>{ waiters.push(r); setTimeout(()=>r(null),ms); });
    ws.closed=new Promise(r=>ws.addEventListener('close',e=>r(e.code)));
    ws.onopen=()=>res(ws); ws.onerror=()=>res(null);
  });
  const bad=await open(wsBase+'&role=host');
  ok(!bad,'host role rejected without the owner');
  const bad2=await open(wsBase+'&role=host&owner='+'0'.repeat(32));
  ok(!bad2,'host role rejected with the wrong owner');
  const host=await open(wsBase+'&role=host&owner='+owner);
  const viewer=await open(wsBase+'&role=viewer');
  ok(host && viewer,'host and viewer pair on the session');
  viewer.send(new Uint8Array([3,1,2,3]));
  let fwd=await host.next();
  ok(fwd && Buffer.from(fwd).equals(Buffer.from([3,1,2,3])),'viewer -> host forwarded verbatim');
  host.send(new Uint8Array([2,9,9]));
  fwd=await viewer.next();
  ok(fwd && Buffer.from(fwd).equals(Buffer.from([2,9,9])),'host -> viewer forwarded verbatim');
  const big=new Uint8Array(700*1024).fill(7);
  viewer.send(big);
  fwd=await host.next(10000);
  ok(fwd && fwd.byteLength===big.length,'a 700 KiB keyframe message passes');
  host.send("ping"); fwd=await host.next();
  ok(fwd==='pong','keepalive ping answered');
  const staleWs=await open(base.replace(/^http/,'ws')+`/api/room/${id}/relay?session=${session()}&role=viewer`);
  ok(!staleWs,"viewer on a stale session rejected");
  viewer.close();
  const closeCode=await Promise.race([host.closed, new Promise(r=>setTimeout(()=>r('timeout'),5000))]);
  ok(closeCode!=="timeout","viewer close propagates to the host");
  await fetch(base+`/api/room/${id}`,{method:'DELETE'});

  console.log(failures?`\n${failures} FAILURE(S)`:'\nALL PASSED');
  process.exit(failures?1:0);
};
run().catch(e=>{console.error(e);process.exit(2)});
