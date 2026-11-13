package verify_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/verify"
)

func TestExportInterrupted(t *testing.T) {
	c := newChain(t, "true")
	for _, stage := range []string{verify.StageExportInventory, verify.StageExportWrite, verify.StageExportRename} {
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
	dst := c.earlier()
	n, logs := dst.objectCount(), dst.logs()
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
	if dst.objectCount() != n || dst.logs() != logs {
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

func TestImportPublication(t *testing.T) {
	c := newChain(t, "true")
	path := c.mustExport()
	base := c.earlierWith(fixedOpts(nil))
	base.mustImport(path)
	if _, err := base.store.Reindex(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantDigest, wantLogs := base.digest(), base.logs()

	type point struct {
		name   string
		record bool // fault in the RecordStore rather than the importer
	}
	var points []point
	for _, s := range []string{verify.StageImportStaged, verify.StageImportStore, verify.StageImportOpLogs, verify.StageImportRecords} {
		points = append(points, point{s, false})
	}
	for _, s := range []string{record.StageImportObjects, record.StageObjects, record.StageReceipt, record.StageLog, record.StageIndex} {
		points = append(points, point{s, true})
	}
	for _, p := range points {
		t.Run(p.name, func(t *testing.T) {
			fired, armed := false, false
			hook := func(s string) error {
				if !armed || s != p.name || fired {
					return nil
				}
				fired = true
				return errCrash
			}
			opts := verify.ImportOptions{}
			var dst *site
			if p.record {
				dst = c.earlierWith(fixedOpts(hook))
			} else {
				dst = c.earlierWith(fixedOpts(nil))
				opts.Fault = hook
			}
			armed = true
			if _, err := dst.importBundle(path, opts); err == nil {
				t.Fatalf("stage %s did not interrupt the import", p.name)
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
				t.Fatalf("durable state differs after interruption at %s", p.name)
			}
			if dst.logs() != wantLogs {
				t.Fatalf("operation logs differ: %q vs %q", dst.logs(), wantLogs)
			}
			if r, err := verify.Verify(context.Background(), dst.source(), verify.Options{}); err != nil || r.Integrity != verify.Verified {
				t.Fatalf("verify after recovery: %v %+v", err, r)
			}
		})
	}
}
