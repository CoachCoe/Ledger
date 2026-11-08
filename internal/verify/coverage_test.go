package verify_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestM3ACCoverage keeps "every case is a named assertion" honest: each M3
// acceptance criterion listed in testdata/m3_acs.txt has a TestAC<id> function
// (or named recovery tests, or is asserted by demos/m3.sh itself), and every
// listed id is selected by the demo script.
func TestM3ACCoverage(t *testing.T) {
	list, err := os.ReadFile("testdata/m3_acs.txt")
	if err != nil {
		t.Fatal(err)
	}
	demo, err := os.ReadFile(filepath.Join("..", "..", "demos", "m3.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for _, pkg := range []string{"evidence", "selection", "verify", "core", "record", "index"} {
		files, _ := filepath.Glob(filepath.Join("..", pkg, "*_test.go"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src.Write(b)
		}
	}
	text := src.String()
	named := regexp.MustCompile(`(?m)^func (TestAC[0-9_]+)[A-Za-z]*`)
	have := map[string]bool{}
	for _, m := range named.FindAllStringSubmatch(text, -1) {
		for _, id := range strings.Split(strings.TrimPrefix(m[1], "TestAC"), "_") {
			have[id] = true
		}
	}
	for _, line := range strings.Split(string(list), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, mark, _ := strings.Cut(line, " ")
		if !regexp.MustCompile(`\b` + id + `\b`).Match(demo) {
			t.Errorf("AC-%s is not selected or checked by demos/m3.sh", id)
		}
		switch {
		case mark == "demo-only":
		case strings.HasPrefix(mark, "tests:"):
			for _, name := range strings.Split(strings.TrimPrefix(mark, "tests:"), ",") {
				if !strings.Contains(text, "func "+name+"(") {
					t.Errorf("AC-%s: recovery test %s not found", id, name)
				}
			}
		default:
			if !have[id] {
				t.Errorf("AC-%s has no TestAC%s function", id, id)
			}
		}
	}
}
