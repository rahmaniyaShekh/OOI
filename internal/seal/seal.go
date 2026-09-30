// Package seal implements the sealed signalling blob shared by the native host
// and the browser joiner.
//
// The format is byte-compatible with the WebCrypto implementation in the joiner
// page, so a blob sealed here opens there and vice versa:
//
//	"OOI1:" + base64url( header || salt || iv || AES-256-GCM(deflate-raw(SDP)) || tag )
//
//	header : 4-byte magic "OOI1" || 1 flag byte (bit0 = encrypted)
//	salt   : 16 random bytes -> PBKDF2-HMAC-SHA256, 200 000 iterations -> 32-byte key
//	iv     : 12 random bytes
//	tag    : 16-byte GCM tag (appended to the ciphertext, as WebCrypto does)
//	AAD    : header || salt || iv, so the flag, salt and iv are authenticated too
//	key    : PBKDF2(code) - the join code is the passphrase
//
// A raw SDP carries LAN and public IPs and the DTLS fingerprint, so it is never
// stored or sent in the clear. The rendezvous only ever sees this ciphertext.
package seal

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// Prefix marks a blob on the wire. It differs from the reference "APP1:" so a
	// blob from an unrelated app on the same rendezvous can never be mistaken
	// for one of ours.
	Prefix = "OOI1:"

	flagEncrypted = 0x01

	saltLen    = 16
	ivLen      = 12
	tagLen     = 16
	keyLen     = 32
	pbkdf2Iter = 200000

	headerLen = 5 + saltLen + ivLen // magic(4) + flag(1) + salt + iv
)

var magic = [4]byte{'O', 'O', 'I', '1'}

// maxPlain bounds how large an inflated SDP may be. A real SDP is a few KB; this
// stops a crafted blob from inflating into a memory bomb.
const maxPlain = 256 * 1024

// ErrWrongCode means authentication failed. GCM cannot distinguish a wrong key
// from a tampered blob, and neither should the caller: both mean "not for you".
var ErrWrongCode = errors.New("seal: wrong code or tampered blob")

// ErrNotBlob means the input does not carry our prefix or magic.
var ErrNotBlob = errors.New("seal: not an OOI blob")

// deriveKey stretches the join code into an AES-256 key.
func deriveKey(code string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, code, salt, pbkdf2Iter, keyLen)
}

// Seal compresses, encrypts and encodes sdp under code.
func Seal(sdp, code string) (string, error) {
	body, err := deflateRaw([]byte(sdp))
	if err != nil {
		return "", err
	}

	salt := make([]byte, saltLen)
	iv := make([]byte, ivLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("seal: salt: %w", err)
	}
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("seal: iv: %w", err)
	}

	head := make([]byte, 0, headerLen)
	head = append(head, magic[:]...)
	head = append(head, flagEncrypted)
	head = append(head, salt...)
	head = append(head, iv...)

	key, err := deriveKey(code, salt)
	if err != nil {
		return "", fmt.Errorf("seal: derive key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	// Seal appends the 16-byte tag to the ciphertext, matching WebCrypto.
	ct := gcm.Seal(nil, iv, body, head)

	out := append(head, ct...)
	return Prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decodes, authenticates, decrypts and inflates a blob under code.
func Open(blob, code string) (string, error) {
	blob = strings.TrimSpace(blob)
	if !strings.HasPrefix(blob, Prefix) {
		return "", ErrNotBlob
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(blob[len(Prefix):], "="))
	if err != nil {
		return "", fmt.Errorf("%w: bad base64", ErrNotBlob)
	}
	if len(raw) < 5 || !bytes.Equal(raw[:4], magic[:]) {
		return "", ErrNotBlob
	}

	// Unencrypted blobs are accepted for interop, but only when the flag says so.
	if raw[4]&flagEncrypted == 0 {
		plain, err := inflateRaw(raw[5:])
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}

	if len(raw) < headerLen+tagLen {
		return "", ErrWrongCode
	}
	head := raw[:headerLen]
	salt := raw[5 : 5+saltLen]
	iv := raw[5+saltLen : headerLen]

	key, err := deriveKey(code, salt)
	if err != nil {
		return "", fmt.Errorf("seal: derive key: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	body, err := gcm.Open(nil, iv, raw[headerLen:], head)
	if err != nil {
		return "", ErrWrongCode
	}
	plain, err := inflateRaw(body)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: aes: %w", err)
	}
	gcm, err := cipher.NewGCMWithTagSize(block, tagLen)
	if err != nil {
		return nil, fmt.Errorf("seal: gcm: %w", err)
	}
	return gcm, nil
}

// deflateRaw produces a raw DEFLATE stream (no zlib header), the same framing
// as the browser's CompressionStream('deflate-raw').
func deflateRaw(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("seal: deflate: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return nil, fmt.Errorf("seal: deflate: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("seal: deflate: %w", err)
	}
	return buf.Bytes(), nil
}

func inflateRaw(b []byte) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(b))
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxPlain+1))
	if err != nil {
		return nil, fmt.Errorf("seal: inflate: %w", err)
	}
	if len(out) > maxPlain {
		return nil, errors.New("seal: inflated blob too large")
	}
	return out, nil
}
