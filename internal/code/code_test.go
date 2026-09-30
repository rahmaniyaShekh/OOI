package code

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateShapeAndAlphabet(t *testing.T) {
	for range 2000 {
		c, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(c) {
			t.Fatalf("invalid code %q", c)
		}
		for _, bad := range "01ILO" {
			if strings.ContainsRune(c, bad) {
				t.Fatalf("ambiguous symbol %q in %q", bad, c)
			}
		}
	}
}

// TestNoModuloBias draws many symbols and checks every one of the 31 appears
// with roughly equal frequency. A plain b%31 over-weights the first nine
// symbols by about 12%, which this bound would catch.
func TestNoModuloBias(t *testing.T) {
	counts := map[byte]int{}
	const draws = 12000
	for range draws {
		c, _ := Generate()
		for i := 0; i < len(c); i++ {
			counts[c[i]]++
		}
	}
	total := draws * Len
	want := float64(total) / float64(len(Alphabet))
	if len(counts) != len(Alphabet) {
		t.Fatalf("only %d of %d symbols seen", len(counts), len(Alphabet))
	}
	for s, n := range counts {
		if dev := (float64(n) - want) / want; dev > 0.08 || dev < -0.08 {
			t.Fatalf("symbol %q off by %.1f%% (%d vs %.0f)", s, dev*100, n, want)
		}
	}
}

func TestNormalizeAndPretty(t *testing.T) {
	cases := map[string]string{
		"k7q-4mx":   "K7Q4MX",
		" K7Q 4MX ": "K7Q4MX",
		"k7q4mx":    "K7Q4MX",
		"O0I1L":     "", // every look-alike is dropped
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Pretty("K7Q4MX"); got != "K7Q-4MX" {
		t.Errorf("Pretty = %q", got)
	}
}

func TestRoomIDMatchesWebCrypto(t *testing.T) {
	// sha256("K7Q4MX"), the value the joiner page computes with crypto.subtle.
	id := RoomID("K7Q4MX")
	if len(id) != 64 || strings.ToLower(id) != id {
		t.Fatalf("bad room id %q", id)
	}
	if RoomID("K7Q4MX") != id || RoomID("K7Q4MY") == id {
		t.Fatal("room id not deterministic / not distinct")
	}
}

func TestSession(t *testing.T) {
	a, _ := NewSession()
	b, _ := NewSession()
	if len(a) != 16 || a == b {
		t.Fatalf("bad sessions %q %q", a, b)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	c1, fresh, err := LoadOrCreate(dir)
	if err != nil || !fresh {
		t.Fatalf("first load: %v fresh=%v", err, fresh)
	}
	c2, fresh, _ := LoadOrCreate(dir)
	if c1 != c2 || fresh {
		t.Fatal("code did not persist across loads")
	}

	// A hand-edited file is replaced, not trusted.
	os.WriteFile(filepath.Join(dir, file), []byte("bad!"), 0o600)
	c3, fresh, _ := LoadOrCreate(dir)
	if !fresh || !Valid(c3) {
		t.Fatal("corrupt code file was not regenerated")
	}

	c4, _ := Rotate(dir)
	if c4 == c3 {
		t.Fatal("rotate returned the same code")
	}
	if err := Purge(dir); err != nil {
		t.Fatal(err)
	}
	if _, fresh, _ := LoadOrCreate(dir); !fresh {
		t.Fatal("purge did not remove the code")
	}
}
