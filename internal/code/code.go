// Package code owns the persistent 6-character join code.
//
// The code belongs to the device, not the session: it is generated once, stored
// under %LOCALAPPDATA%\ooi\host.code, and reused on every start so the friend
// can rejoin tomorrow without anyone re-sending anything.
//
// It is both the address and the key. SHA-256(code) tells the rendezvous where
// the offer lives; PBKDF2(code) decrypts it. The code itself is never sent.
package code

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Alphabet has 31 unambiguous symbols: no 0/O and no 1/I/L, so a code survives
// being read aloud or typed on a phone.
const Alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// Len is the code length. 31^6 is about 887 million codes.
const Len = 6

// limit is the largest multiple of len(Alphabet) that fits in a byte. Bytes at or
// above it are rejected so that no symbol is favoured (a plain b%31 is biased,
// because 256 is not a multiple of 31).
var limit = byte(256 - (256 % len(Alphabet))) // 248

// Generate returns a fresh code from the OS CSPRNG with rejection sampling.
func Generate() (string, error) {
	var out strings.Builder
	buf := make([]byte, 16)
	for out.Len() < Len {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("code: random: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue // reject the biased tail
			}
			out.WriteByte(Alphabet[int(b)%len(Alphabet)])
			if out.Len() == Len {
				break
			}
		}
	}
	return out.String(), nil
}

// Normalize accepts anything a person types: lower case, spaces, hyphens, and
// drops characters outside the alphabet (including the look-alikes).
func Normalize(s string) string {
	var out strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(Alphabet, r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// Valid reports whether c is a well-formed, already-normalized code.
func Valid(c string) bool {
	return len(c) == Len && Normalize(c) == c
}

// Pretty renders ABCDEF as ABC-DEF. The hyphen is cosmetic and stripped before
// hashing.
func Pretty(c string) string {
	if len(c) > 3 {
		return c[:3] + "-" + c[3:]
	}
	return c
}

// RoomID is the rendezvous address: 64 lowercase hex characters.
func RoomID(c string) string {
	sum := sha256.Sum256([]byte(c))
	return hex.EncodeToString(sum[:])
}

// NewSession returns a random 16-hex-character session id. Every connection
// attempt gets a fresh one, so a reconnect can never consume a stale answer.
func NewSession() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("code: session: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// file is the persisted code inside the app data directory.
const file = "host.code"

// LoadOrCreate returns the persisted code, generating and storing one on first
// run. A truncated or hand-edited file is treated as missing and replaced.
func LoadOrCreate(dir string) (string, bool, error) {
	p := filepath.Join(dir, file)
	if b, err := os.ReadFile(p); err == nil {
		if c := strings.TrimSpace(string(b)); Valid(c) {
			return c, false, nil
		}
	}
	c, err := Generate()
	if err != nil {
		return "", false, err
	}
	if err := save(p, c); err != nil {
		return "", false, err
	}
	return c, true, nil
}

// Rotate replaces the persisted code. It is also how access is revoked from
// everyone holding the old one.
func Rotate(dir string) (string, error) {
	c, err := Generate()
	if err != nil {
		return "", err
	}
	if err := save(filepath.Join(dir, file), c); err != nil {
		return "", err
	}
	return c, nil
}

// Purge deletes the persisted code so the next start behaves like a new device.
func Purge(dir string) error {
	err := os.Remove(filepath.Join(dir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func save(p, c string) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(c+"\n"), 0o600); err != nil {
		return fmt.Errorf("code: write: %w", err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("code: commit: %w", err)
	}
	return nil
}
