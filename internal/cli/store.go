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

func withStore(name string, args []string, fn func(context.Context, *record.Store) (any, error)) (result any, err error) {
	if err := noFlags(name, args); err != nil {
		return nil, err
	}
	ctx := context.Background()
	repo, err := openRepo(ctx)
	if err != nil {
		return nil, err
	}
	s, err := record.Open(ctx, repo, record.Options{Effect: workspace.GitProvider{Repo: repo}.Apply})
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	return fn(ctx, s)
}

func cmdReindex(args []string) (any, error) {
	return withStore("reindex", args, func(ctx context.Context, s *record.Store) (any, error) {
		r, err := s.Reindex(ctx)
		if err != nil {
			return nil, err
		}
		workspaces, err := restoreWorkspaces(ctx, s)
		if err != nil {
			return nil, err
		}
		digest, err := s.Index.Digest()
		if err != nil {
			return nil, err
		}
		return map[string]any{"report": r, "index_digest": digest, "workspace_recovery": workspaces}, nil
	})
}

func cmdRecover(args []string) (any, error) {
	return withStore("recover", args, func(ctx context.Context, s *record.Store) (any, error) {
		return s.Recover(ctx)
	})
}

func cmdStatus(args []string) (any, error) {
	return withStore("status", args, func(ctx context.Context, s *record.Store) (any, error) {
		subjects, err := s.Index.Subjects(ctx)
		if err != nil {
			return nil, err
		}
		digest, err := s.Index.Digest()
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
