//go:build windows

package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func clearTokenEnv(t *testing.T) {
	for _, v := range tokenEnvVars {
		t.Setenv(v, "")
	}
}

func TestTokenRoundTripIsEncrypted(t *testing.T) {
	clearTokenEnv(t)
	dir := t.TempDir()
	const secret = "github_pat_TESTONLY_1234567890abcdef"
	if err := SaveToken(dir, "  "+secret+"\n"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, tokenFile))
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("github_pat")) {
		t.Fatal("token stored in plaintext")
	}
	got, err := Token(dir)
	if err != nil || got != secret {
		t.Fatalf("Token = %q, %v", got, err)
	}
	if err := ForgetToken(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(dir); err != ErrNoToken {
		t.Fatalf("after forget: %v", err)
	}
}

func TestTamperedTokenIsRejected(t *testing.T) {
	clearTokenEnv(t)
	dir := t.TempDir()
	SaveToken(dir, "github_pat_x")
	p := filepath.Join(dir, tokenFile)
	b, _ := os.ReadFile(p)
	b[len(b)/2] ^= 0xFF
	os.WriteFile(p, b, 0o600)
	if _, err := Token(dir); err == nil {
		t.Fatal("a tampered token decrypted")
	}
}

func TestEnvironmentTokenWins(t *testing.T) {
	clearTokenEnv(t)
	dir := t.TempDir()
	SaveToken(dir, "saved")
	t.Setenv("GH_TOKEN", "from-env")
	if got, _ := Token(dir); got != "from-env" {
		t.Fatalf("Token = %q, want the environment value", got)
	}
}
