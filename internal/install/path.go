// Package install puts ooi.exe in a fixed per-user location and on the user's
// PATH, so `ooi` works from any terminal. No administrator rights are needed:
// everything lives under %LOCALAPPDATA% and HKCU.
package install

import (
	"path/filepath"
	"strings"
)

// These helpers are pure string logic so they can be tested without touching
// the registry.

// normDir canonicalises a PATH entry for comparison: expands nothing, but
// ignores case, surrounding quotes/space and a trailing backslash, which is how
// Windows itself treats two entries as the same directory.
func normDir(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"`)
	p = strings.TrimRight(p, `\/`)
	return strings.ToLower(filepath.Clean(p))
}

// splitPath splits a PATH value, dropping empty entries.
func splitPath(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ";") {
		if strings.TrimSpace(e) != "" {
			out = append(out, e)
		}
	}
	return out
}

// PathHas reports whether dir is already an entry of the PATH value v.
func PathHas(v, dir string) bool {
	want := normDir(dir)
	for _, e := range splitPath(v) {
		if normDir(e) == want {
			return true
		}
	}
	return false
}

// PathAdd appends dir to the PATH value v unless it is already present. Existing
// entries are kept exactly as they were (including %VARIABLES%).
func PathAdd(v, dir string) string {
	if PathHas(v, dir) {
		return v
	}
	entries := splitPath(v)
	entries = append(entries, dir)
	return strings.Join(entries, ";")
}

// PathRemove removes every entry equal to dir from the PATH value v.
func PathRemove(v, dir string) string {
	want := normDir(dir)
	var keep []string
	for _, e := range splitPath(v) {
		if normDir(e) != want {
			keep = append(keep, e)
		}
	}
	return strings.Join(keep, ";")
}
