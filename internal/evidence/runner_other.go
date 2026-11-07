//go:build !unix

package evidence

import (
	"context"
	"errors"
)

// RunPhase is unavailable: process-group control needs a Unix platform.
func RunPhase(_ context.Context, _ PhaseSpec, _ *BoundedOutput) PhaseResult {
	return PhaseResult{StartErr: errors.New("evidence execution requires a Unix platform")}
}
