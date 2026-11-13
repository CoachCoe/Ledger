package core

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/verify"
)

type publisher struct{ store *record.Store }

func (p publisher) Publish(ctx context.Context, objs []envelope.Object) (verify.PublishResult, error) {
	r, err := p.store.ImportRecords(ctx, objs)
	if err != nil {
		return verify.PublishResult{}, err
	}
	return verify.PublishResult{Written: r.Imported, Indexed: r.Indexed, Detail: r}, nil
}

func (s *Service) maintenance(ctx context.Context, r api.Request) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.SessionID != "" {
		return nil, apierr.New("unauthorized", "repository maintenance requires local root")
	}
	if _, err := s.root(); err != nil {
		return nil, err
	}
	if r.Operation == "system.recover" {
		report, err := s.Store.Recover(ctx)
		if err == nil {
			err = s.reconcile(ctx)
		}
		return report, err
	}
	if r.Operation == "system.reindex" {
		s.coordMu.Lock()
		defer s.coordMu.Unlock()
		report, err := s.Store.Reindex(ctx)
		if err != nil {
			return nil, err
		}
		s.coordPublished = nil
		digest, err := s.Store.Index.Digest(ctx)
		return map[string]any{"report": report, "index_digest": digest}, err
	}
	in, err := decode[struct {
		Bundle       string   `json:"bundle"`
		Out          string   `json:"out"`
		Manifests    []string `json:"manifests"`
		Intents      []string `json:"intents"`
		AllowPartial bool     `json:"allow_partial"`
		Trust        []string `json:"trust"`
		TrustFile    string   `json:"trust_file"`
		Strict       bool     `json:"strict"`
	}](r.Input)
	if err != nil {
		return nil, err
	}
	// Resolve filesystem arguments against the served repository, never the
	// adapter or daemon process's incidental current directory.
	root := filepath.Dir(s.Store.Dir)
	local := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(root, p)
	}
	if r.Operation == "bundle.export" {
		opts := verify.ExportOptions{Out: local(in.Out), Manifests: in.Manifests, Intents: in.Intents, AllowPartial: in.AllowPartial}
		if s.native() {
			return verify.ExportNative(root, opts)
		}
		return verify.Export(ctx, s.Store.Repo, opts)
	}
	trust := identity.NewTrustList()
	file := local(in.TrustFile)
	if file == "" {
		if name, e := identity.DefaultTrustPath(); e == nil {
			list, e := identity.LoadTrustFile(name)
			if e == nil {
				trust = list
			} else if !errors.Is(e, fs.ErrNotExist) {
				return nil, e
			}
		}
	} else {
		trust, err = identity.LoadTrustFile(file)
		if err != nil {
			return nil, err
		}
	}
	if err := trust.Merge(in.Trust); err != nil {
		return nil, err
	}
	if r.Operation == "bundle.import" {
		opts := verify.ImportOptions{Trusted: trust}
		var report *verify.ImportReport
		if s.native() {
			report, err = verify.ImportNative(ctx, local(in.Bundle), publisher{s.Store}, root, opts)
		} else {
			report, err = verify.Import(ctx, local(in.Bundle), publisher{s.Store}, s.Store.Repo, opts)
		}
		if err != nil {
			return nil, err
		}
		if err := s.reconcile(ctx); err != nil {
			return nil, err
		}
		digest, err := s.Store.Index.Digest(ctx)
		return map[string]any{"import": report, "index_digest": digest}, err
	}
	var src *verify.Source
	if in.Bundle == "" {
		src, err = s.repositorySource(ctx)
	} else {
		src, err = verify.OpenBundleSource(ctx, local(in.Bundle), verify.DefaultLimits())
	}
	if err != nil {
		return nil, err
	}
	defer src.Close()
	report, err := verify.Verify(ctx, src, verify.Options{Trusted: trust})
	if err != nil {
		return nil, err
	}
	if in.Strict && report.Integrity != verify.Verified {
		return nil, apierr.New("verification_failed", "integrity is %s", report.Integrity).With("report", report)
	}
	return report, nil
}

// repositorySource reads this repository's own records for verification.
func (s *Service) repositorySource(ctx context.Context) (*verify.Source, error) {
	if s.native() {
		return verify.OpenNativeSource(filepath.Dir(s.Store.Dir))
	}
	return verify.OpenRepoSource(ctx, s.Store.Repo)
}
