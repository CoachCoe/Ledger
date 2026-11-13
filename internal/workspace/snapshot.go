package workspace

import (
	"slices"

	"golang.org/x/text/unicode/norm"

	"github.com/CoachCoe/Ledger/internal/apierr"
)

type Snapshot struct {
	Tree   string            `json:"tree"`
	Files  []string          `json:"files"`
	Status map[string]string `json:"status"`
	// OpHead is the operation log entry that recorded Tree, in a native
	// workspace.
	OpHead string `json:"-"`
}

// RecordFiles returns NFC paths for immutable manifests while the snapshot
// retains exact Git names for tree/blob lookup. Ambiguous aliases fail closed.
func (s Snapshot) RecordFiles() ([]string, error) {
	out := make([]string, 0, len(s.Files))
	seen := map[string]bool{}
	for _, p := range s.Files {
		n := norm.NFC.String(p)
		if seen[n] {
			return nil, apierr.New("path_alias", "multiple Git paths normalize to %q", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}
