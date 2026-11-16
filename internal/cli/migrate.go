package cli

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/atomicfile"
	"github.com/CoachCoe/Ledger/internal/bridge"
	"github.com/CoachCoe/Ledger/internal/canon"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/index"
	"github.com/CoachCoe/Ledger/internal/lifecycle"
	"github.com/CoachCoe/Ledger/internal/lock"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/schema"
	"github.com/CoachCoe/Ledger/internal/store"
	"github.com/CoachCoe/Ledger/internal/verify"
)

// migrateReason is recorded on every Phase 1 workspace migrate closes.
const migrateReason = "migrated: a Phase 1 workspace cannot continue in the native store"

// cmdMigrate converts the Phase 1 repository of the working directory in
// place (ADR 0027). It writes v2 records beside the v1 ones and rewrites
// none: an import mapping for HEAD's first-parent history and for every
// commit a v1 record names, closing events for Phase 1 workspaces still
// open or proposed, and the repository record, whose tree is HEAD's. The
// content store is moved into place last, so until then the repository
// stays a Phase 1 one and migrate can be run again.
func cmdMigrate(args []string) (result any, err error) {
	if err := noFlags("migrate", args); err != nil {
		return nil, err
	}
	ctx := context.Background()
	repo, err := locate(ctx)
	if err != nil {
		return nil, err
	}
	if repo.Native() {
		return nil, apierr.New("already_native", "%s is already a native repository", repo.Root)
	}
	dir := filepath.Join(repo.Root, record.DirName)
	if err := atomicfile.MakeDir(filepath.Join(dir, "runtime"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "runtime", "init.lock"), false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := verifyPhase1(ctx, repo); err != nil {
		return nil, err
	}
	c, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	keyDir, err := identity.DefaultKeyDir()
	if err != nil {
		return nil, err
	}
	root, err := identity.LocalKeys{Dir: keyDir}.Root()
	if err == nil && root.ID() != c.RootFingerprint {
		err = apierr.New("root_mismatch", "repository root %s does not match the configured key %s", c.RootFingerprint, root.ID())
	}
	if err != nil {
		return nil, err
	}
	format, err := bridge.ObjectFormat(ctx, repo.Root)
	if err != nil {
		return nil, err
	}

	pending := filepath.Join(dir, "runtime", "migrate", record.StoreDir)
	objects, err := store.Open(filepath.Join(pending, "objects"), c.CaseSensitive)
	if err != nil {
		return nil, err
	}
	s, err := record.OpenNative(ctx, repo.Root, record.Options{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := s.Recover(ctx); err != nil {
		return nil, err
	}
	staging, err := bridge.OpenStaging(ctx, stagingDir(dir), format)
	if err != nil {
		return nil, err
	}
	br := &bridge.Bridge{Records: s, Objects: objects, Staging: staging, Root: root}
	head, err := staging.Convert(ctx, objects, repo.Root, "HEAD")
	if err != nil {
		return nil, err
	}
	named, err := s.Index.Phase1Commits(ctx)
	if err != nil {
		return nil, err
	}
	total := len(named)
	named = slices.DeleteFunc(named, func(sha string) bool {
		return slices.ContainsFunc(head.Commits, func(c bridge.Imported) bool { return c.SHA == sha })
	})
	extra, err := staging.ConvertCommits(ctx, objects, repo.Root, named)
	if err != nil {
		return nil, err
	}
	imported, err := br.Publish(ctx, bridge.Fetched{Ref: "HEAD", Commits: append(extra, head.Commits...)})
	if err != nil {
		return nil, err
	}
	made, err := publishMigration(ctx, s, root, c.CaseSensitive, head.Tip().NativeTree)
	if err != nil {
		return nil, err
	}
	if err := writeBridgeConfig(dir, bridgeConfig{Source: repo.Root, Ref: "HEAD", Format: format}); err != nil {
		return nil, err
	}
	if err := os.Rename(pending, filepath.Join(dir, record.StoreDir)); err != nil {
		return nil, err
	}
	made["repository"] = repo.Root
	made["git"] = imported
	made["phase1_commits"] = total
	return made, nil
}

// verifyPhase1 refuses to migrate a repository whose v1 records do not
// verify, so nothing damaged is carried into the native store.
func verifyPhase1(ctx context.Context, repo record.Repository) error {
	src, err := verify.OpenRepoSource(ctx, repo.Git)
	if err != nil {
		return err
	}
	defer src.Close()
	rep, err := verify.Verify(ctx, src, verify.Options{})
	if err != nil {
		return err
	}
	if rep.Integrity != verify.Verified {
		return apierr.New("verification_failed", "the Phase 1 records do not verify (integrity is %s); run ledger verify for the report", rep.Integrity)
	}
	return nil
}

// publishMigration signs the repository record and closes every Phase 1
// workspace still open or proposed, in one operation. A repeated migrate
// finds the record and publishes nothing.
func publishMigration(ctx context.Context, s *record.Store, root identity.Signer, caseSensitive bool, tree string) (map[string]any, error) {
	if r, err := s.Index.Repository(ctx); err == nil {
		return map[string]any{"repository_id": r.ID, "tree": r.Tree, "closed_workspaces": []any{}}, nil
	} else if !apierr.HasCode(err, "not_native") {
		return nil, err
	}
	request, err := canon.Marshal(map[string]string{"operation": "ledger.migrate", "root": root.ID(), "tree": tree})
	if err != nil {
		return nil, err
	}
	op := record.Op{ID: schema.NewID(schema.PrefixOperation), Name: "ledger.migrate", Digest: canon.Hash(request), Signer: root}
	var closed []map[string]string
	res, err := s.Do(ctx, op, func(idx *index.Index) (any, []envelope.Object, error) {
		at := schema.Timestamp(time.Now())
		repo, id, err := repositoryRecord(root, caseSensitive, tree, at)
		if err != nil {
			return nil, nil, err
		}
		members := []envelope.Object{repo}
		closed = nil
		for _, state := range []string{"open", "proposed"} {
			subs, err := idx.SubjectsIn(ctx, lifecycle.Workspace, state)
			if err != nil {
				return nil, nil, err
			}
			for _, sub := range subs {
				if sub.Conflict != "" {
					continue
				}
				reason := migrateReason
				var prev *string
				if sub.Head != "" {
					prev = &sub.Head
				}
				e, err := envelope.SealVersion(2, schema.KindLifecycleEvent, schema.LifecycleEvent{SubjectKind: sub.Kind, SubjectID: sub.ID, Creation: sub.Creation,
					Previous: prev, FromState: sub.State, ToState: "closed", Reason: &reason, Actor: root.ID(), At: at}, root)
				if err != nil {
					return nil, nil, err
				}
				members = append(members, e)
				closed = append(closed, map[string]string{"workspace_id": sub.ID, "state": sub.State})
			}
		}
		return map[string]any{"repository": id, "tree": tree, "closed_workspaces": closed}, members, nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"receipt": res.Receipt, "tree": tree, "closed_workspaces": closed}
	var made struct {
		Repository string `json:"repository"`
	}
	if err := json.Unmarshal(res.Result, &made); err != nil {
		return nil, err
	}
	out["repository_id"] = made.Repository
	return out, nil
}
