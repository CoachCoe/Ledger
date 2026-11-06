package capability

import (
	"fmt"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/CoachCoe/Ledger/internal/schema"
)

func TestPathLanguage(t *testing.T) {
	// AC-066–072, AC-160, AC-162, AC-243.
	for _, p := range []string{"", "/a", "a/../b", "a/./b", "a//b", `a\b`, "C:/a", "a/*", "a/?", "a/[b]", "a/{b}", "a/", "a\x00b"} {
		if _, err := Parse(p, true); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
	for _, p := range []string{"**", "src/**", "src/a.ts"} {
		if _, err := Parse(p, true); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := Normalize("Src/Cafe\u0301.ts", false)
	b, _ := Normalize("src/caf\u00e9.ts", false)
	if a != b {
		t.Fatalf("aliases: %q %q", a, b)
	}
}
func capFor(id string) schema.Capability {
	return schema.Capability{ID: id, Allow: []string{"src/**"}, Deny: []string{"src/private/**"}, ExpiresAt: "2099-01-01T00:00:00Z", CreatedAt: "2020-01-01T00:00:00Z"}
}
func TestNarrowingAndInheritedDenies(t *testing.T) {
	// AC-073–075, AC-159, AC-163–164, AC-245.
	parent := capFor("parent")
	child := capFor("child")
	child.Deny = []string{}
	child.Issuer = schema.Issuer{Kind: "capability", CapabilityID: parent.ID}
	if err := Narrow(parent, child, true); err != nil {
		t.Fatal(err)
	}
	chain := []schema.Capability{child, parent}
	if err := Authorize(chain, "src/a.ts", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Authorize(chain, "src/private/a.ts", true, time.Now()); err == nil {
		t.Fatal("lost parent deny")
	}
	for _, mutate := range []func(*schema.Capability){func(c *schema.Capability) { c.Allow = []string{"**"} }, func(c *schema.Capability) { c.ExpiresAt = "2100-01-01T00:00:00Z" }, func(c *schema.Capability) { c.MayAddDependencies = true }} {
		widened := child
		mutate(&widened)
		if Narrow(parent, widened, true) == nil {
			t.Fatal("accepted widening")
		}
	}
	parent.ExpiresAt = "2021-01-01T00:00:00Z"
	if Authorize([]schema.Capability{parent}, "src/a.ts", true, time.Now()) == nil {
		t.Fatal("expired")
	}
}
func TestDelegatedPathsNeverWiden(t *testing.T) {
	// Generate valid path families, including inherited denies omitted by the
	// child. Every accepted delegated path must remain parent-authorized.
	property := func(a, b uint32, denied bool) bool {
		parent := capFor("p")
		child := capFor("c")
		prefix := fmt.Sprintf("src/%x", a)
		p := fmt.Sprintf("%s/%x.ts", prefix, b)
		child.Allow = []string{prefix + "/**"}
		child.Deny = []string{}
		if denied {
			parent.Deny = []string{p}
		} else {
			parent.Deny = []string{prefix + "/private/**"}
		}
		if Narrow(parent, child, true) != nil {
			return false
		}
		childErr := Authorize([]schema.Capability{child, parent}, p, true, time.Now())
		parentErr := Authorize([]schema.Capability{parent}, p, true, time.Now())
		return (childErr == nil) == !denied && (childErr != nil || parentErr == nil)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Fatal(err)
	}

	parent := capFor("p")
	child := capFor("c")
	child.Allow = []string{"src/public/**"}
	child.Deny = nil
	if err := Narrow(parent, child, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"src/public/a", "src/private/a", "src/a", "src/publicity/a", "other/a"} {
		if Authorize([]schema.Capability{child, parent}, p, true, time.Now()) == nil && Authorize([]schema.Capability{parent}, p, true, time.Now()) != nil {
			t.Fatal("widened", p)
		}
	}
}
func TestDependencyFiles(t *testing.T) {
	// AC-109: operation type is irrelevant; both rename sides are checked.
	c := capFor("c")
	c.Allow = []string{"**"}
	for _, p := range []string{"package.json", "nested/package-lock.json", "pnpm-lock.yaml", "yarn.lock", "pyproject.toml", "requirements-dev.txt", "Pipfile.lock", "poetry.lock", "uv.lock", "setup.py", "setup.cfg"} {
		if Authorize([]schema.Capability{c}, p, true, time.Now()) == nil {
			t.Error("allowed", p)
		}
	}
}
func FuzzNormalize(f *testing.F) {
	for _, p := range []string{"src/a", "../a", "a/../b", "a\u0301/b", "src/**"} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		n, err := Normalize(p, true)
		if err != nil {
			return
		}
		if strings.HasPrefix(n, "/") || strings.Contains(n, "\\") {
			t.Fatal(n)
		}
		for s := range strings.SplitSeq(n, "/") {
			if s == ".." || s == "." || s == "" {
				t.Fatal(n)
			}
		}
		n2, err := Normalize(n, true)
		if err != nil || n != n2 {
			t.Fatal("not idempotent")
		}
	})
}

func TestPatternBoundariesAndCasePolicy(t *testing.T) {
	for _, tc := range []struct {
		pattern, path   string
		sensitive, want bool
	}{
		{"src/**", "src/a.ts", true, true}, {"src/**", "src", true, false}, {"src/**", "src2/a.ts", true, false}, {"src/a.ts", "src/a.ts", true, true}, {"src/a.ts", "src/a.ts/more", true, false}, {"**", "anything", true, true}, {"SRC/**", "src/a.ts", false, true}, {"SRC/**", "src/a.ts", true, false}, {"src/Caf\u00e9.ts", "src/Cafe\u0301.ts", true, true},
	} {
		pat, err := Parse(tc.pattern, tc.sensitive)
		if err != nil {
			t.Fatal(err)
		}
		n, err := Normalize(tc.path, tc.sensitive)
		if err != nil {
			t.Fatal(err)
		}
		if pat.matches(n) != tc.want {
			t.Errorf("%q vs %q sensitive=%v", tc.pattern, tc.path, tc.sensitive)
		}
	}
}
