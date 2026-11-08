package identity

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAC089_TrustList(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	dir := t.TempDir()
	path := filepath.Join(dir, "trusted-roots.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"roots":[{"fingerprint":"`+fp+`","label":"me"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := LoadTrustFile(path)
	if err != nil || !l.Contains(fp) || l.Contains(strings.Repeat("cd", 32)) {
		t.Fatalf("load: %v %v", l, err)
	}
	if _, err := LoadTrustFile(filepath.Join(dir, "none.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	for name, body := range map[string]string{
		"version": `{"version":2,"roots":[]}`,
		"unknown": `{"version":1,"roots":[],"x":1}`,
		"bad fp":  `{"version":1,"roots":[{"fingerprint":"ABC"}]}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTrustFile(path); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	l = NewTrustList()
	if err := l.Merge([]string{fp}); err != nil || !l.Contains(fp) {
		t.Fatal("flag merge")
	}
	if err := l.Merge([]string{"nothex"}); err == nil {
		t.Fatal("invalid flag accepted")
	}
	var nilList *TrustList
	if nilList.Contains(fp) {
		t.Fatal("nil list trusts nothing")
	}
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if p, _ := DefaultTrustPath(); p != "/x/ledger/trusted-roots.json" {
		t.Fatal(p)
	}
}
