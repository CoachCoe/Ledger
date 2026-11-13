package evidence

import (
	"context"
	"os"

	"github.com/CoachCoe/Ledger/internal/cow"
)

// NativeCheckouts checks a v2 manifest's tree out as a copy-on-write clone.
// There is no commit: the manifest names the tree it binds directly.
type NativeCheckouts struct{ Workspaces *cow.Manager }

func (n NativeCheckouts) Add(_ context.Context, path string, b Binding) error {
	return n.Workspaces.Checkout(b.Tree, path)
}

func (n NativeCheckouts) Tree(_ context.Context, path string, b Binding) (string, error) {
	return n.Workspaces.TrackedTree(path, b.Tree)
}

// Remove removes the clone. Prepare only ever gives it the run's own
// verify path, which nothing else writes.
func (n NativeCheckouts) Remove(_ context.Context, path string) error {
	return os.RemoveAll(path)
}
