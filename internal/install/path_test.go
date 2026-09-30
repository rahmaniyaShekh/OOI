package install

import "testing"

const dir = `C:\Users\me\AppData\Local\Programs\ooi`

func TestPathAddIsIdempotent(t *testing.T) {
	v := `C:\Windows\system32;%USERPROFILE%\bin`
	got := PathAdd(v, dir)
	if got != v+";"+dir {
		t.Fatalf("PathAdd = %q", got)
	}
	if again := PathAdd(got, dir); again != got {
		t.Fatalf("second add changed PATH: %q", again)
	}
}

func TestPathHasIgnoresCaseSlashAndQuotes(t *testing.T) {
	for _, v := range []string{
		`c:\users\me\appdata\local\programs\OOI`,
		`C:\Users\me\AppData\Local\Programs\ooi\`,
		`"C:\Users\me\AppData\Local\Programs\ooi"`,
		`x;  C:\Users\me\AppData\Local\Programs\ooi  ;y`,
	} {
		if !PathHas(v, dir) {
			t.Errorf("PathHas(%q) = false", v)
		}
	}
	if PathHas(`C:\Users\me\AppData\Local\Programs\ooi2`, dir) {
		t.Error("prefix match counted as the same directory")
	}
}

func TestPathRemoveKeepsOthers(t *testing.T) {
	v := `A;` + dir + `;%SystemRoot%;` + dir + `\;B`
	got := PathRemove(v, dir)
	if got != `A;%SystemRoot%;B` {
		t.Fatalf("PathRemove = %q", got)
	}
}

func TestEmptyAndMessyPath(t *testing.T) {
	if got := PathAdd("", dir); got != dir {
		t.Fatalf("add to empty = %q", got)
	}
	if got := PathAdd(";;A;;", dir); got != "A;"+dir {
		t.Fatalf("add to messy = %q", got)
	}
	if got := PathRemove(dir, dir); got != "" {
		t.Fatalf("remove only entry = %q", got)
	}
}
