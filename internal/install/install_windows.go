//go:build windows

package install

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// Repo is where releases are published.
const Repo = "rahmaniyaShekh/OOI"

// ExeName is the installed file name.
const ExeName = "ooi.exe"

// Dir is the fixed install location: %LOCALAPPDATA%\Programs\ooi, the standard
// per-user program folder. It never changes, so PATH stays valid across updates.
func Dir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", fmt.Errorf("install: cannot find %%LOCALAPPDATA%%: %w", err)
		}
	}
	return filepath.Join(base, "Programs", "ooi"), nil
}

// Target is the full path of the installed executable.
func Target() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, ExeName), nil
}

// IsInstalledCopy reports whether exe is the installed executable.
func IsInstalledCopy(exe string) bool {
	t, err := Target()
	return err == nil && samePath(exe, t)
}

func samePath(a, b string) bool {
	ca, _ := filepath.Abs(a)
	cb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(ca), filepath.Clean(cb))
}

// Result describes what Install did.
type Result struct {
	Target      string
	Copied      bool // false when already running from the install location
	PathAdded   bool // false when the directory was already on PATH
	PathPresent bool
}

// Install copies src into the install directory and puts that directory on the
// user PATH. Re-running it is safe; it is also how an update is applied.
func Install(src string) (Result, error) {
	var res Result
	dir, err := Dir()
	if err != nil {
		return res, err
	}
	target := filepath.Join(dir, ExeName)
	res.Target = target

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("install: create %s: %w", dir, err)
	}
	CleanupOld()

	if !samePath(src, target) {
		if err := replaceFile(src, target); err != nil {
			return res, err
		}
		res.Copied = true
	}
	// A file downloaded by a browser carries a Mark-of-the-Web stream that makes
	// SmartScreen interrupt every launch. The user chose to install it; drop it.
	os.Remove(target + ":Zone.Identifier")

	added, err := addUserPath(dir)
	if err != nil {
		return res, err
	}
	res.PathAdded = added
	res.PathPresent = true
	return res, nil
}

// replaceFile installs src at dst. If dst is in use (a running ooi), it is first
// renamed aside: Windows forbids deleting a running image but allows renaming it.
func replaceFile(src, dst string) error {
	tmp := dst + ".new"
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		old := dst + ".old"
		os.Remove(old)
		if err := os.Rename(dst, old); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("install: %s is locked (is ooi running? try `ooi stop`): %w", dst, err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("install: place %s: %w", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("install: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("install: create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("install: copy: %w", err)
	}
	return out.Close()
}

// CleanupOld removes the previous executable left behind by an update, once it
// is no longer running. Best effort.
func CleanupOld() {
	if t, err := Target(); err == nil {
		os.Remove(t + ".old")
		os.Remove(t + ".new")
	}
}

// ---------------------------------------------------------------- PATH

const envKey = `Environment`

// UserPath reads the user PATH from HKCU\Environment.
func UserPath() (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("install: open HKCU\\Environment: %w", err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return "", nil
	}
	return v, err
}

func setUserPath(v string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("install: open HKCU\\Environment: %w", err)
	}
	defer k.Close()
	// REG_EXPAND_SZ keeps entries such as %USERPROFILE%\bin working.
	if err := k.SetExpandStringValue("Path", v); err != nil {
		return fmt.Errorf("install: write PATH: %w", err)
	}
	broadcastEnvChange()
	return nil
}

func addUserPath(dir string) (bool, error) {
	cur, err := UserPath()
	if err != nil {
		return false, err
	}
	if PathHas(cur, dir) {
		return false, nil
	}
	return true, setUserPath(PathAdd(cur, dir))
}

func removeUserPath(dir string) (bool, error) {
	cur, err := UserPath()
	if err != nil {
		return false, err
	}
	if !PathHas(cur, dir) {
		return false, nil
	}
	return true, setUserPath(PathRemove(cur, dir))
}

// broadcastEnvChange tells Explorer (and so every terminal opened afterwards)
// that the environment changed, so no sign-out is needed.
func broadcastEnvChange() {
	const (
		hwndBroadcast   = 0xFFFF
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")
	s, _ := syscall.UTF16PtrFromString("Environment")
	var result uintptr
	proc.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(s)),
		smtoAbortIfHung, 3000, uintptr(unsafe.Pointer(&result)))
}

// ---------------------------------------------------------------- uninstall

// Uninstall removes ooi from PATH and deletes the install directory. The data
// directory (the persistent join code) is only removed with purgeData.
func Uninstall(dataDir string, purgeData bool) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if _, err := removeUserPath(dir); err != nil {
		return err
	}
	if purgeData && dataDir != "" {
		os.RemoveAll(dataDir)
	}

	self, _ := os.Executable()
	if self != "" && strings.HasPrefix(strings.ToLower(self), strings.ToLower(dir)+`\`) {
		// A running exe cannot delete itself. Hand the job to a detached cmd that
		// waits for this process to exit first.
		// The command line is passed raw: Go's argument quoting would turn the
		// quotes around the path into \" , which cmd.exe does not understand.
		cmd := exec.Command(`C:\Windows\System32\cmd.exe`)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CmdLine:       fmt.Sprintf(`cmd.exe /d /c "ping 127.0.0.1 -n 3 >nul & rmdir /s /q "%s""`, dir),
			HideWindow:    true,
			CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		}
		return cmd.Start()
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("install: remove %s: %w", dir, err)
	}
	return nil
}

// ---------------------------------------------------------------- update

// Release is the latest published release.
type Release struct {
	Tag    string
	ExeURL string
	SumURL string
}

var httpClient = &http.Client{
	Timeout:   2 * time.Minute,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

// Latest asks GitHub for the newest release.
func Latest(ctx context.Context) (Release, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+Repo+"/releases/latest", nil)
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("user-agent", "ooi-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("update: cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("update: GitHub returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("update: bad release data: %w", err)
	}
	r := Release{Tag: body.TagName}
	for _, a := range body.Assets {
		switch a.Name {
		case ExeName:
			r.ExeURL = a.URL
		case "SHA256SUMS.txt":
			r.SumURL = a.URL
		}
	}
	if r.ExeURL == "" {
		return r, fmt.Errorf("update: release %s has no %s", r.Tag, ExeName)
	}
	return r, nil
}

// Download fetches the release executable to a temporary file and verifies it
// against the published SHA-256. It returns the temp path.
func Download(ctx context.Context, r Release) (string, error) {
	data, err := fetch(ctx, r.ExeURL, 200<<20)
	if err != nil {
		return "", err
	}
	if len(data) < 2 || data[0] != 'M' || data[1] != 'Z' {
		return "", errors.New("update: download is not a Windows executable")
	}
	if r.SumURL == "" {
		return "", errors.New("update: release has no SHA256SUMS.txt; refusing an unverified binary")
	}
	sums, err := fetch(ctx, r.SumURL, 1<<16)
	if err != nil {
		return "", err
	}
	want, ok := findSum(sums, ExeName)
	if !ok {
		return "", errors.New("update: SHA256SUMS.txt does not list " + ExeName)
	}
	got := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return "", errors.New("update: checksum mismatch; download discarded")
	}
	f, err := os.CreateTemp("", "ooi-update-*.exe")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	f.Close()
	return f.Name(), nil
}

func fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("user-agent", "ooi-updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: download HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// findSum reads a `sha256sum`-style file: "<hex>  <name>" per line.
func findSum(b []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return f[0], true
		}
	}
	return "", false
}
