package record

import (
	"context"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/envelope"
	"github.com/CoachCoe/Ledger/internal/index"
)

// Effect kinds.
const (
	EffectRef            = "ref"
	EffectWorktreeAdd    = "worktree_add"
	EffectWorktreeRemove = "worktree_remove"
)

// Effect is a frozen external side effect recorded before execution.
// After effects are cleanup: committed state remains visible if cleanup fails.
type Effect struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	Kind        string `json:"kind"`
	Path        string `json:"path,omitempty"`
	Base        string `json:"base,omitempty"`
	Ref         string `json:"ref,omitempty"`
	Old         string `json:"old,omitempty"`
	New         string `json:"new,omitempty"`
	After       bool   `json:"after"`
}

// EffectsFunc is a BuildFunc that also returns the operation's effects.
type EffectsFunc func(*index.Index) (any, []envelope.Object, []Effect, error)

// DoEffects prepares signed records and frozen external effects under the
// same lock. Recovery reuses the exact prepared bytes and ref preconditions.
func (s *Store) DoEffects(ctx context.Context, op Op, build EffectsFunc) (*Result, error) {
	return s.do(ctx, op, build)
}

func (s *Store) effects(ctx context.Context, list []Effect, after bool) error {
	for _, effect := range list {
		if effect.After != after {
			continue
		}
		if s.opts.Effect == nil {
			return apierr.New("effect_unavailable", "no handler for prepared %s", effect.Kind)
		}
		if err := s.opts.Effect(ctx, effect); err != nil {
			if after {
				return apierr.New("cleanup_pending", "records committed; cleanup pending: %v", err)
			}
			return err
		}
	}
	return nil
}

// SetEffectHandler configures recovery before the store is shared with callers.
func (s *Store) SetEffectHandler(fn func(context.Context, Effect) error) { s.opts.Effect = fn }
