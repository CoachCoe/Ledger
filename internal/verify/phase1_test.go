package verify_test

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// P2-AC-021: a bundle exported by the Phase 1 binary from demos/m3.sh still
// verifies, with the same report it produced then. The binary can no longer
// write v1 records, so the bundle is kept as a file.
func TestPhase1BundleVerifiesUnchanged(t *testing.T) {
	raw, err := os.ReadFile("testdata/m3-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Root   string `json:"root"`
		Report any    `json:"report"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src, err := verify.OpenBundleSource(ctx, "testdata/m3-v1.ledger", verify.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	trust := identity.NewTrustList()
	if err := trust.Merge([]string{want.Root}); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Verify(ctx, src, verify.Options{Trusted: trust})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	delete(got, "source")
	if !reflect.DeepEqual(got, want.Report) {
		t.Fatalf("report changed:\n got %s\nwant %v", out, want.Report)
	}
}

var gitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@invalid"}

// v1repo is a Phase 1 repository written by the last binary that wrote
// v1 records: two revisions of one change, evidence on both, a judgment
// and a selection of the second. This binary only reads it.
type v1repo struct {
	dir       string
	repo      *gitx.Repo
	root      string
	manifests []string
}

func newV1Repo(t *testing.T) *v1repo {
	t.Helper()
	var meta struct {
		Root      string   `json:"root"`
		Manifests []string `json:"manifests"`
	}
	raw, err := os.ReadFile("testdata/m3-v1-repo.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f, err := os.Open("testdata/m3-v1-repo.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	repo, err := gitx.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return &v1repo{dir: dir, repo: repo, root: meta.Root, manifests: meta.Manifests}
}

// commit is the proposal commit Phase 1 retained for manifest.
func (r *v1repo) commit(t *testing.T, manifest string) string {
	t.Helper()
	sha, err := r.repo.ResolveRef(context.Background(), verify.ProposalRefPrefix+manifest)
	if err != nil || sha == "" {
		t.Fatalf("no retained commit for %s: %v", manifest, err)
	}
	return sha
}

func (r *v1repo) export(t *testing.T, opts verify.ExportOptions) (*verify.ExportReport, error) {
	t.Helper()
	if opts.Out == "" {
		opts.Out = filepath.Join(t.TempDir(), "v1.ledger")
	}
	return verify.Export(context.Background(), r.repo, opts)
}

func (r *v1repo) mustExport(t *testing.T) string {
	t.Helper()
	rep, err := r.export(t, verify.ExportOptions{})
	if err != nil || !rep.Complete {
		t.Fatalf("export: %v %+v", err, rep)
	}
	return rep.Out
}

// A Phase 1 repository still verifies in place and still exports a v1
// bundle that verifies, both trusted under its root.
func TestPhase1RepositoryVerifiesAndExports(t *testing.T) {
	r := newV1Repo(t)
	ctx := context.Background()
	src, err := verify.OpenRepoSource(ctx, r.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	trust := identity.NewTrustList()
	if err := trust.Merge([]string{r.root}); err != nil {
		t.Fatal(err)
	}
	rep, err := verify.Verify(ctx, src, verify.Options{Trusted: trust})
	if err != nil || rep.Integrity != verify.Verified || rep.Trust != verify.Trusted || len(rep.Manifests) != 2 {
		t.Fatalf("in place: %v %+v", err, rep)
	}
	b := verifyBundle(t, r.mustExport(t), r.root)
	if b.BundleVersion != 1 || b.Integrity != verify.Verified || b.Trust != verify.Trusted || len(b.Manifests) != 2 {
		t.Fatalf("exported: v%d %s/%s %d", b.BundleVersion, b.Integrity, b.Trust, len(b.Manifests))
	}
}
