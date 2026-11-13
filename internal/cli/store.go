package cli

import (
	"context"
	"flag"
	"io"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/record"
	"github.com/CoachCoe/Ledger/internal/workspace"
)

func noFlags(name string, args []string) error {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		return apierr.New("usage", "ledger %s takes no arguments", name)
	}
	return nil
}

// openStore opens the RecordStore of the working directory's repository
// with the workspace provider that completes its journaled effects. A
// Phase 1 repository is opened only when readOnly is set, and then has no
// provider: nothing may change it before it is migrated.
func openStore(ctx context.Context, readOnly bool) (*record.Store, workspace.NativeProvider, error) {
	var none workspace.NativeProvider
	repo, err := locate(ctx)
	if err != nil {
		return nil, none, err
	}
	if !repo.Native() && !readOnly {
		return nil, none, migrateRequired(repo.Root)
	}
	s, err := repo.Open(ctx, record.Options{})
	if err != nil {
		return nil, none, err
	}
	if !repo.Native() {
		return s, none, nil
	}
	c, err := readConfig(s.Dir)
	if err != nil {
		_ = s.Close()
		return nil, none, err
	}
	provider, err := workspace.OpenNative(s.Dir, c.CaseSensitive)
	if err != nil {
		_ = s.Close()
		return nil, none, err
	}
	s.SetEffectHandler(provider.Apply)
	return s, provider, nil
}

func withStore(name string, args []string, readOnly bool, fn func(context.Context, *record.Store, workspace.NativeProvider) (any, error)) (result any, err error) {
	if err := noFlags(name, args); err != nil {
		return nil, err
	}
	ctx := context.Background()
	s, provider, err := openStore(ctx, readOnly)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	return fn(ctx, s, provider)
}

func cmdReindex(args []string) (any, error) {
	return withStore("reindex", args, false, func(ctx context.Context, s *record.Store, provider workspace.NativeProvider) (any, error) {
		r, err := s.Reindex(ctx)
		if err != nil {
			return nil, err
		}
		workspaces, err := restoreWorkspaces(ctx, s, provider)
		if err != nil {
			return nil, err
		}
		digest, err := s.Index.Digest(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"report": r, "index_digest": digest, "workspace_recovery": workspaces}, nil
	})
}

func cmdRecover(args []string) (any, error) {
	return withStore("recover", args, false, func(ctx context.Context, s *record.Store, _ workspace.NativeProvider) (any, error) {
		return s.Recover(ctx)
	})
}

func cmdStatus(args []string) (any, error) {
	return withStore("status", args, true, func(ctx context.Context, s *record.Store, _ workspace.NativeProvider) (any, error) {
		subjects, err := s.Index.Subjects(ctx)
		if err != nil {
			return nil, err
		}
		digest, err := s.Index.Digest(ctx)
		if err != nil {
			return nil, err
		}
		type entry struct {
			ID       string `json:"id"`
			Kind     string `json:"kind"`
			State    string `json:"state"`
			Owner    string `json:"owner"`
			Conflict string `json:"conflict,omitempty"`
		}
		out := make([]entry, 0, len(subjects))
		for _, sub := range subjects {
			out = append(out, entry{sub.ID, sub.Kind, sub.State, sub.Owner, sub.Conflict})
		}
		return map[string]any{"subjects": out, "index_digest": digest}, nil
	})
}
