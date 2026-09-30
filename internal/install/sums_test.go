//go:build windows

package install

import "testing"

func TestFindSum(t *testing.T) {
	h := "a3f1c9e0b7d24e6f8a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f50617"
	sums := []byte(h + "  ooi.exe\n" + "0000000000000000000000000000000000000000000000000000000000000000 *install.ps1\n")
	if got, ok := findSum(sums, "ooi.exe"); !ok || got != h {
		t.Fatalf("findSum = %q %v", got, ok)
	}
	if _, ok := findSum(sums, "install.ps1"); !ok {
		t.Fatal("binary-mode (*name) entry not found")
	}
	if _, ok := findSum(sums, "missing.exe"); ok {
		t.Fatal("found a name that is not listed")
	}
	if _, ok := findSum([]byte("short  ooi.exe\n"), "ooi.exe"); ok {
		t.Fatal("accepted a malformed hash")
	}
}

func TestDirIsUnderLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\x\AppData\Local`)
	d, err := Dir()
	if err != nil || d != `C:\Users\x\AppData\Local\Programs\ooi` {
		t.Fatalf("Dir = %q, %v", d, err)
	}
	if !IsInstalledCopy(`c:\users\x\appdata\local\programs\OOI\ooi.exe`) {
		t.Fatal("case-insensitive install path not recognised")
	}
}
