package verify_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/verify"
)

func TestExportInterrupted(t *testing.T) {
	c := newChain(t, "true")
	for _, stage := range []string{verify.StageExportInventory, verify.StageExportGit, verify.StageExportWrite, verify.StageExportRename} {
		t.Run(stage, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "b.ledger")
			_, err := c.export(verify.ExportOptions{Out: out, Fault: func(s string) error {
				if s == stage {
					return errCrash
				}
				return nil
			}})
			if err == nil {
				t.Fatal("the fault did not stop the export")
			}
			if _, err := os.Stat(out); err == nil {
				t.Fatal("a final file appeared before the export finished")
			}
			if _, err := os.Stat(out + ".partial"); err == nil {
				if _, err := tryVerify(out + ".partial"); code(t, err) != "bundle_partial" {
					t.Fatalf("the partial file must never verify: %v", err)
				}
			}
			rep, err := c.export(verify.ExportOptions{Out: out})
			if err != nil || !rep.Complete {
				t.Fatalf("retry: %v", err)
			}
			if r := verifyBundle(t, out); r.Integrity != verify.Verified {
				t.Fatalf("retried export: %s", r.Integrity)
			}
		})
	}
}

func TestImportStaging(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	dst := newSite(t)
	n, refs := dst.objectCount(), dst.refs()
	_, err := dst.importBundle(path, verify.ImportOptions{Fault: func(s string) error {
		if s == verify.StageImportStaged {
			return errCrash
		}
		return nil
	}})
	if err == nil {
		t.Fatal("no crash")
	}
	dst.stagingEmpty()
	if dst.objectCount() != n || dst.refs() != refs {
		t.Fatal("a crash while staged changed the repository")
	}
	if rep := dst.mustImport(path); rep.Publish.Written == 0 {
		t.Fatal("retry did not import")
	}
}

func fixedOpts(fault func(string) error) record.Options {
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	return record.Options{Now: func() time.Time { return at }, Fault: fault}
}

// advanceRecordsRef moves refs/ledger/records externally, as a fetch would.
func advanceRecordsRef(t *testing.T, s *site) {
	t.Helper()
	ctx := context.Background()
	tip, err := s.repo.ResolveRef(ctx, record.RecordsRef)
	if err != nil || tip == "" {
		t.Fatalf("records ref: %q %v", tip, err)
	}
	tree, err := gitx.Run(ctx, s.dir, nil, gitEnv, "rev-parse", tip+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := gitx.Run(ctx, s.dir, strings.NewReader("external\n"), gitEnv, "commit-tree", strings.TrimSpace(string(tree)), "-p", tip, "-F", "-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(ctx, s.dir, nil, gitEnv, "update-ref", record.RecordsRef, strings.TrimSpace(string(commit)), tip); err != nil {
		t.Fatal(err)
	}
}

func TestImportPublication(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	keys := identity.LocalKeys{Dir: filepath.Join(t.TempDir(), "keys")}
	if _, _, err := keys.Ensure(); err != nil {
		t.Fatal(err)
	}
	base := newSiteWith(t, fixedOpts(nil), keys)
	base.mustImport(path)
	if _, err := base.store.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantDigest, wantRefs := base.digest(), base.refs()

	type point struct {
		name   string
		record bool // fault in the RecordStore rather than the importer
		crash  bool // false: advance the records ref externally and carry on
	}
	var points []point
	for _, s := range []string{verify.StageImportStaged, verify.StageImportGit, verify.StageImportRef, verify.StageImportRecords} {
		points = append(points, point{s, false, true})
	}
	for _, s := range []string{record.StageImportObjects, record.StageObjects, record.StageReceipt, record.StageRef, record.StageIndex} {
		points = append(points, point{s, true, true})
	}
	points = append(points, point{record.StageRef, true, false}, point{record.StageIndex, true, false})
	for _, p := range points {
		name := p.name
		if !p.crash {
			name += "+external_ref_advance"
		}
		t.Run(name, func(t *testing.T) {
			var dst *site
			fired, armed := false, false
			hook := func(s string) error {
				if !armed || s != p.name || fired {
					return nil
				}
				fired = true
				if !p.crash {
					advanceRecordsRef(t, dst)
					return nil
				}
				return errCrash
			}
			opts := verify.ImportOptions{}
			if p.record {
				dst = newSiteWith(t, fixedOpts(hook), keys)
			} else {
				dst = newSiteWith(t, fixedOpts(nil), keys)
				opts.Fault = hook
			}
			armed = true
			_, err := dst.importBundle(path, opts)
			if p.crash && err == nil {
				t.Fatalf("stage %s did not interrupt the import", p.name)
			}
			if !p.crash && err != nil {
				t.Fatal(err)
			}
			dst.stagingEmpty()
			// Restart, retry with the same bundle, reindex.
			dst.open(fixedOpts(nil))
			if _, err := dst.importBundle(path, verify.ImportOptions{}); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if _, err := dst.store.Reindex(context.Background()); err != nil {
				t.Fatal(err)
			}
			if dst.digest() != wantDigest {
				t.Fatalf("durable state differs after interruption at %s", name)
			}
			if proposalRefs(dst.refs()) != proposalRefs(wantRefs) {
				t.Fatalf("refs differ: %q vs %q", dst.refs(), wantRefs)
			}
			src, err := verify.OpenRepoSource(context.Background(), dst.repo)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			if r, err := verify.Verify(context.Background(), src, verify.Options{}); err != nil || r.Integrity != verify.Verified {
				t.Fatalf("verify after recovery: %v %+v", err, r)
			}
		})
	}
}

// proposalRefs keeps only the retention refs; each repository's own
// branches differ because their base commits do.
func proposalRefs(all string) string {
	var out []string
	for _, l := range strings.Split(all, "\n") {
		if strings.Contains(l, "refs/ledger/proposals/") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
