//go:build windows

package install

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The repository is private, so downloading a release needs a GitHub token with
// read access to it. The token is stored encrypted with DPAPI, bound to this
// Windows user account: another account, or the file copied to another machine,
// cannot decrypt it.

const tokenFile = "github.token"

// ErrNoToken means no token is configured anywhere.
var ErrNoToken = errors.New("install: no GitHub token configured")

// tokenEnvVars are checked, in order, before the saved token.
var tokenEnvVars = []string{"OOI_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN"}

// Token returns a GitHub token from the environment or the saved store.
func Token(dataDir string) (string, error) {
	for _, v := range tokenEnvVars {
		if t := strings.TrimSpace(os.Getenv(v)); t != "" {
			return t, nil
		}
	}
	b, err := os.ReadFile(filepath.Join(dataDir, tokenFile))
	if err != nil {
		return "", ErrNoToken
	}
	plain, err := unprotect(b)
	if err != nil {
		return "", fmt.Errorf("install: saved token unreadable (re-enter it): %w", err)
	}
	return strings.TrimSpace(string(plain)), nil
}

// SaveToken stores the token encrypted for the current user.
func SaveToken(dataDir, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("install: empty token")
	}
	enc, err := protect([]byte(token))
	if err != nil {
		return fmt.Errorf("install: encrypt token: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, tokenFile), enc, 0o600)
}

// ForgetToken deletes the saved token.
func ForgetToken(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, tokenFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// entropy binds the ciphertext to this application, so another program running
// as the same user cannot simply call CryptUnprotectData on the file.
var entropy = []byte("ooi/github-release-token/v1")

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func protect(plain []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptProtectData(blob(plain), nil, blob(entropy), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func unprotect(enc []byte) ([]byte, error) {
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(enc), nil, blob(entropy), 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}
