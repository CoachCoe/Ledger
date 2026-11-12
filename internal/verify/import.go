package verify

import (
	"context"
	"os"
	"path/filepath"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/gitx"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

// Import stages, in order. ImportOptions.Fault is called before each.
const (
	StageImportStaged  = "import_staged"
	StageImportGit     = "import_git"
	StageImportRef     = "import_ref"
	StageImportRecords = "import_records"
	StageImportStore   = "import_store"
	StageImportOpLogs  = "import_oplogs"
)

// PublishResult is what a Publisher reports.
type PublishResult struct {
	// Written counts record files that were not present before.
	Written int `json:"written"`
	// Indexed counts groups newly indexed.
	Indexed int `json:"indexed"`
	// Detail is the publisher's own report.
	Detail any `json:"detail,omitempty"`
}

// Publisher publishes verified records through the RecordStore (the verify
// package cannot import it). Publishing must be idempotent and must never
// choose between conflicting records.
type Publisher interface {
	Publish(ctx context.Context, objs []envelope.Object) (PublishResult, error)
}

// ImportOptions configures Import.
type ImportOptions struct {
	Limits  Limits
	Trusted *identity.TrustList
	// Fault, if set, is called before each import stage; an error stops the
	// import there, as a crash would.
	Fault func(stage string) error
}

// RefResult is the outcome for one retention ref.
type RefResult struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	// Status is "created", "present" or "ref_conflict".
	Status string `json:"status"`
	Local  string `json:"local,omitempty"`
}

// ImportReport describes an import. Trust is never granted by importing:
// the verification inside it uses only the local trust list.
type ImportReport struct {
	Verification   *Report       `json:"verification"`
	Refs           []RefResult   `json:"refs"`
	Records        int           `json:"records"`
	AlreadyPresent bool          `json:"already_present"`
	Publish        PublishResult `json:"publish"`
	// Objects counts store objects a v2 bundle added, and OpLogs reports
	// each operation log it carried.
	Objects int           `json:"objects,omitzero"`
	OpLogs  []OpLogResult `json:"oplogs,omitzero"`
}

// OpLogResult is the outcome for one operation log. Status is "created",
// "present" (the local log already starts with every bundled entry) or
// "oplog_conflict", in which case the local log is left alone.
type OpLogResult struct {
	Workspace string `json:"workspace"`
	Head      string `json:"head"`
	Status    string `json:"status"`
}

func importFault(o ImportOptions, stage string) error {
	if o.Fault == nil {
		return nil
	}
	return o.Fault(stage)
}

// Import stages and verifies a bundle, retains its proposal commits, and
// publishes its records. Nothing is published unless verification finds the
// bundle valid and complete. It never imports private keys, trust, config,
// hooks, sessions' liveness or code branches; it creates only
// refs/ledger/proposals/<manifest> and never overwrites one that names a
// different commit. Importing again changes nothing.
func Import(ctx context.Context, bundle string, dst Publisher, repo *gitx.Repo, opts ImportOptions) (*ImportReport, error) {
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	root := filepath.Join(repo.MainDir, ".ledger", "runtime", "import")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, schema.NewID(schema.PrefixOperation))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	src, err := openStaged(ctx, bundle, dir, opts.Limits)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if err := importFault(opts, StageImportStaged); err != nil {
		return nil, err
	}
	rep, err := Verify(ctx, src, Options{Trusted: opts.Trusted})
	if err != nil {
		return nil, err
	}
	switch {
	case rep.Integrity == Invalid:
		return nil, apierr.New("bundle_invalid", "the bundle failed verification; nothing was imported").With("report", rep)
	case !rep.Complete || len(rep.Missing) > 0 || (src.Strict && len(rep.Pending) > 0):
		return nil, apierr.New("bundle_incomplete", "the bundle is missing dependencies; nothing was imported").With("report", rep)
	}
	out := &ImportReport{Verification: rep, Refs: []RefResult{}, Records: len(src.Corpus.Objects)}
	if err := importFault(opts, StageImportGit); err != nil {
		return nil, err
	}
	if err := fetchGit(ctx, repo, src, opts, out); err != nil {
		return nil, err
	}
	if err := importFault(opts, StageImportRecords); err != nil {
		return nil, err
	}
	objs := make([]envelope.Object, 0, len(src.Corpus.Objects))
	for _, h := range sortedKeys(src.Corpus.Objects) {
		objs = append(objs, src.Corpus.Objects[h])
	}
	res, err := dst.Publish(ctx, objs)
	if err != nil {
		return nil, err
	}
	out.Publish = res
	out.AlreadyPresent = res.Written == 0 && res.Indexed == 0
	return out, nil
}

// fetchGit brings the staged commits into the repository and creates only
// the retention refs, create-only.
func fetchGit(ctx context.Context, repo *gitx.Repo, src *Source, opts ImportOptions, out *ImportReport) error {
	inv := src.Inventory
	if inv == nil || len(inv.Git.Commits) == 0 || src.Staged.Repo == nil {
		return nil
	}
	args := []string{"-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "-q", src.Staged.Repo.Dir}
	for _, c := range inv.Git.Commits {
		args = append(args, ProposalRefPrefix+c.Manifest)
	}
	if _, err := gitx.Run(ctx, repo.Dir, nil, nil, args...); err != nil {
		return apierr.New("import_git_failed", "fetching the staged Git objects: %v", err)
	}
	if err := importFault(opts, StageImportRef); err != nil {
		return err
	}
	for _, c := range inv.Git.Commits {
		ref := ProposalRefPrefix + c.Manifest
		r := RefResult{Ref: ref, Commit: c.Commit}
		local, err := repo.ResolveRef(ctx, ref)
		if err != nil {
			return err
		}
		switch {
		case local == c.Commit:
			r.Status = "present"
		case local != "":
			r.Status, r.Local = "ref_conflict", local
		default:
			if err := repo.UpdateRefCAS(ctx, ref, c.Commit, ""); err != nil {
				return err
			}
			r.Status = "created"
		}
		out.Refs = append(out.Refs, r)
	}
	return nil
}
