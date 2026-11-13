package core

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/evidence"
	"github.com/CoachCoe/Ledger/internal/speculate"
)

// Scorer result files larger than this are a scorer failure.
const maxScorerResult = 1 << 20

// scorerLogLimit bounds the scorer output kept for error reports.
const scorerLogLimit = 8 << 10

// runScorer runs a command scorer in a scratch directory with a reduced
// environment: the definition's allowlist plus LEDGER_SCORER_INPUT (the
// frozen candidate JSON), LEDGER_SCORER_OUTPUT (where it writes
// {"scores": {...}}), LEDGER_REPOSITORY, LEDGER_RUN_ID and
// LEDGER_CANDIDATES, which holds a checkout of each candidate's tree named by
// its id. Like checks,
// it runs as the local user without isolation. Any failure is a structured
// error and never a winner.
func (s *Service) runScorer(ctx context.Context, rs *runState, input []byte, trees map[string]string) (map[string]int64, error) {
	def := rs.run.ScorerDefinition
	parent := filepath.Join(s.runtimeDir(), "speculate")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, rs.run.ID+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "input.json"), filepath.Join(dir, "scores.json")
	if err := os.WriteFile(in, input, 0o600); err != nil {
		return nil, err
	}
	env := []string{"LEDGER_SCORER_INPUT=" + in, "LEDGER_SCORER_OUTPUT=" + out, "LEDGER_REPOSITORY=" + s.repositoryDir(), "LEDGER_RUN_ID=" + rs.run.ID}
	candidates := filepath.Join(dir, "candidates")
	if err := os.Mkdir(candidates, 0o700); err != nil {
		return nil, err
	}
	for id, tree := range trees {
		if err := s.Provider.Workspaces.Checkout(tree, filepath.Join(candidates, id)); err != nil {
			return nil, err
		}
	}
	env = append(env, "LEDGER_CANDIDATES="+candidates)
	allowed := map[string]bool{}
	for _, name := range def.EnvAllowlist {
		allowed[name] = true
	}
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && allowed[name] && !strings.HasPrefix(name, "LEDGER_") {
			env = append(env, kv)
		}
	}
	log := evidence.NewBoundedOutput(scorerLogLimit)
	res := evidence.RunPhase(ctx, evidence.PhaseSpec{Command: *def.Command, Dir: dir, Env: env, Timeout: time.Duration(*def.TimeoutSeconds) * time.Second}, log)
	detail := func(e *apierr.Error) error {
		return e.With("output", string(log.Bytes())).With("output_truncated", log.Truncated())
	}
	switch {
	case res.Cancelled:
		return nil, apierr.New("scorer_failed", "scorer was cancelled")
	case res.TimedOut:
		return nil, detail(apierr.New("scorer_timeout", "scorer exceeded %d seconds", *def.TimeoutSeconds))
	case res.StartErr != nil:
		return nil, apierr.New("scorer_failed", "scorer could not start: %v", res.StartErr)
	case !res.OK():
		code := int64(-1)
		if res.ExitStatus != nil {
			code = *res.ExitStatus
		}
		return nil, detail(apierr.New("scorer_failed", "scorer exited with status %d", code).With("exit_status", code))
	}
	raw, err := readBounded(out, maxScorerResult)
	if err != nil {
		return nil, detail(apierr.New("scorer_failed", "scorer wrote no usable result: %v", err))
	}
	return speculate.ParseScores(raw, slices.Sorted(maps.Keys(trees)))
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("result exceeds the size limit")
	}
	return raw, nil
}
