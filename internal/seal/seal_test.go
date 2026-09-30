package seal

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleSDP = "v=0\r\no=- 4611731400430051336 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" +
	"a=group:BUNDLE 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 98\r\nc=IN IP4 192.168.1.23\r\n" +
	"a=candidate:1 1 udp 2130706431 192.168.1.23 54321 typ host\r\n" +
	"a=candidate:2 1 udp 1694498815 203.0.113.7 54321 typ srflx raddr 192.168.1.23 rport 54321\r\n" +
	"a=fingerprint:sha-256 AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89\r\n"

func TestRoundTrip(t *testing.T) {
	blob, err := Seal(sampleSDP, "K7Q4MX")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(blob, Prefix) {
		t.Fatalf("missing prefix: %q", blob[:10])
	}
	got, err := Open(blob, "K7Q4MX")
	if err != nil {
		t.Fatal(err)
	}
	if got != sampleSDP {
		t.Fatal("round trip changed the SDP")
	}
}

func TestWrongCodeFails(t *testing.T) {
	blob, _ := Seal(sampleSDP, "K7Q4MX")
	if _, err := Open(blob, "K7Q4MY"); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("want ErrWrongCode, got %v", err)
	}
}

func TestTamperFails(t *testing.T) {
	blob, _ := Seal(sampleSDP, "K7Q4MX")
	// Flip a character well inside the ciphertext.
	b := []byte(blob)
	i := len(b) - 20
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	if _, err := Open(string(b), "K7Q4MX"); err == nil {
		t.Fatal("tampered blob opened")
	}
}

// TestNoPlaintextLeak is the negative test from the spec: the blob must not
// contain the code, the SDP, or any IP address in the clear.
func TestNoPlaintextLeak(t *testing.T) {
	blob, _ := Seal(sampleSDP, "K7Q4MX")
	for _, needle := range []string{"K7Q4MX", "K7Q-4MX", "192.168.1.23", "203.0.113.7",
		"candidate", "fingerprint", "v=0", "IN IP4"} {
		if strings.Contains(blob, needle) {
			t.Fatalf("blob leaks %q", needle)
		}
	}
}

func TestSaltAndIVAreFresh(t *testing.T) {
	a, _ := Seal(sampleSDP, "K7Q4MX")
	b, _ := Seal(sampleSDP, "K7Q4MX")
	if a == b {
		t.Fatal("two seals of the same input are identical: salt/iv not random")
	}
}

func TestRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "APP1:abcd", "OOI1:", "OOI1:!!!!", "OOI1:QUJD"} {
		if _, err := Open(in, "K7Q4MX"); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
}

// ---- cross-implementation: Go <-> browser WebCrypto (via Node) -------------

// nodeHelper seals or opens with the same JavaScript the joiner page runs.
const nodeHelper = `
const PREFIX='OOI1:',MAGIC=[0x4f,0x4f,0x49,0x31],FLAG_ENC=1,SALT=16,IV=12,TAG=16,ITER=200000;
const enc=b=>{let s='';for(const x of b)s+=String.fromCharCode(x);return btoa(s).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'')};
const dec=t=>{const n=t.replace(/-/g,'+').replace(/_/g,'/');const b=atob(n+'='.repeat((4-n.length%4)%4));return Uint8Array.from(b,c=>c.charCodeAt(0))};
const pipe=(t,b)=>new Response(new Blob([b]).stream().pipeThrough(t)).arrayBuffer().then(a=>new Uint8Array(a));
const cat=(...p)=>{const o=new Uint8Array(p.reduce((a,x)=>a+x.length,0));let k=0;for(const x of p){o.set(x,k);k+=x.length}return o};
async function key(pass,salt){const m=await crypto.subtle.importKey('raw',new TextEncoder().encode(pass),'PBKDF2',false,['deriveKey']);
 return crypto.subtle.deriveKey({name:'PBKDF2',salt,iterations:ITER,hash:'SHA-256'},m,{name:'AES-GCM',length:256},false,['encrypt','decrypt'])}
async function seal(sdp,code){const body=await pipe(new CompressionStream('deflate-raw'),new TextEncoder().encode(sdp));
 const salt=crypto.getRandomValues(new Uint8Array(SALT)),iv=crypto.getRandomValues(new Uint8Array(IV));
 const head=cat(new Uint8Array(MAGIC),new Uint8Array([FLAG_ENC]),salt,iv);
 const ct=new Uint8Array(await crypto.subtle.encrypt({name:'AES-GCM',iv,additionalData:head,tagLength:TAG*8},await key(code,salt),body));
 return PREFIX+enc(cat(head,ct))}
async function open(blob,code){const raw=dec(blob.trim().slice(PREFIX.length));const hl=5+SALT+IV;const head=raw.subarray(0,hl);
 const pt=await crypto.subtle.decrypt({name:'AES-GCM',iv:raw.subarray(5+SALT,hl),additionalData:head,tagLength:TAG*8},await key(code,raw.subarray(5,5+SALT)),raw.subarray(hl));
 return new TextDecoder().decode(await pipe(new DecompressionStream('deflate-raw'),new Uint8Array(pt)))}
const [mode,code]=process.argv.slice(2);let input='';process.stdin.on('data',d=>input+=d).on('end',async()=>{
 try{process.stdout.write(mode==='seal'?await seal(input,code):await open(input,code))}catch(e){console.error(e);process.exit(3)}});
`

func runNode(t *testing.T, mode, code, stdin string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cross-implementation test skipped")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "helper.js")
	if err := os.WriteFile(script, []byte(nodeHelper), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, script, mode, code)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("node %s failed: %v\n%s", mode, err, ee.Stderr)
		}
		t.Fatalf("node %s failed: %v", mode, err)
	}
	return string(out)
}

func TestGoSealsBrowserOpens(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip()
	}
	blob, err := Seal(sampleSDP, "K7Q4MX")
	if err != nil {
		t.Fatal(err)
	}
	if got := runNode(t, "open", "K7Q4MX", blob); got != sampleSDP {
		t.Fatalf("browser opened a different SDP:\n%q", got)
	}
}

func TestBrowserSealsGoOpens(t *testing.T) {
	blob := runNode(t, "seal", "K7Q4MX", sampleSDP)
	got, err := Open(blob, "K7Q4MX")
	if err != nil {
		t.Fatalf("Go could not open a browser blob: %v", err)
	}
	if got != sampleSDP {
		t.Fatal("Go opened a different SDP")
	}
	if _, err := Open(blob, "WRONG2"); !errors.Is(err, ErrWrongCode) {
		t.Fatalf("browser blob opened under the wrong code: %v", err)
	}
}
